// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

// Package workload owns the group's Pods. See docs/design/contracts/workload.md.
package workload

import (
	"context"
	"errors"
)

type Workload interface {
	Discover(ctx context.Context) (Group, error)
	Reset(ctx context.Context, pods []PodRef) error
	ResetAll(ctx context.Context, pods []PodRef) error
	Release(ctx context.Context, pods []PodRef) error
}

var ErrUnsupported = errors.New("unsupported")

type Group struct {
	Pods            []Pod
	Recreating      bool
	RecreateStopped bool
}

type Pod struct {
	Name     string
	UID      string
	IP       string
	Index    int
	Identity string
	Node     string
	State    PodState
}

type PodState string

const (
	PodStarting    PodState = "Starting"
	PodReady       PodState = "Ready"
	PodTerminating PodState = "Terminating"
)

type PodRef struct {
	Name string
	UID  string
}
