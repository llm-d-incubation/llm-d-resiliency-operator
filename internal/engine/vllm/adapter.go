// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0
package vllm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/llm-d/llm-d-resiliency-operator/internal/engine"
)

type Adapter struct {
	Client *http.Client
	Model  string
}

func New(model string) *Adapter {
	return &Adapter{Client: &http.Client{Timeout: 10 * time.Second}, Model: model}
}

//nolint:gocritic // Unnamed returns follow nonamedreturns; callers need both body and HTTP status.
func (adapter *Adapter) request(
	ctx context.Context, method, url string, payload any,
) ([]byte, int, error) {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, 0, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, 0, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := adapter.Client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return b, resp.StatusCode, fmt.Errorf("HTTP %d: %.300s", resp.StatusCode, b)
	}
	return b, resp.StatusCode, nil
}

func (adapter *Adapter) EngineStatus(ctx context.Context, group engine.Group) []engine.RankStatus {
	result := make([]engine.RankStatus, len(group.Endpoints))
	var wg sync.WaitGroup
	for i, endpoint := range group.Endpoints {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status := engine.RankStatus{Rank: endpoint.Rank, Status: "unknown"}
			defer func() {
				if reason := adapter.confirmedProcessExit(ctx, endpoint); reason != "" {
					status.ProcessExitConfirmed = true
					if status.FaultInfo == "" {
						status.FaultInfo = reason
					}
				}
				result[i] = status
			}()
			b, _, err := adapter.request(ctx, "GET", endpoint.URL+"/v1/fault_tolerance/status", nil)
			if err != nil {
				status.Error = err.Error()
				return
			}
			var payload struct {
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
			if err := json.Unmarshal(b, &payload); err != nil {
				status.Error = err.Error()
				return
			}
			if payload.SchemaVersion != 1 || len(payload.Engines) != 1 ||
				payload.Engines[0].ID == nil || *payload.Engines[0].ID != endpoint.Rank {
				status.Error = "unexpected schema or rank identity"
				return
			}
			e := payload.Engines[0]
			if !slices.Contains([]string{"healthy", "unhealthy", "diagnosing", "dead"}, e.Status) {
				status.Error = "unknown engine state"
				return
			}
			status.Status, status.FaultInfo, status.Mask = e.Status, e.FaultInfo, e.Mask
			status.FTState, status.FTError, status.RequestID = e.FTState, e.FTError, e.RequestID
		}()
	}
	wg.Wait()
	return result
}

func (adapter *Adapter) confirmedProcessExit(ctx context.Context, endpoint engine.Endpoint) string {
	if endpoint.DiagnosticsURL == "" || endpoint.PodUID == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	body, _, err := adapter.request(ctx, http.MethodGet, endpoint.DiagnosticsURL, nil)
	if err != nil {
		return ""
	}
	var payload struct {
		SchemaVersion int    `json:"schema_version"`
		PodUID        string `json:"pod_uid"`
		Ranks         []struct {
			ID         *int   `json:"id"`
			EngineDead bool   `json:"engine_dead"`
			Reason     string `json:"reason"`
		} `json:"ranks"`
	}
	if json.Unmarshal(body, &payload) != nil || payload.SchemaVersion != 1 || payload.PodUID != endpoint.PodUID {
		return ""
	}
	for _, rank := range payload.Ranks {
		if rank.ID != nil && *rank.ID == endpoint.Rank && rank.EngineDead {
			return "Pod process observer confirmed engine loss: " + rank.Reason
		}
	}
	return ""
}

//nolint:gocritic // The interface passes an immutable round snapshot by value.
func (adapter *Adapter) ResumeEngine(ctx context.Context, group engine.Group, round engine.Round) ([]engine.Acceptance, error) {
	if len(round.Removed) != 0 {
		return nil, fmt.Errorf("retry cannot remove ranks")
	}
	return adapter.apply(ctx, group, round, "retry")
}

