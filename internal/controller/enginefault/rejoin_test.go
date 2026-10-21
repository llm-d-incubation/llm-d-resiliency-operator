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
	test.eng.plan = engine.Plan{Kind: engine.Change, Participants: survivors, Remove: []engine.MemberID{old}}
	test.poll()
	test.poll()
	test.poll()
	test.eng.progress = engine.Progress{State: engine.Succeeded}
	for i := range test.eng.members {
		test.eng.members[i].Serving = !test.eng.members[i].Terminal
	}
	test.poll()
	test.eng.plan = engine.Plan{Kind: engine.Healthy}
	if test.record().Members[old] != Excluded {
		t.Fatal("failed member was not excluded")
	}
	return old
}

func replacement(test *harness, old engine.MemberID, generation string) engine.Member {
	return engine.Member{
		Name: "dp1", Pod: test.eng.members[0].Pod, PodIdentity: "idA",
		Incarnation: generation, Replaces: old, Joinable: true,
	}
}

func addPlan(test *harness, candidate *engine.Member) engine.Plan {
	var participants []engine.MemberID
	for _, m := range test.eng.members {
		if m.Serving {
			participants = append(participants, m.ID())
		}
	}
	return engine.Plan{Kind: engine.Change, Participants: participants, Add: []engine.MemberID{candidate.ID()}}
}

func TestMemberRejoinsSharedPod(t *testing.T) {
	test := sharedGroup(t)
	old := excludeMember(t, test)
	candidate := replacement(test, old, "generation-2")
	test.eng.members = append(test.eng.members, candidate)
	test.eng.plan = addPlan(test, &candidate)
	test.eng.onApply = func() {
		record := test.record()
		if record.Replacements[old] != candidate.ID() || record.Reconfig == nil {
			t.Fatal("replacement attempt was not saved before apply")
		}
	}
	test.poll()
	if len(test.eng.candidates) != 1 || test.eng.candidates[0] != candidate.ID() {
		t.Fatalf("replacement not offered as candidate: %v", test.eng.candidates)
	}
	test.eng.members[len(test.eng.members)-1].Serving = true
	test.eng.progress = engine.Progress{State: engine.Succeeded}
	test.poll()
	record := test.record()
	if record.Members[old] != Excluded || record.Members[candidate.ID()] != Admitted {
		t.Fatalf("wrong incarnations admitted: %v", record.Members)
	}
	if len(test.wl.resets) != 0 || len(test.wl.resetAlls) != 0 {
		t.Fatal("shared Pod was reset")
	}
	test.eng.plan = engine.Plan{Kind: engine.Healthy}
	test.poll()
}

func TestReplacementFailureKeepsSurvivors(t *testing.T) {
	for _, atApply := range []bool{false, true} {
		t.Run(fmt.Sprintf("apply=%t", atApply), func(t *testing.T) {
			test := sharedGroup(t)
			old := excludeMember(t, test)
			candidate := replacement(test, old, "generation-2")
			test.eng.members = append(test.eng.members, candidate)
			test.eng.plan = addPlan(test, &candidate)
			if atApply {
				test.eng.applyErr = &engine.ApplyError{
					Scope: engine.Joiners, Err: errors.New("launch failed; candidate stopped"),
				}
			}
			test.poll()
			if !atApply {
				test.eng.progress = engine.Progress{
					State: engine.Failed, Scope: engine.Joiners, Reason: "join rolled back; candidate stopped",
				}
				test.poll()
			}
			if test.record().Members[candidate.ID()] != Rejected || len(test.wl.resetAlls) != 0 {
				t.Fatalf("joiner failure did not preserve survivors: %+v", test.record())
			}
			if !slices.Contains(test.rec.calls, "ReconfigFinished Failed") {
				t.Fatal("failure was not recorded")
			}
			test.eng.plan = engine.Plan{Kind: engine.Healthy}
			test.eng.applyErr = nil
			test.load()
			test.poll()
			newer := replacement(test, old, "generation-3")
			test.eng.members = append(test.eng.members, newer)
			test.poll()
			if len(test.eng.candidates) != 0 {
				t.Fatalf("replacement retried after reload: %v", test.eng.candidates)
			}
			if test.record().Replacements[old] != candidate.ID() {
				t.Fatal("attempt identity changed")
			}
			if len(test.wl.resetAlls) != 0 {
				t.Fatal("survivors were reset")
			}
		})
	}
}

func TestReplacementRequiresExcludedMember(t *testing.T) {
	test := sharedGroup(t)
	candidate := replacement(test, test.member("dp1").ID(), "generation-2")
	test.eng.members = append(test.eng.members, candidate)
	test.eng.plan = engine.Plan{Kind: engine.Healthy}
	test.poll()
	if len(test.eng.candidates) != 0 {
		t.Fatal("replacement of admitted member was offered")
	}
}

