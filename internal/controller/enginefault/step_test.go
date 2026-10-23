// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

//nolint:testpackage // Tests drive the clock through the unexported load.
package enginefault

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/llm-d/llm-d-resiliency-operator/internal/engine"
	"github.com/llm-d/llm-d-resiliency-operator/internal/workload"
)

var replaced = workload.Pod{
	Name: "lws-0-3", UID: "d2", IP: "10.0.0.9", Index: 3, Identity: "idD2", Node: "gpu-node-9", State: workload.PodStarting,
}

func (test *harness) shrunk() {
	test.t.Helper()
	test.serving()
	test.dead()
	test.eng.plan = test.shrinkPlan()
	test.poll()
	test.poll()
	test.poll()
	test.eng.progress = engine.Progress{State: engine.Succeeded}
	for _, name := range []string{"dp0", "dp1", "dp2"} {
		test.member(name).Serving = true
	}
	test.eng.plan = engine.Plan{Kind: engine.Healthy}
	test.poll()
	if record := test.record(); record.Phase != Serving || record.Members[engine.MemberID("dp3@idD")] != Excluded {
		test.t.Fatalf("shrink did not finish: %+v", record)
	}
	test.rec.calls = nil
}

func (test *harness) unconfirmed() {
	test.t.Helper()
	var data []byte
	var version int
	test.eng.onApply = func() { data, version = slices.Clone(test.store.data), test.store.version }
	test.poll()
	test.eng.onApply = nil
	test.store.data, test.store.version = data, version
	if record := test.record(); record.Reconfig == nil || len(record.Acceptances) != 0 {
		test.t.Fatalf("record %+v", record)
	}
	test.load()
	test.eng.progress = engine.Progress{State: engine.NotStarted, Reason: "no rank has it"}
}

func (test *harness) wantReset(reason ResetReason) {
	test.t.Helper()
	record := test.record()
	if len(test.wl.resetAlls) == 0 || record.Phase != Starting || !strings.HasPrefix(record.Reason, string(reason)) {
		test.t.Fatalf("want ResetAll(%s), got %d resets, record %+v", reason, len(test.wl.resetAlls), record)
	}
	for _, pod := range record.Pods {
		if !pod.Removing {
			test.t.Fatalf("pod not marked Removing: %+v", record.Pods)
		}
	}
}

func (test *harness) wantKept(id engine.MemberID) {
	test.t.Helper()
	resets, releases := len(test.wl.resets), len(test.wl.releases)
	for range 3 {
		test.poll()
	}
	record := test.record()
	if len(test.wl.resetAlls) != 0 || record.Phase != Serving || record.Members[id] != Rejected {
		test.t.Fatalf("not kept: %d group resets, record %+v", len(test.wl.resetAlls), record)
	}
	for _, pod := range record.Pods {
		if pod.Removing {
			test.t.Fatalf("pod still Removing: %+v", record.Pods)
		}
	}
	if len(test.wl.resets) != resets || len(test.wl.releases) != releases {
		test.t.Fatalf("tried again: resets %v, releases %v", test.wl.resets, test.wl.releases)
	}
}

