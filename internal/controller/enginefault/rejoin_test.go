// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

//nolint:testpackage // Exercises journal recovery through load.
package enginefault

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/llm-d/llm-d-resiliency-operator/internal/engine"
)

func sharedGroup(t *testing.T) *harness {
	t.Helper()
	test := newHarness(t)
	test.wl.group.Pods = test.wl.group.Pods[:1]
	for i := range test.eng.members {
		test.eng.members[i].Pod = test.eng.members[0].Pod
		test.eng.members[i].PodIdentity = "idA"
	}
	test.serving()
	return test
}

func excludeMember(t *testing.T, test *harness) engine.MemberID {
	t.Helper()
	old := test.member("dp1").ID()
	var survivors []engine.MemberID
	for i := range test.eng.members {
		m := &test.eng.members[i]
		m.Serving = false
		if m.ID() != old {
			survivors = append(survivors, m.ID())
		}
	}
	test.member("dp1").Terminal = true
	test.eng.plan = engine.Action{Kind: engine.Change, Participants: survivors, Remove: []engine.MemberID{old}}
	test.poll()
	test.eng.progress = engine.Report{State: engine.Succeeded}
	for i := range test.eng.members {
		test.eng.members[i].Serving = !test.eng.members[i].Terminal
	}
	test.poll()
	test.eng.plan = engine.Action{Kind: engine.Verify, Participants: survivors}
	test.poll()
	test.eng.plan = engine.Action{Kind: engine.Healthy}
	if test.record().Members[old] != Excluded {
		t.Fatal("failed member was not excluded")
	}
	return old
}

func replacement(test *harness, old engine.MemberID, name string) engine.Member {
	return engine.Member{
		Name: name, Pod: test.eng.members[0].Pod, PodIdentity: "idA",
		Replaces: old, Joinable: true,
	}
}

func addPlan(test *harness, candidate *engine.Member) engine.Action {
	var participants []engine.MemberID
	for _, m := range test.eng.members {
		if m.Serving {
			participants = append(participants, m.ID())
		}
	}
	return engine.Action{Kind: engine.Change, Participants: participants, Add: []engine.MemberID{candidate.ID()}}
}

func TestMemberRejoinsSharedPod(t *testing.T) {
	test := sharedGroup(t)
	old := excludeMember(t, test)
	candidate := replacement(test, old, "dp1-process-2")
	test.eng.members = append(test.eng.members, candidate)
	test.eng.plan = addPlan(test, &candidate)
	test.eng.onApply = func() {
		record := test.record()
		if record.Operation == nil || !slices.Equal(record.Operation.Action.Add, []engine.MemberID{candidate.ID()}) {
			t.Fatal("replacement attempt was not saved before apply")
		}
	}
	test.poll()
	if len(test.eng.candidates) != 1 || test.eng.candidates[0] != candidate.ID() {
		t.Fatalf("replacement not offered as candidate: %v", test.eng.candidates)
	}
	test.eng.members[len(test.eng.members)-1].Serving = true
	test.eng.progress = engine.Report{State: engine.Succeeded}
	test.poll()
	record := test.record()
	if record.Members[old] != Excluded || record.Members[candidate.ID()] != Admitted {
		t.Fatalf("wrong members admitted: %v", record.Members)
	}
	if len(test.wl.resets) != 0 || len(test.wl.resetAll) != 0 {
		t.Fatal("shared Pod was reset")
	}
	test.eng.plan = engine.Action{
		Kind: engine.Verify, Participants: append(slices.Clone(test.eng.plan.Participants), candidate.ID()),
	}
	test.poll()
	test.eng.plan = engine.Action{Kind: engine.Healthy}
	test.poll()
}

