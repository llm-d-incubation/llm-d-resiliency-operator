// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0
//
//nolint:testpackage // Reuse controller fakes to exercise manager cancellation.
package enginefault

import (
	"context"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-resiliency-operator/internal/engine"
)

type heldVerification struct {
	*fakeAdapter
	entered chan struct{}
	release chan struct{}
}

func (adapter *heldVerification) Verify(ctx context.Context, group engine.Group, ranks []int) error {
	close(adapter.entered)
	select {
	case <-adapter.release:
		return adapter.fakeAdapter.Verify(ctx, group, ranks)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestRunnerRevalidatesSavedStateAndCancelsVerification(t *testing.T) {
	_, adapter, workload := setup(t)
	held := &heldVerification{fakeAdapter: adapter, entered: make(chan struct{}), release: make(chan struct{})}
	config := Defaults()
	config.PollInterval = time.Hour
	runner := &Runner{Adapter: held, Workload: workload, Config: config}
	if !runner.NeedLeaderElection() {
		t.Fatal("runner must require leadership")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- runner.Start(ctx) }()
	select {
	case <-held.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not reverify saved state")
	}
	cancel()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not stop on cancellation")
	}
	if len(workload.resets) != 0 {
		t.Fatal("shutdown triggered reset")
	}
}

func TestRunnerRejectsInvalidSavedState(t *testing.T) {
	_, adapter, workload := setup(t)
	workload.saved = []byte("invalid")
	runner := &Runner{Adapter: adapter, Workload: workload, Config: Defaults()}
	if err := runner.Start(context.Background()); err == nil {
		t.Fatal("accepted invalid state")
	}
}
