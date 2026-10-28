// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

// Package telemetry reports what the controller does. See docs/components/telemetry.md.
package telemetry

import (
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"

	"github.com/llm-d/llm-d-resiliency-operator/internal/controller/enginefault"
	"github.com/llm-d/llm-d-resiliency-operator/internal/engine"
	"github.com/llm-d/llm-d-resiliency-operator/internal/workload"
)

// Recorder writes one log line per event. Metrics, Events and incident history come with #25 and #30.
type Recorder struct {
	log     logr.Logger
	stale   time.Duration
	mu      sync.Mutex
	last    time.Time
	blocked string
}

var _ enginefault.Recorder = (*Recorder)(nil)

func New(log logr.Logger, stale time.Duration) *Recorder {
	return &Recorder{log: log, stale: stale}
}

func (rec *Recorder) Stepped(record *enginefault.Record, err error) {
	rec.mu.Lock()
	rec.last = time.Now()
	blocked := record.OperationBlocked
	if blocked == "" {
		blocked = record.Budget.Blocked
	}
	previous := rec.blocked
	rec.blocked = blocked
	rec.mu.Unlock()
	if blocked != previous {
		if blocked != "" {
			rec.log.Info("recovery blocked", "reason", blocked, "actions", record.Budget.Actions)
		} else {
			rec.log.Info("recovery unblocked")
		}
	}
	if err != nil {
		rec.log.Error(err, "step failed", "phase", record.Phase, "reason", record.Reason)
	}
}

func (rec *Recorder) PhaseChanged(from, to enginefault.Phase, reason string) {
	rec.log.Info("phase changed", "from", from, "to", to, "reason", reason)
}

func (rec *Recorder) IncidentOpened(incident *enginefault.Incident) {
	rec.log.Info("incident opened", "incident", incident.ID, "cause", incident.Cause,
		"affected", incident.Affected, "nodes", incident.Nodes)
}

func (rec *Recorder) ReconfigStarted(reconfig *engine.Reconfig, plan *engine.Action) {
	rec.log.Info("reconfig started", "reconfig", reconfig.ID, "remove", reconfig.Remove, "add", reconfig.Add, "reason", plan.Reason)
}

func (rec *Recorder) ReconfigFinished(reconfig *engine.Reconfig, progress engine.Report) {
	rec.log.Info("reconfig finished", "reconfig", reconfig.ID, "state", progress.State, "scope", progress.Scope,
		"reason", progress.Reason)
}

func (rec *Recorder) PodResetRequested(pods []workload.PodRef, err error) {
	rec.log.Info("pod reset requested", "pods", pods, "error", err)
}

func (rec *Recorder) ResetAllRequested(reason enginefault.ResetReason, pods []workload.PodRef) {
	rec.log.Info("group reset requested", "reason", reason, "pods", len(pods))
}

func (rec *Recorder) ReleaseRequested(pods []workload.PodRef, err error) {
	rec.log.Info("release requested", "pods", pods, "error", err)
}

func (rec *Recorder) SaveFailed(err error) {
	rec.log.Error(err, "journal save failed")
}

func (rec *Recorder) IncidentClosed(incident *enginefault.Incident) {
	rec.log.Info("incident closed", "incident", incident.ID, "outcome", incident.Outcome,
		"total", incident.Durations.Total)
}

func (rec *Recorder) WouldDo(from, to, reason string) {
	if to == "" {
		rec.log.Info("dry run: nothing to do any more", "was", from, "reason", reason)
		return
	}
	rec.log.Info("dry run: would "+to, "reason", reason)
}

// Ready fails once the loop has stepped and then stopped stepping.
func (rec *Recorder) Ready() error {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if !rec.last.IsZero() && time.Since(rec.last) > rec.stale {
		return fmt.Errorf("no completed step for %s", time.Since(rec.last).Round(time.Second))
	}
	return nil
}
