// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

// Package engine is the inference engine plugin. See docs/components/engines/index.md.
package engine

import (
	"context"

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
	Probe(ctx context.Context, observation Observation) error
	Apply(ctx context.Context, observation Observation, reconfig Reconfig) ([]Acceptance, error)
	Verify(ctx context.Context, observation Observation, members []MemberID) error
}

// Policy is pure: no I/O, no clock.
type Policy interface {
	Plan(observation Observation, admitted, candidates []MemberID) Plan
	Progress(observation Observation, reconfig Reconfig) Progress
}

type MemberID string

type Member struct {
	Pod         workload.PodRef
	PodIdentity string
	Name        string
	Incarnation string
	Replaces    MemberID
	Serving     bool
	Terminal    bool
	Unknown     bool
	Joinable    bool
	Detail      string
}

func (member *Member) ID() MemberID {
	name := member.Name
	if member.Incarnation != "" {
		name += "@" + member.Incarnation
	}
	return MemberID(name + "@" + member.PodIdentity)
}

type Observation struct {
	Members []Member
	Engine  any
}

type Reconfig struct {
	ID           string
	Participants []MemberID
	Remove       []MemberID
	Add          []MemberID
}

type Acceptance struct {
	Member   MemberID
	Accepted bool
	Error    string
}

type Plan struct {
	Kind         PlanKind
	Participants []MemberID
	Remove       []MemberID
	Add          []MemberID
	Reason       string
}

type PlanKind string

const (
	Healthy       PlanKind = "Healthy"
	Wait          PlanKind = "Wait"
	Change        PlanKind = "Change"
	Unrecoverable PlanKind = "Unrecoverable"
)

type Progress struct {
	State  ProgressState
	Scope  FailureScope
	Reason string
}

type ProgressState string

const (
	NotStarted ProgressState = "NotStarted"
	Running    ProgressState = "Running"
	Succeeded  ProgressState = "Succeeded"
	Failed     ProgressState = "Failed"
)

type FailureScope string

const (
	Joiners FailureScope = "Joiners"
	Group   FailureScope = "Group"
)

// ApplyError scopes an apply failure after the driver has established its effects.
type ApplyError struct {
	Scope FailureScope
	Err   error
}

func (err *ApplyError) Error() string { return err.Err.Error() }
func (err *ApplyError) Unwrap() error { return err.Err }
