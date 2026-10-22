// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

package enginefault

import (
	"fmt"

	"github.com/llm-d/llm-d-resiliency-operator/internal/engine"
)

func describeAction(action *engine.Action, selected *WorkloadAction) string {
	if selected != nil {
		return fmt.Sprintf("%s %v", selected.Kind, selected.Pods)
	}
	switch action.Kind {
	case engine.Change:
		return fmt.Sprintf("reconfig: remove %v, add %v", action.Remove, action.Add)
	case engine.ReplaceMembers:
		return fmt.Sprintf("%s %v", action.Kind, action.Members)
	default:
		return fmt.Sprintf("%s %v", action.Kind, action.Participants)
	}
}
