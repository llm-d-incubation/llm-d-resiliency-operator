// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0
//
//nolint:testpackage // Inspect persisted phase transitions and inject precise deadlines.
package enginefault

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/llm-d/llm-d-resiliency-operator/internal/engine"
)

func statuses(states ...string) []engine.RankStatus {
	r := make([]engine.RankStatus, 0, len(states))
	for i, s := range states {
		r = append(r, engine.RankStatus{Rank: i, Status: s})
	}
	return r
}

func TestFaultDecisionsPreserveOriginalMembership(t *testing.T) {
	tests := []struct {
		name         string
		states       []engine.RankStatus
		excluded     []int
		action       string
		removed      []int
		participants []int
	}{
		{"transient", statuses("unhealthy", "unhealthy", "unhealthy", "unhealthy"), nil, "retry", nil, []int{0, 1, 2, 3}},
		{"worker death", statuses("unhealthy", "dead", "unhealthy", "unhealthy"), nil, "scale_down", []int{1}, []int{0, 2, 3}},
		{"multiple deaths", statuses("unhealthy", "dead", "unhealthy", "dead"), nil, "scale_down", []int{1, 3}, []int{0, 2}},
		{"repeated loss", statuses("unhealthy", "dead", "unhealthy", "unknown"), []int{3}, "scale_down", []int{1}, []int{0, 2}},
		{
			"single survivor delegates capacity to vLLM", statuses("unhealthy", "dead", "dead", "dead"), nil,
			"scale_down",
			[]int{1, 2, 3},
			[]int{0},
		},
		{"single survivor waits for diagnosis", statuses("healthy", "dead", "dead", "dead"), nil, "wait", nil, nil},
		{"no survivors", statuses("dead", "dead", "dead", "dead"), nil, "reset", nil, nil},
		{"no survivors after exclusion", statuses("unknown", "dead", "dead", "dead"), []int{0}, "reset", nil, nil},
		{"unknown is not lost capacity", statuses("healthy", "unknown", "unknown", "unknown"), nil, "frontend_degraded", nil, []int{0}},
		{"wait for peers", statuses("healthy", "dead", "unhealthy", "diagnosing"), nil, "wait", nil, nil},
		{"frontend only", statuses("healthy", "unknown", "healthy", "healthy"), nil, "frontend_degraded", nil, []int{0, 2, 3}},
		{"missing is not dead", statuses("unknown", "unknown", "unknown", "unknown"), nil, "wait", nil, nil},
		{"excluded stays out", statuses("healthy", "healthy", "healthy", "healthy"), []int{3}, "healthy", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := Decide(tt.states, tt.excluded)
			if d.Action != tt.action || !reflect.DeepEqual(d.Removed, tt.removed) || !reflect.DeepEqual(d.Participants, tt.participants) {
				t.Fatalf("unexpected decision: %+v", d)
			}
		})
	}
}

func TestHungRankMasksDoNotAuthorizeUnfencedExclusion(t *testing.T) {
	s := statuses("unhealthy", "healthy", "unhealthy", "unhealthy")
	for _, i := range []int{0, 2, 3} {
		s[i].Mask = []int{0, 1, 0, 0}
	}
	if d := Decide(s, nil); d.Action != "wait" {
		t.Fatalf("mask-only diagnosis authorized unsafe exclusion: %+v", d)
	}
	controller, adapter, workload := setup(t)
	adapter.status = s
	step(t, controller)
	state := controller.Snapshot()
	state.Since = time.Now().Add(-2 * time.Minute)
	controller.set(state)
	step(t, controller)
	if len(adapter.dispatches) != 0 || len(workload.resets) != 1 {
		t.Fatal("unfenced stall must reset after bounded diagnosis")
	}
}