//nolint:gocritic // The interface passes an immutable round snapshot by value.
func (adapter *Adapter) ScaleDown(ctx context.Context, group engine.Group, round engine.Round) ([]engine.Acceptance, error) {
	if len(round.Removed) == 0 {
		return nil, fmt.Errorf("scale_down requires removed ranks")
	}
	return adapter.apply(ctx, group, round, "scale_down")
}

//nolint:gocritic // Fanout captures an immutable round; all requests share its identity.
func (adapter *Adapter) apply(
	ctx context.Context, group engine.Group, round engine.Round, instruction string,
) ([]engine.Acceptance, error) {
	if round.ID == "" || group.ID == "" || round.GroupID != group.ID || len(round.Participants) == 0 {
		return nil, fmt.Errorf("invalid round identity or empty participant set")
	}
	endpoints := make(map[int]string)
	for _, e := range group.Endpoints {
		if e.Rank < 0 {
			return nil, fmt.Errorf("invalid endpoint rank %d", e.Rank)
		}
		if _, ok := endpoints[e.Rank]; ok {
			return nil, fmt.Errorf("duplicate endpoint rank %d", e.Rank)
		}
		endpoints[e.Rank] = e.URL
	}
	seen := map[int]bool{}
	for _, rank := range append(slices.Clone(round.Participants), round.Removed...) {
		if seen[rank] || endpoints[rank] == "" {
			return nil, fmt.Errorf("invalid or duplicate rank %d", rank)
		}
		seen[rank] = true
	}
	params := map[string]any{}
	if instruction == "scale_down" {
		params["removed_dp_ranks"] = round.Removed
	}
	if round.MasterIP != "" || round.StorePort != 0 {
		if round.MasterIP == "" || round.StorePort < 1 || round.StorePort > 65535 {
			return nil, fmt.Errorf("invalid replacement store")
		}
		params["dp_master_ip"], params["dp_store_port"] = round.MasterIP, round.StorePort
	}
	payload := map[string]any{"instruction": instruction, "request_id": round.ID, "params": params}
	result := make([]engine.Acceptance, len(round.Participants))
	var wg sync.WaitGroup
	for i, rank := range round.Participants {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, code, err := adapter.request(ctx, "POST", endpoints[rank]+"/v1/fault_tolerance/apply", payload)
			result[i] = engine.Acceptance{Rank: rank, Accepted: err == nil && code == http.StatusAccepted}
			if err != nil {
				result[i].Error = err.Error()
			} else if code != http.StatusAccepted {
				result[i].Error = fmt.Sprintf("expected 202, got %d", code)
			}
		}()
	}
	wg.Wait()
	for _, ack := range result {
		if !ack.Accepted {
			return result, fmt.Errorf("partial or rejected dispatch; round must be reconciled")
		}
	}
	return result, nil
}

func (adapter *Adapter) Verify(ctx context.Context, group engine.Group, ranks []int) error {
	if len(ranks) == 0 {
		return fmt.Errorf("cannot verify an empty serving set")
	}
	for _, rank := range ranks {
		i := slices.IndexFunc(group.Endpoints, func(e engine.Endpoint) bool { return e.Rank == rank })
		if i < 0 {
			return fmt.Errorf("missing rank %d", rank)
		}
		payload := map[string]any{
			"model": adapter.Model, "prompt": "One plus one equals", "max_tokens": 8, "temperature": 0,
		}
		b, _, err := adapter.request(ctx, http.MethodPost, group.Endpoints[i].URL+"/v1/completions", payload)
		if err != nil {
			return fmt.Errorf("rank %d inference: %w", rank, err)
		}
		var response struct {
			Choices []struct {
				Text string `json:"text"`
			} `json:"choices"`
			Usage struct {
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(b, &response) != nil || len(response.Choices) == 0 ||
			response.Choices[0].Text == "" || response.Usage.CompletionTokens <= 0 {
			return fmt.Errorf("rank %d returned no generated completion", rank)
		}
	}
	return nil
}

var _ engine.Adapter = (*Adapter)(nil)
