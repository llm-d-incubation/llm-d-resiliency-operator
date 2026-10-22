// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

package enginefault

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/llm-d/llm-d-resiliency-operator/internal/engine"
)

func (fsm *controller) watch(ctx context.Context, next *Record, current *view, now time.Time) error {
	reconfig := *next.Reconfig
	progress := fsm.eng.Progress(current.obs, reconfig)
	switch progress.State {
	case engine.Succeeded:
		for _, id := range reconfig.Remove {
			next.Members[id] = Excluded
		}
		for _, id := range reconfig.Add {
			next.Members[id] = Admitted
		}
		fsm.finishReconfig(next, &progress, now)
		next.Verifying = append(slices.Clone(reconfig.Participants), reconfig.Add...)
		next.VerifyFailedSince = time.Time{}
		enter(next, Serving, "reconfig "+reconfig.ID+" succeeded", now)
		if err := fsm.save(ctx, next); err != nil {
			return err
		}
		return fsm.verify(ctx, next, current, now)
	case engine.Failed:
		fsm.finishReconfig(next, &progress, now)
		if progress.Scope != engine.Joiners || len(reconfig.Add) == 0 || len(reconfig.Remove) != 0 {
			return fsm.resetAll(ctx, next, ResetReconfigFailed, progress.Reason, now)
		}
		rejectJoiners(next, &reconfig)
		if next.Phase == Recovering {
			next.Since = now
		}
		return nil
	case engine.NotStarted:
		if len(next.Acceptances) == 0 {
			return fsm.resend(ctx, next, current, &progress, now)
		}
	}
	if now.Sub(next.ReconfigSince) > fsm.config.ApplyTimeout {
		fsm.finishReconfig(next, &progress, now)
		return fsm.resetAll(ctx, next, ResetApplyDeadline, "reconfig "+reconfig.ID+" did not finish", now)
	}
	return nil
}

func (fsm *controller) send(ctx context.Context, next *Record, current *view, plan *engine.Plan, now time.Time) error {
	if reason := checkPlan(next, current, plan); reason != "" {
		return fsm.resetAll(ctx, next, ResetUnrecoverable, reason, now)
	}
	for _, id := range plan.Participants {
		if member := current.members[id]; member != nil && member.Unknown {
			next.Reason = "waiting to read " + string(id) + ": " + plan.Reason
			return nil
		}
	}
	noChange := len(plan.Remove) == 0 && len(plan.Add) == 0
	if noChange && !next.LastNoChangeReconfig.IsZero() && now.Sub(next.LastNoChangeReconfig) < fsm.config.FlapWindow {
		return fsm.resetAll(ctx, next, ResetFlap, "C6: "+plan.Reason, now)
	}
	if fsm.would(fmt.Sprintf("reconfig: remove %v, add %v", plan.Remove, plan.Add)) {
		return nil
	}
	reconfig := engine.Reconfig{
		ID: newID("iro-ft-", 16), Participants: slices.Clone(plan.Participants),
		Remove: slices.Clone(plan.Remove), Add: slices.Clone(plan.Add),
	}
	for _, id := range reconfig.Add {
		if previous := current.members[id].Replaces; previous != "" {
			if next.Replacements == nil {
				next.Replacements = map[engine.MemberID]engine.MemberID{}
			}
			next.Replacements[previous] = id
		}
	}
	next.Reconfig, next.ReconfigSince, next.Acceptances, next.Reason = &reconfig, now, nil, plan.Reason
	if noChange {
		next.LastNoChangeReconfig = now
	}
	if inc := next.Incident; inc != nil {
		if inc.Durations.Diagnose == 0 {
			inc.Durations.Diagnose = now.Sub(inc.Opened)
		}
		affect(next, inc, reconfig.Remove)
	}
	return fsm.apply(ctx, next, current, &reconfig, plan, now)
}

func (fsm *controller) resend(ctx context.Context, next *Record, current *view, progress *engine.Progress, now time.Time) error {
	reconfig := *next.Reconfig
	plan := engine.Plan{
		Kind: engine.Change, Participants: reconfig.Participants, Remove: reconfig.Remove, Add: reconfig.Add,
		Reason: "resent: no member got it",
	}
	if reason := checkPlan(next, current, &plan); reason != "" {
		if len(reconfig.Add) > 0 && len(reconfig.Remove) == 0 {
			rejectJoiners(next, &reconfig)
		}
		fsm.finishReconfig(next, &engine.Progress{State: engine.NotStarted, Reason: reason}, now)
		next.Reason = "dropped " + reconfig.ID + ", no member got it: " + reason
		if next.Phase == Recovering {
			next.Since = now
		}
		return nil
	}
	next.ReconfigSince = now
	next.Reason = plan.Reason + ": " + progress.Reason
	return fsm.apply(ctx, next, current, &reconfig, &plan, now)
}