func TestSingleSurvivorRecoveryUsesRuntimeResult(t *testing.T) {
	for _, outcome := range []string{"success", "dispatch rejected", "capacity failure"} {
		t.Run(outcome, func(t *testing.T) {
			controller, adapter, workload := setup(t)
			adapter.status = statuses("unhealthy", "dead", "dead", "dead")
			if outcome == "dispatch rejected" {
				adapter.dispatchErr = errors.New("runtime rejected scale_down")
			}
			step(t, controller)
			ageDiagnosis(controller)
			step(t, controller)
			if len(adapter.dispatches) != 1 || !slices.Equal(adapter.dispatches[0].Participants, []int{0}) ||
				!slices.Equal(adapter.dispatches[0].Removed, []int{1, 2, 3}) {
				t.Fatal("controller did not delegate single-survivor recovery to vLLM")
			}
			switch outcome {
			case "capacity failure":
				adapter.status[0].FTState = "failed"
				adapter.status[0].FTError = "EPLB redundancy insufficient"
				step(t, controller)
			case "success":
				adapter.status[0].Status = "healthy"
				step(t, controller)
			}
			state := controller.Snapshot()
			if outcome == "success" {
				if state.Phase != "serving" || !slices.Equal(state.Verified, []int{0}) ||
					!slices.Equal(state.Excluded, []int{1, 2, 3}) || len(workload.resets) != 0 {
					t.Fatalf("successful runtime recovery was not verified: %+v", state)
				}
			} else if state.Phase != "resetting" || len(workload.resets) != 1 || len(state.Verified) != 0 {
				t.Fatalf("runtime failure did not request reset: %+v", state)
			}
		})
	}
}

func TestMissingFrontendWaitsForEngineDiagnosisBeforeReset(t *testing.T) {
	controller, adapter, workload := setup(t)
	adapter.status = statuses("healthy", "unknown", "healthy", "healthy")
	adapter.verifyErr = errors.New("collective has not completed")
	step(t, controller)
	state := controller.Snapshot()
	if state.Phase != "diagnosing" || len(state.Verified) != 0 || len(workload.resets) != 0 {
		t.Fatalf("frontend uncertainty caused premature reset or verification: %+v", state)
	}
	step(t, controller)
	if !controller.Snapshot().Since.Equal(state.Since) {
		t.Fatal("repeated verification restarted the diagnosis deadline")
	}
	adapter.status = statuses("unhealthy", "dead", "unhealthy", "unhealthy")
	adapter.verifyErr = nil
	ageDiagnosis(controller)
	step(t, controller)
	if controller.Snapshot().Instruction != "scale_down" || len(adapter.dispatches) != 1 || len(workload.resets) != 0 {
		t.Fatal("diagnosed engine loss did not proceed to FT")
	}
}

func TestUnresolvedFrontendFaultEventuallyResets(t *testing.T) {
	controller, adapter, workload := setup(t)
	adapter.status = statuses("healthy", "unknown", "healthy", "healthy")
	adapter.verifyErr = errors.New("collective stalled")
	step(t, controller)
	state := controller.Snapshot()
	state.Since = time.Now().Add(-2 * time.Minute)
	controller.set(state)
	step(t, controller)
	if controller.Snapshot().Phase != "resetting" || len(workload.resets) != 1 {
		t.Fatal("unresolved inference failure did not reach bounded reset")
	}
}

func TestIdlePeersReceiveOneDiagnosisProbeWithoutMarkingRecoveryVerified(t *testing.T) {
	controller, adapter, workload := setup(t)
	adapter.status = statuses("healthy", "dead", "healthy", "healthy")
	adapter.verifyErr = errors.New("peer collective timed out")
	step(t, controller)
	step(t, controller)
	if adapter.verifications != 2 || controller.Snapshot().Phase != "diagnosing" ||
		len(controller.Snapshot().Verified) != 0 || len(workload.resets) != 0 {
		t.Fatal("idle diagnosis probe was repeated, marked recovery verified, or forced an early reset")
	}
	adapter.status = statuses("unhealthy", "dead", "unhealthy", "unhealthy")
	ageDiagnosis(controller)
	step(t, controller)
	if len(adapter.dispatches) != 1 {
		t.Fatal("did not dispatch FT after peer diagnosis")
	}
}

type fakeAdapter struct {
	engine.Adapter
	status                 []engine.RankStatus
	dispatches             []engine.Round
	verifyErr, dispatchErr error
	verifications          int
	onVerify               func()
}

func (f *fakeAdapter) EngineStatus(context.Context, engine.Group) []engine.RankStatus {
	return f.status
}

//nolint:gocritic // Implements the immutable round value required by the adapter interface.
func (f *fakeAdapter) ResumeEngine(_ context.Context, _ engine.Group, r engine.Round) ([]engine.Acceptance, error) {
	f.dispatches = append(f.dispatches, r)
	return nil, f.dispatchErr
}

//nolint:gocritic // Implements the immutable round value required by the adapter interface.
func (f *fakeAdapter) ScaleDown(ctx context.Context, g engine.Group, r engine.Round) ([]engine.Acceptance, error) {
	return f.ResumeEngine(ctx, g, r)
}

