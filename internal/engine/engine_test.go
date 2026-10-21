// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

package engine_test

import (
	"testing"

	"github.com/llm-d/llm-d-resiliency-operator/internal/engine"
)

func TestMemberIncarnations(t *testing.T) {
	member := engine.Member{Name: "member-1", PodIdentity: "pod-generation"}
	if member.ID() != "member-1@pod-generation" {
		t.Fatal("empty incarnation changed existing IDs")
	}
	previous := member.ID()
	member.Incarnation = "process-2"
	if member.ID() != "member-1@process-2@pod-generation" || member.ID() == previous {
		t.Fatal("new process reused the excluded identity")
	}
}
