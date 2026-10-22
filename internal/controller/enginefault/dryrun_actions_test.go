// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

//nolint:testpackage // Tests inspect the controller's in-memory dry-run record.
package enginefault

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/llm-d/llm-d-resiliency-operator/internal/engine"
)

func TestDryRunDoesNotAdvanceSkippedActions(t *testing.T) {
	for _, kind := range []engine.ActionKind{engine.Change, engine.Probe, engine.Verify, engine.RecreateGroup} {
		t.Run(string(kind), func(t *testing.T) {
			test := newHarness(t)
			test.serving()
			saved, saves := bytes.Clone(test.store.data), test.store.saves
			test.config.DryRun = true
			test.load()
			test.eng.plan = test.shrinkPlan()
			test.eng.plan.Kind = kind
			test.eng.plan.State = json.RawMessage(`{"stage":"must not be saved"}`)
			if kind == engine.Change {
				test.dead()
			} else {
				test.eng.plan.Remove = nil
			}
			before := test.fsm.record()
			for range 3 {
				test.poll()
			}
			after := test.fsm.record()
			if after.Operation != nil || after.Result != nil || after.Workload != nil ||
				!bytes.Equal(after.PolicyState, before.PolicyState) || after.Budget.Actions != before.Budget.Actions {
				t.Fatalf("skipped action advanced execution: %+v", after)
			}
			if len(test.eng.applyCalls) != 0 || len(test.wl.resetAll)+len(test.wl.resets)+len(test.wl.releases) != 0 {
				t.Fatal("dry run called an external action")
			}
			if !bytes.Equal(test.store.data, saved) || test.store.saves != saves {
				t.Fatal("dry run changed the durable journal")
			}
			if after.WouldDo == "" {
				t.Fatal("skipped action was not reported")
			}
		})
	}
}

func TestDryRunDoesNotResumeSavedExecution(t *testing.T) {
	for _, scenario := range []string{"engine", "workload", "engine-pods-gone"} {
		t.Run(scenario, func(t *testing.T) {
			test := newHarness(t)
			test.serving()
			test.dead()
			test.eng.plan = test.shrinkPlan()
			if scenario == "workload" {
				test.eng.plan = engine.Action{Kind: engine.RecreateGroup, Reason: "recreate"}
			}
			test.poll()
			before := test.record()
			saved, saves := bytes.Clone(test.store.data), test.store.saves
			calls, resets := len(test.eng.applyCalls), len(test.wl.resetAll)
			test.config.DryRun = true
			test.load()
			test.eng.progress = engine.Report{State: engine.Succeeded}
			if scenario != "engine" {
				test.wl.group.Pods = nil
			}
			for range 3 {
				test.poll()
			}
			after := test.fsm.record()
			if !reflect.DeepEqual(after.Operation, before.Operation) || !reflect.DeepEqual(after.Result, before.Result) ||
				!reflect.DeepEqual(after.Workload, before.Workload) || !reflect.DeepEqual(after.Members, before.Members) {
				t.Fatalf("dry run advanced saved execution: before=%+v after=%+v", before, after)
			}
			if len(test.eng.applyCalls) != calls || len(test.wl.resetAll) != resets {
				t.Fatal("dry run resumed an external action")
			}
			if !bytes.Equal(test.store.data, saved) || test.store.saves != saves {
				t.Fatal("dry run changed the saved execution")
			}
			if after.WouldDo == "" {
				t.Fatal("saved action was not reported")
			}
		})
	}
}
