// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0
package vllm_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-resiliency-operator/internal/engine"
	"github.com/llm-d-incubation/llm-d-resiliency-operator/internal/engine/vllm"
)

type command struct {
	Instruction string `json:"instruction"`
	RequestID   string `json:"request_id"`
	Params      struct {
		Removed []int  `json:"removed_dp_ranks"`
		Master  string `json:"dp_master_ip"`
		Port    int    `json:"dp_store_port"`
	} `json:"params"`
}

func TestScaleDownFansOutOneRoundBeforeWaiting(t *testing.T) {
	// Coordinated engine commands can deadlock if the client waits for one
	// participant to finish before dispatching the rest.
	var mu sync.Mutex
	var commands []command
	ready := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p command
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Error(err)
		}
		mu.Lock()
		commands = append(commands, p)
		if len(commands) == 3 {
			close(ready)
		}
		mu.Unlock()
		select {
		case <-ready:
			w.WriteHeader(http.StatusAccepted)
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	a := vllm.New()
	a.Client.Timeout = time.Second
	g := engine.Group{ID: "group"}
	for i := range 4 {
		g.Endpoints = append(g.Endpoints, engine.Endpoint{Rank: i, URL: server.URL})
	}
	round := engine.Round{ID: "one-round", GroupID: g.ID, Participants: []int{0, 2, 3}, Removed: []int{1}}
	acks, err := a.ScaleDown(context.Background(), g, round)
	if err != nil || len(acks) != 3 {
		t.Fatalf("dispatch: %v %v", acks, err)
	}
	for _, p := range commands {
		if p.RequestID != "one-round" || p.Instruction != "scale_down" {
			t.Fatalf("mismatched round: %v", p)
		}
		removed := p.Params.Removed
		if len(removed) != 1 || removed[0] != 1 {
			t.Fatalf("renumbered removed ranks: %v", p)
		}
	}
}

func TestAcceptanceDoesNotHideSubsequentFTFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		write(t, w, `{"schema_version":1,"engines":[{"id":0,"status":"unhealthy",`+
			`"ft_state":"failed","ft_error":"status is HEALTHY","last_ft_request_id":"r"}]}`)
	}))
	defer server.Close()
	a := vllm.New()
	g := engine.Group{ID: "g", Endpoints: []engine.Endpoint{{Rank: 0, URL: server.URL}}}
	_, err := a.ResumeEngine(context.Background(), g, engine.Round{ID: "r", GroupID: "g", Participants: []int{0}})
	if err != nil {
		t.Fatal(err)
	}
	s := a.EngineStatus(context.Background(), g)[0]
	if s.FTState != "failed" || s.RequestID != "r" || s.FTError == "" {
		t.Fatalf("lost background failure: %+v", s)
	}
}

func TestStatusRejectsMisroutedOrMissingEngine(t *testing.T) {
	for _, payload := range []string{
		`{"schema_version":1,"engines":[]}`,
		`{"schema_version":1,"engines":[{"id":3,"status":"healthy"}]}`,
		`{"schema_version":2,"engines":[{"id":0,"status":"healthy"}]}`,
		`{"schema_version":1,"engines":[{"status":"healthy"}]}`,
		`{"schema_version":1,"engines":[{"id":0,"status":"ready"}]}`,
		`{"schema_version":1,"engines":[{"id":0,"status":"healthy"},{"id":1,"status":"healthy"}]}`,
	} {
		t.Run(payload, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { write(t, w, payload) }))
			defer srv.Close()
			group := engine.Group{Endpoints: []engine.Endpoint{{Rank: 0, URL: srv.URL}}}
			s := vllm.New().EngineStatus(context.Background(), group)[0]
			if s.Status != "unknown" || s.Error == "" {
				t.Fatalf("untrusted endpoint admitted: %+v", s)
			}
		})
	}
}

func TestProcessExitEvidenceIsPodScopedAndPreservesNativeServingState(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		uid    string
		dead   bool
		native string
		want   string
	}{
		{"engine exited", "pod-a", true, "", "unknown"},
		{"API only exited", "pod-a", false, "", "unknown"},
		{"stale Pod evidence", "old-pod", true, "", "unknown"},
		{"health lags worker exit", "pod-a", true, "healthy", "healthy"},
		{"healthy worker", "pod-a", false, "healthy", "healthy"},
		{"healthy with stale Pod evidence", "old-pod", true, "healthy", "healthy"},
		{"exit retains FT failure", "pod-a", true, "unhealthy", "unhealthy"},
		{"native terminal death", "pod-a", false, "dead", "dead"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.URL.Path != "/processes" {
					if scenario.native != "" {
						extra := ""
						if scenario.native == "unhealthy" {
							extra = `,"ft_state":"failed","ft_error":"collective failed"`
						}
						write(t, w, `{"schema_version":1,"engines":[{"id":1,"status":"`+scenario.native+`"`+extra+`}]}`)
						return
					}
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				payload := map[string]any{
					"schema_version": 1, "pod_uid": scenario.uid,
					"ranks": []map[string]any{{"id": 1, "engine_dead": scenario.dead, "reason": "EngineCore exited"}},
				}
				if err := json.NewEncoder(w).Encode(payload); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			group := engine.Group{Endpoints: []engine.Endpoint{{
				Rank: 1, URL: server.URL, PodUID: "pod-a", DiagnosticsURL: server.URL + "/processes",
			}}}
			status := vllm.New().EngineStatus(context.Background(), group)[0]
			if status.Status != scenario.want {
				t.Fatalf("unexpected classification: %+v", status)
			}
			confirmedExit := scenario.dead && scenario.uid == "pod-a"
			if status.ProcessExitConfirmed != confirmedExit {
				t.Fatalf("process evidence must be independent of native health: %+v", status)
			}
			if scenario.native == "" && status.Error == "" {
				t.Fatal("process evidence hid the unavailable native status")
			}
			if scenario.native == "unhealthy" && (status.FTState != "failed" || status.FTError == "") {
				t.Fatal("process evidence lost the terminal FT failure")
			}
		})
	}
}

