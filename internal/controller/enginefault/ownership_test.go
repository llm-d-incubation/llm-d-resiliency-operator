// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

//nolint:testpackage // Tests exercise recovery through the persisted controller record.
package enginefault

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/llm-d/llm-d-resiliency-operator/internal/engine"
	"github.com/llm-d/llm-d-resiliency-operator/internal/workload"
)

type ownershipCheckpoint struct {
	Stage    string          `json:"stage"`
	Removed  engine.MemberID `json:"removed"`
	Attempts int             `json:"attempts"`
}

func ownershipState(state ownershipCheckpoint) json.RawMessage {
	data, err := json.Marshal(state)
	if err != nil {
		panic(err)
	}
	return data
}

func ownershipPlan(limit int) func(engine.DecisionInput) engine.Action {
	return func(input engine.DecisionInput) engine.Action {
		var state ownershipCheckpoint
		if len(input.State) > 0 {
			if err := json.Unmarshal(input.State, &state); err != nil {
				return engine.Action{Kind: engine.RecreateGroup, Reason: err.Error()}
			}
		}
		plan := engine.Action{Kind: engine.Wait, State: input.State}
		result := input.LastResult
		switch state.Stage {
		case "":
			plan.Kind = engine.Healthy
			for _, member := range input.Observation.Members {
				if member.Terminal && slices.Contains(input.Admitted, member.ID()) {
					state.Stage, state.Removed = "remove", member.ID()
					plan.Kind, plan.Remove = engine.Change, []engine.MemberID{member.ID()}
					for _, id := range input.Admitted {
						if id != member.ID() {
							plan.Participants = append(plan.Participants, id)
						}
					}
					break
				}
			}
		case "remove":
			if result != nil && result.State == engine.Succeeded && result.Operation.Action.Kind == engine.Change {
				plan.Kind, plan.Participants, state.Stage = engine.Verify, input.Admitted, "verify"
			}
		case "verify", "admit":
			ready := result != nil && result.State == engine.Succeeded && result.Operation.Action.Kind == engine.Verify
			failed := result != nil && result.State == engine.Failed && result.Scope == engine.Joiners
			if state.Stage == "admit" && failed && state.Attempts >= limit {
				plan.Kind, state.Stage = engine.Healthy, "exhausted"
			} else if ready || failed {
				for _, member := range input.Observation.Members {
					if member.Replaces == state.Removed && slices.Contains(input.Candidates, member.ID()) {
						plan.Kind, plan.Participants = engine.Change, input.Admitted
						plan.Add = []engine.MemberID{member.ID()}
						state.Stage, state.Attempts = "admit", state.Attempts+1
						break
					}
				}
			}
		case "exhausted":
			plan.Kind = engine.Healthy
		}
		plan.State = ownershipState(state)
		return plan
	}
}

func ownershipPollUntil(t *testing.T, test *harness, done func() bool) {
	t.Helper()
	for range 12 {
		if done() {
			return
		}
		test.poll()
	}
	t.Fatal("recovery did not reach the expected action")
}

func ownershipSharedGroup(t *testing.T) *harness {
	t.Helper()
	test := newHarness(t)
	test.wl.group.Pods = test.wl.group.Pods[:1]
	for index := range test.eng.members {
		test.eng.members[index].Pod = test.eng.members[0].Pod
		test.eng.members[index].PodIdentity = "idA"
	}
	test.serving()
	return test
}

func ownershipReplacement(test *harness, old engine.MemberID, name string) engine.Member {
	return engine.Member{
		Name: name, Pod: test.eng.members[0].Pod, PodIdentity: "idA", Replaces: old, Joinable: true,
	}
}