func TestRunningExample(t *testing.T) {
	t.Parallel()
	test := newHarness(t)
	test.serving()

	test.dead()
	test.eng.plan = test.shrinkPlan()
	test.poll()
	if record := test.record(); record.Phase != Recovering || record.Incident == nil || test.eng.probes != 1 {
		t.Fatalf("poll 2: %+v, probes %d", record, test.eng.probes)
	}
	test.wantCalls("PhaseChanged serving->recovering", "IncidentOpened [dp3@idD] [gpu-node-4]")

	test.poll()
	if len(test.eng.applied) != 0 {
		t.Fatal("poll 3 sent a reconfig before two polls in recovering")
	}

	saved := false
	test.eng.onApply = func() { saved = test.record().Reconfig != nil }
	test.poll()
	if len(test.eng.applied) != 1 || !saved {
		t.Fatalf("poll 4: applied %d, reconfig saved before Apply %v", len(test.eng.applied), saved)
	}
	test.wantCalls("ReconfigStarted remove=[dp3@idD] add=[]")

	test.eng.progress = engine.Progress{State: engine.Running}
	test.poll()
	if len(test.eng.applied) != 1 || test.record().Reconfig == nil {
		t.Fatal("poll 5 did not keep watching the reconfig")
	}

	test.eng.progress = engine.Progress{State: engine.Succeeded}
	for _, name := range []string{"dp0", "dp1", "dp2"} {
		test.member(name).Serving = true
	}
	test.eng.plan = engine.Plan{Kind: engine.Healthy}
	test.poll()
	record := test.record()
	if record.Phase != Serving || record.Members[engine.MemberID("dp3@idD")] != Excluded || len(record.Verifying) != 0 ||
		record.Incident != nil || !slices.Equal(test.eng.verified[0], ids("dp0@idA", "dp1@idB", "dp2@idC")) {
		t.Fatalf("poll 6: %+v, verified %v", record, test.eng.verified)
	}
	test.wantCalls("PhaseChanged recovering->serving", "ReconfigFinished Succeeded", "IncidentClosed Shrunk")

	test.poll()
	if len(test.wl.resets) != 1 || test.wl.resets[0] != (workload.PodRef{Name: "lws-0-3", UID: "d1"}) ||
		!test.record().Pods["d1"].Removing {
		t.Fatalf("poll 7: resets %v", test.wl.resets)
	}
	test.wantCalls("PodResetRequested lws-0-3")

	test.wl.group.Pods[3] = replaced
	test.poll()
	record = test.record()
	if _, old := record.Pods["d1"]; old || record.Pods["d2"].Identity != "idD2" || len(test.wl.resets) != 1 {
		t.Fatalf("poll 8: pods %+v", record.Pods)
	}
	if _, tracked := record.Members[engine.MemberID("dp3@idD")]; tracked {
		t.Fatal("poll 8 kept the old Pod's member")
	}

	test.wl.group.Pods[3].State = workload.PodReady
	*test.member("dp3") = engine.Member{
		Pod: workload.PodRef{Name: "lws-0-3", UID: "d2"}, PodIdentity: "idD2", Name: "dp3", Joinable: true,
	}
	test.eng.plan = engine.Plan{Kind: engine.Change, Participants: ids("dp0@idA", "dp1@idB", "dp2@idC"), Add: ids("dp3@idD2")}
	test.poll()
	if len(test.eng.applied) != 2 || test.record().Phase != Serving {
		t.Fatalf("grow reconfig: applied %d", len(test.eng.applied))
	}
	test.wantCalls("ReconfigStarted remove=[] add=[dp3@idD2]")

	test.eng.progress = engine.Progress{State: engine.Succeeded}
	test.member("dp3").Serving, test.member("dp3").Joinable = true, false
	test.eng.plan = engine.Plan{Kind: engine.Healthy}
	test.poll()
	if test.record().Members[engine.MemberID("dp3@idD2")] != Admitted ||
		!slices.Equal(test.eng.verified[len(test.eng.verified)-1], ids("dp0@idA", "dp1@idB", "dp2@idC", "dp3@idD2")) {
		t.Fatalf("grow did not finish: %+v, verified %v", test.record().Members, test.eng.verified)
	}
	test.wantCalls("ReconfigFinished Succeeded")
}

func TestNew(t *testing.T) {
	t.Parallel()
	test := newHarness(t)
	test.load()
	if test.store.saves != 0 {
		t.Fatal("New wrote the journal")
	}
	test.poll()
	if test.record().Phase != Serving {
		t.Fatalf("first step: %+v", test.record())
	}

	test = newHarness(t)
	test.store.data = []byte("{bad")
	if _, err := load(t.Context(), test.wl, test.eng, test.store, test.rec, test.config, time.Now); err == nil {
		t.Fatal("garbage journal accepted")
	}

	test = newHarness(t)
	test.store.data = []byte(`{"phase":"intervention_required","excluded":[3]}`)
	test.load()
	test.poll()
	if record := test.record(); record.Phase != Serving || len(record.Members) != 4 {
		t.Fatalf("old phase did not start over: %+v", record)
	}
}

