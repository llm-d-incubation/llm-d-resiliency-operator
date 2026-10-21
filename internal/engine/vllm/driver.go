// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

package vllm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"sync"

	"github.com/llm-d/llm-d-resiliency-operator/internal/engine"
	"github.com/llm-d/llm-d-resiliency-operator/internal/workload"
)

func (eng *Engine) Observe(ctx context.Context, pods []workload.Pod) engine.Observation {
	observation := engine.Observation{}
	found := ranks{}
	for index := range pods {
		pod := &pods[index]
		if pod.State == workload.PodTerminating || pod.IP == "" {
			continue
		}
		for local := range eng.options.RanksPerPod {
			number := pod.Index*eng.options.RanksPerPod + local
			member := engine.Member{
				Pod: workload.PodRef{Name: pod.Name, UID: pod.UID}, PodIdentity: pod.Identity, Name: fmt.Sprintf("dp%d", number),
			}
			observation.Members = append(observation.Members, member)
			found[member.ID()] = &rank{
				Rank: number, PodIP: pod.IP, Address: net.JoinHostPort(pod.IP, strconv.Itoa(eng.options.APIPort+local)),
			}
		}
	}
	var wg sync.WaitGroup
	for _, r := range found {
		wg.Go(func() { eng.read(ctx, r) })
	}
	wg.Wait()
	for index := range observation.Members {
		member := &observation.Members[index]
		r := found[member.ID()]
		member.Serving, member.Terminal, member.Unknown, member.Detail = r.serving(), r.dead(), !r.Reached, r.detail()
	}
	observation.Engine = found
	return observation
}

func (eng *Engine) read(ctx context.Context, target *rank) {
	var status struct {
		SchemaVersion int `json:"schema_version"`
		Engines       []struct {
			ID        *int   `json:"id"`
			Status    string `json:"status"`
			FaultInfo string `json:"fault_info"`
			Mask      []int  `json:"mask"`
			FTState   string `json:"ft_state"`
			FTError   string `json:"ft_error"`
			RequestID string `json:"last_ft_request_id"`
		} `json:"engines"`
	}
	ctx, cancel := context.WithTimeout(ctx, eng.options.Timeout)
	defer cancel()
	body, _, err := eng.call(ctx, http.MethodGet, target.Address, "/v1/fault_tolerance/status", nil)
	if err == nil {
		err = json.Unmarshal(body, &status)
	}
	switch {
	case err != nil:
	case status.SchemaVersion != 1 || len(status.Engines) != 1 || status.Engines[0].ID == nil:
		err = fmt.Errorf("unexpected status %.200s", body)
	case *status.Engines[0].ID != target.Rank:
		err = fmt.Errorf("serves rank %d, not %d", *status.Engines[0].ID, target.Rank)
	}
	if err != nil {
		target.Error = err.Error()
		return
	}
	e := status.Engines[0]
	target.Status, target.FaultInfo, target.Mask = e.Status, e.FaultInfo, e.Mask
	target.FTState, target.FTError, target.RequestID = e.FTState, e.FTError, e.RequestID
	_, target.Health, err = eng.call(ctx, http.MethodGet, target.Address, "/health", nil)
	if target.Health == 0 {
		target.Error = err.Error()
		return
	}
	target.Reached = true
}

func (eng *Engine) Probe(ctx context.Context, observation engine.Observation) error {
	payload := map[string]any{"prompt": "ping", "max_tokens": 1}
	var mu sync.Mutex
	var errs []error
	var wg sync.WaitGroup
	for id, target := range ranksOf(observation) {
		if target.serving() {
			wg.Go(func() {
				probeCtx, cancel := context.WithTimeout(ctx, eng.options.Timeout)
				defer cancel()
				if _, _, err := eng.call(probeCtx, http.MethodPost, target.Address, "/v1/completions", payload); err != nil {
					mu.Lock()
					errs = append(errs, fmt.Errorf("probe %s: %w", id, err))
					mu.Unlock()
				}
			})
		}
	}
	wg.Wait()
	return errors.Join(errs...)
}