func TestPolicyChoosesReplacementRetryFromSavedOutcome(t *testing.T) {
	for _, limit := range []int{1, 2} {
		name := "keep excluded"
		if limit == 2 {
			name = "retry replacement"
		}
		t.Run(name, func(t *testing.T) {
			test := ownershipSharedGroup(t)
			test.eng.planFunc = ownershipPlan(limit)
			old := test.member("dp1").ID()
			test.member("dp1").Serving, test.member("dp1").Terminal = false, true
			ownershipPollUntil(t, test, func() bool { return len(test.eng.applied) == 1 })
			if !slices.Equal(test.eng.applied[0].Remove, []engine.MemberID{old}) {
				t.Fatal("policy removal was not dispatched")
			}
			test.eng.progress = engine.Report{State: engine.Succeeded}
			ownershipPollUntil(t, test, func() bool { return len(test.eng.verified) == 1 })
			if slices.Contains(test.eng.verified[0], old) || len(test.eng.verified[0]) != 3 {
				t.Fatalf("wrong survivors verified: %v", test.eng.verified)
			}
			candidate := ownershipReplacement(test, old, "dp1-process-2")
			test.eng.members = append(test.eng.members, candidate)
			test.eng.initialReport = &engine.Report{
				State: engine.Failed, Scope: engine.Joiners, Reason: "candidate fenced; survivors unchanged",
			}
			test.eng.onApply = func() {
				record := test.record()
				var saved ownershipCheckpoint
				if err := json.Unmarshal(record.PolicyState, &saved); err != nil {
					t.Fatal(err)
				}
				if record.Operation == nil || saved.Stage != "admit" || saved.Attempts == 0 {
					t.Fatal("admission ran before its action and policy checkpoint were saved")
				}
			}
			ownershipPollUntil(t, test, func() bool { return len(test.eng.applied) == 2 })
			record := test.record()
			if record.Result == nil || record.Result.State != engine.Failed || record.Members[candidate.ID()] != Rejected {
				t.Fatalf("safe admission failure was not preserved: %+v", record)
			}
			test.eng.applyErr, test.eng.initialReport = nil, nil
			test.eng.progress = engine.Report{State: engine.Running}
			test.eng.planFunc = ownershipPlan(limit)
			test.load()
			fresh := ownershipReplacement(test, old, "dp1-process-3")
			test.eng.members = append(test.eng.members, fresh)
			if limit == 2 {
				ownershipPollUntil(t, test, func() bool { return len(test.eng.applied) == 3 })
				if !slices.Equal(test.eng.applied[2].Add, []engine.MemberID{fresh.ID()}) {
					t.Fatalf("policy retry did not receive the fresh candidate: %+v", test.eng.applied[2])
				}
			} else {
				for range 4 {
					test.poll()
				}
				if len(test.eng.applied) != 2 || !slices.Contains(test.eng.input.Candidates, fresh.ID()) {
					t.Fatal("controller retried the replacement or hid the fresh candidate from policy")
				}
			}
			if test.record().Members[old] != Excluded || len(test.eng.verified) != 1 || test.eng.probes != 0 ||
				len(test.wl.resets)+len(test.wl.resetAll) != 0 {
				t.Fatal("controller added an unplanned recovery stage")
			}
		})
	}
}

func TestHealthyPlanDoesNotScheduleRecoveryActions(t *testing.T) {
	test := newHarness(t)
	test.serving()
	test.eng.plan = test.shrinkPlan()
	test.member("dp3").Serving, test.member("dp3").Terminal = false, true
	ownershipPollUntil(t, test, func() bool { return len(test.eng.applied) == 1 })
	test.eng.progress = engine.Report{State: engine.Succeeded}
	ownershipPollUntil(t, test, func() bool { return test.record().Members[test.member("dp3").ID()] == Excluded })
	test.eng.plan = engine.Action{Kind: engine.Healthy}
	for range 4 {
		test.poll()
	}
	if len(test.eng.verified)+test.eng.probes+len(test.wl.resets)+len(test.wl.resetAll)+len(test.wl.releases) != 0 {
		t.Fatal("healthy plan implicitly verified, probed, or restored capacity")
	}
}

func TestUnsupportedPodResetReturnsToPolicy(t *testing.T) {
	test := newHarness(t)
	test.serving()
	test.eng.plan = test.shrinkPlan()
	old := test.member("dp3").ID()
	test.member("dp3").Serving, test.member("dp3").Terminal = false, true
	ownershipPollUntil(t, test, func() bool { return len(test.eng.applied) == 1 })
	test.eng.progress = engine.Report{State: engine.Succeeded}
	ownershipPollUntil(t, test, func() bool { return test.record().Members[old] == Excluded })
	test.wl.resetErr = workload.ErrUnsupported
	test.eng.plan = engine.Action{Kind: engine.ReplaceMembers, Members: []engine.MemberID{old}}
	ownershipPollUntil(t, test, func() bool { return len(test.wl.resets) > 0 })
	test.eng.plan = engine.Action{Kind: engine.Healthy}
	test.load()
	test.poll()
	result := test.eng.input.LastResult
	if result == nil || result.Operation.Action.Kind != engine.ReplaceMembers ||
		result.State != engine.Failed || result.Reason == "" {
		t.Fatalf("unsupported reset was not exposed to policy: %+v", result)
	}
	if len(test.wl.resetCalls) != 1 || test.record().Members[old] != Excluded || len(test.wl.resetAll) != 0 {
		t.Fatal("unsupported reset changed the policy's recovery decision")
	}
}