func (fsm *controller) apply(
	ctx context.Context, next *Record, current *view, reconfig *engine.Reconfig, plan *engine.Plan, now time.Time,
) error {
	next.Acceptances = nil
	if err := fsm.save(ctx, next); err != nil {
		return err
	}
	acceptances, err := fsm.eng.Apply(ctx, current.obs, *reconfig)
	fsm.recorder.ReconfigStarted(reconfig, plan)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	next.Acceptances = acceptances
	if err != nil {
		var applyErr *engine.ApplyError
		if errors.As(err, &applyErr) && applyErr.Scope == engine.Joiners && len(reconfig.Add) > 0 && len(reconfig.Remove) == 0 {
			fsm.finishReconfig(next, &engine.Progress{State: engine.Failed, Scope: engine.Joiners, Reason: err.Error()}, now)
			rejectJoiners(next, reconfig)
			return nil
		}
		fsm.finishReconfig(next, &engine.Progress{State: engine.Failed, Scope: engine.Group, Reason: err.Error()}, now)
		return fsm.resetAll(ctx, next, ResetReconfigFailed, err.Error(), now)
	}
	return nil
}

func rejectJoiners(next *Record, reconfig *engine.Reconfig) {
	for _, id := range reconfig.Add {
		next.Members[id] = Rejected
	}
}

func checkPlan(next *Record, current *view, plan *engine.Plan) string {
	seen := map[engine.MemberID]bool{}
	for _, list := range [][]engine.MemberID{plan.Participants, plan.Remove, plan.Add} {
		for _, id := range list {
			if seen[id] {
				return fmt.Sprintf("C3: member %s is named twice", id)
			}
			seen[id] = true
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
		if member := current.members[id]; member == nil || !member.Terminal {
			return fmt.Sprintf("C1: member %s is not terminal", id)
		}
	}
	replacements := map[engine.MemberID]bool{}
	for _, id := range plan.Add {
		member := current.members[id]
		if next.Members[id] != Candidate || member == nil || !member.Joinable ||
			!joinable(next, current.group, member) || !replacementAllowed(next, member) {
			return fmt.Sprintf("C2: member %s cannot join", id)
		}
		if member.Replaces != "" {
			if replacements[member.Replaces] {
				return "C2: a member has more than one replacement"
			}
			replacements[member.Replaces] = true
			if len(plan.Remove) > 0 || len(next.Verifying) > 0 {
				return "C2: replacement must follow verified recovery"
			}
			for _, admitted := range current.admitted {
				survivor := current.members[admitted]
				if survivor == nil || !survivor.Serving || !slices.Contains(plan.Participants, admitted) {
					return "C2: replacement needs every survivor serving and participating"
				}
			}
		}
	}
	return ""
}

func (fsm *controller) verify(ctx context.Context, next *Record, current *view, now time.Time) error {
	// A Verify that outlives its share of the step is a failed check, not a stall.
	verifyCtx, cancel := context.WithTimeout(ctx, fsm.config.ApplyTimeout/2)
	err := fsm.eng.Verify(verifyCtx, current.obs, next.Verifying)
	cancel()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err == nil {
		next.Verifying, next.VerifyFailedSince, next.Reason = nil, time.Time{}, "verified"
		if inc := next.Incident; inc != nil {
			inc.Durations.Verify = now.Sub(next.Since)
			fsm.closeIncident(next, outcome(inc), now)
		}
		return nil
	}
	if next.VerifyFailedSince.IsZero() && fsm.config.VerifyTimeout > 0 {
		next.VerifyFailedSince, next.Reason = now, "verify failed; retrying: "+err.Error()
		return nil
	}
	if now.Sub(next.VerifyFailedSince) < fsm.config.VerifyTimeout {
		return nil
	}
	return fsm.resetAll(ctx, next, ResetVerifyFailed, err.Error(), now)
}

func (fsm *controller) resetAll(ctx context.Context, next *Record, reason ResetReason, detail string, now time.Time) error {
	if fsm.would("reset all: " + string(reason)) {
		return nil
	}
	if next.Reconfig != nil {
		fsm.finishReconfig(next, &engine.Progress{State: engine.Running, Reason: "dropped by reset"}, now)
	}
	for uid, pod := range next.Pods {
		pod.Removing = true
		next.Pods[uid] = pod
	}
	next.Members, next.Verifying, next.VerifyFailedSince, next.LastNoChangeReconfig = nil, nil, time.Time{}, time.Time{}
	next.UnseenSince, next.UnseenByDiscover = time.Time{}, false
	if inc := next.Incident; inc != nil && inc.Durations.Diagnose == 0 {
		inc.Durations.Diagnose = now.Sub(inc.Opened)
	}
	fsm.closeIncident(next, Reset, now)
	enter(next, Starting, string(reason)+": "+detail, now)
	if err := fsm.save(ctx, next); err != nil {
		return err
	}
	pods := refs(next)
	fsm.called(fsm.wl.ResetAll(ctx, pods))
	fsm.recorder.ResetAllRequested(reason, pods)
	return nil
}

func (fsm *controller) finishReconfig(next *Record, progress *engine.Progress, now time.Time) {
	reconfig, result := *next.Reconfig, *progress
	if inc := next.Incident; inc != nil {
		inc.Reconfigs = append(inc.Reconfigs, ReconfigResult{Reconfig: reconfig, Progress: result})
		inc.Durations.Apply += now.Sub(next.ReconfigSince)
	}
	next.Reconfig, next.ReconfigSince, next.Acceptances = nil, time.Time{}, nil
	fsm.later(func() { fsm.recorder.ReconfigFinished(&reconfig, result) })
}
