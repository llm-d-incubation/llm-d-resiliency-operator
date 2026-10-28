// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

package enginefault

import (
	"slices"
	"time"

	"github.com/llm-d/llm-d-resiliency-operator/internal/engine"
	"github.com/llm-d/llm-d-resiliency-operator/internal/workload"
)

func (fsm *controller) openIncident(next *Record, cause string, current *view, now time.Time) {
	inc := &Incident{ID: newID("inc-", 8), Opened: now, Cause: cause}
	if current != nil {
		inc.Affected = current.pick(func(member *engine.Member) bool { return member != nil && member.Terminal })
		if len(inc.Affected) == 0 {
			inc.Affected = current.pick(func(member *engine.Member) bool { return member == nil || !member.Serving })
		}
	}
	affect(next, inc, inc.Affected)
	next.Incident = inc
	opened := *inc
	fsm.later(func() { fsm.recorder.IncidentOpened(&opened) })
}

func affect(next *Record, inc *Incident, ids []engine.MemberID) {
	for _, id := range ids {
		if !slices.Contains(inc.Affected, id) {
			inc.Affected = append(inc.Affected, id)
		}
		uid, saved, ok := podOf(next, id)
		if !ok || slices.ContainsFunc(inc.Pods, func(pod workload.PodRef) bool { return pod.UID == uid }) {
			continue
		}
		inc.Pods = append(inc.Pods, podRef(uid, &saved))
		if saved.Node != "" && !slices.Contains(inc.Nodes, saved.Node) {
			inc.Nodes = append(inc.Nodes, saved.Node)
		}
	}
}

func (fsm *controller) closeIncident(next *Record, result Outcome, now time.Time) {
	if next.Incident == nil {
		return
	}
	closed := *next.Incident
	closed.Closed, closed.Outcome, closed.Durations.Total = now, result, now.Sub(closed.Opened)
	next.Incident = nil
	fsm.later(func() { fsm.recorder.IncidentClosed(&closed) })
}

func (current *view) pick(match func(*engine.Member) bool) []engine.MemberID {
	var ids []engine.MemberID
	for _, id := range current.admitted {
		if match(current.members[id]) {
			ids = append(ids, id)
		}
	}
	return ids
}

func outcome(inc *Incident) Outcome {
	removed := 0
	for index := 0; inc != nil && index < len(inc.Reconfigs); index++ {
		if result := &inc.Reconfigs[index]; result.Report.State == engine.Succeeded {
			removed += len(result.Reconfig.Remove) - len(result.Reconfig.Add)
		}
	}
	if removed > 0 {
		return Shrunk
	}
	return Recovered
}
