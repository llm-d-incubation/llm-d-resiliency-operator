// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

package enginefault_test

import (
	"testing"

	"github.com/llm-d-incubation/llm-d-resiliency-operator/internal/controller/enginefault"
	"github.com/llm-d-incubation/llm-d-resiliency-operator/internal/engine"
)

func TestProcessLossDoesNotAuthorizeNativeExclusion(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		native []string
	}{
		{"stale healthy frontend", []string{"healthy", "healthy", "healthy", "healthy"}},
		{"missing frontend with engine loss", []string{"healthy", "unknown", "healthy", "healthy"}},
		{"unhealthy frontend with engine loss", []string{"unhealthy", "unhealthy", "unhealthy", "unhealthy"}},
		{"unusable participant alongside dead rank", []string{"unhealthy", "unhealthy", "dead", "unhealthy"}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			status := make([]engine.RankStatus, len(scenario.native))
			for rank, native := range scenario.native {
				status[rank] = engine.RankStatus{Rank: rank, Status: native}
			}
			status[1].ProcessExitConfirmed = true
			decision := enginefault.Decide(status, nil)
			if decision.Action != "wait" || len(decision.Removed) != 0 || len(decision.Participants) != 0 {
				t.Fatalf("process loss cannot establish native request rejection or a usable survivor: %+v", decision)
			}
		})
	}
}

func TestInFlightRecoveryCannotAuthorizeAnotherAction(t *testing.T) {
	for _, native := range []string{"healthy", "unhealthy"} {
		t.Run(native, func(t *testing.T) {
			status := []engine.RankStatus{
				{Rank: 0, Status: native},
				{Rank: 1, Status: native, FTState: "recovering"},
			}
			if decision := enginefault.Decide(status, nil); decision.Action != "wait" {
				t.Fatalf("in-flight recovery allowed premature success or a concurrent retry: %+v", decision)
			}
		})
	}
}