func TestInvalidRoundNeverDispatches(t *testing.T) {
	for _, round := range []engine.Round{
		{GroupID: "g", Participants: []int{0}},
		{ID: "r", GroupID: "old", Participants: []int{0}},
		{ID: "r", GroupID: "g", Participants: []int{0, 0}},
		{ID: "r", GroupID: "g", Participants: []int{0}, Removed: []int{0}},
		{ID: "r", GroupID: "g", Participants: []int{1}},
		{ID: "r", GroupID: "g", Participants: []int{0}, MasterIP: "10.0.0.1"},
	} {
		a := vllm.New()
		g := engine.Group{ID: "g", Endpoints: []engine.Endpoint{{Rank: 0, URL: "http://must-not-be-contacted.invalid"}}}
		if _, err := a.ResumeEngine(context.Background(), g, round); err == nil {
			t.Fatalf("accepted invalid round %+v", round)
		}
	}
}

func TestPartialDispatchRetainsEachAcceptance(t *testing.T) {
	for _, code := range []int{http.StatusOK, http.StatusConflict, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload command
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				if payload.Instruction != "scale_down" || payload.RequestID != "round" ||
					payload.Params.Master != "10.0.0.1" || payload.Params.Port != 29600 {
					t.Errorf("invalid replacement store command: %+v", payload)
				}
				if strings.HasPrefix(r.URL.Path, "/rank1/") {
					w.WriteHeader(http.StatusAccepted)
				} else {
					w.WriteHeader(code)
				}
			}))
			defer server.Close()
			group := engine.Group{ID: "g", Endpoints: []engine.Endpoint{
				{Rank: 0, URL: server.URL + "/rank0"},
				{Rank: 1, URL: server.URL + "/rank1"},
				{Rank: 3, URL: server.URL + "/rank3"},
			}}
			round := engine.Round{
				ID: "round", GroupID: "g", Participants: []int{1, 3}, Removed: []int{0},
				MasterIP: "10.0.0.1", StorePort: 29600,
			}
			acks, err := vllm.New().ScaleDown(context.Background(), group, round)
			if err == nil || len(acks) != 2 || !acks[0].Accepted || acks[1].Accepted || acks[1].Error == "" {
				t.Fatalf("lost partial acceptance: %+v, %v", acks, err)
			}
		})
	}
}

func TestInvalidTopologyNeverDispatches(t *testing.T) {
	endpoint := engine.Endpoint{Rank: 0, URL: "http://must-not-be-contacted.invalid"}
	for _, group := range []engine.Group{
		{Endpoints: []engine.Endpoint{endpoint}},
		{ID: "g", Endpoints: []engine.Endpoint{endpoint, endpoint}},
		{ID: "g", Endpoints: []engine.Endpoint{{Rank: -1, URL: endpoint.URL}}},
	} {
		round := engine.Round{ID: "r", GroupID: group.ID, Participants: []int{group.Endpoints[0].Rank}}
		if _, err := vllm.New().ResumeEngine(context.Background(), group, round); err == nil {
			t.Fatalf("accepted invalid topology: %+v", group)
		}
	}
}

func TestVerifyRequiresGeneratedTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-IRO-Verification") != "" {
			t.Error("verification must use the ordinary inference path")
		}
		write(t, w, `{"choices":[]}`)
	}))
	defer srv.Close()
	group := engine.Group{Endpoints: []engine.Endpoint{{Rank: 0, URL: srv.URL}}}
	err := vllm.New().Verify(context.Background(), group, []int{0})
	if err == nil || !strings.Contains(err.Error(), "no generated completion") {
		t.Fatalf("accepted empty inference: %v", err)
	}
}

func TestVerifyUsesRuntimeBaseModelForEveryRank(t *testing.T) {
	var completions atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if (request.URL.Path != "/rank0/v1/completions" && request.URL.Path != "/rank1/v1/completions") ||
			request.Method != http.MethodPost {
			t.Errorf("unexpected verification request: %s %s", request.Method, request.URL.Path)
		}
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if _, configured := payload["model"]; configured {
			t.Error("verification must let vLLM select its base model")
		}
		if request.Header.Get("X-IRO-Verification") != "" {
			t.Error("verification must use the ordinary inference path")
		}
		completions.Add(1)
		write(t, w, `{"choices":[{"text":"2"}],"usage":{"completion_tokens":1}}`)
	}))
	defer server.Close()
	group := engine.Group{Endpoints: []engine.Endpoint{
		{Rank: 0, URL: server.URL + "/rank0"}, {Rank: 1, URL: server.URL + "/rank1"},
	}}
	if err := vllm.New().Verify(t.Context(), group, []int{0, 1}); err != nil {
		t.Fatal(err)
	}
	if completions.Load() != 2 {
		t.Fatalf("expected two inference probes, got %d", completions.Load())
	}
}

func write(t *testing.T, writer http.ResponseWriter, text string) {
	t.Helper()
	if _, err := writer.Write([]byte(text)); err != nil {
		t.Error(err)
	}
}