func TestPolicyCheckpointRequiresSavedAction(t *testing.T) {
	test := ownershipSharedGroup(t)
	test.eng.planFunc = ownershipPlan(1)
	test.member("dp1").Serving, test.member("dp1").Terminal = false, true
	test.store.saveErrs = []error{errors.New("journal unavailable")}
	if err := test.pollErr(); err == nil {
		t.Fatal("journal failure was hidden")
	}
	if record := test.record(); record.Operation != nil || len(record.PolicyState) != 0 || len(test.eng.applied) != 0 {
		t.Fatal("policy advanced or action ran without saved intent")
	}
	test.load()
	ownershipPollUntil(t, test, func() bool { return len(test.eng.applied) == 1 })
	if test.record().Operation == nil || len(test.record().PolicyState) == 0 {
		t.Fatal("recovered journal did not save the action and checkpoint")
	}
}

func TestPolicyCannotAdvanceBeforeOutcomeIsSaved(t *testing.T) {
	test := ownershipSharedGroup(t)
	test.eng.planFunc = ownershipPlan(1)
	test.member("dp1").Serving, test.member("dp1").Terminal = false, true
	ownershipPollUntil(t, test, func() bool { return len(test.eng.applied) == 1 })
	operation := test.record().Operation.ID
	test.eng.progress = engine.Report{State: engine.Succeeded}
	test.store.saveErrs = []error{errors.New("journal unavailable")}
	if err := test.pollErr(); err == nil {
		t.Fatal("completion save failure was hidden")
	}
	record := test.record()
	if record.Operation == nil || record.Operation.ID != operation || record.Result != nil || len(test.eng.verified) != 0 {
		t.Fatal("policy advanced before the removal outcome was durable")
	}
	test.load()
	ownershipPollUntil(t, test, func() bool { return len(test.eng.verified) == 1 })
	if len(test.eng.applied) != 1 {
		t.Fatal("completed removal was dispatched again after reload")
	}
}

func TestPendingActionSurvivesLeaderChange(t *testing.T) {
	test := ownershipSharedGroup(t)
	test.eng.planFunc = ownershipPlan(1)
	test.member("dp1").Serving, test.member("dp1").Terminal = false, true
	ownershipPollUntil(t, test, func() bool { return len(test.eng.applied) == 1 })
	before := test.record()
	test.eng.progress = engine.Report{State: engine.Running}
	test.eng.planFunc = func(engine.DecisionInput) engine.Action {
		t.Fatal("policy was consulted while its action was pending")
		return engine.Action{}
	}
	test.load()
	test.poll()
	after := test.record()
	if after.Operation == nil || after.Operation.ID != before.Operation.ID || !after.OperationSince.Equal(before.OperationSince) ||
		string(after.PolicyState) != string(before.PolicyState) || len(test.eng.applied) != 1 {
		t.Fatal("leadership change replaced the pending action, deadline, or checkpoint")
	}
}

func TestAmbiguousActionCannotStartAnotherMembershipChange(t *testing.T) {
	test := ownershipSharedGroup(t)
	test.member("dp1").Serving, test.member("dp1").Terminal = false, true
	var participants []engine.MemberID
	for _, member := range test.eng.members {
		if !member.Terminal {
			participants = append(participants, member.ID())
		}
	}
	test.eng.plan = engine.Action{
		Kind: engine.Change, Participants: participants, Remove: []engine.MemberID{test.member("dp1").ID()},
	}
	test.eng.applyErr = errors.New("membership commit outcome unknown")
	if err := test.pollErr(); err == nil || len(test.eng.applied) != 1 || test.record().Operation == nil {
		t.Fatal("uncertain dispatch did not retain its pending action")
	}
	test.eng.applyErr, test.eng.initialReport = nil, nil
	test.eng.plan = engine.Action{Kind: engine.Change, Participants: participants}
	for range 3 {
		test.pollAnyway()
	}
	if len(test.eng.applied) != 1 {
		t.Fatal("another membership change overlapped an uncertain operation")
	}
	if len(test.wl.resetAll) != 0 {
		t.Fatal("controller selected a group reset without a policy decision")
	}
}

