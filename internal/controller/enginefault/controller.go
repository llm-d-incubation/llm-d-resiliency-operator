// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

// Package enginefault is the controller. See docs/components/controller.md.
package enginefault

import (
	"context"
	"encoding/json"
	"time"

	"github.com/llm-d/llm-d-resiliency-operator/internal/engine"
)

type Controller interface {
	Step(ctx context.Context) error
}

type Config struct {
	PollInterval         time.Duration
	DiagnosisTimeout     time.Duration
	ApplyTimeout         time.Duration
	VerifyTimeout        time.Duration
	UnseenTimeout        time.Duration
	FlapWindow           time.Duration
	CapacityRestore      CapacityRestore
	RecoveryMaxActions   int
	RecoveryTimeout      time.Duration
	RecoveryStablePeriod time.Duration
	DryRun               bool
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
	Version int       `json:"version"`
	Phase   Phase     `json:"phase"`
	Since   time.Time `json:"since"`
	Reason  string    `json:"reason"`

	Pods    map[string]PodRecord            `json:"pods"`
	Members map[engine.MemberID]MemberState `json:"members"`

	Epoch            time.Time         `json:"epoch"`
	PolicyState      json.RawMessage   `json:"policy_state,omitempty"`
	Operation        *engine.Operation `json:"operation,omitempty"`
	OperationSince   time.Time         `json:"operation_since,omitzero"`
	OperationBlocked string            `json:"operation_blocked,omitempty"`
	Workload         *WorkloadAction   `json:"workload,omitempty"`
	Result           *engine.Result    `json:"result,omitempty"`
	Unsafe           string            `json:"unsafe,omitempty"`
	Budget           RecoveryBudget    `json:"budget,omitzero"`

	UnseenSince      time.Time `json:"unseen_since,omitzero"`
	UnseenByDiscover bool      `json:"unseen_by_discover,omitempty"`
	RecreateStopped  bool      `json:"recreate_stopped,omitempty"`

	Incident *Incident `json:"incident,omitempty"`
	WouldDo  string    `json:"would_do,omitempty"`
}
