// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

package enginefault_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/llm-d/llm-d-resiliency-operator/internal/controller/enginefault"
	"github.com/llm-d/llm-d-resiliency-operator/internal/telemetry"
	"github.com/llm-d/llm-d-resiliency-operator/internal/workload"
)

type lossStore struct {
	memoryStore
	fail bool
}

func (store *lossStore) Save(ctx context.Context, data []byte, version string) (string, error) {
	if store.fail {
		return "", errors.New("journal unavailable")
	}
	return store.memoryStore.Save(ctx, data, version)
}

type lossGroup struct {
	workload.Workload
	group       workload.Group
	discoverErr error
	reset       func([]workload.PodRef) error
}

func (wl *lossGroup) Discover(context.Context) (workload.Group, error) {
	return wl.group, wl.discoverErr
}
func (wl *lossGroup) ResetAll(_ context.Context, pods []workload.PodRef) error { return wl.reset(pods) }

func TestJournalLossResetSurvivesReload(t *testing.T) {
	t.Parallel()
	store := &lossStore{}
	wl := &lossGroup{discoverErr: errors.New("discovery unavailable")}
	resets := 0
	wl.reset = func(pods []workload.PodRef) error {
		var record enginefault.Record
		if err := json.Unmarshal(store.data, &record); err != nil {
			t.Fatal(err)
		}
		if record.ResetPending || record.Phase != enginefault.Starting || !record.Pods["old-uid"].Removing {
			t.Fatalf("reset not saved: %+v", record)
		}
		if len(pods) != 1 || pods[0].UID != "old-uid" {
			t.Fatalf("reset targets: %v", pods)
		}
		resets++
		return errors.New("reset unavailable")
	}
	reload := func() enginefault.Controller {
		t.Helper()
		// No engine call is needed to replace a group whose saved state is missing.
		fsm, err := enginefault.New(t.Context(), wl, nil, store,
			telemetry.New(logr.Discard(), time.Minute), enginefault.Defaults())
		if err != nil {
			t.Fatal(err)
		}
		return fsm
	}
	fsm := reload()
	if err := fsm.Step(t.Context()); err == nil {
		t.Fatal("expected discovery error")
	}
	var record enginefault.Record
	if err := json.Unmarshal(store.data, &record); err != nil {
		t.Fatal(err)
	}
	if !record.ResetPending || resets != 0 {
		t.Fatalf("lost reset intent: %+v", record)
	}
	wl.discoverErr = nil
	wl.group.Pods = []workload.Pod{{Name: "p0", UID: "old-uid", Identity: "old-id", State: workload.PodReady}}
	fsm = reload()
	store.fail = true
	if err := fsm.Step(t.Context()); err == nil {
		t.Fatal("expected save error")
	}
	if resets != 0 {
		t.Fatal("reset before journal save")
	}
	store.fail = false
	if err := fsm.Step(t.Context()); err == nil {
		t.Fatal("expected reset error")
	}
	if resets != 1 {
		t.Fatalf("resets %d", resets)
	}
	fsm = reload()
	if err := fsm.Step(t.Context()); err == nil {
		t.Fatal("expected repeated reset error")
	}
	if resets != 2 {
		t.Fatalf("resets %d", resets)
	}
}

func TestMissingJournalWithNoPods(t *testing.T) {
	t.Parallel()
	wl := &lossGroup{}
	store := &lossStore{}
	fsm, err := enginefault.New(t.Context(), wl, &deadEngine{}, store,
		telemetry.New(logr.Discard(), time.Minute), enginefault.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	if err := fsm.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	var record enginefault.Record
	if err := json.Unmarshal(store.data, &record); err != nil {
		t.Fatal(err)
	}
	if record.ResetPending {
		t.Fatalf("unexpected reset intent: %+v", record)
	}
}
