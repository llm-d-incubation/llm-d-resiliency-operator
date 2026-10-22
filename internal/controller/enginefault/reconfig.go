// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

package enginefault

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/llm-d/llm-d-resiliency-operator/internal/engine"
)

func (fsm *controller) execute(ctx context.Context, next *Record, current *view, now time.Time) error {
	if next.Workload != nil {
		return fsm.watchPods(ctx, next, current.group, now)
	}
	if fsm.config.DryRun {
		fsm.wouldDo = describeAction(&next.Operation.Action, nil)
		return nil
	}
	operation := *next.Operation
	mayAdvance := now.Sub(next.OperationSince) < fsm.config.ApplyTimeout && next.Unsafe == "" && !current.observationTimedOut
	if mayAdvance {
		next.OperationBlocked = ""
	}
	if !mayAdvance {
		next.OperationBlocked = "operation deadline exceeded; outcome is unresolved"
		if next.Unsafe != "" {
			next.OperationBlocked = next.Unsafe
		} else if current.observationTimedOut {
			next.OperationBlocked = "no admitted member could be read before the observation deadline"
		}
		next.Reason = next.OperationBlocked
	}
	next.Operation.Attempted = true
	if err := fsm.save(ctx, next); err != nil {
		return err
	}
	if !operation.Attempted && operation.Action.Kind == engine.Change {
		reconfig := operation.Reconfig()
		fsm.recorder.ReconfigStarted(&reconfig, &operation.Action)
	}
	callTimeout := fsm.config.ApplyTimeout / 2
	if remaining := fsm.config.ApplyTimeout - now.Sub(next.OperationSince); mayAdvance && remaining < callTimeout {
		callTimeout = remaining
	}
	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	report, err := fsm.eng.Apply(callCtx, engine.ApplyInput{Observation: current.obs, Operation: operation, MayAdvance: mayAdvance})
	if report.Checkpoint != nil {
		if !json.Valid(report.Checkpoint) {
			return errors.New("engine execution checkpoint must be valid JSON")
		}
		next.Operation.Checkpoint = slices.Clone(report.Checkpoint)
	}
	if err != nil {
		next.Reason = err.Error()
		if saveErr := fsm.save(ctx, next); saveErr != nil {
			return saveErr
		}
		return err
	}
	switch report.State {
	case engine.Succeeded:
		if next.Unsafe != "" && operation.Action.Kind == engine.Change {
			fsm.failedChange(next, engine.Group, next.Unsafe, now)
			return nil
		}
		for _, id := range operation.Action.Remove {
			next.Members[id] = Excluded
		}
		for _, id := range operation.Action.Add {
			next.Members[id] = Admitted
		}
		fsm.finish(next, engine.Succeeded, "", report.Reason, now)
	case engine.Failed:
		if operation.Action.Kind == engine.Change {
			fsm.failedChange(next, report.Scope, report.Reason, now)
		} else {
			fsm.finish(next, engine.Failed, report.Scope, report.Reason, now)
		}
	case engine.Running:
		if next.OperationBlocked == "" {
			next.Reason = report.Reason
		}
	default:
		return fmt.Errorf("unknown apply status %q", report.State)
	}
	return nil
}

func (fsm *controller) failedChange(next *Record, scope engine.FailureScope, reason string, now time.Time) {
	plan := &next.Operation.Action
	if scope == engine.Joiners && len(plan.Add) > 0 && len(plan.Remove) == 0 {
		for _, id := range plan.Add {
			next.Members[id] = Rejected
		}
	} else {
		scope = engine.Group
		next.Unsafe = "membership outcome is uncertain: " + reason
	}
	fsm.finish(next, engine.Failed, scope, reason, now)
}

