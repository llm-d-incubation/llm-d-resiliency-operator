// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

//nolint:testpackage // Tests drive the clock through the unexported load.
package enginefault

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/llm-d/llm-d-resiliency-operator/internal/engine"
	"github.com/llm-d/llm-d-resiliency-operator/internal/workload"
)

func (test *harness) replacementCandidate() {
	test.t.Helper()
	test.shrunk()
	test.poll()
	test.wl.group.Pods[3] = replaced
	test.wl.group.Pods[3].State = workload.PodReady
	*test.member("dp3") = engine.Member{
		Pod: workload.PodRef{Name: "lws-0-3", UID: "d2"}, PodIdentity: "idD2", Name: "dp3", Joinable: true,
	}
	test.poll()
	if test.record().Members["dp3@idD2"] != Candidate {
		test.t.Fatalf("members %+v", test.record().Members)
	}
}

func TestCandidateLostBeforeJoining(t *testing.T) {
	t.Parallel()
	for name, lose := range map[string]func(*harness){
		"restarts": func(test *harness) { test.wl.group.Pods[3].Identity = "idD3" },
		"goes":     func(test *harness) { test.wl.group.Pods = test.wl.group.Pods[:3] },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			test := newHarness(t)
			test.replacementCandidate()
			lose(test)
			test.poll()
			test.wantKept("dp3@idD2")
		})
	}
}

func TestWorkloadErrorsDoNotStopFaultHandling(t *testing.T) {
	t.Parallel()
	test := newHarness(t)
	test.shrunk()
	test.wl.resetErr = errors.New("forbidden")
	for range 3 {
		if err := test.pollErr(); err == nil {
			t.Fatal("the Reset error was not reported")
		}
	}
	test.member("dp1").Terminal = true
	test.eng.plan = engine.Plan{Kind: engine.Unrecoverable}
	test.pollAnyway()
	test.pollAnyway()
	test.wantReset(ResetUnrecoverable)
}

func TestRepeatedResetUnsupportedKeepsTheGroup(t *testing.T) {
	t.Parallel()
	test := newHarness(t)
	test.shrunk()
	test.wl.resetErr = errors.New("api timeout")
	test.pollAnyway()
	test.wl.resetErr = workload.ErrUnsupported
	test.poll()
	test.wantKept("dp3@idD")
}

func TestVerifyHangIsAFailedCheck(t *testing.T) {
	t.Parallel()
	test := newHarness(t)
	test.config.ApplyTimeout = 40 * time.Millisecond
	test.eng.hangVerify = true
	test.shrunk()
	if record := test.record(); record.VerifyFailedSince.IsZero() || len(record.Verifying) != 3 {
		t.Fatalf("a hung Verify left %+v", record)
	}
}

func TestWaitingDoesNotRewriteTheJournal(t *testing.T) {
	t.Parallel()
	test := newHarness(t)
	test.serving()
	test.dead()
	test.eng.plan = engine.Plan{Kind: engine.Wait}
	test.poll()
	saves := test.store.saves
	test.wl.group.RecreateStopped = true
	for range 10 {
		test.clock = test.clock.Add(time.Minute)
		test.poll()
	}
	if test.store.saves-saves > 1 {
		t.Fatalf("%d saves while waiting", test.store.saves-saves)
	}
	test.wl.group.RecreateStopped = false
	test.poll()
	test.clock = test.clock.Add(test.config.DiagnosisTimeout - 2*test.config.PollInterval)
	test.poll()
	if len(test.wl.resetAlls) != 0 {
		t.Fatal("the diagnosis deadline did not restart after the wait")
	}
}