func TestReplacementFailureKeepsSurvivors(t *testing.T) {
	for _, atApply := range []bool{false, true} {
		t.Run(fmt.Sprintf("apply=%t", atApply), func(t *testing.T) {
			test := sharedGroup(t)
			old := excludeMember(t, test)
			candidate := replacement(test, old, "dp1-process-2")
			test.eng.members = append(test.eng.members, candidate)
			test.eng.plan = addPlan(test, &candidate)
			if atApply {
				test.eng.initialReport = &engine.Report{
					State: engine.Failed, Scope: engine.Joiners, Reason: "launch failed; candidate stopped",
				}
			}
			test.poll()
			if !atApply {
				test.eng.progress = engine.Report{
					State: engine.Failed, Scope: engine.Joiners, Reason: "join rolled back; candidate stopped",
				}
				test.poll()
			}
			if test.record().Members[candidate.ID()] != Rejected || len(test.wl.resetAll) != 0 {
				t.Fatalf("joiner failure did not preserve survivors: %+v", test.record())
			}
			if !slices.Contains(test.rec.calls, "ReconfigFinished Failed") {
				t.Fatal("failure was not recorded")
			}
			test.eng.plan = engine.Action{Kind: engine.Healthy}
			test.eng.applyErr, test.eng.initialReport = nil, nil
			test.load()
			test.poll()
			newer := replacement(test, old, "dp1-process-3")
			test.eng.members = append(test.eng.members, newer)
			test.poll()
			if !slices.Equal(test.eng.candidates, []engine.MemberID{newer.ID()}) {
				t.Fatalf("fresh replacement was hidden or rejected replacement offered: %v", test.eng.candidates)
			}
			result := test.eng.input.LastResult
			if result == nil || result.State != engine.Failed ||
				!slices.Equal(result.Operation.Action.Add, []engine.MemberID{candidate.ID()}) {
				t.Fatal("replacement failure was not supplied to policy after reload")
			}
			if len(test.eng.applied) != 2 {
				t.Fatal("controller retried a replacement without a policy decision")
			}
			if len(test.wl.resetAll) != 0 {
				t.Fatal("survivors were reset")
			}
		})
	}
}

func TestReplacementRequiresExcludedMember(t *testing.T) {
	test := sharedGroup(t)
	candidate := replacement(test, test.member("dp1").ID(), "dp1-process-2")
	test.eng.members = append(test.eng.members, candidate)
	test.eng.plan = engine.Action{Kind: engine.Healthy}
	test.poll()
	if len(test.eng.candidates) != 0 {
		t.Fatal("replacement of admitted member was offered")
	}
}

func TestReplacementRequiresDistinctMemberInSamePod(t *testing.T) {
	for _, invalid := range []string{"same member", "different pod"} {
		t.Run(invalid, func(t *testing.T) {
			test := sharedGroup(t)
			old := excludeMember(t, test)
			candidate := replacement(test, old, "dp1-process-2")
			if invalid == "same member" {
				candidate.Name = "dp1"
			} else {
				pod := test.wl.group.Pods[0]
				pod.UID, pod.Identity = "other-pod", "idB"
				test.wl.group.Pods = append(test.wl.group.Pods, pod)
				candidate.Pod.UID, candidate.PodIdentity = pod.UID, pod.Identity
			}
			test.eng.members = append(test.eng.members, candidate)
			test.eng.plan = engine.Action{Kind: engine.Healthy}
			test.pollAnyway()
			if len(test.eng.candidates) != 0 {
				t.Fatalf("invalid replacement offered: %v", test.eng.candidates)
			}
			if len(test.eng.applied) != 1 {
				t.Fatal("invalid replacement attempted")
			}
		})
	}
}

func TestReplacementUsesPolicyParticipants(t *testing.T) {
	test := sharedGroup(t)
	old := excludeMember(t, test)
	candidate := replacement(test, old, "dp1-process-2")
	test.eng.members = append(test.eng.members, candidate)
	test.eng.members[0].Serving = false
	test.eng.plan = addPlan(test, &candidate)
	test.eng.plan.Participants = []engine.MemberID{test.eng.members[0].ID()}
	before := len(test.eng.applied)
	test.poll()
	if len(test.eng.applied) != before+1 || !slices.Equal(test.eng.applied[before].Participants, test.eng.plan.Participants) {
		t.Fatal("controller overrode the policy's admission participants")
	}
}

