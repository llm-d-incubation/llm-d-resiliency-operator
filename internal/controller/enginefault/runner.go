// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

package enginefault

import (
	"context"
	"errors"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-resiliency-operator/internal/engine"
	"github.com/llm-d/llm-d-resiliency-operator/internal/journal"
	"github.com/llm-d/llm-d-resiliency-operator/internal/workload"
)

// Runner shares the manager's leader election and stops with it.
type Runner struct {
	Workload workload.Workload
	Engine   engine.Engine
	Journal  journal.Journal
	Recorder Recorder
	Config   Config
	// DryRun reads and decides as usual but deletes no Pod, changes no engine and writes no journal.
	DryRun bool
}

func (*Runner) NeedLeaderElection() bool { return true }

func (runner *Runner) Start(ctx context.Context) error {
	ticker := time.NewTicker(runner.Config.PollInterval)
	defer ticker.Stop()
	wl, eng, store := runner.Workload, runner.Engine, runner.Journal
	if runner.DryRun {
		wl, eng, store = dryWorkload{wl}, dryEngine{eng}, &memJournal{}
	}
	var current Controller
	for {
		// Returning an error would stop the manager and every controller in it.
		if current == nil {
			loaded, err := New(ctx, wl, eng, store, runner.Recorder, runner.Config)
			if err != nil && ctx.Err() == nil {
				ctrl.LoggerFrom(ctx).Error(err, "load engine fault journal")
			}
			current = loaded
		}
		if current != nil {
			stepCtx, cancel := context.WithTimeout(ctx, runner.Config.ApplyTimeout)
			err := current.Step(stepCtx)
			if errors.Is(err, errDryRun) {
				// In dryrun we should throw the state away so the next poll will look at the real Pods again.
				store = &memJournal{}
			}
			if errors.Is(err, journal.ErrConflict) || errors.Is(err, errDryRun) {
				current = nil
			}
			cancel()
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