func TestStartingHasNoDeadline(t *testing.T) {
	t.Parallel()
	for name, plan := range map[string]engine.PlanKind{
		"wait": engine.Wait, "unrecoverable": engine.Unrecoverable, "change": engine.Change,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			test := newHarness(t)
			test.eng.plan = engine.Plan{Kind: plan, Participants: ids("dp0@idA")}
			test.load()
			for range 10 {
				test.clock = test.clock.Add(10 * time.Minute)
				test.poll()
			}
			if test.record().Phase != Starting || len(test.wl.resetAlls) != 0 || len(test.eng.applied) != 0 {
				t.Fatalf("starting acted: %+v", test.record())
			}
		})
	}
}

func TestReadingDiscover(t *testing.T) {
	t.Parallel()
	t.Run("recreation", func(t *testing.T) {
		t.Parallel()
		test := newHarness(t)
		test.serving()
		test.wl.group.Recreating = true
		test.poll()
		if record := test.record(); record.Phase != Serving || record.Incident == nil || len(test.eng.applied) != 0 {
			t.Fatalf("recreating: %+v", record)
		}
		test.wantCalls("IncidentOpened [] []")
		test.wl.group.Recreating = false
		for index := range test.wl.group.Pods {
			test.wl.group.Pods[index].UID += "-new"
			test.eng.members[index].Pod.UID += "-new"
		}
		test.poll()
		if record := test.record(); record.Phase != Starting || record.Incident != nil {
			t.Fatalf("new group: %+v", record)
		}
		test.wantCalls("PhaseChanged serving->starting", "IncidentClosed Recreated")
		test.poll()
		if test.record().Phase != Serving {
			t.Fatal("new group did not serve")
		}
	})
	t.Run("owed reset", func(t *testing.T) {
		t.Parallel()
		test := newHarness(t)
		test.serving()
		test.wl.group.Pods[2].State = workload.PodStarting
		test.eng.plan = engine.Plan{Kind: engine.Unrecoverable}
		test.poll()
		test.poll()
		test.wantReset(ResetUnrecoverable)
		test.poll()
		if len(test.wl.resetAlls) != 2 {
			t.Fatal("owed ResetAll was not repeated while old Pods remain")
		}
		test.wl.group.Pods = test.wl.group.Pods[:0]
		test.poll()
		if record := test.record(); record.Phase != Starting || len(record.Pods) != 0 {
			t.Fatalf("after the old Pods left: %+v", record)
		}
	})
	t.Run("discovery failure", func(t *testing.T) {
		t.Parallel()
		test := newHarness(t)
		test.serving()
		saves := test.store.saves
		test.wl.discoverErr = errors.New("api down")
		for range 3 {
			if err := test.pollErr(); err == nil {
				t.Fatal("discovery error swallowed")
			}
		}
		if record := test.record(); record.Phase != Serving || record.UnseenSince.IsZero() ||
			test.store.saves != saves+1 || len(test.wl.resetAlls) != 0 {
			t.Fatalf("discovery failure: %+v, saves %d", record, test.store.saves-saves)
		}
		test.wl.discoverErr = nil
		test.poll()
		if !test.record().UnseenSince.IsZero() {
			t.Fatal("discovery recovery not noted")
		}
	})
}

func TestObservationChecks(t *testing.T) {
	t.Parallel()
	t.Run("C4 excluded member serves again", func(t *testing.T) {
		t.Parallel()
		test := newHarness(t)
		test.shrunk()
		test.member("dp3").Terminal = false
		test.poll()
		test.wantReset(ResetExclusionBroken)
	})
	t.Run("C5 candidate serves", func(t *testing.T) {
		t.Parallel()
		test := newHarness(t)
		test.serving()
		test.wl.group.Pods = append(test.wl.group.Pods, workload.Pod{
			Name: "lws-0-4", UID: "e1", Index: 4, Identity: "idE", State: workload.PodReady,
		})
		test.eng.members = append(test.eng.members, engine.Member{
			Pod: workload.PodRef{Name: "lws-0-4", UID: "e1"}, PodIdentity: "idE", Name: "dp4", Serving: true,
		})
		test.poll()
		test.wantReset(ResetExclusionBroken)
	})
}

