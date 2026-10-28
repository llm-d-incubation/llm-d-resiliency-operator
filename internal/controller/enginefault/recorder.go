// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

package enginefault

import (
	"time"

	"github.com/llm-d/llm-d-resiliency-operator/internal/engine"
	"github.com/llm-d/llm-d-resiliency-operator/internal/workload"
)

// Recorder is telemetry. See docs/components/telemetry.md.
type Recorder interface {
	Stepped(record *Record, err error)
	PhaseChanged(from, to Phase, reason string)
	IncidentOpened(incident *Incident)
	ReconfigStarted(reconfig *engine.Reconfig, plan *engine.Plan)
	ReconfigFinished(reconfig *engine.Reconfig, progress engine.Progress)
	PodResetRequested(pods []workload.PodRef, err error)
	ResetAllRequested(reason ResetReason, pods []workload.PodRef)
	ReleaseRequested(pods []workload.PodRef, err error)
	SaveFailed(err error)
	IncidentClosed(incident *Incident)
	WouldDo(from, to, reason string)
	Ready() error
}

type Incident struct {
	ID        string            `json:"id"`
	Opened    time.Time         `json:"opened"`
	Closed    time.Time         `json:"closed,omitzero"`
	Cause     string            `json:"cause"`
	Affected  []engine.MemberID `json:"affected,omitempty"`
	Pods      []workload.PodRef `json:"pods,omitempty"`
	Nodes     []string          `json:"nodes,omitempty"`
	Reconfigs []ReconfigResult  `json:"reconfigs,omitempty"`
	Outcome   Outcome           `json:"outcome,omitempty"`
	Durations Durations         `json:"durations"`
}

type ReconfigResult struct {
	Reconfig engine.Reconfig `json:"reconfig"`
	Progress engine.Progress `json:"progress"`
}

type Outcome string

const (
	Recovered Outcome = "Recovered"
	Shrunk    Outcome = "Shrunk"
	Reset     Outcome = "Reset"
	Recreated Outcome = "Recreated"
)

type Durations struct {
	Diagnose time.Duration `json:"diagnose"`
	Apply    time.Duration `json:"apply"`
	Verify   time.Duration `json:"verify"`
	Total    time.Duration `json:"total"`
}

type ResetReason string

const (
	ResetUnrecoverable     ResetReason = "unrecoverable"
	ResetReconfigFailed    ResetReason = "reconfig_failed"
	ResetDiagnosisDeadline ResetReason = "diagnosis_deadline"
	ResetApplyDeadline     ResetReason = "apply_deadline"
	ResetFlap              ResetReason = "flap"
	ResetVerifyFailed      ResetReason = "verify_failed"
	ResetExclusionBroken   ResetReason = "exclusion_broken"
	ResetCapacity          ResetReason = "capacity"
	ResetUnseen            ResetReason = "unseen"
	ResetJournalLost       ResetReason = "journal_lost"
)
