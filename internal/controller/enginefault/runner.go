// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

package enginefault

import (
	"context"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-resiliency-operator/internal/engine"
)

// Runner shares the manager's leadership and cancellation lifecycle.
type Runner struct {
	Adapter  engine.Adapter
	Workload Workload
	Config   Config
}

func (*Runner) NeedLeaderElection() bool { return true }

func (runner *Runner) Start(ctx context.Context) error {
	controller, err := New(ctx, runner.Adapter, runner.Workload, runner.Config)
	if err != nil {
		return err
	}
	ticker := time.NewTicker(runner.Config.PollInterval)
	defer ticker.Stop()
	for {
		if err := controller.Step(ctx); err != nil && ctx.Err() == nil {
			ctrl.LoggerFrom(ctx).Error(err, "reconcile engine fault")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