func TestPlanChecks(t *testing.T) {
	t.Parallel()
	for name, plan := range map[string]engine.Plan{
		"C1 remove a live member": {Kind: engine.Change, Participants: ids("dp0@idA", "dp1@idB", "dp2@idC"), Remove: ids("dp3@idD")},
		"C3 unknown participant":  {Kind: engine.Change, Participants: ids("dp9@idZ")},
		"C3 no participants":      {Kind: engine.Change, Remove: ids("dp3@idD")},
		"C3 named twice":          {Kind: engine.Change, Participants: ids("dp0@idA", "dp0@idA")},
		"C2 add a stranger":       {Kind: engine.Change, Participants: ids("dp0@idA"), Add: ids("dp9@idZ")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			test := newHarness(t)
			test.serving()
			for index := range test.eng.members {
				test.eng.members[index].Serving = false
			}
			test.eng.plan = plan
			test.poll()
			test.poll()
			test.poll()
			test.wantReset(ResetUnrecoverable)
			if len(test.eng.applied) != 0 || !strings.Contains(test.record().Reason, name[:2]) {
				t.Fatalf("applied %d, reason %q", len(test.eng.applied), test.record().Reason)
			}
		})
	}
}

func TestRecovering(t *testing.T) {
	t.Parallel()
	t.Run("unrecoverable", func(t *testing.T) {
		t.Parallel()
		test := newHarness(t)
		test.serving()
		test.dead()
		test.eng.plan = engine.Plan{Kind: engine.Unrecoverable}
		test.poll()
		if len(test.wl.resetAlls) != 0 {
			t.Fatal("reset on the poll that entered recovering")
		}
		test.poll()
		test.wantReset(ResetUnrecoverable)
	})
	t.Run("diagnosis deadline", func(t *testing.T) {
		t.Parallel()
		test := newHarness(t)
		test.serving()
		test.dead()
		test.eng.plan = engine.Plan{Kind: engine.Wait}
		test.poll()
		test.clock = test.clock.Add(test.config.DiagnosisTimeout)
		test.poll()
		test.wantReset(ResetDiagnosisDeadline)
	})
	t.Run("fault clears", func(t *testing.T) {
		t.Parallel()
		test := newHarness(t)
		test.serving()
		test.member("dp3").Serving = false
		test.eng.plan = engine.Plan{Kind: engine.Wait}
		test.poll()
		test.member("dp3").Serving = true
		test.eng.plan = engine.Plan{Kind: engine.Healthy}
		test.poll()
		if test.record().Phase != Serving || len(test.eng.verified) != 0 {
			t.Fatalf("recovered: %+v, verified %v", test.record(), test.eng.verified)
		}
		test.wantCalls("PhaseChanged serving->recovering", "IncidentOpened [dp3@idD] [gpu-node-4]",
			"PhaseChanged recovering->serving", "IncidentClosed Recovered")
	})
}