func (f *fakeAdapter) Verify(context.Context, engine.Group, []int) error {
	f.verifications++
	if f.onVerify != nil {
		f.onVerify()
	}
	return f.verifyErr
}

type fakeWorkload struct {
	group          engine.Group
	saved          []byte
	saveErr        error
	discoverErrors []error
	resets         []engine.Group
}

func (f *fakeWorkload) Discover(context.Context) (engine.Group, error) {
	if len(f.discoverErrors) > 0 {
		err := f.discoverErrors[0]
		f.discoverErrors = f.discoverErrors[1:]
		if err != nil {
			return engine.Group{}, err
		}
	}

	return f.group, nil
}

func (f *fakeWorkload) Reset(_ context.Context, g engine.Group) error {
	f.resets = append(f.resets, g)
	return nil
}
func (f *fakeWorkload) Load(context.Context) ([]byte, error) { return f.saved, nil }
func (f *fakeWorkload) Save(_ context.Context, b []byte) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	f.saved = slices.Clone(b)
	return nil
}

func setup(t *testing.T) (*Controller, *fakeAdapter, *fakeWorkload) {
	t.Helper()
	g := engine.Group{ID: "original", Endpoints: []engine.Endpoint{
		{Rank: 0, URL: "http://a:8000", Pod: "test-0", PodUID: "a"},
		{Rank: 1, URL: "http://a:8001", Pod: "test-0", PodUID: "a"},
		{Rank: 2, URL: "http://b:8000", Pod: "test-0-1", PodUID: "b"},
		{Rank: 3, URL: "http://b:8001", Pod: "test-0-1", PodUID: "b"},
	}}
	adapter := &fakeAdapter{status: statuses("healthy", "healthy", "healthy", "healthy")}
	workload := &fakeWorkload{group: g}
	controller, err := New(context.Background(), adapter, workload, Defaults())
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	return controller, adapter, workload
}

func ageDiagnosis(controller *Controller) {
	s := controller.Snapshot()
	s.Since = time.Now().Add(-5 * time.Second)
	controller.set(s)
}

func TestScaleDownRequiresInferenceBeforeReportingRecovery(t *testing.T) {
	controller, adapter, workload := setup(t)
	adapter.status = statuses("unhealthy", "dead", "unhealthy", "unhealthy")
	if err := controller.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(controller.Snapshot().Verified) != 0 {
		t.Fatal("diagnosing group was reported as verified")
	}
	ageDiagnosis(controller)
	if err := controller.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := controller.Snapshot()
	if s.Phase != "applying" || len(adapter.dispatches) != 1 || len(s.Verified) != 0 {
		t.Fatalf("bad dispatch: %+v", s)
	}
	adapter.status = statuses("healthy", "dead", "healthy", "healthy")
	if err := controller.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	s = controller.Snapshot()
	if s.Phase != "serving" || !slices.Equal(s.Excluded, []int{1}) ||
		!slices.Equal(s.Verified, []int{0, 2, 3}) || adapter.verifications != 2 || len(workload.resets) != 0 {
		t.Fatalf("bad completion: %+v", s)
	}
	// An excluded endpoint returning healthy violates the native terminal-death contract.
	adapter.status = statuses("healthy", "healthy", "healthy", "healthy")
	step(t, controller)
	if controller.Snapshot().Phase != "resetting" || len(workload.resets) != 1 {
		t.Fatal("revived excluded rank did not reset the inconsistent group")
	}
}

func TestExcludedRankRevivingDuringVerificationResetsGroup(t *testing.T) {
	controller, adapter, workload := setup(t)
	adapter.status = statuses("unhealthy", "dead", "unhealthy", "unhealthy")
	step(t, controller)
	ageDiagnosis(controller)
	step(t, controller)
	adapter.status = statuses("healthy", "dead", "healthy", "healthy")
	adapter.onVerify = func() {
		adapter.status = statuses("healthy", "healthy", "healthy", "healthy")
	}
	step(t, controller)
	state := controller.Snapshot()
	if state.Phase != "resetting" || len(state.Verified) != 0 || len(workload.resets) != 1 {
		t.Fatal("verification reported recovery after an excluded frontend revived")
	}
}

