// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

//nolint:testpackage // The tests drive the controller's clock.
package enginefault

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/llm-d/llm-d-resiliency-operator/internal/engine"
	"github.com/llm-d/llm-d-resiliency-operator/internal/workload"
)

type unseenWorkload struct {
	workload.Workload
	failed bool
	resets int
}

func (wl *unseenWorkload) Discover(context.Context) (workload.Group, error) {
	if wl.failed {
		return workload.Group{}, errors.New("discovery failed")
	}
	return workload.Group{Pods: []workload.Pod{{Name: "p", UID: "u", Identity: "i", State: workload.PodReady}}}, nil
}

func (wl *unseenWorkload) ResetAll(context.Context, []workload.PodRef) error {
	wl.resets++
	return nil
}

type unseenEngine struct {
	engine.Engine
	unknown bool
}

func (eng *unseenEngine) Observe(context.Context, []workload.Pod) engine.Observation {
	return engine.Observation{Members: []engine.Member{{
		Pod:         workload.PodRef{Name: "p", UID: "u"},
		PodIdentity: "i", Name: "m", Serving: !eng.unknown, Unknown: eng.unknown,
	}}}
}

func (*unseenEngine) Plan(engine.Observation, []engine.MemberID, []engine.MemberID) engine.Plan {
	return engine.Plan{Kind: engine.Healthy, Reason: "healthy"}
}

func (*unseenEngine) Capabilities() engine.Capabilities { return engine.Capabilities{} }

func (*unseenEngine) Progress(engine.Observation, engine.Reconfig) engine.Progress {
	return engine.Progress{State: engine.Running}
}

type unseenRecorder struct{ Recorder }

func (unseenRecorder) Stepped(*Record, error)                           {}
func (unseenRecorder) PhaseChanged(Phase, Phase, string)                {}
func (unseenRecorder) SaveFailed(error)                                 {}
func (unseenRecorder) ResetAllRequested(ResetReason, []workload.PodRef) {}

func TestUnseenTimeoutExcludesDiscoveryOutages(t *testing.T) {
	t.Parallel()
	type poll struct {
		at      int
		unknown bool
		failed  bool
		reset   bool
	}
	for name, polls := range map[string][]poll{
		"engine only": {{10, true, false, false}, {69, true, false, false}, {71, true, false, true}},
		"preserve accrued time": {
			{10, true, false, false},
			{25, true, true, false},
			{100, true, false, false},
			{144, true, false, false},
			{146, true, false, true},
		},
		"discovery before engine blindness": {
			{10, false, true, false},
			{85, true, false, false},
			{144, true, false, false},
			{146, true, false, true},
		},
		"repeated discovery outages": {
			{10, true, false, false},
			{25, true, true, false},
			{100, true, false, false},
			{120, true, true, false},
			{195, true, false, false},
			{219, true, false, false},
			{221, true, false, true},
		},
		"readable engine clears accrued time": {
			{10, true, false, false},
			{25, true, true, false},
			{100, true, false, false},
			{110, false, false, false},
			{120, true, false, false},
			{179, true, false, false},
			{181, true, false, true},
		},
		"never reset during failed discovery": {
			{10, true, false, false},
			{25, true, true, false},
			{200, true, true, false},
			{300, true, true, false},
			{310, true, false, false},
			{354, true, false, false},
			{356, true, false, true},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			clock := start
			wl, eng := &unseenWorkload{}, &unseenEngine{}
			config := Defaults()
			config.UnseenTimeout = time.Minute
			fsm, err := load(t.Context(), wl, eng, &memJournal{}, unseenRecorder{}, config, func() time.Time { return clock })
			if err != nil {
				t.Fatal(err)
			}
			if err := fsm.Step(t.Context()); err != nil {
				t.Fatal(err)
			}
			for _, poll := range polls {
				clock = start.Add(time.Duration(poll.at) * time.Second)
				eng.unknown, wl.failed = poll.unknown, poll.failed
				err := fsm.Step(t.Context())
				if (wl.resets > 0) != poll.reset {
					t.Fatalf("at %ds: resets=%d, want reset=%t, error=%v", poll.at, wl.resets, poll.reset, err)
				}
			}
		})
	}
}

func TestDiscoveryPausePreservesRecoveryDeadlines(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := start
	wl, eng := &unseenWorkload{}, &unseenEngine{}
	fsm, err := load(t.Context(), wl, eng, &memJournal{}, unseenRecorder{}, Defaults(), func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	if err := fsm.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	record := fsm.record()
	record.Phase = Recovering
	record.Reconfig = &engine.Reconfig{ID: "pending", Participants: []engine.MemberID{"m@i"}}
	record.ReconfigSince, record.VerifyFailedSince = start, start.Add(time.Second)
	if err := fsm.save(t.Context(), &record); err != nil {
		t.Fatal(err)
	}
	for _, at := range []int{10, 25, 100, 110} {
		clock = start.Add(time.Duration(at) * time.Second)
		eng.unknown, wl.failed = at != 110, at == 25
		if err := fsm.Step(t.Context()); (err != nil) != (at != 110) {
			t.Fatalf("at %ds: unexpected step result: %v", at, err)
		}
	}
	record = fsm.record()
	if wl.resets != 0 || !record.Since.Equal(start.Add(100*time.Second)) ||
		!record.ReconfigSince.Equal(start.Add(100*time.Second)) || !record.VerifyFailedSince.Equal(start.Add(101*time.Second)) {
		t.Fatalf("resets=%d, record=%+v", wl.resets, record)
	}
}