func TestStarting(t *testing.T) {
	t.Parallel()
	t.Run("waits for every Pod", func(t *testing.T) {
		t.Parallel()
		test := newHarness(t)
		test.eng.members = test.eng.members[:3]
		test.load()
		test.poll()
		if test.record().Phase != Starting {
			t.Fatal("served without a member on every Pod")
		}
	})
	t.Run("serving members are admitted, not checked", func(t *testing.T) {
		t.Parallel()
		test := newHarness(t)
		test.eng.plan = engine.Plan{Kind: engine.Healthy, Reason: "4 ranks healthy"}
		test.load()
		test.poll()
		record := test.record()
		if record.Phase != Serving || len(record.Members) != 4 || len(test.wl.resetAlls) != 0 || record.Reason != "4 ranks healthy" {
			t.Fatalf("record %+v", record)
		}
		for id, state := range record.Members {
			if state != Admitted {
				t.Fatalf("%s is %s", id, state)
			}
		}
	})
	t.Run("a reset group that already serves is admitted once", func(t *testing.T) {
		t.Parallel()
		test := newHarness(t)
		test.serving()
		test.eng.plan = engine.Plan{Kind: engine.Unrecoverable}
		test.poll()
		test.poll()
		test.wantReset(ResetUnrecoverable)
		test.wl.group.Pods = nil
		test.poll()
		for index := range names {
			test.wl.group.Pods = append(test.wl.group.Pods, workload.Pod{
				Name: names[index], UID: names[index] + "-new", IP: "10.0.1.1", Index: index, Identity: "new" + names[index],
				State: workload.PodReady,
			})
			test.eng.members[index].Pod.UID, test.eng.members[index].PodIdentity = names[index]+"-new", "new"+names[index]
		}
		test.eng.plan = engine.Plan{Kind: engine.Healthy}
		resets := len(test.wl.resetAlls)
		test.poll()
		test.poll()
		test.poll()
		record := test.record()
		if record.Phase != Serving || len(record.Members) != 4 || len(test.wl.resetAlls) != resets {
			t.Fatalf("record %+v, group resets %d", record, len(test.wl.resetAlls)-resets)
		}
	})
	t.Run("admits a terminal member", func(t *testing.T) {
		t.Parallel()
		test := newHarness(t)
		test.member("dp3").Terminal = true
		test.load()
		test.poll()
		if test.record().Members["dp3@idD"] != Admitted {
			t.Fatalf("members %+v", test.record().Members)
		}
		test.eng.plan = test.shrinkPlan()
		test.poll()
		if test.record().Phase != Recovering || len(test.wl.resetAlls) != 0 {
			t.Fatalf("record %+v", test.record())
		}
	})
}

func TestPodLostWhileServing(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		lose  func(*harness)
		reset bool
	}{
		"gone":        {lose: func(test *harness) { test.wl.group.Pods = test.wl.group.Pods[:3] }},
		"terminating": {lose: func(test *harness) { test.wl.group.Pods[3].State = workload.PodTerminating }, reset: true},
		"restarted":   {lose: func(test *harness) { test.wl.group.Pods[3].Identity = "idD9" }, reset: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			harness := newHarness(t)
			harness.serving()
			test.lose(harness)
			harness.eng.plan = harness.shrinkPlan()
			harness.poll()
			harness.poll()
			harness.poll()
			if len(harness.eng.applied) != 1 || !slices.Contains(harness.eng.admitted, "dp3@idD") {
				t.Fatalf("applied %d, admitted %v", len(harness.eng.applied), harness.eng.admitted)
			}
			lost := slices.IndexFunc(harness.eng.appliedOn[0].Members, func(member engine.Member) bool { return member.ID() == "dp3@idD" })
			if member := harness.eng.appliedOn[0].Members; lost < 0 || !member[lost].Terminal || member[lost].Detail != "pod lost" {
				t.Fatalf("Apply did not see the lost member: %+v", harness.eng.appliedOn[0].Members)
			}
			harness.eng.progress = engine.Progress{State: engine.Succeeded}
			harness.eng.plan = engine.Plan{Kind: engine.Healthy}
			harness.poll()
			harness.poll()
			if (len(harness.wl.resets) == 1) != test.reset {
				t.Fatalf("resets %v", harness.wl.resets)
			}
		})
	}
}

func TestOutcome(t *testing.T) {
	t.Parallel()
	reconfig := func(remove, add []engine.MemberID) ReconfigResult {
		return ReconfigResult{Reconfig: engine.Reconfig{Remove: remove, Add: add}, Progress: engine.Progress{State: engine.Succeeded}}
	}
	for want, reconfigs := range map[Outcome][]ReconfigResult{
		Shrunk:    {reconfig(ids("a@1"), nil)},
		Recovered: {reconfig(ids("a@1"), ids("b@2"))},
	} {
		if got := outcome(&Incident{Reconfigs: reconfigs}); got != want {
			t.Fatalf("outcome %s, want %s", got, want)
		}
	}
}

func TestIncidentNamesTheRemovedMember(t *testing.T) {
	t.Parallel()
	test := newHarness(t)
	test.serving()
	for _, name := range []string{"dp0", "dp1", "dp2"} {
		test.member(name).Serving = false
	}
	test.eng.plan = engine.Plan{Kind: engine.Wait, Reason: "survivors faulted first"}
	test.poll()
	test.dead()
	test.eng.plan = test.shrinkPlan()
	test.poll()
	test.poll()
	test.eng.progress = engine.Progress{State: engine.Succeeded}
	for _, name := range []string{"dp0", "dp1", "dp2"} {
		test.member(name).Serving = true
	}
	test.eng.plan = engine.Plan{Kind: engine.Healthy}
	test.poll()
	closed := test.rec.closed
	if closed == nil || !slices.Contains(closed.Affected, "dp3@idD") || !slices.Contains(closed.Nodes, "gpu-node-4") {
		t.Fatalf("closed incident %+v", closed)
	}
}