func TestInFlightRecoveryCannotCompleteRound(t *testing.T) {
	controller, adapter, workload := setup(t)
	adapter.status = statuses("unhealthy", "dead", "unhealthy", "unhealthy")
	step(t, controller)
	ageDiagnosis(controller)
	step(t, controller)
	adapter.status = statuses("healthy", "dead", "healthy", "healthy")
	adapter.status[0].FTState = "recovering"
	step(t, controller)
	if controller.Snapshot().Phase != "applying" || adapter.verifications != 1 || len(workload.resets) != 0 {
		t.Fatal("stale healthy status completed an in-flight FT operation")
	}
	adapter.status[0].FTState = ""
	step(t, controller)
	if controller.Snapshot().Phase != "serving" || adapter.verifications != 2 {
		t.Fatal("completed operation did not proceed to inference verification")
	}
}

func TestPersistedDispatchIsNeverReplayedAfterRestart(t *testing.T) {
	controller, adapter, workload := setup(t)
	adapter.status = statuses("unhealthy", "dead", "unhealthy", "unhealthy")
	step(t, controller)
	ageDiagnosis(controller)
	step(t, controller)
	before := controller.Snapshot().Round

	replacement, err := New(context.Background(), adapter, workload, Defaults())
	if err != nil {
		t.Fatal(err)
	}
	if err := replacement.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(adapter.dispatches) != 1 || replacement.Snapshot().Round.ID != before.ID {
		t.Fatal("replacement duplicated the collective command")
	}
	adapter.status = statuses("healthy", "dead", "healthy", "healthy")
	step(t, replacement)
	if replacement.Snapshot().Phase != "serving" {
		t.Fatal("did not reconcile completed round")
	}
}

func TestRemovingCurrentStoreMasterSuppliesReplacementAfterEarlierExclusion(t *testing.T) {
	controller, adapter, _ := setup(t)
	state := controller.Snapshot()
	state.Excluded = []int{0}
	state.Verified = []int{1, 2, 3}
	controller.set(state)
	adapter.status = statuses("unknown", "dead", "unhealthy", "unhealthy")
	step(t, controller)
	ageDiagnosis(controller)
	step(t, controller)
	round := controller.Snapshot().Round
	if !slices.Equal(round.Participants, []int{2, 3}) || !slices.Equal(round.Removed, []int{1}) ||
		round.MasterIP != "b" || round.StorePort != controller.Config.StorePort {
		t.Fatalf("missing replacement for current master rank 1: %+v", round)
	}
}

