// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

//nolint:testpackage // Tests exercise persistence and clock boundaries through the controller harness.
package enginefault

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/llm-d/llm-d-resiliency-operator/internal/engine"
	"github.com/llm-d/llm-d-resiliency-operator/internal/workload"
)

func TestApplyResumesStagesAcrossLostCheckpoint(t *testing.T) {
	test := newHarness(t)
	test.serving()
	action := engine.Action{
		Kind: engine.Change, Participants: slices.Clone(test.eng.admitted), Payload: json.RawMessage(`{"repair":"native"}`),
	}
	test.eng.plan = action
	var operationID string
	launched, launches := false, 0
	adapter := func(input engine.ApplyInput) (engine.Report, error) {
		saved := test.record()
		if saved.Operation == nil || !saved.Operation.Attempted || saved.Budget.Actions != 1 {
			t.Fatal("execution ran without its durable intent and budget reservation")
		}
		if operationID == "" {
			operationID = input.Operation.ID
		}
		if input.Operation.ID != operationID || !reflect.DeepEqual(input.Operation.Action, action) {
			t.Fatal("controller replaced the native action while it was running")
		}
		switch string(input.Operation.Checkpoint) {
		case "":
			return engine.Report{State: engine.Running, Checkpoint: json.RawMessage(`{"stage":"prepared"}`)}, nil
		case `{"stage":"prepared"}`:
			if !launched {
				launched = true
				launches++
				test.store.saveErrs = []error{errors.New("checkpoint save interrupted")}
			}
			return engine.Report{State: engine.Running, Checkpoint: json.RawMessage(`{"stage":"launched"}`)}, nil
		case `{"stage":"launched"}`:
			return engine.Report{State: engine.Succeeded}, nil
		default:
			t.Fatalf("unexpected checkpoint %s", input.Operation.Checkpoint)
			return engine.Report{}, nil
		}
	}
	test.eng.applyFunc = adapter
	test.poll()
	test.eng.plan = engine.Action{Kind: engine.RecreateGroup}
	if err := test.pollErr(); err == nil {
		t.Fatal("checkpoint persistence failure was hidden")
	}
	if string(test.record().Operation.Checkpoint) != `{"stage":"prepared"}` {
		t.Fatal("failed save advanced the durable checkpoint")
	}
	test.load()
	test.poll()
	test.poll()
	record := test.record()
	if launches != 1 || record.Operation != nil || record.Result == nil || record.Result.Operation.ID != operationID ||
		record.Result.State != engine.Succeeded || record.Budget.Actions != 1 || len(test.wl.resetAll) != 0 {
		t.Fatalf("staged execution lost replay safety: launches=%d record=%+v", launches, record)
	}
}

func TestApplyTransportErrorRetainsOperation(t *testing.T) {
	test := newHarness(t)
	test.serving()
	test.eng.plan = engine.Action{Kind: engine.Change, Participants: slices.Clone(test.eng.admitted)}
	test.eng.applyErr = errors.New("response lost after submission")
	if err := test.pollErr(); err == nil {
		t.Fatal("transport error was hidden")
	}
	before := test.record()
	if before.Operation == nil || !before.Operation.Attempted || before.Result != nil || before.Unsafe != "" {
		t.Fatal("transport error was interpreted as terminal failure")
	}
	test.eng.applyErr = nil
	test.eng.progress = engine.Report{State: engine.Running}
	test.eng.plan = engine.Action{Kind: engine.RecreateGroup}
	test.load()
	test.poll()
	after := test.record()
	if after.Operation == nil || after.Operation.ID != before.Operation.ID || !after.OperationSince.Equal(before.OperationSince) ||
		after.Budget.Actions != 1 || len(test.wl.resetAll) != 0 {
		t.Fatal("lost response created another recovery attempt")
	}
}

func TestApplyDeadlineAllowsOnlyObservation(t *testing.T) {
	test := newHarness(t)
	test.serving()
	test.eng.plan = engine.Action{Kind: engine.Change, Participants: slices.Clone(test.eng.admitted)}
	test.poll()
	before := test.record()
	test.eng.progress = engine.Report{State: engine.Running}
	test.clock = test.clock.Add(test.config.ApplyTimeout)
	test.load()
	test.poll()
	last := test.eng.applyCalls[len(test.eng.applyCalls)-1]
	if last.MayAdvance || test.record().Operation == nil || test.record().OperationBlocked == "" || test.record().Result != nil {
		t.Fatal("deadline authorized more effects or fabricated a terminal outcome")
	}
	test.eng.progress = engine.Report{State: engine.Succeeded}
	test.poll()
	record := test.record()
	if record.Operation != nil || record.OperationBlocked != "" || record.Result == nil ||
		record.Result.Operation.ID != before.Operation.ID || record.Result.State != engine.Succeeded {
		t.Fatal("confirmed completion could not resolve an expired operation")
	}
}

