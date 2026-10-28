// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

// Package engine is the inference engine plugin. See docs/components/engines/index.md.
package engine

import (
	"context"
	"encoding/json"
	"time"

	"github.com/llm-d/llm-d-resiliency-operator/internal/workload"
)

type Engine interface {
	Capabilities() Capabilities
	Driver
	Policy
}

type Capabilities struct {
	Grow bool
}

type Driver interface {
	Observe(ctx context.Context, pods []workload.Pod) Observation
	Apply(ctx context.Context, input ApplyInput) (Report, error)
}

// Policy is pure: no I/O, no clock.
type Policy interface {
	Decide(input DecisionInput) Action
}

type DecisionInput struct {
	Observation         Observation
	Admitted            []MemberID
	Candidates          []MemberID
	Excluded            []MemberID
	Rejected            []MemberID
	State               json.RawMessage
	LastResult          *Result
	Starting            bool
	ObservationTimedOut bool
	Fault               string
	Elapsed             time.Duration
	DiagnosisTimeout    time.Duration
	VerifyTimeout       time.Duration
	FlapWindow          time.Duration
	Budget              RecoveryBudget
}

type RecoveryBudget struct {
	Actions    int
	MaxActions int
	Elapsed    time.Duration
	Timeout    time.Duration
	Blocked    string
}

type ApplyInput struct {
	Observation Observation
	Operation   Operation
	MayAdvance  bool
}

type MemberID string

type Member struct {
	Pod         workload.PodRef
	PodIdentity string
	Name        string
	Replaces    MemberID
	Serving     bool
	Terminal    bool
	Unknown     bool
	Joinable    bool
	Detail      string
}

func (member *Member) ID() MemberID { return MemberID(member.Name + "@" + member.PodIdentity) }

type Observation struct {
	Members []Member
	Engine  any
}

type Reconfig struct {
	ID           string
	Participants []MemberID
	Remove       []MemberID
	Add          []MemberID
	Payload      json.RawMessage `json:",omitempty"`
}

type Action struct {
	Kind         ActionKind
	Participants []MemberID
	Remove       []MemberID
	Add          []MemberID
	Members      []MemberID
	Payload      json.RawMessage `json:",omitempty"`
	State        json.RawMessage `json:",omitempty"`
	Reason       string
}

type Operation struct {
	ID         string
	Action     Action
	Checkpoint json.RawMessage `json:",omitempty"`
	Attempted  bool
}

func (operation *Operation) Reconfig() Reconfig {
	return Reconfig{
		ID: operation.ID, Participants: operation.Action.Participants,
		Remove: operation.Action.Remove, Add: operation.Action.Add, Payload: operation.Action.Payload,
	}
}

type Result struct {
	Operation Operation
	State     Status
	Scope     FailureScope
	Reason    string
}

type ActionKind string

const (
	Healthy        ActionKind = "Healthy"
	Wait           ActionKind = "Wait"
	Change         ActionKind = "Change"
	Probe          ActionKind = "Probe"
	Verify         ActionKind = "Verify"
	ReplaceMembers ActionKind = "ReplaceMembers"
	RecreateGroup  ActionKind = "RecreateGroup"
)

type Report struct {
	State      Status
	Scope      FailureScope
	Reason     string
	Checkpoint json.RawMessage `json:",omitempty"`
}

type Status string

const (
	Running   Status = "Running"
	Succeeded Status = "Succeeded"
	Failed    Status = "Failed"
)

type FailureScope string

const (
	Joiners FailureScope = "Joiners"
	Group   FailureScope = "Group"
)
