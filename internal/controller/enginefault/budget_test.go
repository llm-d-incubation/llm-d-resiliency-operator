// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

//nolint:testpackage // Tests exercise the persisted recovery budget.
package enginefault

import (
	"encoding/json"
	"flag"
	"strings"
	"testing"
	"time"

	"github.com/llm-d/llm-d-resiliency-operator/internal/engine"
)

func TestRecoveryBudgetCountsActionsAcrossReload(t *testing.T) {
	fsm := &controller{config: Defaults()}
	fsm.config.RecoveryMaxActions = 2
	now := time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)
	next := &Record{}
	if !fsm.authorizeAction(next, now) || !fsm.authorizeAction(next, now.Add(time.Second)) {
		t.Fatal("the configured actions were rejected")
	}
	data, err := json.Marshal(next)
	if err != nil {
		t.Fatal(err)
	}
	var resumed Record
	if err := json.Unmarshal(data, &resumed); err != nil {
		t.Fatal(err)
	}
	leader := &controller{config: fsm.config}
	if leader.authorizeAction(&resumed, now.Add(2*time.Second)) {
		t.Fatal("a new leader allowed an action beyond the saved budget")
	}
	input := leader.budgetInput(&resumed, now.Add(3*time.Second))
	if input.Actions != 2 || input.MaxActions != 2 || input.Elapsed != 3*time.Second || input.Blocked == "" {
		t.Fatalf("incorrect decision input: %+v", input)
	}
	leader.config.RecoveryMaxActions = 3
	if leader.authorizeAction(&resumed, now.Add(4*time.Second)) {
		t.Fatal("a configuration change cleared a persisted block")
	}
}

func TestRecoveryBudgetDeadlineIncludesDiagnosis(t *testing.T) {
	fsm := &controller{config: Defaults()}
	fsm.config.RecoveryTimeout = time.Minute
	now := time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)
	next := &Record{}
	fsm.beginRecovery(next, now)
	fsm.beginRecovery(next, now.Add(30*time.Second))
	if fsm.authorizeAction(next, now.Add(time.Minute)) {
		t.Fatal("diagnosis restarted the recovery deadline")
	}
	if next.Budget.Actions != 0 || !strings.Contains(next.Budget.Blocked, "deadline") {
		t.Fatalf("the deadline did not block the first action: %+v", next.Budget)
	}
}

func TestRecoveryBudgetRequiresContinuousConfirmedHealth(t *testing.T) {
	fsm := &controller{config: Defaults()}
	fsm.config.RecoveryStablePeriod = time.Minute
	now := time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)
	next := &Record{Budget: RecoveryBudget{Started: now, Actions: 2, Blocked: "action limit reached"}}
	member := &engine.Member{Name: "member", PodIdentity: "pod", Serving: true}
	current := &view{
		admitted: []engine.MemberID{member.ID()}, members: map[engine.MemberID]*engine.Member{member.ID(): member},
	}
	fsm.stableRecovery(next, current, now)
	fsm.stableRecovery(next, current, now.Add(59*time.Second))
	if next.Budget.Blocked == "" {
		t.Fatal("brief health cleared the blocked recovery")
	}
	member.Unknown = true
	fsm.stableRecovery(next, current, now.Add(time.Minute))
	member.Unknown = false
	fsm.stableRecovery(next, current, now.Add(61*time.Second))
	fsm.stableRecovery(next, current, now.Add(120*time.Second))
	if next.Budget.Blocked == "" {
		t.Fatal("an unreadable observation counted toward stable health")
	}
	fsm.stableRecovery(next, current, now.Add(121*time.Second))
	if next.Budget != (RecoveryBudget{}) {
		t.Fatalf("continuous confirmed health did not reset the budget: %+v", next.Budget)
	}
	if !fsm.authorizeAction(next, now.Add(122*time.Second)) || next.Budget.Actions != 1 {
		t.Fatal("a stable group could not begin a fresh recovery episode")
	}
}

func TestRecoveryBudgetDoesNotRearmWhileUnsafeOrBusy(t *testing.T) {
	for _, reason := range []string{
		"operation", "workload", "uncertain", "unsafe", "empty", "missing", "terminal", "unhealthy", "blind",
	} {
		t.Run(reason, func(t *testing.T) {
			fsm := &controller{config: Defaults()}
			now := time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)
			next := &Record{Budget: RecoveryBudget{Started: now, Actions: 1, StableSince: now}}
			member := &engine.Member{Name: "member", PodIdentity: "pod", Serving: true}
			current := &view{
				admitted: []engine.MemberID{member.ID()}, members: map[engine.MemberID]*engine.Member{member.ID(): member},
			}
			switch reason {
			case "operation":
				next.Operation = &engine.Operation{}
			case "workload":
				next.Workload = &WorkloadAction{}
			case "uncertain":
				next.OperationBlocked = "action deadline exceeded"
			case "unsafe":
				next.Unsafe = "uncertain outcome"
			case "empty":
				current.admitted = nil
			case "missing":
				delete(current.members, member.ID())
			case "terminal":
				member.Terminal = true
			case "unhealthy":
				member.Serving = false
			case "blind":
				current.observationTimedOut = true
			}
			fsm.stableRecovery(next, current, now.Add(fsm.config.RecoveryStablePeriod))
			if next.Budget.Started.IsZero() || !next.Budget.StableSince.IsZero() {
				t.Fatalf("unstable state rearmed recovery: %+v", next.Budget)
			}
		})
	}
}

func TestRecoveryBudgetFlagsRequirePositiveLimits(t *testing.T) {
	var options Options
	flags := flag.NewFlagSet("budget", flag.ContinueOnError)
	options.BindFlags(flags)
	if err := flags.Parse([]string{
		"--ft-recovery-max-actions=4", "--ft-recovery-timeout=7m", "--ft-recovery-stable-period=30s",
	}); err != nil {
		t.Fatal(err)
	}
	config := options.Config
	if config.RecoveryMaxActions != 4 || config.RecoveryTimeout != 7*time.Minute || config.RecoveryStablePeriod != 30*time.Second {
		t.Fatalf("recovery flags were not applied: %+v", config)
	}
	if err := config.validate(); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"actions", "timeout", "stable"} {
		for _, value := range []int{0, -1} {
			invalid := config
			switch field {
			case "actions":
				invalid.RecoveryMaxActions = value
			case "timeout":
				invalid.RecoveryTimeout = time.Duration(value)
			case "stable":
				invalid.RecoveryStablePeriod = time.Duration(value)
			}
			if err := invalid.validate(); err == nil {
				t.Fatalf("accepted %s=%d", field, value)
			}
		}
	}
}