func TestSavedRetryRoundReconcilesWithoutReplay(t *testing.T) {
	controller, adapter, workload := setup(t)
	state := controller.Snapshot()
	state.Phase = "applying"
	state.Instruction = "retry"
	state.Round = engine.Round{ID: "saved", GroupID: state.Group.ID, Participants: []int{0, 1, 2, 3}}
	if err := controller.persist(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	replacement, err := New(context.Background(), adapter, workload, Defaults())
	if err != nil {
		t.Fatal(err)
	}
	step(t, replacement)
	if replacement.Snapshot().Phase != "serving" || len(workload.resets) != 0 ||
		len(adapter.dispatches) != 0 || adapter.verifications != 2 {
		t.Fatal("legacy retry-only round did not reconcile without replay")
	}
}

func TestLegacyExclusionsDoNotResetReplacementGroup(t *testing.T) {
	controller, adapter, workload := setup(t)
	state := controller.Snapshot()
	state.Excluded = []int{1}
	if err := controller.persist(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	workload.group.ID = "replacement"
	workload.group.Endpoints = slices.Clone(workload.group.Endpoints)
	for index := range workload.group.Endpoints {
		workload.group.Endpoints[index].PodUID += "-new"
	}
	step(t, controller)
	if controller.Snapshot().Phase != "serving" || len(controller.Snapshot().Excluded) != 0 ||
		len(workload.resets) != 0 || adapter.verifications != 2 {
		t.Fatal("legacy exclusions reset a replacement with new identities")
	}
}

func TestDiscoveryFailurePreservesFaultReconciliation(t *testing.T) {
	for _, phase := range []string{"diagnosing", "applying"} {
		t.Run(phase, func(t *testing.T) {
			controller, adapter, workload := setup(t)
			adapter.status = statuses("unhealthy", "dead", "unhealthy", "unhealthy")
			step(t, controller)
			ageDiagnosis(controller)
			if phase == "applying" {
				step(t, controller)
			}
			before := controller.Snapshot()
			workload.discoverErrors = []error{errors.New("temporary Kubernetes read failure")}
			step(t, controller)
			after := controller.Snapshot()
			if after.Phase != phase || !after.Since.Equal(before.Since) ||
				!reflect.DeepEqual(after.Round, before.Round) || len(after.Verified) != 0 || len(workload.resets) != 0 {
				t.Fatalf("discovery uncertainty changed fault reconciliation: %+v", after)
			}
			// The saved uncertainty must also preserve the round across a restart.
			replacement, err := New(context.Background(), adapter, workload, Defaults())
			if err != nil {
				t.Fatal(err)
			}
			if phase == "applying" {
				adapter.status = statuses("healthy", "dead", "healthy", "healthy")
			}
			step(t, replacement)
			want := "applying"
			if phase == "applying" {
				want = "serving"
			}
			if replacement.Snapshot().Phase != want || len(adapter.dispatches) != 1 {
				t.Fatalf("unchanged group did not resume without replay: %+v", replacement.Snapshot())
			}
		})
	}
}

func TestDiscoveryFailureDuringVerificationPreservesRound(t *testing.T) {
	controller, adapter, workload := setup(t)
	adapter.status = statuses("unhealthy", "dead", "unhealthy", "unhealthy")
	step(t, controller)
	ageDiagnosis(controller)
	step(t, controller)
	before := controller.Snapshot()
	adapter.status = statuses("healthy", "dead", "healthy", "healthy")
	workload.discoverErrors = []error{nil, errors.New("post-inference Kubernetes read failure")}
	step(t, controller)
	after := controller.Snapshot()
	if after.Phase != "applying" || !after.Since.Equal(before.Since) ||
		!reflect.DeepEqual(after.Round, before.Round) || len(after.Verified) != 0 {
		t.Fatalf("verification read failure discarded round: %+v", after)
	}
	step(t, controller)
	if controller.Snapshot().Phase != "serving" || len(adapter.dispatches) != 1 ||
		!slices.Equal(controller.Snapshot().Excluded, []int{1}) {
		t.Fatal("completed round did not reverify after discovery resumed")
	}
}

func TestDiscoveryFailureDoesNotExtendFTDeadline(t *testing.T) {
	controller, adapter, workload := setup(t)
	adapter.status = statuses("unhealthy", "dead", "unhealthy", "unhealthy")
	step(t, controller)
	ageDiagnosis(controller)
	step(t, controller)
	state := controller.Snapshot()
	state.Since = time.Now().Add(-2 * controller.Config.ApplyTimeout)
	controller.set(state)
	workload.discoverErrors = []error{errors.New("temporary Kubernetes read failure")}
	step(t, controller)
	step(t, controller)
	if controller.Snapshot().Phase != "resetting" || len(workload.resets) != 1 || len(adapter.dispatches) != 1 {
		t.Fatal("discovery failure suppressed the expired FT deadline")
	}
}

func TestDiscoveryFailureInvalidatesVerificationBeforePersistence(t *testing.T) {
	controller, adapter, workload := setup(t)
	workload.discoverErrors = []error{errors.New("temporary Kubernetes read failure")}
	workload.saveErr = errors.New("Kubernetes state write also unavailable")
	if err := controller.Step(context.Background()); err == nil {
		t.Fatal("state persistence failure was hidden")
	}
	state := controller.Snapshot()
	if state.Phase != "serving" || len(state.Verified) != 0 || state.DiscoveryUnavailableSince.IsZero() {
		t.Fatalf("uncertain identity left verification valid or changed phase: %+v", state)
	}
	workload.saveErr = nil
	step(t, controller)
	state = controller.Snapshot()
	if len(state.Verified) != 4 || adapter.verifications != 2 || !state.DiscoveryUnavailableSince.IsZero() {
		t.Fatal("same healthy group was not reverified after discovery resumed")
	}
}

func TestProlongedDiscoveryUncertaintyEscalatesWithoutDeletingUnknownWorkload(t *testing.T) {
	controller, adapter, workload := setup(t)
	state := controller.Snapshot()
	state.DiscoveryUnavailableSince = time.Now().Add(-2 * controller.Config.StartupTimeout)
	controller.set(state)
	workload.discoverErrors = []error{errors.New("Kubernetes discovery still unavailable")}
	step(t, controller)
	if controller.Snapshot().Phase != "intervention_required" || len(workload.resets) != 0 ||
		len(adapter.dispatches) != 0 || len(controller.Snapshot().Verified) != 0 {
		t.Fatal("unbounded discovery uncertainty or action against an unconfirmed workload")
	}
}

func TestRepeatedPostVerificationReadFailureKeepsUncertaintyDeadline(t *testing.T) {
	controller, _, workload := setup(t)
	state := controller.Snapshot()
	state.Verified = nil
	state.DiscoveryUnavailableSince = time.Now().Add(-2 * controller.Config.StartupTimeout)
	controller.set(state)
	// A successful initial read does not resolve the uncertainty when the
	// identity check after inference keeps failing.
	workload.discoverErrors = []error{nil, errors.New("post-inference Kubernetes read failure")}
	step(t, controller)
	if controller.Snapshot().Phase != "intervention_required" || len(workload.resets) != 0 ||
		len(controller.Snapshot().Verified) != 0 {
		t.Fatal("successful preliminary read restarted an unresolved verification deadline")
	}
}

func TestPersistenceFailurePreventsDispatch(t *testing.T) {
	controller, adapter, workload := setup(t)
	adapter.status = statuses("unhealthy", "dead", "unhealthy", "unhealthy")
	step(t, controller)
	ageDiagnosis(controller)
	workload.saveErr = errors.New("storage unavailable")
	if err := controller.Step(context.Background()); err == nil {
		t.Fatal("persistence failure ignored")
	}
	if len(adapter.dispatches) != 0 || len(controller.Snapshot().Verified) != 0 {
		t.Fatal("dispatched or verified without durable intent")
	}
}

func TestFailureAndTimeoutHandOffToReset(t *testing.T) {
	for _, kind := range []string{"rejected", "failed", "deadline", "inference"} {
		t.Run(kind, func(t *testing.T) {
			controller, adapter, workload := setup(t)
			adapter.status = statuses("unhealthy", "dead", "unhealthy", "unhealthy")
			step(t, controller)
			ageDiagnosis(controller)
			if kind == "rejected" {
				adapter.dispatchErr = errors.New("HTTP rejection")
			}
			step(t, controller)
			s := controller.Snapshot()
			switch kind {
			case "failed":
				adapter.status[0].FTState = "failed"
				adapter.status[0].FTError = "collective failed"
			case "deadline":
				s.Since = time.Now().Add(-2 * time.Minute)
				controller.set(s)
			case "inference":
				adapter.status = statuses("healthy", "dead", "healthy", "healthy")
				adapter.verifyErr = errors.New("no inference")
			}
			if kind != "rejected" {
				step(t, controller)
			}
			if controller.Snapshot().Phase != "resetting" || len(workload.resets) != 1 || len(controller.Snapshot().Verified) != 0 {
				t.Fatalf("failure did not reset: %+v", controller.Snapshot())
			}
			if workload.resets[0].ID != "original" {
				t.Fatal("reset targeted a new group")
			}
		})
	}
}

func TestObserverEvidenceCannotVerifyAStaleHealthyEndpoint(t *testing.T) {
	controller, adapter, workload := setup(t)
	adapter.status[1].ProcessExitConfirmed = true
	step(t, controller)
	if controller.Snapshot().Phase != "diagnosing" || len(controller.Snapshot().Verified) != 0 {
		t.Fatal("stale healthy endpoint remained verified")
	}
	state := controller.Snapshot()
	state.Since = time.Now().Add(-2 * time.Minute)
	controller.set(state)
	step(t, controller)
	if len(adapter.dispatches) != 0 || len(workload.resets) != 1 {
		t.Fatal("observer-only evidence must reach bounded reset without scale_down")
	}
}

func TestRevivedRemovedRankResetsDuringApplyAndAfterCoordinatorRestart(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprint(restart), func(t *testing.T) {
			controller, adapter, workload := setup(t)
			adapter.status = statuses("unhealthy", "dead", "unhealthy", "unhealthy")
			step(t, controller)
			ageDiagnosis(controller)
			step(t, controller)
			if restart {
				var err error
				controller, err = New(context.Background(), adapter, workload, Defaults())
				if err != nil {
					t.Fatal(err)
				}
			}
			adapter.status = statuses("healthy", "healthy", "healthy", "healthy")
			step(t, controller)
			if controller.Snapshot().Phase != "resetting" || len(workload.resets) != 1 || len(adapter.dispatches) != 1 {
				t.Fatal("unsafe round completed or replayed after removed endpoint revived")
			}
		})
	}
}

func step(t *testing.T, controller *Controller) {
	t.Helper()
	if err := controller.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
}