func (fsm *controller) finish(next *Record, state engine.Status, scope engine.FailureScope, reason string, now time.Time) {
	operation := *next.Operation
	next.Result = &engine.Result{Operation: operation, State: state, Scope: scope, Reason: reason}
	if operation.Action.Kind == engine.Verify && next.Incident != nil {
		next.Incident.Durations.Verify += now.Sub(next.OperationSince)
	}
	if operation.Action.Kind == engine.Change {
		reconfig, progress := operation.Reconfig(), engine.Report{State: state, Scope: scope, Reason: reason}
		if inc := next.Incident; inc != nil {
			inc.Reconfigs = append(inc.Reconfigs, ReconfigResult{Reconfig: reconfig, Report: progress})
			inc.Durations.Apply += now.Sub(next.OperationSince)
		}
		fsm.later(func() { fsm.recorder.ReconfigFinished(&reconfig, progress) })
	}
	next.Operation, next.OperationSince, next.Workload = nil, time.Time{}, nil
	next.OperationBlocked = ""
}

func checkAction(next *Record, current *view, plan *engine.Action) string {
	if plan.State != nil && !json.Valid(plan.State) {
		return "policy state must be valid JSON"
	}
	if plan.Payload != nil && !json.Valid(plan.Payload) {
		return "engine payload must be valid JSON"
	}
	if next.Unsafe != "" && plan.Kind != engine.Wait && plan.Kind != engine.RecreateGroup {
		return "unresolved group safety fault: " + next.Unsafe
	}
	if next.Phase == Starting && (plan.Kind == engine.Change || plan.Kind == engine.ReplaceMembers) {
		return "group has not been adopted"
	}
	switch plan.Kind {
	case engine.Healthy:
		for _, id := range current.admitted {
			if member := current.members[id]; member == nil || !member.Serving || member.Unknown {
				return fmt.Sprintf("member %s is not observed serving", id)
			}
		}
	case engine.Wait, engine.Probe, engine.RecreateGroup:
	case engine.Verify:
		if len(plan.Participants) == 0 {
			return "verification needs participants"
		}
		for _, id := range plan.Participants {
			member := current.members[id]
			if next.Members[id] != Admitted || member == nil || member.Unknown {
				return fmt.Sprintf("cannot verify member %s", id)
			}
		}
	case engine.Change:
		return checkPlan(next, current, plan)
	case engine.ReplaceMembers:
		return checkReplacement(next, plan)
	default:
		return fmt.Sprintf("unknown action %q", plan.Kind)
	}
	if len(plan.Remove) != 0 || len(plan.Add) != 0 || len(plan.Members) != 0 || plan.Payload != nil {
		return "action has unrelated targets or payload"
	}
	return ""
}

func checkPlan(next *Record, current *view, plan *engine.Action) string {
	if len(plan.Members) > 0 {
		return "engine change cannot also request replacement resources"
	}
	seen := map[engine.MemberID]bool{}
	for _, list := range [][]engine.MemberID{plan.Participants, plan.Remove, plan.Add} {
		for _, id := range list {
			if seen[id] {
				return fmt.Sprintf("C3: member %s is named twice", id)
			}
			seen[id] = true
			if current.members[id] == nil || current.members[id].Unknown {
				return fmt.Sprintf("C7: member %s is not observed", id)
			}
		}
	}
	if len(plan.Participants) == 0 {
		return "C3: no participants"
	}
	for _, id := range append(slices.Clone(plan.Participants), plan.Remove...) {
		if next.Members[id] != Admitted {
			return fmt.Sprintf("C3: member %s is not admitted", id)
		}
	}
	for _, id := range plan.Remove {
		if !current.members[id].Terminal {
			return fmt.Sprintf("C1: member %s is not terminal", id)
		}
	}
	replacements := map[engine.MemberID]bool{}
	for _, id := range plan.Add {
		member := current.members[id]
		if next.Members[id] != Candidate || !member.Joinable ||
			!joinable(next, current.group, member) || !replacementAllowed(next, member) {
			return fmt.Sprintf("C2: member %s cannot join", id)
		}
		if previous := member.Replaces; previous != "" {
			if replacements[previous] {
				return "C2: a member has more than one replacement"
			}
			replacements[previous] = true
			for otherID, other := range current.members {
				if other.Replaces == previous && next.Members[otherID] == Admitted {
					return "C2: replacement is already admitted"
				}
			}
		}
	}
	return ""
}
