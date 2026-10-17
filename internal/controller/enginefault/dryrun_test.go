// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

package enginefault_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/llm-d/llm-d-resiliency-operator/internal/controller/enginefault"
	"github.com/llm-d/llm-d-resiliency-operator/internal/engine"
	"github.com/llm-d/llm-d-resiliency-operator/internal/telemetry"
	"github.com/llm-d/llm-d-resiliency-operator/internal/workload"
)

// Reset, ResetAll and Release are left nil, so a call that slips past dry run panics.
type oneMemberGroup struct{ workload.Workload }

func (oneMemberGroup) Discover(context.Context) (workload.Group, error) {
	return workload.Group{Pods: []workload.Pod{{Name: "p0", UID: "u0", Identity: "p0", State: workload.PodReady}}}, nil
}

// flakyEngine is readable on every third Observe: the member is adopted, goes unseen twice and the group
// is reset. Apply, Probe and Verify are left nil.
type flakyEngine struct {
	engine.Engine
	observes atomic.Int32
}

func (eng *flakyEngine) Observe(context.Context, []workload.Pod) engine.Observation {
	unknown := eng.observes.Add(1)%3 != 1
	member := engine.Member{
		Pod: workload.PodRef{Name: "p0", UID: "u0"}, PodIdentity: "p0", Name: "m0", Serving: !unknown, Unknown: unknown,
	}
	return engine.Observation{Members: []engine.Member{member}}
}

func (*flakyEngine) Plan(engine.Observation, []engine.MemberID, []engine.MemberID) engine.Plan {
	return engine.Plan{Kind: engine.Healthy}
}

func TestDryRunSkipsResetAndStartsOver(t *testing.T) {
	eng := &flakyEngine{}
	config := enginefault.Defaults()
	config.PollInterval, config.UnseenTimeout = time.Millisecond, time.Nanosecond
	runner := &enginefault.Runner{
		Workload: oneMemberGroup{}, Engine: eng, Recorder: telemetry.New(logr.Discard(), time.Hour), Config: config, DryRun: true,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() {
		if err := runner.Start(ctx); err != nil {
			t.Error(err)
		}
	}()
	// Without starting over, the runner would retry the skipped reset forever and never Observe again.
	for eng.observes.Load() < 10 {
		if ctx.Err() != nil {
			t.Fatalf("stuck after %d observes", eng.observes.Load())
		}
		time.Sleep(time.Millisecond)
	}
}