func TestReconfigOutcomes(t *testing.T) {
	t.Parallel()
	start := func(t *testing.T) *harness {
		t.Helper()
		test := newHarness(t)
		test.serving()
		test.dead()
		test.eng.plan = test.shrinkPlan()
		test.poll()
		test.poll()
		return test
	}
	t.Run("apply fails", func(t *testing.T) {
		t.Parallel()
		test := start(t)
		test.eng.applyErr = errors.New("refused")
		test.poll()
		test.wantReset(ResetReconfigFailed)
	})
	t.Run("progress fails", func(t *testing.T) {
		t.Parallel()
		test := start(t)
		test.poll()
		test.eng.progress = engine.Progress{State: engine.Failed, Scope: engine.Group}
		test.poll()
		test.wantReset(ResetReconfigFailed)
	})
	t.Run("apply deadline", func(t *testing.T) {
		t.Parallel()
		test := start(t)
		test.poll()
		test.eng.progress = engine.Progress{State: engine.Running}
		test.clock = test.clock.Add(test.config.ApplyTimeout)
		test.poll()
		test.wantReset(ResetApplyDeadline)
	})
	t.Run("not resent after a leader change once the send was confirmed", func(t *testing.T) {
		t.Parallel()
		test := start(t)
		test.poll()
		test.load()
		test.eng.progress = engine.Progress{State: engine.NotStarted}
		test.poll()
		if len(test.eng.applied) != 1 {
			t.Fatalf("reconfig sent %d times", len(test.eng.applied))
		}
		test.clock = test.clock.Add(test.config.ApplyTimeout)
		test.poll()
		test.wantReset(ResetApplyDeadline)
	})
	t.Run("not resent once its acceptances are saved", func(t *testing.T) {
		t.Parallel()
		test := start(t)
		test.poll()
		test.eng.progress = engine.Progress{State: engine.NotStarted}
		test.poll()
		test.poll()
		if len(test.eng.applied) != 1 {
			t.Fatalf("reconfig sent %d times", len(test.eng.applied))
		}
	})
	t.Run("resent when the send was not confirmed", func(t *testing.T) {
		t.Parallel()
		test := start(t)
		test.unconfirmed()
		test.poll()
		record := test.record()
		if len(test.eng.applied) != 2 || !reflect.DeepEqual(test.eng.applied[0], test.eng.applied[1]) ||
			len(record.Acceptances) != 3 || !record.ReconfigSince.Equal(test.clock) || !record.LastNoChangeReconfig.IsZero() ||
			len(test.wl.resetAlls) != 0 {
			t.Fatalf("applied %v, record %+v", test.eng.applied, record)
		}
		test.eng.progress = engine.Progress{State: engine.Succeeded}
		for _, name := range []string{"dp0", "dp1", "dp2"} {
			test.member(name).Serving = true
		}
		test.eng.plan = engine.Plan{Kind: engine.Healthy}
		test.poll()
		if record := test.record(); record.Members["dp3@idD"] != Excluded || record.Phase != Serving {
			t.Fatalf("record %+v", record)
		}
	})
	t.Run("resent by the same leader when its acceptances were not saved", func(t *testing.T) {
		t.Parallel()
		test := start(t)
		test.store.saveErrs = []error{nil, errors.New("api timeout")}
		test.pollAnyway()
		if record := test.record(); record.Reconfig == nil || len(record.Acceptances) != 0 {
			t.Fatalf("record %+v", record)
		}
		test.eng.progress = engine.Progress{State: engine.NotStarted}
		test.poll()
		if len(test.eng.applied) != 2 || len(test.record().Acceptances) != 3 {
			t.Fatalf("applied %d, record %+v", len(test.eng.applied), test.record())
		}
	})
	t.Run("a resent retry restarts the apply deadline and is not a flap", func(t *testing.T) {
		t.Parallel()
		test := newHarness(t)
		test.serving()
		for index := range test.eng.members {
			test.eng.members[index].Serving = false
		}
		test.eng.plan = engine.Plan{Kind: engine.Change, Participants: ids("dp0@idA", "dp1@idB", "dp2@idC", "dp3@idD")}
		test.poll()
		test.poll()
		test.unconfirmed()
		test.eng.progress = engine.Progress{State: engine.Running}
		test.poll()
		test.clock = test.clock.Add(time.Minute)
		test.eng.progress = engine.Progress{State: engine.NotStarted}
		test.poll()
		record := test.record()
		if len(test.eng.applied) != 2 || !record.ReconfigSince.Equal(test.clock) || len(test.wl.resetAlls) != 0 {
			t.Fatalf("applied %d, record %+v", len(test.eng.applied), record)
		}
		test.eng.progress = engine.Progress{State: engine.Running}
		test.clock = test.clock.Add(test.config.ApplyTimeout - time.Second)
		test.poll()
		if len(test.wl.resetAlls) != 0 {
			t.Fatalf("apply deadline counted from the first send: %+v", test.record())
		}
	})
	t.Run("resend save fails", func(t *testing.T) {
		t.Parallel()
		test := start(t)
		test.unconfirmed()
		test.store.saveErrs = []error{errors.New("conflict")}
		if err := test.pollErr(); err == nil || len(test.eng.applied) != 1 {
			t.Fatalf("applied %d after a failed save", len(test.eng.applied))
		}
	})
	t.Run("resend refused", func(t *testing.T) {
		t.Parallel()
		test := start(t)
		test.unconfirmed()
		test.eng.applyErr = errors.New("refused")
		test.poll()
		test.wantReset(ResetReconfigFailed)
	})
	t.Run("save fails before apply", func(t *testing.T) {
		t.Parallel()
		test := start(t)
		test.store.saveErrs = []error{errors.New("conflict")}
		if err := test.pollErr(); err == nil || len(test.eng.applied) != 0 {
			t.Fatalf("applied %d after a failed save", len(test.eng.applied))
		}
		test.poll()
		if len(test.eng.applied) != 1 {
			t.Fatal("reconfig not sent once the journal works")
		}
	})
}