//nolint:gocritic // The interface passes the reconfig by value.
func (eng *Engine) Apply(
	ctx context.Context, observation engine.Observation, reconfig engine.Reconfig,
) ([]engine.Acceptance, error) {
	if len(reconfig.Add) > 0 {
		err := errors.New("vLLM member admission is unsupported: FT leaves inactive slots; " +
			"replacement launch and slot rejoin are unavailable")
		if len(reconfig.Remove) == 0 {
			return nil, &engine.ApplyError{Scope: engine.Joiners, Err: err}
		}
		return nil, err
	}
	found := ranksOf(observation)
	for _, id := range reconfig.Participants {
		if r := found[id]; r == nil || !r.Reached {
			return nil, fmt.Errorf("%s cannot be reached", id)
		}
	}
	members := membersOf(observation)
	for _, id := range reconfig.Remove {
		if member := members[id]; member == nil || !member.Terminal {
			return nil, fmt.Errorf("%s is not a terminal member of the observation", id)
		}
	}
	instruction, params := "retry", map[string]any{}
	if len(reconfig.Remove) > 0 {
		var err error
		instruction = "scale_down"
		if params, err = eng.scaleDown(found, reconfig); err != nil {
			return nil, err
		}
	}
	payload := map[string]any{"instruction": instruction, "request_id": reconfig.ID, "params": params}
	acceptances := make([]engine.Acceptance, len(reconfig.Participants))
	var wg sync.WaitGroup
	for index, id := range reconfig.Participants {
		acceptances[index].Member = id
		r := found[id]
		wg.Go(func() {
			callCtx, cancel := context.WithTimeout(ctx, eng.options.Timeout)
			defer cancel()
			_, code, err := eng.call(callCtx, http.MethodPost, r.Address, "/v1/fault_tolerance/apply", payload)
			acceptances[index].Accepted = err == nil && code == http.StatusAccepted
			if err != nil {
				acceptances[index].Error = err.Error()
			} else if code != http.StatusAccepted {
				acceptances[index].Error = fmt.Sprintf("HTTP %d, want 202", code)
			}
		})
	}
	wg.Wait()
	for _, acceptance := range acceptances {
		if !acceptance.Accepted {
			return acceptances, fmt.Errorf("%s did not accept %s: %s", acceptance.Member, instruction, acceptance.Error)
		}
	}
	return acceptances, nil
}

//nolint:gocritic // The interface passes the reconfig by value.
func (eng *Engine) scaleDown(found ranks, reconfig engine.Reconfig) (map[string]any, error) {
	removed, err := numbers(reconfig.Remove)
	if err != nil {
		return nil, err
	}
	alive, err := numbers(slices.Concat(reconfig.Participants, reconfig.Remove))
	if err != nil {
		return nil, err
	}
	params := map[string]any{"removed_dp_ranks": removed}
	if !slices.Contains(removed, slices.Min(alive)) {
		return params, nil
	}
	master := lowest(found, reconfig.Participants)
	if master == nil {
		return nil, errors.New("no participant can host the FT store")
	}
	params["dp_master_ip"], params["dp_store_port"] = master.PodIP, eng.options.StorePort+master.Rank
	return params, nil
}

func lowest(found ranks, ids []engine.MemberID) *rank {
	var result *rank
	for _, id := range ids {
		if candidate := found[id]; candidate != nil && (result == nil || candidate.Rank < result.Rank) {
			result = candidate
		}
	}
	return result
}

func numbers(ids []engine.MemberID) ([]int, error) {
	result := make([]int, 0, len(ids))
	for _, id := range ids {
		number, err := rankNumber(id)
		if err != nil {
			return nil, err
		}
		result = append(result, number)
	}
	slices.Sort(result)
	return result, nil
}

func (eng *Engine) Verify(ctx context.Context, observation engine.Observation, members []engine.MemberID) error {
	if len(members) == 0 {
		return errors.New("nothing to verify")
	}
	found := ranksOf(observation)
	payload := map[string]any{"prompt": "One plus one equals", "max_tokens": 8, "temperature": 0, "ignore_eos": true}
	errs := make([]error, len(members))
	var wg sync.WaitGroup
	for index, id := range members {
		r := found[id]
		if r == nil {
			errs[index] = fmt.Errorf("%s: not observed", id)
			continue
		}
		wg.Go(func() {
			var response struct {
				Usage struct {
					CompletionTokens int `json:"completion_tokens"`
				} `json:"usage"`
			}
			body, _, err := eng.call(ctx, http.MethodPost, r.Address, "/v1/completions", payload)
			if err == nil {
				err = json.Unmarshal(body, &response)
			}
			if err == nil && response.Usage.CompletionTokens != 8 {
				err = fmt.Errorf("generated %d tokens, want 8", response.Usage.CompletionTokens)
			}
			if err != nil {
				errs[index] = fmt.Errorf("%s: %w", id, err)
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

//nolint:gocritic // Callers need the body, the status code and the error.
func (eng *Engine) call(ctx context.Context, method, address, path string, payload any) ([]byte, int, error) {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, 0, err
		}
		body = bytes.NewReader(data)
	}
	scheme := "http"
	if eng.options.HTTPS {
		scheme = "https"
	}
	request, err := http.NewRequestWithContext(ctx, method, scheme+"://"+address+path, body)
	if err != nil {
		return nil, 0, err
	}
	if eng.apiKey != "" {
		request.Header.Set("Authorization", "Bearer "+eng.apiKey)
	}
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := eng.client.Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err == nil && (response.StatusCode < 200 || response.StatusCode > 299) {
		err = fmt.Errorf("HTTP %d: %.200s", response.StatusCode, data)
	}
	return data, response.StatusCode, err
}
