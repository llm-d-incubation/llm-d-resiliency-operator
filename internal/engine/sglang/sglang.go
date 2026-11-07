// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

// Package sglang is the SGLang engine. See docs/components/engines/sglang.md.
package sglang

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/llm-d/llm-d-resiliency-operator/internal/engine"
	"github.com/llm-d/llm-d-resiliency-operator/internal/workload"
)

type Options struct {
	APIPort     int
	RanksPerPod int
	Timeout     time.Duration
}

func (options *Options) BindFlags(flags *flag.FlagSet) {
	flags.IntVar(&options.APIPort, "sglang-api-port", 30000, "HTTP port of the SGLang server on the first Pod")
	flags.IntVar(&options.RanksPerPod, "sglang-ranks-per-pod", 1, "data parallel ranks in each Pod")
	flags.DurationVar(&options.Timeout, "sglang-timeout", 5*time.Second, "timeout of one status or apply request")
}

func (options *Options) New() (engine.Engine, error) {
	if options.RanksPerPod < 1 || options.Timeout <= 0 || options.APIPort < 1 || options.APIPort > 65535 {
		return nil, errors.New("invalid --sglang-* flags")
	}
	return &Engine{options: *options, client: &http.Client{}}, nil
}

// Engine drives one SGLang server. Unlike vLLM, SGLang has one control endpoint for all of its DP ranks: the
// HTTP server on the first Pod.
type Engine struct {
	options Options
	client  *http.Client
}

func (*Engine) Capabilities() engine.Capabilities { return engine.Capabilities{} }

// status is what the first Pod reported, shared by every member.
type status struct {
	Address   string
	Reached   bool
	Ranks     []string // the state of each DP rank: healthy, unhealthy or dead
	RequestID string   // the last FT request SGLang finished
	FTError   string   // why it failed, if it did
	Error     string
}

func statusOf(observation engine.Observation) status {
	if found, ok := observation.Engine.(status); ok {
		return found
	}
	return status{}
}

func (eng *Engine) Observe(ctx context.Context, pods []workload.Pod) engine.Observation {
	observation := engine.Observation{}
	var numbers []int
	found := status{Error: "first Pod not running"}
	for index := range pods {
		pod := &pods[index]
		if pod.State == workload.PodTerminating || pod.IP == "" {
			continue
		}
		if pod.Index == 0 {
			found.Address = net.JoinHostPort(pod.IP, strconv.Itoa(eng.options.APIPort))
		}
		for local := range eng.options.RanksPerPod {
			number := pod.Index*eng.options.RanksPerPod + local
			numbers = append(numbers, number)
			observation.Members = append(observation.Members, engine.Member{
				Pod: workload.PodRef{Name: pod.Name, UID: pod.UID}, PodIdentity: pod.Identity, Name: fmt.Sprintf("dp%d", number),
			})
		}
	}
	if found.Address != "" {
		eng.read(ctx, &found)
	}
	for index := range observation.Members {
		member, number := &observation.Members[index], numbers[index]
		switch {
		case !found.Reached:
			member.Unknown, member.Detail = true, "unreachable: "+found.Error
		case number >= len(found.Ranks):
			member.Unknown, member.Detail = true, fmt.Sprintf("not in the status of %d ranks", len(found.Ranks))
		default:
			state := found.Ranks[number]
			member.Serving, member.Terminal, member.Detail = state == "healthy", state == "dead", state
			if found.RequestID != "" {
				member.Detail += fmt.Sprintf(", last FT request %s %s", found.RequestID, found.FTError)
			}
		}
	}
	observation.Engine = found
	return observation
}

func (eng *Engine) read(ctx context.Context, target *status) {
	var body struct {
		SchemaVersion int `json:"schema_version"`
		Engines       []struct {
			ID     *int   `json:"id"`
			Status string `json:"status"`
		} `json:"engines"`
		RequestID string `json:"last_ft_request_id"`
		FTError   string `json:"ft_error"`
	}
	ctx, cancel := context.WithTimeout(ctx, eng.options.Timeout)
	defer cancel()
	data, _, err := eng.call(ctx, http.MethodGet, target.Address, "/v1/fault_tolerance/status", nil)
	if err == nil {
		err = json.Unmarshal(data, &body)
	}
	if err == nil && body.SchemaVersion != 1 {
		err = fmt.Errorf("unexpected status %.200s", data)
	}
	for index, e := range body.Engines {
		if err == nil && (e.ID == nil || *e.ID != index) {
			err = fmt.Errorf("engine %d out of order in %.200s", index, data)
		}
		target.Ranks = append(target.Ranks, e.Status)
	}
	if err != nil {
		target.Ranks, target.Error = nil, err.Error()
		return
	}
	target.Reached, target.RequestID, target.FTError = true, body.RequestID, body.FTError
}