func TestLeaderChangeWithAFailedSave(t *testing.T) {
	t.Parallel()
	test := newHarness(t)
	test.serving()
	test.eng.plan = engine.Plan{Kind: engine.Wait, Reason: "dp1 is diagnosing"}
	test.poll()
	test.after(60 * time.Second)
	test.load()
	test.store.saveErrs = []error{errors.New("etcd timeout")}
	test.pollAnyway()
	test.after(20 * time.Second)
	if len(test.wl.resetAlls) != 0 {
		t.Fatalf("reset before the restarted deadline: %+v", test.record())
	}
	test.after(80 * time.Second)
	test.wantReset(ResetDiagnosisDeadline)
}

func TestVerifyAfterReconfig(t *testing.T) {
	t.Parallel()
	t.Run("retried, then reset", func(t *testing.T) {
		t.Parallel()
		test := newHarness(t)
		test.eng.verifyErr = errors.New("no tokens")
		test.shrunk()
		if record := test.record(); record.VerifyFailedSince.IsZero() || len(record.Verifying) != 3 {
			t.Fatalf("first failure: %+v", record)
		}
		test.load()
		test.poll()
		if len(test.eng.verified) != 2 || len(test.wl.resetAlls) != 0 {
			t.Fatal("a new leader did not continue the check")
		}
		test.clock = test.clock.Add(test.config.VerifyTimeout)
		test.poll()
		test.wantReset(ResetVerifyFailed)
	})
	t.Run("passes on retry", func(t *testing.T) {
		t.Parallel()
		test := newHarness(t)
		test.eng.verifyErr = errors.New("no tokens")
		test.shrunk()
		test.eng.verifyErr = nil
		test.clock = test.clock.Add(test.config.VerifyTimeout / 2)
		test.poll()
		record := test.record()
		if len(record.Verifying) != 0 || !record.VerifyFailedSince.IsZero() || record.Reason != "verified" ||
			len(test.wl.resetAlls) != 0 {
			t.Fatalf("record %+v", record)
		}
		test.poll()
		if len(test.wl.resets) != 1 || test.wl.resets[0].UID != "d1" {
			t.Fatalf("resets %v", test.wl.resets)
		}
	})
	t.Run("a fault during the window keeps the check", func(t *testing.T) {
		t.Parallel()
		test := newHarness(t)
		test.eng.verifyErr = errors.New("no tokens")
		test.shrunk()
		test.eng.plan = engine.Plan{Kind: engine.Wait, Reason: "dp1 is diagnosing"}
		test.poll()
		if record := test.record(); record.Phase != Recovering || len(record.Verifying) != 3 || record.Incident == nil {
			t.Fatalf("record %+v", record)
		}
		test.eng.plan = engine.Plan{Kind: engine.Healthy}
		test.poll()
		if record := test.record(); record.Phase != Serving || record.Incident == nil || len(record.Verifying) != 3 {
			t.Fatalf("incident closed before the check passed: %+v", record)
		}
		test.eng.verifyErr = nil
		test.poll()
		if record := test.record(); record.Incident != nil || len(record.Verifying) != 0 || len(test.wl.resetAlls) != 0 {
			t.Fatalf("record %+v", record)
		}
	})
	t.Run("one unreadable poll does not drop a failing check", func(t *testing.T) {
		t.Parallel()
		test := newHarness(t)
		test.eng.verifyErr = errors.New("no tokens")
		test.shrunk()
		test.unknown("dp1")
		test.poll()
		test.readable()
		test.poll()
		if record := test.record(); record.Phase != Serving || record.Incident == nil || len(record.Verifying) != 3 {
			t.Fatalf("record %+v", record)
		}
		test.clock = test.clock.Add(test.config.VerifyTimeout)
		test.poll()
		test.wantReset(ResetVerifyFailed)
	})
	t.Run("a pause for the workload keeps the window paused", func(t *testing.T) {
		t.Parallel()
		test := newHarness(t)
		test.eng.verifyErr = errors.New("no tokens")
		test.shrunk()
		test.after(30 * time.Second)
		test.wl.discoverErr = errors.New("api down")
		test.pollAnyway()
		test.after(5 * time.Minute)
		test.wl.discoverErr = nil
		test.wl.group.Recreating = true
		test.poll()
		test.wl.group.Recreating = false
		test.poll()
		if len(test.wl.resetAlls) != 0 {
			t.Fatalf("verify_failed reset after the pause: %+v", test.record())
		}
	})
	for _, mode := range []CapacityRestore{RestoreResetPod, RestoreRelease, RestoreResetAll} {
		t.Run("resets the group, never one Pod, with "+string(mode), func(t *testing.T) {
			t.Parallel()
			test := newHarness(t)
			test.config.CapacityRestore = mode
			test.eng.verifyErr = errors.New("no tokens")
			test.shrunk()
			test.clock = test.clock.Add(test.config.VerifyTimeout)
			test.poll()
			test.wantReset(ResetVerifyFailed)
			if len(test.wl.resets) != 0 || len(test.wl.releases) != 0 {
				t.Fatalf("resets %v, releases %v", test.wl.resets, test.wl.releases)
			}
		})
	}
	t.Run("no window", func(t *testing.T) {
		t.Parallel()
		test := newHarness(t)
		test.config.VerifyTimeout = 0
		test.eng.verifyErr = errors.New("no tokens")
		test.serving()
		test.dead()
		test.eng.plan = test.shrinkPlan()
		test.poll()
		test.poll()
		test.poll()
		test.eng.progress = engine.Progress{State: engine.Succeeded}
		test.poll()
		test.wantReset(ResetVerifyFailed)
	})
}

