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
	"github.com/llm-d/llm-d-resiliency-operator/internal/workload"
)

type WorkloadAction struct {
	Kind CapacityRestore   `json:"kind"`
	Pods []workload.PodRef `json:"pods"`
}

func externalAction(kind engine.ActionKind) bool {
	return kind == engine.ReplaceMembers || kind == engine.RecreateGroup
}

func checkReplacement(next *Record, action *engine.Action) string {
	if len(action.Members) == 0 || len(action.Participants)+len(action.Remove)+len(action.Add) > 0 || action.Payload != nil {
		return "replacement requirement needs only member targets"
	}
	seen := map[engine.MemberID]bool{}
	for _, id := range action.Members {
		if seen[id] || (next.Members[id] != Excluded && next.Members[id] != Rejected) {
			return fmt.Sprintf("member %s is not a unique excluded or rejected target", id)
		}
		seen[id] = true
	}
	return ""
}

func (fsm *controller) selectWorkload(next *Record, current *view, action *engine.Action) (*WorkloadAction, engine.Report) {
	if action.Kind == engine.RecreateGroup || fsm.config.CapacityRestore == RestoreResetAll {
		return &WorkloadAction{Kind: RestoreResetAll, Pods: refs(next)}, engine.Report{}
	}
	if fsm.config.CapacityRestore == RestoreResetPod && !fsm.eng.Capabilities().Grow {
		return nil, engine.Report{State: engine.Succeeded, Reason: "reduced capacity retained; engine cannot admit replacements"}
	}
	selected := &WorkloadAction{Kind: fsm.config.CapacityRestore}
	for _, id := range action.Members {
		uid, saved, found := podOf(next, id)
		if !found || find(current.group, uid) == nil {
			continue
		}
		pod := find(current.group, uid)
		if saved.Removing || pod.Identity != saved.Identity || !onlyExcluded(next, saved.Identity) {
			return nil, engine.Report{State: engine.Failed, Reason: "replacement would affect a Pod with live or uncertain members"}
		}
		if !slices.ContainsFunc(selected.Pods, func(ref workload.PodRef) bool { return ref.UID == uid }) {
			selected.Pods = append(selected.Pods, podRef(uid, &saved))
		}
	}
	if len(selected.Pods) == 0 {
		return nil, engine.Report{State: engine.Succeeded, Reason: "target Pods are already absent"}
	}
	return selected, engine.Report{}
}

func (fsm *controller) watchPods(ctx context.Context, next *Record, group *workload.Group, now time.Time) error {
	if fsm.config.DryRun {
		fsm.wouldDo = describeAction(&next.Operation.Action, next.Workload)
		return nil
	}
	action := *next.Workload
	var present []workload.PodRef
	for _, target := range action.Pods {
		if find(group, target.UID) != nil {
			present = append(present, target)
		}
	}
	if len(present) == 0 {
		fsm.finish(next, engine.Succeeded, "", "target Pods are gone", now)
		if action.Kind == RestoreResetAll {
			fsm.closeIncident(next, Reset, now)
			next.Members, next.Pods, next.Unsafe = nil, nil, ""
			enter(next, Starting, "group reset completed", now)
		} else {
			for _, target := range action.Pods {
				forget(next, target.UID)
			}
		}
		return nil
	}
	if now.Sub(next.OperationSince) >= fsm.config.ApplyTimeout {
		next.OperationBlocked = "Pod operation deadline exceeded; outcome is unresolved"
		next.Reason = next.OperationBlocked
		return nil
	}
	if group.Recreating || group.RecreateStopped {
		return nil
	}
	if err := fsm.save(ctx, next); err != nil {
		return err
	}
	var err error
	switch action.Kind {
	case RestoreResetPod:
		err = fsm.wl.Reset(ctx, present)
		fsm.recorder.PodResetRequested(present, err)
	case RestoreRelease:
		err = fsm.wl.Release(ctx, present)
		fsm.recorder.ReleaseRequested(present, err)
	case RestoreResetAll:
		err = fsm.wl.ResetAll(ctx, present)
		fsm.recorder.ResetAllRequested(ResetPolicy, present)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, workload.ErrUnsupported) {
		for _, target := range action.Pods {
			pod := next.Pods[target.UID]
			pod.Removing = false
			next.Pods[target.UID] = pod
		}
		fsm.finish(next, engine.Failed, engine.Group, err.Error(), now)
		return nil
	}
	return err
}

func onlyExcluded(next *Record, identity string) bool {
	found := false
	for id, state := range next.Members {
		if identityOf(id) == identity {
			if state != Excluded && state != Rejected {
				return false
			}
			found = true
		}
	}
	return found
}