func TestReplacementRequiresHealthySurvivors(t *testing.T) {
	test := sharedGroup(t)
	old := excludeMember(t, test)
	candidate := replacement(test, old, "generation-2")
	test.eng.members = append(test.eng.members, candidate)
	test.eng.members[0].Serving = false
	test.eng.plan = addPlan(test, &candidate)
	test.eng.plan.Participants = append(test.eng.plan.Participants, test.eng.members[0].ID())
	before := len(test.eng.applied)
	test.poll()
	if len(test.eng.applied) != before {
		t.Fatal("replacement started before survivor verification")
	}
}

func TestAmbiguousReplacementFailureResetsGroup(t *testing.T) {
	test := sharedGroup(t)
	old := excludeMember(t, test)
	candidate := replacement(test, old, "generation-2")
	test.eng.members = append(test.eng.members, candidate)
	test.eng.plan = addPlan(test, &candidate)
	test.eng.applyErr = errors.New("membership commit unknown")
	test.poll()
	if len(test.wl.resetAlls) != 1 {
		t.Fatal("ambiguous join left the group serving")
	}
}

func TestReplacementUsesSameOperationAfterReload(t *testing.T) {
	test := sharedGroup(t)
	old := excludeMember(t, test)
	candidate := replacement(test, old, "generation-2")
	test.eng.members = append(test.eng.members, candidate)
	test.eng.plan = addPlan(test, &candidate)
	test.poll()
	first := test.record().Reconfig.ID
	test.eng.progress = engine.Progress{State: engine.Running}
	test.load()
	test.poll()
	if test.record().Reconfig.ID != first || test.record().Replacements[old] != candidate.ID() {
		t.Fatal("operation changed after reload")
	}
	if len(test.eng.applied) != 2 {
		t.Fatal("replacement was dispatched again after acceptance")
	}
}

func TestReplacementSaveFailurePreventsLaunch(t *testing.T) {
	test := sharedGroup(t)
	old := excludeMember(t, test)
	candidate := replacement(test, old, "generation-2")
	test.eng.members = append(test.eng.members, candidate)
	test.eng.plan = addPlan(test, &candidate)
	test.store.saveErrs = []error{errors.New("journal unavailable")}
	before := len(test.eng.applied)
	if err := test.pollErr(); err == nil {
		t.Fatal("save error was hidden")
	}
	if len(test.eng.applied) != before || len(test.record().Replacements) != 0 {
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
	first, second := replacement(test, old, "generation-2"), replacement(test, old, "generation-3")
	test.eng.members = append(test.eng.members, first, second)
	test.eng.plan = addPlan(test, &first)
	test.eng.plan.Add = append(test.eng.plan.Add, second.ID())
	before := len(test.eng.applied)
	test.poll()
	if len(test.eng.applied) != before {
		t.Fatal("two replacements were launched for the same member")
	}
}

func TestReplacementDeadlinePreservesSafety(t *testing.T) {
	test := sharedGroup(t)
	old := excludeMember(t, test)
	candidate := replacement(test, old, "generation-2")
	test.eng.members = append(test.eng.members, candidate)
	test.eng.plan = addPlan(test, &candidate)
	test.poll()
	test.eng.progress = engine.Progress{State: engine.Running}
	test.clock = test.clock.Add(test.config.ApplyTimeout)
	test.poll()
	if len(test.wl.resetAlls) != 1 {
		t.Fatal("unknown membership after deadline was ignored")
	}
}

func TestRejectedReplacementCannotServe(t *testing.T) {
	test := sharedGroup(t)
	old := excludeMember(t, test)
	candidate := replacement(test, old, "generation-2")
	test.eng.members = append(test.eng.members, candidate)
	test.eng.plan = addPlan(test, &candidate)
	test.eng.applyErr = &engine.ApplyError{Scope: engine.Joiners, Err: errors.New("launch refused")}
	test.poll()
	test.eng.members[len(test.eng.members)-1].Serving = true
	test.eng.plan = engine.Plan{Kind: engine.Healthy}
	test.poll()
	if len(test.wl.resetAlls) != 1 {
		t.Fatal("rejected member served without admission")
	}
}

func TestReservedReplacementIsNotAdmittedAtStartup(t *testing.T) {
	test := newHarness(t)
	previous := test.eng.members[1]
	candidate := previous
	candidate.Incarnation, candidate.Replaces = "generation-2", previous.ID()
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
		if record.Phase != Serving || record.Members[old] != Excluded || len(record.Replacements) != 0 {
			t.Fatalf("legacy recovery created replacement state: %+v", record)
		}
		if len(test.eng.applied) != 1 || len(test.wl.resets) != 0 || len(test.wl.resetAlls) != 0 {
			t.Fatal("legacy recovery attempted another action")
		}
		for _, member := range test.eng.members {
			if member.Incarnation != "" || member.Replaces != "" || member.ID() != engine.MemberID(member.Name+"@idA") {
				t.Fatalf("legacy member identity changed: %+v", member)
			}
		}
	}
}