// Probe sends one short request, so the ranks run a collective and notice a dead peer.
func (eng *Engine) Probe(ctx context.Context, observation engine.Observation) error {
	found := statusOf(observation)
	if !found.Reached {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, eng.options.Timeout)
	defer cancel()
	_, _, err := eng.call(ctx, http.MethodPost, found.Address, "/v1/completions", map[string]any{"prompt": "ping", "max_tokens": 1})
	return err
}

//nolint:gocritic // The interface passes the reconfig by value.
func (eng *Engine) Apply(
	ctx context.Context, observation engine.Observation, reconfig engine.Reconfig,
) ([]engine.Acceptance, error) {
	if len(reconfig.Add) > 0 {
		return nil, errors.New("SGLang cannot admit members")
	}
	found := statusOf(observation)
	if !found.Reached {
		return nil, errors.New("the first Pod cannot be reached: " + found.Error)
	}
	members := membersOf(observation)
	for _, id := range reconfig.Participants {
		if member := members[id]; member == nil || member.Unknown {
			return nil, fmt.Errorf("%s cannot be reached", id)
		}
	}
	for _, id := range reconfig.Remove {
		if member := members[id]; member == nil || !member.Terminal {
			return nil, fmt.Errorf("%s is not a terminal member of the observation", id)
		}
	}
	payload := map[string]any{"instruction": "retry", "request_id": reconfig.ID}
	if len(reconfig.Remove) > 0 {
		removed := make([]int, 0, len(reconfig.Remove))
		for _, id := range reconfig.Remove {
			number, err := rankNumber(id)
			if err != nil {
				return nil, err
			}
			removed = append(removed, number)
		}
		payload["instruction"], payload["params"] = "scale_down", map[string]any{"removed_dp_ranks": removed}
	}
	// SGLang already finished this reconfig: sending it again would run it twice.
	var err error
	if found.RequestID != reconfig.ID {
		callCtx, cancel := context.WithTimeout(ctx, eng.options.Timeout)
		defer cancel()
		var code int
		if _, code, err = eng.call(callCtx, http.MethodPost, found.Address, "/v1/fault_tolerance/apply", payload); err == nil &&
			code != http.StatusAccepted {
			err = fmt.Errorf("HTTP %d, want 202", code)
		}
	}
	// One request reconfigures every rank, so they all accept it or none does.
	acceptances := make([]engine.Acceptance, len(reconfig.Participants))
	for index, id := range reconfig.Participants {
		acceptances[index] = engine.Acceptance{Member: id, Accepted: err == nil}
		if err != nil {
			acceptances[index].Error = err.Error()
		}
	}
	if err != nil {
		return acceptances, fmt.Errorf("SGLang did not accept %s: %w", payload["instruction"], err)
	}
	return acceptances, nil
}

func (eng *Engine) Verify(ctx context.Context, observation engine.Observation, members []engine.MemberID) error {
	if len(members) == 0 {
		return errors.New("nothing to verify")
	}
	found := statusOf(observation)
	if !found.Reached {
		return errors.New("the first Pod cannot be reached: " + found.Error)
	}
	errs := make([]error, len(members))
	var wg sync.WaitGroup
	for index, id := range members {
		number, err := rankNumber(id)
		if err != nil {
			errs[index] = err
			continue
		}
		wg.Go(func() {
			payload := map[string]any{
				"prompt": "One plus one equals", "max_tokens": 8, "temperature": 0, "ignore_eos": true, "routed_dp_rank": number,
			}
			var response struct {
				Usage struct {
					CompletionTokens int `json:"completion_tokens"`
				} `json:"usage"`
			}
			body, _, err := eng.call(ctx, http.MethodPost, found.Address, "/v1/completions", payload)
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

func rankNumber(id engine.MemberID) (int, error) {
	name, _, _ := strings.Cut(string(id), "@")
	number, err := strconv.Atoi(strings.TrimPrefix(name, "dp"))
	if err != nil || !strings.HasPrefix(name, "dp") || number < 0 {
		return 0, fmt.Errorf("member %s is not an SGLang rank", id)
	}
	return number, nil
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
	request, err := http.NewRequestWithContext(ctx, method, "http://"+address+path, body)
	if err != nil {
		return nil, 0, err
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