func TestRecoveryBudgetCountsActionsAcrossRecreation(t *testing.T) {
	test := newHarness(t)
	test.config.RecoveryMaxActions = 2
	test.serving()
	test.eng.plan = engine.Action{Kind: engine.Change, Participants: slices.Clone(test.eng.admitted)}
	test.poll()
	first := test.record().Operation.ID
	test.eng.progress = engine.Report{State: engine.Running}
	for range 4 {
		test.load()
		test.poll()
		if record := test.record(); record.Operation.ID != first || record.Budget.Actions != 1 {
			t.Fatal("polling or leadership change consumed another action")
		}
	}
	test.eng.progress = engine.Report{State: engine.Succeeded}
	test.poll()
	test.eng.plan = engine.Action{Kind: engine.RecreateGroup}
	test.poll()
	if test.record().Budget.Actions != 2 || len(test.wl.resetAll) != 1 {
		t.Fatal("group recreation did not consume the second action")
	}
	for i := range test.wl.group.Pods {
		test.wl.group.Pods[i].UID += "new"
		test.wl.group.Pods[i].Identity += "new"
		test.eng.members[i].Pod.UID = test.wl.group.Pods[i].UID
		test.eng.members[i].PodIdentity = test.wl.group.Pods[i].Identity
	}
	test.poll()
	test.load()
	test.eng.plan = engine.Action{Kind: engine.Healthy}
	test.poll()
	if record := test.record(); record.Budget.Actions != 2 || record.Budget.Blocked == "" {
		t.Fatal("new Pod identities or brief health replenished the budget")
	}
	test.eng.plan = engine.Action{Kind: engine.Change, Participants: slices.Clone(test.eng.admitted)}
	test.poll()
	if test.record().Operation != nil || len(test.eng.applied) != 1 || len(test.wl.resetAll) != 1 {
		t.Fatal("exhausted budget allowed a new mutation")
	}
}

func TestReplacementRequirementUsesOperatorCapacityPolicy(t *testing.T) {
	for _, strategy := range []CapacityRestore{RestoreResetPod, RestoreRelease, RestoreResetAll} {
		t.Run(string(strategy), func(t *testing.T) {
			test := newHarness(t)
			test.config.CapacityRestore = strategy
			test.serving()
			test.dead()
			test.eng.plan = test.shrinkPlan()
			test.poll()
			test.eng.progress = engine.Report{State: engine.Succeeded}
			test.poll()
			for i := range test.eng.members {
				test.eng.members[i].Serving = !test.eng.members[i].Terminal
			}
			test.eng.plan = engine.Action{Kind: engine.ReplaceMembers, Members: ids("dp3@idD")}
			test.poll()
			record := test.record()
			if record.Operation == nil || record.Operation.Action.Kind != engine.ReplaceMembers || record.Workload == nil ||
				record.Workload.Kind != strategy || record.Budget.Actions != 2 {
				t.Fatalf("operator did not select the configured workload action: %+v", record)
			}
			want := []workload.PodRef{{Name: names[3], UID: "d1"}}
			switch strategy {
			case RestoreResetPod:
				if !slices.Equal(test.wl.resets, want) {
					t.Fatal("wrong reset targets")
				}
			case RestoreRelease:
				if len(test.wl.releases) != 1 || !slices.Equal(test.wl.releases[0], want) {
					t.Fatal("wrong release targets")
				}
			case RestoreResetAll:
				if len(test.wl.resetAll) != 1 || len(test.wl.resetAll[0]) != 4 {
					t.Fatal("wrong group reset targets")
				}
			}
		})
	}
}

func TestRecoveryBudgetHealthGapSurvivesFailedSave(t *testing.T) {
	test := newHarness(t)
	test.config.RecoveryMaxActions = 1
	test.serving()
	test.eng.plan = engine.Action{Kind: engine.Change, Participants: slices.Clone(test.eng.admitted)}
	test.poll()
	test.eng.progress = engine.Report{State: engine.Succeeded}
	test.poll()
	test.eng.plan = engine.Action{Kind: engine.Healthy}
	test.poll()
	stable := test.record().Budget.StableSince
	if stable.IsZero() || test.record().Budget.Blocked == "" {
		t.Fatal("test did not start confirming health with an exhausted budget")
	}
	for i := range test.eng.members {
		test.eng.members[i].Unknown = true
	}
	test.store.saveErrs = []error{errors.New("journal unavailable while health was lost")}
	if err := test.pollErr(); err == nil {
		t.Fatal("save failure was hidden")
	}
	for i := range test.eng.members {
		test.eng.members[i].Unknown = false
	}
	test.clock = stable.Add(test.config.RecoveryStablePeriod)
	test.poll()
	record := test.record()
	if record.Budget.Blocked == "" || record.Budget.Actions != 1 || !record.Budget.StableSince.After(stable) {
		t.Fatal("failed journal write let an interrupted healthy interval reopen the budget")
	}
}