func TestResendCheckedAgain(t *testing.T) {
	t.Parallel()
	test := newHarness(t)
	test.replacementCandidate()
	test.eng.plan = engine.Plan{Kind: engine.Change, Participants: ids("dp0@idA", "dp1@idB", "dp2@idC"), Add: ids("dp3@idD2")}
	test.unconfirmed()
	sent := len(test.eng.applied)
	test.wl.group.Pods[3].State = workload.PodStarting
	test.eng.plan = engine.Plan{Kind: engine.Healthy}
	test.poll()
	record := test.record()
	if len(test.eng.applied) != sent || record.Reconfig != nil || len(test.wl.resetAlls) != 0 ||
		!strings.Contains(record.Reason, "dropped") {
		t.Fatalf("applied %d, record %+v", len(test.eng.applied), record)
	}
}

func TestDroppedResendRestartsTheDiagnosisDeadline(t *testing.T) {
	t.Parallel()
	test := newHarness(t)
	test.serving()
	test.dead()
	test.eng.plan = test.shrinkPlan()
	test.poll()
	test.poll()
	test.unconfirmed()
	test.eng.progress = engine.Progress{State: engine.Running}
	test.poll()
	test.after(80 * time.Second)
	test.eng.progress = engine.Progress{State: engine.NotStarted}
	test.unknown("dp3")
	test.poll()
	if record := test.record(); record.Reconfig != nil || len(test.wl.resetAlls) != 0 {
		t.Fatalf("record %+v", record)
	}
	test.readable()
	test.poll()
	test.poll()
	if len(test.wl.resetAlls) != 0 || len(test.eng.applied) != 2 {
		t.Fatalf("applied %d, group resets %d, record %+v", len(test.eng.applied), len(test.wl.resetAlls), test.record())
	}
}

func (test *harness) restartedInPlace(serving bool) {
	test.t.Helper()
	test.serving()
	test.wl.group.Pods[3].Identity = "idD9"
	test.eng.members = append(test.eng.members, engine.Member{
		Pod: workload.PodRef{Name: "lws-0-3", UID: "d1"}, PodIdentity: "idD9", Name: "dp3", Joinable: true, Serving: serving,
	})
}

func TestPodRestartedInPlace(t *testing.T) {
	t.Parallel()
	for _, grow := range []bool{true, false} {
		t.Run(map[bool]string{true: "engine can grow", false: "engine cannot grow"}[grow], func(t *testing.T) {
			t.Parallel()
			test := newHarness(t)
			test.eng.grow = grow
			test.restartedInPlace(false)
			for index := range test.eng.members[:3] {
				test.eng.members[index].Serving = false
			}
			test.eng.plan = test.shrinkPlan()
			test.poll()
			test.poll()
			test.poll()
			record := test.record()
			if len(test.eng.applied) != 1 || !slices.Equal(test.eng.applied[0].Remove, ids("dp3@idD")) ||
				record.Pods["d1"].Identity != "idD" || slices.Contains(test.eng.candidates, "dp3@idD9") {
				t.Fatalf("applied %v, candidates %v, record %+v", test.eng.applied, test.eng.candidates, record)
			}
			if _, tracked := record.Members["dp3@idD9"]; tracked {
				t.Fatalf("the restarted member is tracked: %+v", record.Members)
			}
			test.eng.progress = engine.Progress{State: engine.Succeeded}
			for index := range test.eng.members[:3] {
				test.eng.members[index].Serving = true
			}
			test.eng.plan = engine.Plan{Kind: engine.Healthy}
			test.poll()
			test.poll()
			record = test.record()
			if grow && (len(test.wl.resets) != 1 || test.wl.resets[0].UID != "d1") {
				t.Fatalf("resets %v", test.wl.resets)
			}
			if !grow && (len(test.wl.resets) != 0 || record.Members["dp3@idD"] != Rejected) {
				t.Fatalf("resets %v, record %+v", test.wl.resets, record)
			}
			if _, tracked := record.Members["dp3@idD9"]; tracked {
				t.Fatalf("the restarted member is tracked: %+v", record.Members)
			}
		})
	}
	t.Run("a restarted member that serves breaks C5", func(t *testing.T) {
		t.Parallel()
		test := newHarness(t)
		test.restartedInPlace(true)
		test.poll()
		test.wantReset(ResetExclusionBroken)
	})
}