func TestAmbiguousReplacementBlocksResetUntilTerminalReport(t *testing.T) {
	test := sharedGroup(t)
	old := excludeMember(t, test)
	candidate := replacement(test, old, "dp1-process-2")
	test.eng.members = append(test.eng.members, candidate)
	test.eng.plan = addPlan(test, &candidate)
	test.eng.applyErr = errors.New("membership commit unknown")
	if err := test.pollErr(); err == nil {
		t.Fatal("uncertain dispatch error was hidden")
	}
	record := test.record()
	if record.Operation == nil || len(test.wl.resetAll) != 0 {
		t.Fatal("uncertain outcome discarded the pending operation")
	}
	operation := record.Operation.ID
	test.eng.applyErr = nil
	test.eng.progress = engine.Report{State: engine.Running}
	test.eng.plan = engine.Action{Kind: engine.RecreateGroup}
	test.load()
	test.poll()
	if record = test.record(); record.Operation == nil || record.Operation.ID != operation || len(test.wl.resetAll) != 0 {
		t.Fatal("group reset overlapped an unresolved admission")
	}
	test.eng.progress = engine.Report{
		State: engine.Failed, Scope: engine.Group, Reason: "admission stopped; group requires recreation",
	}
	test.poll()
	if record = test.record(); record.Operation != nil || record.Result == nil || record.Result.Operation.ID != operation {
		t.Fatal("terminal admission report did not resolve the saved operation")
	}
	test.poll()
	if len(test.wl.resetAll) != 1 {
		t.Fatal("policy group reset was not executed")
	}
}

func TestReplacementUsesSameOperationAfterReload(t *testing.T) {
	test := sharedGroup(t)
	old := excludeMember(t, test)
	candidate := replacement(test, old, "dp1-process-2")
	test.eng.members = append(test.eng.members, candidate)
	test.eng.plan = addPlan(test, &candidate)
	test.poll()
	first := test.record().Operation.ID
	test.eng.progress = engine.Report{State: engine.Running}
	test.load()
	test.poll()
	operation := test.record().Operation
	if operation == nil || operation.ID != first || !slices.Equal(operation.Action.Add, []engine.MemberID{candidate.ID()}) {
		t.Fatal("operation changed after reload")
	}
	if len(test.eng.applied) != 2 {
		t.Fatal("replacement was dispatched again after acceptance")
	}
}

func TestReplacementSaveFailurePreventsLaunch(t *testing.T) {
	test := sharedGroup(t)
	old := excludeMember(t, test)
	candidate := replacement(test, old, "dp1-process-2")
	test.eng.members = append(test.eng.members, candidate)
	test.eng.plan = addPlan(test, &candidate)
	test.store.saveErrs = []error{errors.New("journal unavailable")}
	before := len(test.eng.applied)
	if err := test.pollErr(); err == nil {
		t.Fatal("save error was hidden")
	}
	if len(test.eng.applied) != before || test.record().Operation != nil {
		t.Fatal("launch occurred without saved intent")
	}
	test.poll()
	if len(test.eng.applied) != before+1 {
		t.Fatal("replacement was not attempted after the journal recovered")
	}
}

func TestReplacementRejectsMultipleCandidatesForOneMember(t *testing.T) {
	test := sharedGroup(t)
	old := excludeMember(t, test)
	first, second := replacement(test, old, "dp1-process-2"), replacement(test, old, "dp1-process-3")
	test.eng.members = append(test.eng.members, first, second)
	test.eng.plan = addPlan(test, &first)
	test.eng.plan.Add = append(test.eng.plan.Add, second.ID())
	before := len(test.eng.applied)
	if err := test.pollErr(); err == nil {
		t.Fatal("invalid plan naming two replacements was accepted")
	}
	if len(test.eng.applied) != before {
		t.Fatal("two replacements were launched for the same member")
	}
}

