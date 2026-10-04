// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0
package enginefault

import (
	"slices"

	"github.com/llm-d-incubation/llm-d-resiliency-operator/internal/engine"
)

type Decision struct {
	Action       string `json:"action"`
	Participants []int  `json:"participants,omitempty"`
	Removed      []int  `json:"removed,omitempty"`
	Reason       string `json:"reason"`
}

// Decide only excludes engines in the native terminal dead state. Survivor
// masks and process observations cannot fence an excluded frontend: scale_down
// changes surviving engines, while EPP independently uses native engine health.
func Decide(allStatuses []engine.RankStatus, excluded []int) Decision {
	active := make([]*engine.RankStatus, 0, len(allStatuses))
	for index := range allStatuses {
		status := &allStatuses[index]
		if !slices.Contains(excluded, status.Rank) {
			active = append(active, status)
		}
	}
	if len(active) == 0 {
		return Decision{Action: "reset", Reason: "no active ranks"}
	}
	dead := []int{}
	fault := false
	allUnhealthy := true
	allHealthy := true
	for _, status := range active {
		if status.Status == "dead" {
			dead = append(dead, status.Rank)
		}
		if status.Status == "dead" || status.Status == "diagnosing" || status.Status == "unhealthy" ||
			status.ProcessExitConfirmed || status.FTState != "" {
			fault = true
		}
		eligible := !status.ProcessExitConfirmed && status.FTState == ""
		allUnhealthy = allUnhealthy && status.Status == "unhealthy" && eligible
		allHealthy = allHealthy && status.Status == "healthy" && eligible
		if status.FTState == "failed" {
			return Decision{Action: "reset", Reason: "vLLM reports failed FT: " + status.FTError}
		}
	}
	if allHealthy {
		return Decision{Action: "healthy", Reason: "all active ranks healthy"}
	}
	if !fault {
		// Losing an API is not necessarily losing its EngineCore/EP worker.
		// Verify inference on healthy peers without changing EP membership.
		participants := []int{}
		for _, status := range active {
			if status.Status == "healthy" {
				participants = append(participants, status.Rank)
			}
		}
		if len(participants) > 0 {
			return Decision{
				Action: "frontend_degraded", Participants: participants,
				Reason: "API unavailable; verify peer inference without changing EP membership",
			}
		}
		return Decision{Action: "wait", Reason: "unknown frontend state; no removal evidence"}
	}
	if len(dead) == 0 && allUnhealthy {
		participants := []int{}
		for _, status := range active {
			participants = append(participants, status.Rank)
		}
		return Decision{Action: "retry", Participants: participants, Reason: "all participants faulted and no dead rank"}
	}
	removed := slices.Clone(dead)
	if len(removed) == 0 {
		return Decision{Action: "wait", Reason: "waiting for native terminal death or retryable engine diagnosis"}
	}
	slices.Sort(removed)
	participants := []int{}
	for _, status := range active {
		if slices.Contains(removed, status.Rank) {
			continue
		}
		participants = append(participants, status.Rank)
	}
	if len(participants) == 0 {
		return Decision{Action: "reset", Reason: "no surviving ranks"}
	}
	// vLLM validates expert capacity during scale_down. Reconcile its result
	// rather than predicting recovery from a configured survivor count.
	for _, status := range active {
		if !slices.Contains(removed, status.Rank) &&
			(status.Status != "unhealthy" || status.ProcessExitConfirmed || status.FTState != "") {
			return Decision{Action: "wait", Reason: "survivors are not all unhealthy yet"}
		}
	}
	return Decision{
		Action: "scale_down", Participants: participants, Removed: removed,
		Reason: "native terminal dead ranks cannot resume serving",
	}
}