func TestSafetyFaultSurvivesInvalidPlanAndLeaderChange(t *testing.T) {
	test := sharedGroup(t)
	old := excludeMember(t, test)
	candidate := replacement(test, old, "dp1-process-2")
	test.eng.members = append(test.eng.members, candidate)
	test.eng.plan = addPlan(test, &candidate)
	participants := slices.Clone(test.eng.plan.Participants)
	test.eng.initialReport = &engine.Report{State: engine.Failed, Scope: engine.Joiners, Reason: "candidate fenced"}
	test.poll()
	test.eng.members[len(test.eng.members)-1].Serving = true
	test.eng.plan = engine.Action{Kind: engine.Healthy}
	if err := test.pollErr(); err == nil {
		t.Fatal("healthy decision ignored a rejected member serving")
	}
	if test.record().Unsafe == "" {
		t.Fatal("observed safety fault was lost when policy returned an invalid plan")
	}
	test.load()
	test.eng.members[len(test.eng.members)-1].Serving = false
	test.eng.applyErr, test.eng.initialReport = nil, nil
	test.eng.plan = engine.Action{Kind: engine.Change, Participants: participants}
	before := len(test.eng.applied)
	if err := test.pollErr(); err == nil || len(test.eng.applied) != before {
		t.Fatal("leadership change allowed a membership mutation after an unresolved safety fault")
	}
	test.eng.plan = engine.Action{Kind: engine.RecreateGroup}
	test.poll()
	if len(test.wl.resetAll) != 1 {
		t.Fatal("policy could not reset the unsafe group")
	}
}

func TestCompletedActionIsObservedAfterLeaderOutage(t *testing.T) {
	test := ownershipSharedGroup(t)
	test.eng.planFunc = ownershipPlan(1)
	old := test.member("dp1").ID()
	test.member("dp1").Serving, test.member("dp1").Terminal = false, true
	ownershipPollUntil(t, test, func() bool { return len(test.eng.applied) == 1 })
	test.clock = test.clock.Add(test.config.ApplyTimeout)
	test.eng.progress = engine.Report{State: engine.Succeeded}
	test.load()
	test.poll()
	record := test.record()
	if record.Result == nil || record.Result.State != engine.Succeeded || record.Members[old] != Excluded || record.Unsafe != "" {
		t.Fatal("leader outage turned a completed operation into an uncertain failure")
	}
	if len(test.eng.applied) != 1 {
		t.Fatal("completed operation was dispatched again")
	}
}

func TestUncheckpointedActionCannotAdvanceAfterDeadline(t *testing.T) {
	test := ownershipSharedGroup(t)
	test.eng.planFunc = ownershipPlan(1)
	test.member("dp1").Serving, test.member("dp1").Terminal = false, true
	test.eng.onApply = func() {
		test.store.saveErrs = []error{errors.New("checkpoint save failed")}
	}
	if err := test.pollErr(); err == nil {
		t.Fatal("checkpoint save failure was hidden")
	}
	if len(test.eng.applied) != 1 || test.record().Operation == nil || len(test.record().Operation.Checkpoint) != 0 {
		t.Fatal("test did not interrupt the action before its checkpoint was saved")
	}
	operation := test.record().Operation.ID
	test.eng.onApply = nil
	test.clock = test.clock.Add(test.config.ApplyTimeout)
	test.eng.progress = engine.Report{State: engine.Running}
	test.load()
	test.poll()
	record := test.record()
	last := test.eng.applyCalls[len(test.eng.applyCalls)-1]
	if len(test.eng.applied) != 1 || record.Operation == nil || record.Operation.ID != operation ||
		record.OperationBlocked == "" || record.Result != nil || last.MayAdvance {
		t.Fatal("expired operation was advanced or treated as a confirmed failure")
	}
}

