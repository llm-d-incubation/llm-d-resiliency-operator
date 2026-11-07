// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

package sglang_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/llm-d/llm-d-resiliency-operator/internal/engine"
	"github.com/llm-d/llm-d-resiliency-operator/internal/engine/sglang"
	"github.com/llm-d/llm-d-resiliency-operator/internal/workload"
)

// fakeServer follows the FT API of sgl-project/sglang#40078: one status and apply endpoint for all DP ranks.
// Apply finishes at once.
type fakeServer struct {
	mu        sync.Mutex
	ranks     []string
	requestID string
	applied   []string
}

type applyRequest struct {
	Instruction string `json:"instruction"`
	RequestID   string `json:"request_id"`
	Params      struct {
		Removed []int `json:"removed_dp_ranks"`
	} `json:"params"`
}

func (fake *fakeServer) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	var body any
	switch request.URL.Path {
	case "/v1/fault_tolerance/status":
		engines := make([]map[string]any, 0, len(fake.ranks))
		for id, state := range fake.ranks {
			engines = append(engines, map[string]any{"id": id, "status": state})
		}
		body = map[string]any{"schema_version": 1, "engines": engines, "last_ft_request_id": fake.requestID}
	case "/v1/fault_tolerance/apply":
		var apply applyRequest
		if err := json.NewDecoder(request.Body).Decode(&apply); err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		fake.applied = append(fake.applied, apply.Instruction+" "+apply.RequestID+" "+strconv.Itoa(len(apply.Params.Removed)))
		for id := range fake.ranks {
			if slices.Contains(apply.Params.Removed, id) {
				fake.ranks[id] = "dead"
			} else if fake.ranks[id] == "unhealthy" {
				fake.ranks[id] = "healthy"
			}
		}
		fake.requestID = apply.RequestID
		writer.WriteHeader(http.StatusAccepted)
		return
	case "/v1/completions":
		body = map[string]any{"usage": map[string]any{"completion_tokens": 8}}
	default:
		http.NotFound(writer, request)
		return
	}
	if err := json.NewEncoder(writer).Encode(body); err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
	}
}

func (fake *fakeServer) set(rank int, state string) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.ranks[rank] = state
}

func TestDeadRankIsRemoved(t *testing.T) {
	fake := &fakeServer{ranks: []string{"healthy", "healthy", "healthy", "healthy"}}
	server := httptest.NewServer(fake)
	defer server.Close()
	host, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	options := sglang.Options{RanksPerPod: 2, Timeout: time.Second}
	if options.APIPort, err = strconv.Atoi(port); err != nil {
		t.Fatal(err)
	}
	eng, err := options.New()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// Only the first Pod serves HTTP; the second one's address is never used.
	pods := []workload.Pod{
		{Name: "lws-0", UID: "a", IP: host, Index: 0, Identity: "idA"},
		{Name: "lws-0-1", UID: "b", IP: "192.0.2.1", Index: 1, Identity: "idB"},
	}
	admitted := []engine.MemberID{"dp0@idA", "dp1@idA", "dp2@idB", "dp3@idB"}

	if plan := eng.Plan(eng.Observe(ctx, pods), admitted, nil); plan.Kind != engine.Healthy {
		t.Fatalf("healthy group: %+v", plan)
	}

	fake.set(1, "unhealthy")
	fake.set(3, "dead")
	observation := eng.Observe(ctx, pods)
	plan := eng.Plan(observation, admitted, nil)
	if plan.Kind != engine.Change || !slices.Equal(plan.Remove, []engine.MemberID{"dp3@idB"}) {
		t.Fatalf("dead rank: %+v", plan)
	}
	reconfig := engine.Reconfig{ID: "iro-1", Participants: plan.Participants, Remove: plan.Remove}
	if progress := eng.Progress(observation, reconfig); progress.State != engine.Running {
		t.Fatalf("before apply: %+v", progress)
	}
	if _, err := eng.Apply(ctx, observation, reconfig); err != nil {
		t.Fatal(err)
	}
	observation = eng.Observe(ctx, pods)
	if progress := eng.Progress(observation, reconfig); progress.State != engine.Succeeded {
		t.Fatalf("after apply: %+v", progress)
	}
	// A resend of a finished reconfig is accepted without running it twice.
	if _, err := eng.Apply(ctx, observation, reconfig); err != nil || !slices.Equal(fake.applied, []string{"scale_down iro-1 1"}) {
		t.Fatalf("resend: %v, applied %v", err, fake.applied)
	}
	if err := eng.Verify(ctx, observation, reconfig.Participants); err != nil {
		t.Fatal(err)
	}
}
