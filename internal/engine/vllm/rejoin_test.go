// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

//nolint:testpackage // Exercises adapter-private fake rank endpoints.
package vllm

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/llm-d/llm-d-resiliency-operator/internal/controller/enginefault"
	"github.com/llm-d/llm-d-resiliency-operator/internal/engine"
	"github.com/llm-d/llm-d-resiliency-operator/internal/telemetry"
)

func TestReplacementAdmissionIsUnsupported(t *testing.T) {
	t.Parallel()
	fleet := newFleet(t, 1, 4)
	observation := fleet.observe(t)
	previous := observation.Members[1].ID()
	candidate := observation.Members[1]
	candidate.Incarnation, candidate.Replaces = "process-2", previous
	candidate.Serving, candidate.Joinable = false, true
	observation.Members[1].Serving, observation.Members[1].Terminal = false, true
	observation.Members = append(observation.Members, candidate)
	survivors := []engine.MemberID{observation.Members[0].ID(), observation.Members[2].ID(), observation.Members[3].ID()}
	if fleet.eng.Capabilities().Grow {
		t.Fatal("replacement capability advertised without a native protocol")
	}
	plan := fleet.eng.Plan(observation, survivors, []engine.MemberID{candidate.ID()})
	if plan.Kind != engine.Healthy || len(plan.Add) != 0 {
		t.Fatalf("unsupported admission planned: %+v", plan)
	}
	reconfig := engine.Reconfig{ID: "add-1", Participants: survivors, Add: []engine.MemberID{candidate.ID()}}
	_, err := fleet.eng.Apply(t.Context(), observation, reconfig)
	var scoped *engine.ApplyError
	if !errors.As(err, &scoped) || scoped.Scope != engine.Joiners || !strings.Contains(err.Error(), "inactive slots") {
		t.Fatalf("preflight rejection did not preserve survivors: %v", err)
	}
	for _, rank := range fleet.ranks {
		if len(rank.applied) != 0 {
			t.Fatal("unsupported join changed a live rank")
		}
	}
	progress := fleet.eng.Progress(observation, reconfig)
	if progress.State != engine.Failed || progress.Scope != engine.Group {
		t.Fatalf("healthy survivors were mistaken for a successful join: %+v", progress)
	}
}

type ftOnlyTransport struct {
	t    *testing.T
	base http.RoundTripper
}

func (transport *ftOnlyTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	switch request.Method + " " + request.URL.Path {
	case "GET /v1/fault_tolerance/status", "GET /health", "POST /v1/fault_tolerance/apply", "POST /v1/completions":
	default:
		transport.t.Errorf("current adapter called an unsupported API: %s %s", request.Method, request.URL)
	}
	return transport.base.RoundTrip(request)
}

func TestFlowRejoinRemainsDisabled(t *testing.T) {
	t.Parallel()
	for _, pods := range []int{1, 2} {
		t.Run(fmt.Sprintf("pods=%d", pods), func(t *testing.T) {
			t.Parallel()
			fleet := newFleet(t, pods, 4)
			fleet.eng.client.Transport = &ftOnlyTransport{t: t, base: fleet.eng.client.Transport}
			run := &flow{t: t, fleet: fleet, sim: newCluster(fleet), wl: &lwsToday{pods: fleet.pods}, store: &memoryJournal{}}
			config := enginefault.Defaults()
			config.PollInterval, config.DiagnosisTimeout, config.ApplyTimeout = time.Millisecond, time.Second, time.Second
			count := pods * 4
			for phase := range 2 {
				controller, err := enginefault.New(t.Context(), run.wl, fleet.eng, run.store,
					telemetry.New(logr.Discard(), time.Minute), config)
				if err != nil {
					t.Fatal(err)
				}
				run.fsm = controller
				if phase == 0 {
					run.until("initial serving", func(record enginefault.Record) bool {
						return served(&record) && len(record.Members) == count
					})
					run.fault(1)
					run.until("survivors serving", func(record enginefault.Record) bool {
						return served(&record) && record.Members["dp1@id0"] == enginefault.Excluded
					})
				}
				for range 10 {
					if err := run.fsm.Step(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
				record := run.store.record(t)
				if !served(&record) || len(record.Replacements) != 0 || len(record.Members) != count || run.wl.resetAlls != 0 {
					t.Fatalf("disabled rejoin changed recovered serving: %+v, resets=%d", record, run.wl.resetAlls)
				}
				if fleet.eng.Capabilities().Grow || len(run.sim.applies) != count-1 {
					t.Fatalf("disabled rejoin dispatched extra instructions: %v", run.sim.applies)
				}
				for _, member := range fleet.observe(t).Members {
					if member.Incarnation != "" || member.Replaces != "" || member.Joinable {
						t.Fatalf("adapter advertised a replacement: %+v", member)
					}
				}
				for _, apply := range run.sim.applies {
					if !strings.Contains(apply, ":scale_down:") {
						t.Fatalf("legacy recovery issued another instruction: %s", apply)
					}
				}
			}
		})
	}
}
