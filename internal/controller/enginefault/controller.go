// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

// Package enginefault is the controller. See docs/components/controller.md.
package enginefault

import (
	"context"
	"time"

	"github.com/llm-d/llm-d-resiliency-operator/internal/engine"
)

type Controller interface {
	Step(ctx context.Context) error
}

type Config struct {
	PollInterval     time.Duration
	DiagnosisTimeout time.Duration
	ApplyTimeout     time.Duration
	VerifyTimeout    time.Duration
	UnseenTimeout    time.Duration
	FlapWindow       time.Duration
	CapacityRestore  CapacityRestore
	// DryRun decides as usual but only records what it would do. See docs/components/controller.md#dry-run.
	DryRun bool
}

type CapacityRestore string

const (
	RestoreResetPod CapacityRestore = "reset-pod"
	RestoreResetAll CapacityRestore = "reset-all"
	RestoreRelease  CapacityRestore = "release"
)

type Phase string

const (
	Starting   Phase = "starting"
	Serving    Phase = "serving"
	Recovering Phase = "recovering"
)

type MemberState string

const (
	Candidate MemberState = "candidate"
	Admitted  MemberState = "admitted"
	Excluded  MemberState = "excluded"
	Rejected  MemberState = "rejected"
)

type PodRecord struct {
	Name     string `json:"name"`
	Identity string `json:"identity"`
	Node     string `json:"node,omitempty"`
	Removing bool   `json:"removing,omitempty"`
}

// Record is what the journal stores.
type Record struct {
	Phase  Phase     `json:"phase"`
	Since  time.Time `json:"since"`
	Reason string    `json:"reason"`

	Pods    map[string]PodRecord            `json:"pods"`
	Members map[engine.MemberID]MemberState `json:"members"`

	Replacements map[engine.MemberID]engine.MemberID `json:"replacements,omitempty"`

	Reconfig      *engine.Reconfig    `json:"reconfig,omitempty"`
	ReconfigSince time.Time           `json:"reconfig_since,omitzero"`
	Acceptances   []engine.Acceptance `json:"acceptances,omitempty"`
	Verifying     []engine.MemberID   `json:"verifying,omitempty"`

	LastNoChangeReconfig time.Time `json:"last_no_change_reconfig,omitzero"`
	VerifyFailedSince    time.Time `json:"verify_failed_since,omitzero"`
	UnseenSince          time.Time `json:"unseen_since,omitzero"`
	UnseenByDiscover     bool      `json:"unseen_by_discover,omitempty"`
	RecreateStopped      bool      `json:"recreate_stopped,omitempty"`

	Incident *Incident `json:"incident,omitempty"`

	// WouldDo is the action a dry run skipped on the last poll.
	WouldDo string `json:"would_do,omitempty"`
}
