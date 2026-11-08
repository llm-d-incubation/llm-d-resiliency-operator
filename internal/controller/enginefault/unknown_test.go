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
)

var errVerify = errors.New("dp1@idB: no tokens generated")

func (test *harness) unknown(names ...string) {
	test.before = slices.Clone(test.eng.members)
	for index := range test.eng.members {
		member := &test.eng.members[index]
		if slices.Contains(names, member.Name) {
			member.Unknown, member.Serving, member.Terminal = true, false, false
		}
	}
}

func (test *harness) readable() { test.eng.members = test.before }

func (test *harness) unseenFor(gap time.Duration) {
	test.t.Helper()
	test.pollAnyway()
	test.after(gap)
	test.readable()
}

func (test *harness) after(gap time.Duration) {
	test.t.Helper()
	test.clock = test.clock.Add(gap - test.config.PollInterval)
	test.pollAnyway()
}

func TestNothingReadable(t *testing.T) {
	t.Parallel()
	t.Run("serving waits without an incident", func(t *testing.T) {
		t.Parallel()
		test := newHarness(t)
		test.serving()
		test.unknown("dp0", "dp1", "dp2", "dp3")
		test.eng.plan = engine.Plan{Kind: engine.Wait, Reason: "waiting for dp0-dp3"}
		saves := test.store.saves
		if err := test.pollErr(); err == nil || !strings.Contains(err.Error(), "no admitted member") {
			t.Fatalf("unseen step: %v", err)
		}
		test.after(5 * time.Minute)
		record := test.record()
		if record.Phase != Serving || record.Incident != nil || record.UnseenSince.IsZero() || len(test.wl.resetAlls) != 0 {
			t.Fatalf("record %+v, group resets %d", record, len(test.wl.resetAlls))
		}
		if test.store.saves != saves+1 {
			t.Fatalf("saves %d, want one", test.store.saves-saves)
		}
		test.readable()
		test.eng.plan = engine.Plan{Kind: engine.Healthy}
		test.poll()
		if record = test.record(); record.Phase != Serving || record.Incident != nil || !record.UnseenSince.IsZero() {
			t.Fatalf("after: %+v", record)
		}
	})
	t.Run("the group is reset once it has been unseen for UnseenTimeout", func(t *testing.T) {
		t.Parallel()
		test := newHarness(t)
		test.serving()
		test.unknown("dp0", "dp1", "dp2", "dp3")
		test.pollAnyway()
		test.after(test.config.UnseenTimeout - 10*time.Second)
		if len(test.wl.resetAlls) != 0 {
			t.Fatalf("reset before UnseenTimeout: %+v", test.record())
		}
		test.after(10 * time.Second)
		test.wantReset(ResetUnseen)
		if !test.record().UnseenSince.IsZero() {
			t.Fatalf("record %+v", test.record())
		}
	})
	t.Run("a zero UnseenTimeout waits forever", func(t *testing.T) {
		t.Parallel()
		test := newHarness(t)
		test.config.UnseenTimeout = 0
		test.serving()
		test.unknown("dp0", "dp1", "dp2", "dp3")
		test.pollAnyway()
		test.after(24 * time.Hour)
		if len(test.wl.resetAlls) != 0 {
			t.Fatalf("reset with no timeout: %+v", test.record())
		}
	})
	t.Run("a failing Discover never resets", func(t *testing.T) {
		t.Parallel()
		test := newHarness(t)
		test.serving()
		test.wl.discoverErr = errors.New("api down")
		test.pollAnyway()
		test.after(2 * test.config.UnseenTimeout)
		if len(test.wl.resetAlls) != 0 {
			t.Fatalf("reset while Discover fails: %+v", test.record())
		}
	})
	t.Run("time Discover failed does not count toward the engine's UnseenTimeout", func(t *testing.T) {
		t.Parallel()
		test := newHarness(t)
		test.serving()
		test.wl.discoverErr = errors.New("api down")
		test.pollAnyway()
		test.after(2 * test.config.UnseenTimeout)
		if !test.record().UnseenByDiscover {
			t.Fatalf("record %+v", test.record())
		}
		test.wl.discoverErr = nil
		test.unknown("dp0", "dp1", "dp2", "dp3")
		back := test.clock.Add(test.config.PollInterval)
		test.pollAnyway()
		record := test.record()
		if len(test.wl.resetAlls) != 0 || record.UnseenByDiscover || record.UnseenSince.Before(back) {
			t.Fatalf("reset %d, record %+v", len(test.wl.resetAlls), record)
		}
		test.after(test.config.UnseenTimeout - 10*time.Second)
		if len(test.wl.resetAlls) != 0 {
			t.Fatalf("reset before the engine's own UnseenTimeout: %+v", test.record())
		}
		test.after(10 * time.Second)
		test.wantReset(ResetUnseen)
	})
	t.Run("Discover failing restarts the clock of an engine already unseen", func(t *testing.T) {
		t.Parallel()
		test := newHarness(t)
		test.serving()
		test.unknown("dp0", "dp1", "dp2", "dp3")
		test.pollAnyway()
		test.after(test.config.UnseenTimeout - time.Minute)
		test.wl.discoverErr = errors.New("api down")
		test.after(2 * time.Minute)
		test.wl.discoverErr = nil
		test.pollAnyway()
		test.after(test.config.UnseenTimeout - 10*time.Second)
		if len(test.wl.resetAlls) != 0 {
			t.Fatalf("reset before a full UnseenTimeout after Discover came back: %+v", test.record())
		}
		test.after(10 * time.Second)
		test.wantReset(ResetUnseen)
	})
	t.Run("an unreadable excluded member is not a C4 violation", func(t *testing.T) {
		t.Parallel()
		test := newHarness(t)
		test.wl.resetErr = errors.New("api timeout")
		test.shrunk()
		test.pollAnyway()
		test.unknown("dp3")
		for range 3 {
			test.pollAnyway()
		}
		if record := test.record(); len(test.wl.resetAlls) != 0 || record.Phase != Serving || record.Members["dp3@idD"] != Excluded {
			t.Fatalf("record %+v", record)
		}
	})
	t.Run("deadlines pause in recovering", func(t *testing.T) {
		t.Parallel()
		test := newHarness(t)
		test.serving()
		test.eng.plan = engine.Plan{Kind: engine.Wait, Reason: "dp1 is diagnosing"}
		test.poll()
		test.after(60 * time.Second)
		test.unknown("dp0", "dp1", "dp2", "dp3")
		test.unseenFor(5 * time.Minute)
		test.after(5 * time.Second)
		if record := test.record(); record.Phase != Recovering || len(test.wl.resetAlls) != 0 {
			t.Fatalf("reset too early: %+v", record)
		}
		test.after(15 * time.Second)
		test.wantReset(ResetDiagnosisDeadline)
	})
	t.Run("deadlines pause while Discover fails", func(t *testing.T) {
		t.Parallel()
		test := newHarness(t)
		test.serving()
		test.eng.plan = engine.Plan{Kind: engine.Wait, Reason: "dp1 is diagnosing"}
		test.poll()
		test.after(60 * time.Second)
		test.wl.discoverErr = errors.New("api down")
		test.pollAnyway()
		test.after(5 * time.Minute)
		test.wl.discoverErr = nil
		test.after(5 * time.Second)
		if record := test.record(); record.Phase != Recovering || len(test.wl.resetAlls) != 0 {
			t.Fatalf("reset too early: %+v", record)
		}
		test.after(15 * time.Second)
		test.wantReset(ResetDiagnosisDeadline)
	})
	t.Run("the apply deadline pauses", func(t *testing.T) {
		t.Parallel()
		test := newHarness(t)
		test.serving()
		test.dead()
		test.eng.plan = test.shrinkPlan()
		test.eng.progress = engine.Progress{State: engine.Running}
		test.poll()
		test.poll()
		test.poll()
		if test.record().Reconfig == nil {
			t.Fatalf("no reconfig: %+v", test.record())
		}
		test.after(60 * time.Second)
		test.unknown("dp0", "dp1", "dp2", "dp3")
		test.unseenFor(5 * time.Minute)
		test.after(20 * time.Second)
		if len(test.wl.resetAlls) != 0 {
			t.Fatalf("reset too early: %+v", test.record())
		}
		test.after(30 * time.Second)
		test.wantReset(ResetApplyDeadline)
	})
	t.Run("the verify window pauses", func(t *testing.T) {
		t.Parallel()
		test := newHarness(t)
		test.eng.verifyErr = errVerify
		test.shrunk()
		if test.record().VerifyFailedSince.IsZero() {
			t.Fatalf("verify did not fail: %+v", test.record())
		}
		test.after(time.Minute)
		test.unknown("dp0", "dp1", "dp2", "dp3")
		test.unseenFor(5 * time.Minute)
		test.after(30 * time.Second)
		if len(test.wl.resetAlls) != 0 {
			t.Fatalf("reset too early: %+v", test.record())
		}
		test.after(60 * time.Second)
		test.wantReset(ResetVerifyFailed)
	})
}

