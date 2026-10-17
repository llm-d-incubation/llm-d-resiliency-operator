// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

package enginefault

import (
	"context"
	"errors"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-resiliency-operator/internal/engine"
	"github.com/llm-d/llm-d-resiliency-operator/internal/workload"
)

// errDryRun ends a step at the first action a dry run skips. The runner then forgets the step and reads the
// group again, so the next action is judged on what is really there.
var errDryRun = errors.New("dry run: action skipped")

// dryWorkload reads the group but never deletes a Pod.
type dryWorkload struct{ workload.Workload }

func (dryWorkload) Reset(ctx context.Context, pods []workload.PodRef) error {
	return skip(ctx, "reset pods", "pods", pods)
}

func (dryWorkload) ResetAll(ctx context.Context, pods []workload.PodRef) error {
	return skip(ctx, "reset all pods", "pods", pods)
}

func (dryWorkload) Release(ctx context.Context, pods []workload.PodRef) error {
	return skip(ctx, "release pods", "pods", pods)
}

// dryEngine observes and checks the engine but never changes it.
type dryEngine struct{ engine.Engine }

func (dryEngine) Probe(ctx context.Context, _ engine.Observation) error {
	ctrl.LoggerFrom(ctx).Info("dry run: would probe the engine")
	return nil
}

//nolint:gocritic // The Engine contract passes the reconfig by value.
func (dryEngine) Apply(ctx context.Context, _ engine.Observation, reconfig engine.Reconfig) ([]engine.Acceptance, error) {
	return nil, skip(ctx, "apply reconfig", "reconfig", reconfig)
}

func skip(ctx context.Context, action string, keysAndValues ...any) error {
	ctrl.LoggerFrom(ctx).Info("dry run: would "+action, keysAndValues...)
	return errDryRun
}

// memJournal keeps the record in memory, so a dry run never writes the real journal.
type memJournal struct{ data []byte }

//nolint:gocritic // The contract returns the record, its version and an error.
func (store *memJournal) Load(context.Context) ([]byte, string, error) { return store.data, "", nil }

func (store *memJournal) Save(_ context.Context, data []byte, _ string) (string, error) {
	store.data = data
	return "", nil
}