func TestReplacementDeadlinePreservesSafety(t *testing.T) {
	test := sharedGroup(t)
	old := excludeMember(t, test)
	candidate := replacement(test, old, "dp1-process-2")
	test.eng.members = append(test.eng.members, candidate)
	test.eng.plan = addPlan(test, &candidate)
	test.poll()
	operation := test.record().Operation.ID
	test.eng.progress = engine.Report{State: engine.Running}
	test.clock = test.clock.Add(test.config.ApplyTimeout)
	test.poll()
	record := test.record()
	if record.Operation == nil || record.Operation.ID != operation || record.OperationBlocked == "" || len(test.wl.resetAll) != 0 {
		t.Fatal("deadline did not preserve the unresolved operation")
	}
	test.eng.plan = engine.Action{Kind: engine.RecreateGroup}
	test.load()
	test.poll()
	last := test.eng.applyCalls[len(test.eng.applyCalls)-1]
	if last.MayAdvance || len(test.wl.resetAll) != 0 || test.record().Operation.ID != operation {
		t.Fatal("expired admission was advanced or replaced by a reset")
	}
	test.eng.progress = engine.Report{State: engine.Failed, Scope: engine.Group, Reason: "admission stopped"}
	test.poll()
	test.poll()
	if len(test.wl.resetAll) != 1 || test.record().Result.Operation.ID != operation {
		t.Fatal("confirmed terminal outcome did not permit the next recovery decision")
	}
}

func TestRejectedReplacementCannotServe(t *testing.T) {
	test := sharedGroup(t)
	old := excludeMember(t, test)
	candidate := replacement(test, old, "dp1-process-2")
	test.eng.members = append(test.eng.members, candidate)
	test.eng.plan = addPlan(test, &candidate)
	test.eng.initialReport = &engine.Report{State: engine.Failed, Scope: engine.Joiners, Reason: "launch refused"}
	test.poll()
	test.eng.members[len(test.eng.members)-1].Serving = true
	test.eng.plan = engine.Action{Kind: engine.Healthy}
	if err := test.pollErr(); err == nil || test.eng.input.Fault == "" {
		t.Fatal("rejected member served without admission")
	}
	if len(test.wl.resetAll) != 0 {
		t.Fatal("controller selected recovery for the rejected member")
	}
	test.eng.plan = engine.Action{Kind: engine.RecreateGroup}
	test.poll()
	if len(test.wl.resetAll) != 1 {
		t.Fatal("policy reset was not executed after the safety fault")
	}
}

func TestReservedReplacementIsNotAdmittedAtStartup(t *testing.T) {
	test := newHarness(t)
	previous := test.eng.members[1]
	candidate := previous
	candidate.Name, candidate.Replaces = "dp1-process-2", previous.ID()
	candidate.Serving, candidate.Joinable = false, true
	test.eng.members = append(test.eng.members, candidate)
	test.load()
	test.poll()
	if _, admitted := test.record().Members[candidate.ID()]; admitted {
		t.Fatal("unlaunched replacement was adopted at startup")
	}
}

func TestLegacyRecoveryDoesNotAttemptRejoin(t *testing.T) {
	test := sharedGroup(t)
	test.eng.grow = false
	old := excludeMember(t, test)
	for range 2 {
		test.load()
		for range 10 {
			test.poll()
		}
		record := test.record()
		if record.Phase != Serving || record.Members[old] != Excluded || record.Operation != nil {
			t.Fatalf("legacy recovery created replacement state: %+v", record)
		}
		if len(test.eng.applied) != 1 || len(test.wl.resets) != 0 || len(test.wl.resetAll) != 0 {
			t.Fatal("legacy recovery attempted another action")
		}
		for _, member := range test.eng.members {
			if member.Replaces != "" || member.ID() != engine.MemberID(member.Name+"@idA") {
				t.Fatalf("legacy member identity changed: %+v", member)
			}
		}
	}
}