func TestSomeUnknown(t *testing.T) {
	t.Parallel()
	t.Run("an unknown member is a fault", func(t *testing.T) {
		t.Parallel()
		test := newHarness(t)
		test.serving()
		test.unknown("dp1")
		test.poll()
		record := test.record()
		if record.Phase != Recovering || record.Incident == nil || !slices.Equal(record.Incident.Affected, ids("dp1@idB")) ||
			!strings.Contains(record.Reason, "cannot read dp1@idB") {
			t.Fatalf("record %+v", record)
		}
		test.after(test.config.DiagnosisTimeout + time.Second)
		test.wantReset(ResetDiagnosisDeadline)
		if len(test.eng.applied) != 0 {
			t.Fatalf("applied %v", test.eng.applied)
		}
	})
	t.Run("an admitted member that is not reported is unknown", func(t *testing.T) {
		t.Parallel()
		test := newHarness(t)
		test.serving()
		test.eng.members = slices.DeleteFunc(test.eng.members, func(member engine.Member) bool { return member.Name == "dp2" })
		test.poll()
		if record := test.record(); record.Phase != Recovering || !strings.Contains(record.Reason, "cannot read dp2@idC") {
			t.Fatalf("record %+v", record)
		}
	})
	t.Run("an unknown member is never removed", func(t *testing.T) {
		t.Parallel()
		test := newHarness(t)
		test.serving()
		test.unknown("dp3")
		for index := range test.eng.members {
			test.eng.members[index].Serving = false
		}
		test.eng.plan = test.shrinkPlan()
		test.poll()
		test.poll()
		test.poll()
		test.wantReset(ResetUnrecoverable)
		if len(test.eng.applied) != 0 || !strings.Contains(test.record().Reason, "C1") {
			t.Fatalf("applied %v, reason %q", test.eng.applied, test.record().Reason)
		}
	})
	t.Run("a plan with an unknown participant waits", func(t *testing.T) {
		t.Parallel()
		test := newHarness(t)
		test.serving()
		test.dead()
		test.unknown("dp1")
		test.eng.plan = test.shrinkPlan()
		test.poll()
		test.poll()
		test.poll()
		if len(test.eng.applied) != 0 || len(test.wl.resetAlls) != 0 ||
			!strings.Contains(test.record().Reason, "waiting to read dp1@idB") {
			t.Fatalf("applied %v, record %+v", test.eng.applied, test.record())
		}
	})
	t.Run("starting waits for an unknown member", func(t *testing.T) {
		t.Parallel()
		test := newHarness(t)
		test.unknown("dp2")
		test.load()
		test.poll()
		test.poll()
		if record := test.record(); record.Phase != Starting {
			t.Fatalf("record %+v", record)
		}
		test.readable()
		test.poll()
		if record := test.record(); record.Phase != Serving || len(record.Members) != 4 {
			t.Fatalf("record %+v", record)
		}
	})
}