func TestInvalidJournalCannotDiscardRecoveryIntent(t *testing.T) {
	for name, data := range map[string]string{
		"legacy pending reconfiguration": `{"phase":"recovering","reconfig":{"ID":"unfinished"}}`,
		"previous schema":                `{"version":1,"phase":"serving","epoch":"2026-10-12T10:00:00Z"}`,
		"unknown schema":                 `{"version":3,"phase":"serving","epoch":"2026-10-12T10:00:00Z"}`,
		"unknown phase":                  `{"version":2,"phase":"unexpected","epoch":"2026-10-12T10:00:00Z"}`,
		"missing epoch":                  `{"version":2,"phase":"serving"}`,
		"missing operation identity": `{"version":2,"phase":"recovering","epoch":"2026-10-12T10:00:00Z",
			"operation":{"Action":{"Kind":"Change"}},"operation_since":"2026-10-12T10:00:00Z"}`,
		"missing operation start": `{"version":2,"phase":"recovering","epoch":"2026-10-12T10:00:00Z",
			"operation":{"ID":"unfinished","Action":{"Kind":"Change"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			test := newHarness(t)
			test.store.data = []byte(data)
			loaded, err := New(t.Context(), test.wl, test.eng, test.store, test.rec, test.config)
			if err == nil || loaded != nil {
				t.Fatal("invalid journal was accepted as a new recovery")
			}
			if string(test.store.data) != data || test.store.saves != 0 || len(test.eng.applied)+len(test.wl.resetAll) != 0 {
				t.Fatal("invalid journal was overwritten or triggered a side effect")
			}
		})
	}
}

func TestGroupRecreationPreservesPolicyBudgetAndPodTargets(t *testing.T) {
	test := ownershipSharedGroup(t)
	checkpoint := ownershipState(ownershipCheckpoint{Stage: "reset", Attempts: 2})
	test.eng.plan = engine.Action{Kind: engine.RecreateGroup, State: checkpoint}
	test.poll()
	operation := test.record().Operation.ID
	budget := test.record().Budget
	old := test.wl.group.Pods[0]
	replacementPod := old
	replacementPod.UID, replacementPod.Identity = "a2", "idA2"
	test.wl.group.Pods = append(test.wl.group.Pods, replacementPod)
	test.eng.plan = engine.Action{Kind: engine.Healthy}
	test.load()
	test.poll()
	for _, targets := range test.wl.resetAll {
		if len(targets) != 1 || targets[0].UID != old.UID {
			t.Fatalf("reset replay targeted a replacement Pod: %v", targets)
		}
	}
	test.wl.group.Pods = test.wl.group.Pods[1:]
	for index := range test.eng.members {
		test.eng.members[index].Pod.UID = replacementPod.UID
		test.eng.members[index].PodIdentity = replacementPod.Identity
	}
	test.poll()
	test.load()
	test.poll()
	input := test.eng.input
	if string(input.State) != string(checkpoint) || !input.Starting {
		t.Fatal("group recreation erased policy state or hid the new group from policy")
	}
	if input.LastResult == nil || input.LastResult.Operation.ID != operation || input.LastResult.State != engine.Succeeded {
		t.Fatal("new group did not receive the completed reset outcome")
	}
	if input.Budget.Actions != budget.Actions || input.Budget.Actions != 1 || !test.record().Budget.Started.Equal(budget.Started) {
		t.Fatal("group recreation reset the controller recovery budget")
	}
}

func TestRestartedContainerIsNotOfferedForOldMembershipReset(t *testing.T) {
	for _, observed := range []string{"unreported", "joinable", "serving"} {
		t.Run(observed, func(t *testing.T) {
			test := newHarness(t)
			test.serving()
			old := test.member("dp3").ID()
			test.member("dp3").Serving, test.member("dp3").Terminal = false, true
			test.eng.plan = test.shrinkPlan()
			test.poll()
			test.eng.progress = engine.Report{State: engine.Succeeded}
			test.poll()
			pod := &test.wl.group.Pods[3]
			pod.Identity = "idD2"
			if observed != "unreported" {
				fresh := *test.member("dp3")
				fresh.PodIdentity, fresh.Terminal, fresh.Joinable = pod.Identity, false, true
				fresh.Serving = observed == "serving"
				test.eng.members = append(test.eng.members, fresh)
			}
			test.eng.plan = engine.Action{Kind: engine.Wait}
			test.load()
			test.poll()
			if test.record().Members[old] != Excluded || test.record().Pods[pod.UID].Identity != "idD" {
				t.Fatal("container restart discarded the old membership history")
			}
			test.eng.plan = engine.Action{Kind: engine.ReplaceMembers, Members: []engine.MemberID{old}}
			test.pollAnyway()
			if len(test.wl.resets) != 0 || test.record().Operation != nil {
				t.Fatal("old excluded membership authorized resetting a new container")
			}
			if observed != "serving" {
				result := test.record().Result
				if result == nil || result.Operation.Action.Kind != engine.ReplaceMembers || result.State != engine.Failed {
					t.Fatal("unsafe replacement requirement did not produce a failed result")
				}
			}
		})
	}
}

func TestNativePayloadSurvivesDispatchRecoveryAndCompletion(t *testing.T) {
	test := ownershipSharedGroup(t)
	payload := json.RawMessage(`{"action":"prepare-process","member":"dp1","generation":2}`)
	checkpoint := ownershipState(ownershipCheckpoint{Stage: "prepare", Attempts: 1})
	participants := make([]engine.MemberID, 0, len(test.eng.members))
	for _, member := range test.eng.members {
		participants = append(participants, member.ID())
	}
	test.eng.plan = engine.Action{Kind: engine.Change, Participants: participants, Payload: payload, State: checkpoint}
	test.eng.onApply = func() {
		record := test.record()
		if record.Operation == nil || string(record.Operation.Action.Payload) != string(payload) ||
			string(record.PolicyState) != string(checkpoint) {
			t.Fatal("native payload and checkpoint were not saved before dispatch")
		}
		test.store.saveErrs = []error{errors.New("dispatch receipt was not saved")}
	}
	if err := test.pollErr(); err == nil {
		t.Fatal("dispatch receipt save failure was hidden")
	}
	operation := test.record().Operation.ID
	test.eng.onApply = nil
	test.eng.plan = engine.Action{
		Kind: engine.Change, Participants: participants, Payload: json.RawMessage(`{"action":"different"}`),
	}
	test.eng.applyFunc = func(input engine.ApplyInput) (engine.Report, error) {
		if !input.Operation.Attempted || len(input.Operation.Checkpoint) != 0 || !input.MayAdvance {
			t.Fatal("driver could not resume the saved action after its checkpoint was lost")
		}
		return engine.Report{State: engine.Running, Checkpoint: json.RawMessage(`{"stage":"prepared"}`)}, nil
	}
	test.load()
	test.poll()
	if len(test.eng.applyCalls) != 2 || string(test.record().Operation.Checkpoint) != `{"stage":"prepared"}` {
		t.Fatal("native action did not resume and save its execution checkpoint after reload")
	}
	for _, input := range test.eng.applyCalls {
		if input.Operation.ID != operation || string(input.Operation.Action.Payload) != string(payload) {
			t.Fatal("native action identity or payload changed on replay")
		}
	}
	test.eng.applyFunc = nil
	test.eng.progress = engine.Report{State: engine.Succeeded}
	test.poll()
	test.eng.plan = engine.Action{Kind: engine.Healthy}
	test.poll()
	result := test.eng.input.LastResult
	if result == nil || result.Operation.ID != operation || string(result.Operation.Action.Payload) != string(payload) {
		t.Fatal("policy outcome lost the native action payload")
	}
}

func TestWaitingPolicyRecordsRecoveryAndDurationsAcrossReload(t *testing.T) {
	test := newHarness(t)
	test.serving()
	initiallyFaulted := test.member("dp2").ID()
	test.member("dp2").Serving = false
	test.eng.plan = engine.Action{Kind: engine.Wait, Reason: "engine is diagnosing the fault"}
	test.poll()
	record := test.record()
	if record.Phase != Recovering || record.Incident == nil || !slices.Contains(record.Incident.Affected, initiallyFaulted) {
		t.Fatal("waiting for engine diagnosis left the unhealthy group reported as serving")
	}
	incident := record.Incident
	test.load()
	test.poll()
	if test.record().Incident == nil || test.record().Incident.ID != incident.ID {
		t.Fatal("waiting after reload replaced the ongoing incident")
	}
	if len(test.eng.applied)+len(test.eng.verified)+test.eng.probes+len(test.wl.resets)+len(test.wl.resetAll) != 0 {
		t.Fatal("recording the waiting incident scheduled an unplanned action")
	}
	test.member("dp2").Serving = true
	test.member("dp3").Serving, test.member("dp3").Terminal = false, true
	removed := test.member("dp3").ID()
	test.eng.plan = test.shrinkPlan()
	test.poll()
	record = test.record()
	if record.Incident.Durations.Diagnose != test.clock.Sub(incident.Opened) || !slices.Contains(record.Incident.Affected, removed) {
		t.Fatal("first planned change lost diagnosis time or its affected member")
	}
	test.eng.progress = engine.Report{State: engine.Succeeded}
	test.poll()
	test.eng.plan = engine.Action{Kind: engine.Verify, Participants: test.shrinkPlan().Participants}
	test.eng.applyFunc = func(input engine.ApplyInput) (engine.Report, error) {
		if input.Operation.Action.Kind != engine.Verify {
			t.Fatal("unexpected engine action")
		}
		test.eng.verified = append(test.eng.verified, slices.Clone(input.Operation.Action.Participants))
		test.store.saveErrs = []error{errors.New("verification outcome save failed")}
		return engine.Report{State: engine.Succeeded}, nil
	}
	if err := test.pollErr(); err == nil || test.record().Operation == nil || test.record().Operation.Action.Kind != engine.Verify {
		t.Fatal("test did not interrupt verification before its outcome was saved")
	}
	if len(test.eng.verified) != 1 {
		t.Fatal("verification was not executed before its completion save failed")
	}
	test.eng.applyFunc = nil
	test.load()
	test.poll()
	test.eng.plan = engine.Action{Kind: engine.Healthy}
	test.poll()
	closed := test.rec.closed
	if closed == nil || closed.ID != incident.ID || closed.Outcome != Shrunk || test.record().Incident != nil {
		t.Fatal("healthy policy decision did not close the original recovery incident")
	}
	if closed.Durations.Diagnose <= 0 || closed.Durations.Apply <= 0 || closed.Durations.Verify <= 0 ||
		closed.Durations.Total < closed.Durations.Diagnose+closed.Durations.Apply+closed.Durations.Verify {
		t.Fatalf("recovery durations were lost across explicit stages or reload: %+v", closed.Durations)
	}
}

func TestObservationTimeoutPreservesOnlyMembershipUncertainty(t *testing.T) {
	for _, pending := range []bool{false, true} {
		name := "observation only"
		if pending {
			name = "membership operation pending"
		}
		t.Run(name, func(t *testing.T) {
			test := newHarness(t)
			test.serving()
			if pending {
				test.eng.plan = engine.Action{Kind: engine.Change, Participants: slices.Clone(test.eng.admitted)}
				test.poll()
				test.eng.progress = engine.Report{State: engine.Running}
			}
			for index := range test.eng.members {
				test.eng.members[index].Unknown = true
				test.eng.members[index].Serving = false
			}
			test.eng.plan = engine.Action{Kind: engine.Wait}
			if err := test.pollErr(); err == nil {
				t.Fatal("unreadable members were not reported")
			}
			test.clock = test.clock.Add(test.config.UnseenTimeout)
			test.poll()
			if !pending && (!test.eng.input.ObservationTimedOut || test.eng.input.Fault != "") {
				t.Fatal("policy did not receive the observation timeout")
			}
			test.load()
			for index := range test.eng.members {
				test.eng.members[index].Unknown = false
				test.eng.members[index].Serving = true
			}
			test.eng.plan = engine.Action{Kind: engine.Healthy}
			err := test.pollErr()
			if pending {
				record := test.record()
				last := test.eng.applyCalls[len(test.eng.applyCalls)-1]
				if err != nil || record.Operation == nil || record.OperationBlocked == "" || record.Result != nil || last.MayAdvance {
					t.Fatal("restored visibility erased an uncertain membership operation")
				}
				test.eng.progress = engine.Report{State: engine.Succeeded}
				test.poll()
				test.poll()
				if test.record().Operation != nil || test.record().OperationBlocked != "" || test.record().Phase != Serving {
					t.Fatal("confirmed completion did not resolve the operation after visibility returned")
				}
			} else if err != nil || test.eng.input.ObservationTimedOut || test.eng.input.Fault != "" ||
				test.record().Phase != Serving {
				t.Fatalf("restored visibility retained a permanent observation fault: %v", err)
			}
		})
	}
}
