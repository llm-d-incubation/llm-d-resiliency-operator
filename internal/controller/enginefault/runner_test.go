// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

//nolint:testpackage // Runner tests share the fakes with the step tests.
package enginefault

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/llm-d/llm-d-resiliency-operator/internal/journal"
)

func run(t *testing.T, test *harness, duration time.Duration) {
	t.Helper()
	test.config.PollInterval = 5 * time.Millisecond
	runner := &Runner{Workload: test.wl, Engine: test.eng, Journal: test.store, Recorder: test.rec, Config: test.config}
	if !runner.NeedLeaderElection() {
		t.Fatal("runner must need leader election")
	}
	ctx, cancel := context.WithTimeout(t.Context(), duration)
	defer cancel()
	done := make(chan error)
	go func() { done <- runner.Start(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start returned %v", err)
		}
	case <-time.After(duration + 2*time.Second):
		t.Fatal("Start did not return after cancel")
	}
}

func TestRunnerStopsOnCancel(t *testing.T) {
	t.Parallel()
	test := newHarness(t)
	run(t, test, 30*time.Millisecond)
	if test.record().Phase != Serving {
		t.Fatalf("runner did not step: %+v", test.record())
	}
}

func TestRunnerRetriesUnreadableJournal(t *testing.T) {
	t.Parallel()
	test := newHarness(t)
	test.store.loadErrs = []error{errors.New("api down"), errors.New("api down")}
	run(t, test, 100*time.Millisecond)
	if test.store.loads < 3 || test.wl.discovers == 0 {
		t.Fatalf("loads %d, discovers %d", test.store.loads, test.wl.discovers)
	}
}

func TestRunnerReloadsAfterConflict(t *testing.T) {
	t.Parallel()
	test := newHarness(t)
	test.store.saveErrs = []error{fmt.Errorf("stale: %w", journal.ErrConflict)}
	run(t, test, 100*time.Millisecond)
	if test.store.loads < 2 || test.record().Phase != Serving {
		t.Fatalf("loads %d, record %+v", test.store.loads, test.record())
	}
}
