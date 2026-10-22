// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

package enginefault_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"

	"github.com/llm-d/llm-d-resiliency-operator/internal/controller/enginefault"
	"github.com/llm-d/llm-d-resiliency-operator/internal/engine"
	"github.com/llm-d/llm-d-resiliency-operator/internal/telemetry"
	"github.com/llm-d/llm-d-resiliency-operator/internal/workload"
)

// Reset, ResetAll and Release are left nil, so a call that slips past dry run panics.
type twoPodGroup struct{ workload.Workload }

func (twoPodGroup) Discover(context.Context) (workload.Group, error) {
	return workload.Group{Pods: []workload.Pod{
		{Name: "p0", UID: "u0", Identity: "p0", State: workload.PodReady},
		{Name: "p1", UID: "u1", Identity: "p1", State: workload.PodReady},
	}}, nil
}

// deadEngine reports m<i> dead when dead[i] is set. Apply is left nil.
type deadEngine struct {
	engine.Engine
	dead [2]bool
}

func (eng *deadEngine) Observe(context.Context, []workload.Pod) engine.Observation {
	var obs engine.Observation
	for index, dead := range eng.dead {
		pod := string(rune('0' + index))
		obs.Members = append(obs.Members, engine.Member{
			Pod: workload.PodRef{Name: "p" + pod, UID: "u" + pod}, PodIdentity: "p" + pod, Name: "m" + pod,
			Serving: !dead, Terminal: dead,
		})
	}
	return obs
}

//nolint:gocritic // The policy contract passes DecisionInput by value.
func (*deadEngine) Decide(input engine.DecisionInput) engine.Action {
	var alive, dead []engine.MemberID
	for _, member := range input.Observation.Members {
		if member.Terminal {
			dead = append(dead, member.ID())
		} else {
			alive = append(alive, member.ID())
		}
	}
	switch {
	case len(dead) == 0 && len(alive) == len(input.Admitted):
		return engine.Action{Kind: engine.Healthy, Reason: "all serve"}
	case len(alive) == 0:
		return engine.Action{Kind: engine.RecreateGroup, Reason: "all dead"}
	}
	return engine.Action{Kind: engine.Change, Participants: alive, Remove: dead, Reason: "remove dead"}
}

type memoryStore struct{ data []byte }

//nolint:gocritic // The contract returns the record, its version and an error.
func (store *memoryStore) Load(context.Context) ([]byte, string, error) { return store.data, "", nil }

func (store *memoryStore) Save(_ context.Context, data []byte, _ string) (string, error) {
	store.data = data
	return "", nil
}

func TestDryRunLogsOnlyChanges(t *testing.T) {
	var logged []string
	log := funcr.New(func(_, args string) {
		if _, msg, ok := strings.Cut(args, `"msg"="dry run: `); ok {
			msg, _, _ = strings.Cut(msg, `"`)
			logged = append(logged, msg)
		}
	}, funcr.Options{})
	eng := &deadEngine{}
	config := enginefault.Defaults()
	// The diagnosis deadline passes on every poll: a skipped reconfig must not turn into a reset.
	config.DryRun, config.PollInterval, config.DiagnosisTimeout = true, 0, time.Nanosecond
	store := &memoryStore{}
	fsm, err := enginefault.New(t.Context(), twoPodGroup{}, eng, store, telemetry.New(log, 0), config)
	if err != nil {
		t.Fatal(err)
	}
	for _, dead := range [][2]bool{
		{false, false}, // starting to serving
		{false, true},  // serving to recovering
		{false, true},  // would remove m1
		{false, true},  // same, quiet
		{true, true},   // would reset all
		{true, true},   // same, quiet
		{false, false}, // nothing to do any more
	} {
		eng.dead = dead
		if err := fsm.Step(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if len(store.data) != 0 {
		t.Fatal("dry run wrote the journal")
	}
	want := []string{
		"would reconfig: remove [m1@p1], add []",
		"would reset-all [{p0 u0} {p1 u1}]",
		"nothing to do any more",
	}
	if !slices.Equal(logged, want) {
		t.Fatalf("logged %q, want %q", logged, want)
	}
}