func TestFlapGuard(t *testing.T) {
	t.Parallel()
	test := newHarness(t)
	test.serving()
	retry := engine.Plan{Kind: engine.Change, Participants: ids("dp0@idA", "dp1@idB", "dp2@idC", "dp3@idD")}
	for reconfig := range 2 {
		for index := range test.eng.members {
			test.eng.members[index].Serving = false
		}
		test.eng.plan = retry
		test.poll()
		test.poll()
		test.poll()
		if reconfig == 1 {
			break
		}
		test.eng.progress = engine.Progress{State: engine.Succeeded}
		for index := range test.eng.members {
			test.eng.members[index].Serving = true
		}
		test.eng.plan = engine.Plan{Kind: engine.Healthy}
		test.poll()
	}
	test.wantReset(ResetFlap)
	if !test.record().LastNoChangeReconfig.IsZero() {
		t.Fatal("the flap window carried into the new group")
	}
}

func TestCapacityRestore(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		configure func(*harness)
		resets    int
		releases  int
	}{
		"reset-pod unsupported": {configure: func(test *harness) { test.wl.resetErr = workload.ErrUnsupported }, resets: 1},
		"engine cannot grow":    {configure: func(test *harness) { test.eng.grow = false }},
		"release unsupported": {configure: func(test *harness) {
			test.config.CapacityRestore = RestoreRelease
			test.wl.releaseErr = workload.ErrUnsupported
		}, releases: 1},
	} {
		t.Run(name+" keeps the group as it is", func(t *testing.T) {
			t.Parallel()
			harness := newHarness(t)
			test.configure(harness)
			harness.shrunk()
			harness.poll()
			if len(harness.wl.resets) != test.resets || len(harness.wl.releases) != test.releases {
				t.Fatalf("resets %v, releases %v", harness.wl.resets, harness.wl.releases)
			}
			harness.wantKept("dp3@idD")
		})
	}
	t.Run("reset-all resets the group", func(t *testing.T) {
		t.Parallel()
		test := newHarness(t)
		test.config.CapacityRestore = RestoreResetAll
		test.shrunk()
		test.poll()
		test.wantReset(ResetCapacity)
	})
	for mode, reset := range map[CapacityRestore]bool{RestoreResetAll: true, RestoreResetPod: false} {
		t.Run(string(mode)+" with a Pod that keeps a member", func(t *testing.T) {
			t.Parallel()
			test := newHarness(t)
			test.config.CapacityRestore = mode
			test.eng.members = append(test.eng.members, engine.Member{
				Pod: workload.PodRef{Name: "lws-0-3", UID: "d1"}, PodIdentity: "idD", Name: "dp4", Serving: true,
			})
			test.load()
			test.poll()
			test.dead()
			test.eng.plan = engine.Plan{
				Kind: engine.Change, Participants: ids("dp0@idA", "dp1@idB", "dp2@idC", "dp4@idD"), Remove: ids("dp3@idD"),
			}
			test.poll()
			test.poll()
			test.poll()
			test.eng.progress = engine.Progress{State: engine.Succeeded}
			for _, name := range []string{"dp0", "dp1", "dp2", "dp4"} {
				test.member(name).Serving = true
			}
			test.eng.plan = engine.Plan{Kind: engine.Healthy}
			test.poll()
			test.poll()
			if reset {
				test.wantReset(ResetCapacity)
				return
			}
			if len(test.wl.resetAlls) != 0 || len(test.wl.resets) != 0 || test.record().Phase != Serving {
				t.Fatalf("resets %v %v, record %+v", test.wl.resetAlls, test.wl.resets, test.record())
			}
		})
	}
	t.Run("release error is repeated", func(t *testing.T) {
		t.Parallel()
		test := newHarness(t)
		test.config.CapacityRestore = RestoreRelease
		test.wl.releaseErr = errors.New("api timeout")
		test.shrunk()
		test.pollAnyway()
		test.wl.releaseErr = nil
		test.poll()
		if len(test.wl.releases) != 2 || !test.record().Pods["d1"].Removing {
			t.Fatalf("releases %v, pods %+v", test.wl.releases, test.record().Pods)
		}
	})
	t.Run("gone Pod is forgotten", func(t *testing.T) {
		t.Parallel()
		test := newHarness(t)
		test.shrunk()
		test.wl.group.Pods = test.wl.group.Pods[:3]
		test.poll()
		if _, kept := test.record().Pods["d1"]; kept || len(test.wl.resets) != 0 {
			t.Fatalf("pods %+v, resets %v", test.record().Pods, test.wl.resets)
		}
	})
	t.Run("rejected join keeps the group as it is", func(t *testing.T) {
		t.Parallel()
		test := newHarness(t)
		test.shrunk()
		test.poll()
		test.wl.group.Pods[3] = replaced
		test.wl.group.Pods[3].State = workload.PodReady
		*test.member("dp3") = engine.Member{
			Pod: workload.PodRef{Name: "lws-0-3", UID: "d2"}, PodIdentity: "idD2", Name: "dp3", Joinable: true,
		}
		test.eng.plan = engine.Plan{Kind: engine.Change, Participants: ids("dp0@idA", "dp1@idB", "dp2@idC"), Add: ids("dp3@idD2")}
		test.poll()
		test.eng.progress = engine.Progress{State: engine.Failed, Scope: engine.Joiners}
		test.eng.plan = engine.Plan{Kind: engine.Healthy}
		test.poll()
		test.wantKept("dp3@idD2")
	})
}
