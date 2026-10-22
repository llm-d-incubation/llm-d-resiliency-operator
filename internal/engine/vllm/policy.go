// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

package vllm

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/llm-d/llm-d-resiliency-operator/internal/engine"
)

type recoveryState struct {
	Stage             string            `json:"stage,omitempty"`
	ResultID          string            `json:"resultID,omitempty"`
	DiagnosisSince    time.Duration     `json:"diagnosisSince,omitempty"`
	VerifyFailed      bool              `json:"verifyFailed,omitempty"`
	VerifyFailedSince time.Duration     `json:"verifyFailedSince,omitempty"`
	LastRetry         *time.Duration    `json:"lastRetry,omitempty"`
	Replacements      []engine.MemberID `json:"replacements,omitempty"`
	Verified          []engine.MemberID `json:"verified,omitempty"`
}

//nolint:gocritic // The policy contract passes DecisionInput by value.
func (*Engine) Decide(input engine.DecisionInput) engine.Action {
	var state recoveryState
	if len(input.State) > 0 {
		if err := json.Unmarshal(input.State, &state); err != nil {
			return engine.Action{
				Kind: engine.RecreateGroup, State: json.RawMessage(`{}`), Reason: "invalid vLLM recovery state: " + err.Error(),
			}
		}
	}
	plan := nextAction(&input, &state)
	data, err := json.Marshal(state)
	if err != nil {
		return engine.Action{Kind: engine.RecreateGroup, Reason: "encode vLLM recovery state: " + err.Error()}
	}
	plan.State = data
	return plan
}

func nextAction(input *engine.DecisionInput, state *recoveryState) engine.Action {
	if input.Fault != "" {
		return recreateAction(input.Fault)
	}
	if input.ObservationTimedOut {
		return recreateAction("no admitted rank could be read before the observation deadline")
	}
	if plan := consumeResult(input, state); plan != nil {
		return *plan
	}
	plan := membershipAction(input.Observation, input.Admitted)
	if state.Stage == "verifying" {
		if state.VerifyFailed && input.Elapsed-state.VerifyFailedSince >= input.VerifyTimeout {
			return recreateAction("vLLM verification deadline exceeded")
		}
		return engine.Action{Kind: engine.Verify, Participants: input.Admitted, Reason: "verify admitted ranks"}
	}
	if len(input.Admitted) == 0 {
		if input.Starting {
			return engine.Action{Kind: engine.Wait, Reason: "waiting for ranks to start"}
		}
		return recreateAction("no admitted ranks")
	}
	if plan.Kind == engine.Healthy {
		if input.Starting && (state.Stage != "serving" || !slices.Equal(state.Verified, input.Admitted)) {
			state.Stage = "verifying"
			return engine.Action{Kind: engine.Verify, Participants: input.Admitted, Reason: "verify starting ranks"}
		}
		state.Stage = "serving"
		return replacementAction(input, state, &plan)
	}
	if plan.Kind == engine.RecreateGroup {
		return plan
	}
	if state.Stage != "diagnosing" {
		state.Stage, state.DiagnosisSince = "diagnosing", input.Elapsed
		return engine.Action{Kind: engine.Probe, Reason: plan.Reason}
	}
	if input.Elapsed-state.DiagnosisSince >= input.DiagnosisTimeout {
		return recreateAction("vLLM diagnosis deadline exceeded: " + plan.Reason)
	}
	if input.Starting {
		return engine.Action{Kind: engine.Wait, Reason: "waiting for ranks to start: " + plan.Reason}
	}
	if plan.Kind == engine.Change && len(plan.Remove) == 0 {
		if state.LastRetry != nil && input.Elapsed-*state.LastRetry < input.FlapWindow {
			return recreateAction("vLLM ranks faulted again within the retry window")
		}
		elapsed := input.Elapsed
		state.LastRetry = &elapsed
	}
	return plan
}

func consumeResult(input *engine.DecisionInput, state *recoveryState) *engine.Action {
	result := input.LastResult
	if result == nil || result.Operation.ID == state.ResultID {
		return nil
	}
	state.ResultID = result.Operation.ID
	switch result.Operation.Action.Kind {
	case engine.Change:
		if result.State != engine.Succeeded {
			plan := recreateAction("vLLM reconfiguration failed: " + result.Reason)
			return &plan
		}
		state.Stage, state.VerifyFailed, state.Verified = "verifying", false, nil
	case engine.Verify:
		if result.State == engine.Succeeded {
			state.Stage, state.VerifyFailed = "serving", false
			state.Verified = slices.Clone(result.Operation.Action.Participants)
		} else {
			state.Stage = "verifying"
			if !state.VerifyFailed {
				state.VerifyFailed, state.VerifyFailedSince = true, input.Elapsed
			}
		}
	case engine.ReplaceMembers:
		state.Replacements = append(state.Replacements, result.Operation.Action.Members...)
	case engine.RecreateGroup:
		if result.State != engine.Succeeded {
			plan := recreateAction("vLLM group reset failed: " + result.Reason)
			return &plan
		}
		*state = recoveryState{ResultID: result.Operation.ID}
	}
	return nil
}

func replacementAction(input *engine.DecisionInput, state *recoveryState, healthy *engine.Action) engine.Action {
	action := engine.Action{Kind: engine.ReplaceMembers, Reason: "replacement resources needed for excluded ranks"}
	for _, id := range slices.Concat(input.Excluded, input.Rejected) {
		if !slices.Contains(state.Replacements, id) {
			action.Members = append(action.Members, id)
		}
	}
	if len(action.Members) > 0 {
		return action
	}
	return *healthy
}

func recreateAction(reason string) engine.Action {
	return engine.Action{Kind: engine.RecreateGroup, Reason: reason}
}

func membershipAction(observation engine.Observation, admitted []engine.MemberID) engine.Action {
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
		return engine.Action{Kind: engine.Healthy, Reason: fmt.Sprintf("%d ranks healthy", len(healthy))}
	case len(survivors) == 0 && len(waiting) == 0:
		return engine.Action{Kind: engine.RecreateGroup, Reason: "no rank survives: " + list(dead)}
	case len(waiting) > 0:
		return engine.Action{Kind: engine.Wait, Reason: "waiting for " + describe(members, waiting)}
	case len(healthy) > 0:
		faulted := slices.DeleteFunc(slices.Concat(dead, survivors), func(id engine.MemberID) bool {
			return slices.Contains(healthy, id)
		})
		return engine.Action{
			Kind: engine.Wait, Reason: describe(members, faulted) + "; waiting for " + list(healthy) + " to detect it",
		}
	case len(dead) > 0:
		return engine.Action{
			Kind: engine.Change, Participants: survivors, Remove: dead, Reason: "remove dead " + describe(members, dead),
		}
	}
	return engine.Action{Kind: engine.Change, Participants: survivors, Reason: "retry " + describe(members, survivors)}
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
