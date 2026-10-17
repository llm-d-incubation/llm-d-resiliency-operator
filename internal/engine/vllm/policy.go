// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

package vllm

import (
	"fmt"
	"slices"
	"strings"

	"github.com/llm-d/llm-d-resiliency-operator/internal/engine"
)

func (*Engine) Plan(observation engine.Observation, admitted, _ []engine.MemberID) engine.Plan {
	members := membersOf(observation)
	found := ranksOf(observation)
	var survivors, dead, healthy, waiting []engine.MemberID
	for _, id := range admitted {
		member, r := members[id], found[id]
		switch {
		case member != nil && member.Terminal:
			dead = append(dead, id)
		case member == nil || r == nil || !r.Reached:
			waiting = append(waiting, id)
		case r.FTState == "recovering" || r.Status == "diagnosing":
			survivors, waiting = append(survivors, id), append(waiting, id)
		case member.Serving:
			survivors, healthy = append(survivors, id), append(healthy, id)
		case r.Status == "unhealthy":
			survivors = append(survivors, id)
		default:
			survivors, waiting = append(survivors, id), append(waiting, id)
		}
	}
	switch {
	case len(healthy) == len(admitted):
		return engine.Plan{Kind: engine.Healthy, Reason: fmt.Sprintf("%d ranks healthy", len(healthy))}
	case len(survivors) == 0 && len(waiting) == 0:
		return engine.Plan{Kind: engine.Unrecoverable, Reason: "no rank survives: " + list(dead)}
	case len(waiting) > 0:
		return engine.Plan{Kind: engine.Wait, Reason: "waiting for " + describe(members, waiting)}
	case len(healthy) > 0:
		faulted := slices.DeleteFunc(slices.Concat(dead, survivors), func(id engine.MemberID) bool {
			return slices.Contains(healthy, id)
		})
		return engine.Plan{
			Kind: engine.Wait, Reason: describe(members, faulted) + "; waiting for " + list(healthy) + " to detect it",
		}
	case len(dead) > 0:
		return engine.Plan{
			Kind: engine.Change, Participants: survivors, Remove: dead, Reason: "remove dead " + describe(members, dead),
		}
	}
	return engine.Plan{Kind: engine.Change, Participants: survivors, Reason: "retry " + describe(members, survivors)}
}

//nolint:gocritic // The interface passes the reconfig by value.
func (*Engine) Progress(observation engine.Observation, reconfig engine.Reconfig) engine.Progress {
	members := membersOf(observation)
	found := ranksOf(observation)
	done := 0
	for _, id := range reconfig.Participants {
		member, r := members[id], found[id]
		switch {
		case member != nil && member.Terminal:
			return failed(fmt.Sprintf("%s died during the reconfig: %s", id, member.Detail))
		case member == nil || r == nil || !r.Reached:
		case r.RequestID == "" && (r.Status == "unhealthy" || r.Status == "diagnosing"):
			// The status keeps the reconfig's request ID only until the engine reports a new state, so a
			// fault without it came after the reconfig ended, or the reconfig never arrived.
			return failed(fmt.Sprintf("%s is faulted without a reconfig ID: it faulted again after the reconfig, "+
				"or never got it: %s", id, member.Detail))
		case r.FTState == "failed" && r.RequestID == reconfig.ID:
			return failed(fmt.Sprintf("%s failed the reconfig: %s", id, r.FTError))
		case r.FTState == "recovering" && r.RequestID != reconfig.ID:
			return failed(fmt.Sprintf("%s runs request %s instead", id, r.RequestID))
		case member.Serving:
			done++
		}
	}
	if done == len(reconfig.Participants) {
		return engine.Progress{State: engine.Succeeded, Reason: fmt.Sprintf("%d ranks healthy", done)}
	}
	return engine.Progress{State: engine.Running, Reason: fmt.Sprintf("%d of %d ranks healthy", done, len(reconfig.Participants))}
}

func failed(reason string) engine.Progress {
	return engine.Progress{State: engine.Failed, Scope: engine.Group, Reason: reason}
}

func membersOf(observation engine.Observation) map[engine.MemberID]*engine.Member {
	members := map[engine.MemberID]*engine.Member{}
	for index := range observation.Members {
		member := &observation.Members[index]
		members[member.ID()] = member
	}
	return members
}

func describe(members map[engine.MemberID]*engine.Member, ids []engine.MemberID) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		detail := "not observed"
		if member := members[id]; member != nil {
			detail = member.Detail
		}
		parts = append(parts, fmt.Sprintf("%s (%s)", id, detail))
	}
	return strings.Join(parts, ", ")
}

func list(ids []engine.MemberID) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, string(id))
	}
	return strings.Join(parts, ", ")
}
