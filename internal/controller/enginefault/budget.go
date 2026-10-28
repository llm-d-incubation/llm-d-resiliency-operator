// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

package enginefault

import (
	"fmt"
	"time"

	"github.com/llm-d/llm-d-resiliency-operator/internal/engine"
)

type RecoveryBudget struct {
	Started     time.Time `json:"started,omitzero"`
	Actions     int       `json:"actions,omitempty"`
	StableSince time.Time `json:"stable_since,omitzero"`
	Blocked     string    `json:"blocked,omitempty"`
}

func (fsm *controller) beginRecovery(next *Record, now time.Time) {
	if next.Budget.Started.IsZero() {
		next.Budget.Started = now
	}
	fsm.interruptStability(next)
}

func (*controller) interruptStability(next *Record) {
	next.Budget.StableSince = time.Time{}
}

func (fsm *controller) budgetInput(next *Record, now time.Time) engine.RecoveryBudget {
	budget := &next.Budget
	var elapsed time.Duration
	if !budget.Started.IsZero() {
		elapsed = max(0, now.Sub(budget.Started))
		if budget.Blocked == "" {
			switch {
			case elapsed >= fsm.config.RecoveryTimeout:
				budget.Blocked = "recovery deadline exceeded"
			case budget.Actions >= fsm.config.RecoveryMaxActions:
				budget.Blocked = fmt.Sprintf("recovery action limit reached (%d)", fsm.config.RecoveryMaxActions)
			}
		}
	}
	return engine.RecoveryBudget{
		Actions: budget.Actions, MaxActions: fsm.config.RecoveryMaxActions,
		Elapsed: elapsed, Timeout: fsm.config.RecoveryTimeout, Blocked: budget.Blocked,
	}
}

func (fsm *controller) authorizeAction(next *Record, now time.Time) bool {
	fsm.beginRecovery(next, now)
	if fsm.budgetInput(next, now).Blocked != "" {
		return false
	}
	next.Budget.Actions++
	return true
}

func (fsm *controller) stableRecovery(next *Record, current *view, now time.Time) {
	if next.Budget.Started.IsZero() {
		return
	}
	stable := next.Operation == nil && next.Workload == nil && next.OperationBlocked == "" && next.Unsafe == "" &&
		len(current.admitted) > 0 && !current.observationTimedOut && len(current.unknown) == 0
	for _, id := range current.admitted {
		member := current.members[id]
		if member == nil || member.Unknown || member.Terminal || !member.Serving {
			stable = false
			break
		}
	}
	if !stable {
		fsm.interruptStability(next)
		return
	}
	if next.Budget.StableSince.IsZero() {
		next.Budget.StableSince = now
	}
	if now.Sub(next.Budget.StableSince) >= fsm.config.RecoveryStablePeriod {
		next.Budget = RecoveryBudget{}
	}
}
