// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

//nolint:testpackage // Tests drive the clock through the unexported load.
package enginefault

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/llm-d/llm-d-resiliency-operator/internal/engine"
	"github.com/llm-d/llm-d-resiliency-operator/internal/journal"
	"github.com/llm-d/llm-d-resiliency-operator/internal/workload"
)

func (test *harness) shrunkMembers(names ...string) {
	test.t.Helper()
	test.serving()
	var participants, removed []engine.MemberID
	for index := range test.eng.members {
		member := &test.eng.members[index]
		member.Serving = false
		if slices.Contains(names, member.Name) {
			member.Terminal = true
			removed = append(removed, member.ID())
		} else {
			participants = append(participants, member.ID())
		}
	}
	test.eng.plan = engine.Plan{Kind: engine.Change, Participants: participants, Remove: removed, Reason: "dead"}
	test.poll()
	test.poll()
	test.poll()
	test.eng.progress = engine.Progress{State: engine.Succeeded}
	for index := range test.eng.members {
		if member := &test.eng.members[index]; !member.Terminal {
			member.Serving = true
		}
	}
	test.eng.plan = engine.Plan{Kind: engine.Healthy}
	test.poll()
	record := test.record()
	for _, id := range removed {
		if record.Members[id] != Excluded {
			test.t.Fatalf("%s not excluded: %+v", id, record)
		}
	}
	test.rec.calls = nil
}

func refsOf(names ...string) []workload.PodRef {
	uids := map[string]string{"lws-0": "a1", "lws-0-1": "b1", "lws-0-2": "c1", "lws-0-3": "d1"}
	refs := make([]workload.PodRef, 0, len(names))
	for _, name := range names {
		refs = append(refs, workload.PodRef{Name: name, UID: uids[name]})
	}
	return refs
}

func TestResetSeveralPods(t *testing.T) {
	t.Parallel()
	t.Run("one call names every failed Pod", func(t *testing.T) {
		t.Parallel()
		test := newHarness(t)
		test.shrunkMembers("dp2", "dp3")
		test.poll()
		if !reflect.DeepEqual(test.wl.resetCalls, [][]workload.PodRef{refsOf("lws-0-2", "lws-0-3")}) {
			t.Fatalf("reset calls %v", test.wl.resetCalls)
		}
		test.wantCalls("PodResetRequested lws-0-2,lws-0-3")
		if record := test.record(); !record.Pods["c1"].Removing || !record.Pods["d1"].Removing {
			t.Fatalf("pods %+v", record.Pods)
		}
	})
	t.Run("a Pod the workload cannot reset is asked for alone", func(t *testing.T) {
		t.Parallel()
		test := newHarness(t)
		test.wl.resetFor = func(refs []workload.PodRef) error {
			if slices.ContainsFunc(refs, func(ref workload.PodRef) bool { return ref.Name == "lws-0" }) {
				return workload.ErrUnsupported
			}
			return nil
		}
		test.shrunkMembers("dp0", "dp3")
		test.poll()
		want := [][]workload.PodRef{refsOf("lws-0", "lws-0-3"), refsOf("lws-0"), refsOf("lws-0-3")}
		if !reflect.DeepEqual(test.wl.resetCalls, want) {
			t.Fatalf("reset calls %v", test.wl.resetCalls)
		}
		record := test.record()
		if record.Members["dp0@idA"] != Rejected || record.Pods["a1"].Removing || !record.Pods["d1"].Removing {
			t.Fatalf("record %+v", record)
		}
		calls := len(test.wl.resetCalls)
		test.poll()
		if got := test.wl.resetCalls[calls:]; !reflect.DeepEqual(got, [][]workload.PodRef{refsOf("lws-0-3")}) {
			t.Fatalf("repeat %v", got)
		}
	})
	t.Run("nothing supported keeps both", func(t *testing.T) {
		t.Parallel()
		test := newHarness(t)
		test.wl.resetErr = workload.ErrUnsupported
		test.shrunkMembers("dp2", "dp3")
		test.poll()
		if len(test.wl.resetCalls) != 3 {
			t.Fatalf("reset calls %v", test.wl.resetCalls)
		}
		test.wantCalls("PodResetRequested lws-0-2", "PodResetRequested lws-0-3")
		test.wantKept("dp2@idC")
		if test.record().Members["dp3@idD"] != Rejected {
			t.Fatalf("record %+v", test.record())
		}
	})
	t.Run("the repeat names every owed Pod", func(t *testing.T) {
		t.Parallel()
		test := newHarness(t)
		test.shrunkMembers("dp2", "dp3")
		test.poll()
		test.poll()
		if len(test.wl.resetCalls) != 2 || !reflect.DeepEqual(test.wl.resetCalls[1], refsOf("lws-0-2", "lws-0-3")) {
			t.Fatalf("repeat %v", test.wl.resetCalls)
		}
		test.wl.group.Pods = test.wl.group.Pods[:3]
		test.poll()
		if got := test.wl.resetCalls[len(test.wl.resetCalls)-1]; !reflect.DeepEqual(got, refsOf("lws-0-2")) {
			t.Fatalf("repeat after one is gone %v", got)
		}
	})
	t.Run("release names every failed Pod", func(t *testing.T) {
		t.Parallel()
		test := newHarness(t)
		test.config.CapacityRestore = RestoreRelease
		test.shrunkMembers("dp2", "dp3")
		test.poll()
		if !reflect.DeepEqual(test.wl.releases, [][]workload.PodRef{refsOf("lws-0-2", "lws-0-3")}) {
			t.Fatalf("releases %v", test.wl.releases)
		}
	})
}

func TestJournalRecordDeleted(t *testing.T) {
	t.Parallel()
	t.Run("a deleted record starts over", func(t *testing.T) {
		t.Parallel()
		test := newHarness(t)
		test.serving()
		test.store.remove()
		test.eng.plan = engine.Plan{Kind: engine.Wait, Reason: "dp1 is slow"}
		if err := test.pollErr(); !errors.Is(err, journal.ErrConflict) {
			t.Fatalf("save after the record was deleted: %v", err)
		}
		test.eng.plan = engine.Plan{Kind: engine.Healthy}
		test.load()
		test.poll()
		if record := test.record(); record.Phase != Serving || len(record.Members) != 4 || len(test.wl.resetAlls) != 0 {
			t.Fatalf("record %+v, group resets %d", record, len(test.wl.resetAlls))
		}
	})
	t.Run("a stale writer cannot recreate it", func(t *testing.T) {
		t.Parallel()
		test := newHarness(t)
		test.serving()
		stale := test.fsm
		test.load()
		test.dead()
		test.eng.plan = test.shrinkPlan()
		test.poll()
		test.store.remove()
		test.fsm = stale
		for range 3 {
			if err := test.pollErr(); !errors.Is(err, journal.ErrConflict) {
				t.Fatalf("stale writer: %v", err)
			}
		}
		if test.store.data != nil || len(test.eng.applied) != 0 {
			t.Fatalf("stale writer saved %q, applied %d", test.store.data, len(test.eng.applied))
		}
	})
}
