// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

package sglang

import (
	"fmt"
	"strings"

	"github.com/llm-d/llm-d-resiliency-operator/internal/engine"
)

// Plan acts at once: SGLang's FT manager already sees every rank, so there is no peer left to notice the fault.
func (*Engine) Plan(observation engine.Observation, admitted, _ []engine.MemberID) engine.Plan {
	members := membersOf(observation)
	var survivors, dead, unknown []engine.MemberID
	healthy := 0
	for _, id := range admitted {
		member := members[id]
		switch {
		case member == nil || member.Unknown:
			unknown = append(unknown, id)
		case member.Terminal:
			dead = append(dead, id)
		default:
			survivors = append(survivors, id)
			if member.Serving {
				healthy++
			}
		}
	}
	switch {
	case healthy == len(admitted):
		return engine.Plan{Kind: engine.Healthy, Reason: fmt.Sprintf("%d ranks healthy", healthy)}
	case len(unknown) > 0:
		return engine.Plan{Kind: engine.Wait, Reason: "waiting for " + describe(members, unknown)}
	case len(survivors) == 0:
		return engine.Plan{Kind: engine.Unrecoverable, Reason: "no rank survives: " + describe(members, dead)}
	case len(dead) > 0:
		return engine.Plan{
			Kind: engine.Change, Participants: survivors, Remove: dead, Reason: "remove dead " + describe(members, dead),
		}
	}
	return engine.Plan{Kind: engine.Change, Participants: survivors, Reason: "retry " + describe(members, survivors)}
}

// Progress reads the one result SGLang keeps: the last FT request it finished, and its error.
//
//nolint:gocritic // The interface passes the reconfig by value.
func (*Engine) Progress(observation engine.Observation, reconfig engine.Reconfig) engine.Progress {
	found := statusOf(observation)
	if found.RequestID != reconfig.ID {
		return engine.Progress{State: engine.Running, Reason: "SGLang has not finished " + reconfig.ID}
	}
	if found.FTError != "" {
		return failed("SGLang failed the reconfig: " + found.FTError)
	}
	members := membersOf(observation)
	for _, id := range reconfig.Participants {
		if member := members[id]; member == nil || !member.Serving {
			return failed(fmt.Sprintf("%s is not healthy after the reconfig: %s", id, describe(members, []engine.MemberID{id})))
		}
	}
	return engine.Progress{State: engine.Succeeded, Reason: fmt.Sprintf("%d ranks healthy", len(reconfig.Participants))}
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
