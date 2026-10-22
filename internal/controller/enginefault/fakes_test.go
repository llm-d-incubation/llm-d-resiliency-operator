// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

//nolint:testpackage // Tests drive the clock through the unexported load.
package enginefault

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/llm-d/llm-d-resiliency-operator/internal/engine"
	"github.com/llm-d/llm-d-resiliency-operator/internal/journal"
	"github.com/llm-d/llm-d-resiliency-operator/internal/workload"
)

type fakeWorkload struct {
	group       workload.Group
	discoverErr error
	resetErr    error
	resetFor    func([]workload.PodRef) error
	releaseErr  error
	discovers   int
	resets      []workload.PodRef
	resetCalls  [][]workload.PodRef
	resetAll    [][]workload.PodRef
	releases    [][]workload.PodRef
}

func (fake *fakeWorkload) Discover(context.Context) (workload.Group, error) {
	fake.discovers++
	group := fake.group
	group.Pods = slices.Clone(fake.group.Pods)
	return group, fake.discoverErr
}

func (fake *fakeWorkload) Reset(_ context.Context, pods []workload.PodRef) error {
	fake.resetCalls = append(fake.resetCalls, slices.Clone(pods))
	err := fake.resetErr
	if fake.resetFor != nil {
		err = fake.resetFor(pods)
	}
	fake.resets = append(fake.resets, pods...)
	return err
}

func (fake *fakeWorkload) ResetAll(_ context.Context, pods []workload.PodRef) error {
	fake.resetAll = append(fake.resetAll, pods)
	return nil
}

func (fake *fakeWorkload) Release(_ context.Context, pods []workload.PodRef) error {
	fake.releases = append(fake.releases, pods)
	return fake.releaseErr
}

type fakeEngine struct {
	grow          bool
	members       []engine.Member
	plan          engine.Action
	planFunc      func(engine.DecisionInput) engine.Action
	input         engine.DecisionInput
	progress      engine.Report
	applyErr      error
	verifyErr     error
	hangVerify    bool
	probes        int
	applied       []engine.Reconfig
	appliedOn     []engine.Observation
	verified      [][]engine.MemberID
	admitted      []engine.MemberID
	candidates    []engine.MemberID
	onApply       func()
	applyFunc     func(engine.ApplyInput) (engine.Report, error)
	initialReport *engine.Report
	applyCalls    []engine.ApplyInput
}

func (fake *fakeEngine) Capabilities() engine.Capabilities {
	return engine.Capabilities{Grow: fake.grow}
}

func (fake *fakeEngine) Observe(_ context.Context, pods []workload.Pod) engine.Observation {
	var obs engine.Observation
	for index := range fake.members {
		member := fake.members[index]
		if slices.ContainsFunc(pods, func(pod workload.Pod) bool {
			return pod.UID == member.Pod.UID && pod.Identity == member.PodIdentity && pod.State == workload.PodReady
		}) {
			obs.Members = append(obs.Members, member)
		}
	}
	return obs
}

func (fake *fakeEngine) Probe(context.Context, engine.Observation) error {
	fake.probes++
	return nil
}

//nolint:gocritic // The interface passes ApplyInput by value.
func (fake *fakeEngine) Apply(ctx context.Context, input engine.ApplyInput) (engine.Report, error) {
	fake.applyCalls = append(fake.applyCalls, input)
	if fake.applyFunc != nil {
		return fake.applyFunc(input)
	}
	if !input.MayAdvance {
		if input.Operation.Action.Kind == engine.Change && fake.progress.State != "" {
			return fake.progress, nil
		}
		return engine.Report{State: engine.Running}, nil
	}
	switch input.Operation.Action.Kind {
	case engine.Probe:
		fake.probes++
		return engine.Report{State: engine.Succeeded}, nil
	case engine.Verify:
		err := fake.Verify(ctx, input.Observation, input.Operation.Action.Participants)
		if err != nil {
			//nolint:nilerr // The report carries the confirmed failure.
			return engine.Report{State: engine.Failed, Reason: err.Error()}, nil
		}
		return engine.Report{State: engine.Succeeded}, nil
	}
	if !input.Operation.Attempted {
		if fake.onApply != nil {
			fake.onApply()
		}
		fake.applied = append(fake.applied, input.Operation.Reconfig())
		fake.appliedOn = append(fake.appliedOn, input.Observation)
		if fake.initialReport != nil {
			return *fake.initialReport, nil
		}
		return engine.Report{State: engine.Running, Checkpoint: json.RawMessage(`{"submitted":true}`)}, fake.applyErr
	}
	if fake.progress.State == "" {
		return engine.Report{State: engine.Running}, nil
	}
	return fake.progress, fake.applyErr
}

func (fake *fakeEngine) Verify(ctx context.Context, _ engine.Observation, members []engine.MemberID) error {
	fake.verified = append(fake.verified, slices.Clone(members))
	if fake.hangVerify {
		<-ctx.Done()
		return ctx.Err()
	}
	return fake.verifyErr
}

//nolint:gocritic // The policy contract passes DecisionInput by value.
func (fake *fakeEngine) Decide(input engine.DecisionInput) engine.Action {
	fake.admitted, fake.candidates, fake.input = input.Admitted, input.Candidates, input
	if fake.planFunc != nil {
		return fake.planFunc(input)
	}
	return fake.plan
}

type fakeJournal struct {
	data     []byte
	version  int
	loadErrs []error
	saveErrs []error
	loads    int
	saves    int
}

func (fake *fakeJournal) current() string {
	if fake.data == nil && fake.version == 0 {
		return ""
	}
	if fake.version == 0 {
		fake.version = 1
	}
	return strconv.Itoa(fake.version)
}

//nolint:gocritic // The contract returns the record, its version and an error.
func (fake *fakeJournal) Load(context.Context) ([]byte, string, error) {
	fake.loads++
	if len(fake.loadErrs) > 0 {
		err := fake.loadErrs[0]
		fake.loadErrs = fake.loadErrs[1:]
		return nil, "", err
	}
	return slices.Clone(fake.data), fake.current(), nil
}

func (fake *fakeJournal) Save(_ context.Context, data []byte, version string) (string, error) {
	if len(fake.saveErrs) > 0 {
		err := fake.saveErrs[0]
		fake.saveErrs = fake.saveErrs[1:]
		if err != nil {
			return "", err
		}
	}
	if version != fake.current() {
		return "", journal.ErrConflict
	}
	fake.saves++
	fake.version++
	fake.data = slices.Clone(data)
	return strconv.Itoa(fake.version), nil
}

func (fake *fakeJournal) remove() { fake.data, fake.version = nil, 0 }

type fakeRecorder struct {
	calls  []string
	closed *Incident
}

func (fake *fakeRecorder) add(format string, args ...any) {
	fake.calls = append(fake.calls, fmt.Sprintf(format, args...))
}

func (*fakeRecorder) Stepped(*Record, error) {}

func (fake *fakeRecorder) WouldDo(_, to, _ string) { fake.add("WouldDo %s", to) }

func (fake *fakeRecorder) PhaseChanged(from, to Phase, _ string) {
	fake.add("PhaseChanged %s->%s", from, to)
}

func (fake *fakeRecorder) IncidentOpened(incident *Incident) {
	fake.add("IncidentOpened %v %v", incident.Affected, incident.Nodes)
}

func (fake *fakeRecorder) ReconfigStarted(reconfig *engine.Reconfig, _ *engine.Action) {
	fake.add("ReconfigStarted remove=%v add=%v", reconfig.Remove, reconfig.Add)
}

func (fake *fakeRecorder) ReconfigFinished(_ *engine.Reconfig, progress engine.Report) {
	fake.add("ReconfigFinished %s", progress.State)
}

func (fake *fakeRecorder) PodResetRequested(pods []workload.PodRef, _ error) {
	names := make([]string, 0, len(pods))
	for _, pod := range pods {
		names = append(names, pod.Name)
	}
	fake.add("PodResetRequested %s", strings.Join(names, ","))
}

func (fake *fakeRecorder) ResetAllRequested(reason ResetReason, pods []workload.PodRef) {
	fake.add("ResetAllRequested %s %d", reason, len(pods))
}

func (fake *fakeRecorder) ReleaseRequested(pods []workload.PodRef, _ error) {
	fake.add("ReleaseRequested %d", len(pods))
}

func (fake *fakeRecorder) SaveFailed(error) { fake.add("SaveFailed") }

func (fake *fakeRecorder) IncidentClosed(incident *Incident) {
	fake.closed = incident
	fake.add("IncidentClosed %s", incident.Outcome)
}

func (*fakeRecorder) Ready() error { return nil }

type harness struct {
	t      *testing.T
	wl     *fakeWorkload
	eng    *fakeEngine
	store  *fakeJournal
	rec    *fakeRecorder
	config Config
	clock  time.Time
	fsm    *controller
	before []engine.Member
}

var names = []string{"lws-0", "lws-0-1", "lws-0-2", "lws-0-3"}

func ids(list ...string) []engine.MemberID {
	result := make([]engine.MemberID, 0, len(list))
	for _, name := range list {
		result = append(result, engine.MemberID(name))
	}
	return result
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	test := &harness{
		t: t, wl: &fakeWorkload{}, eng: &fakeEngine{grow: true, plan: engine.Action{Kind: engine.Healthy}},
		store: &fakeJournal{}, rec: &fakeRecorder{}, config: Defaults(),
		clock: time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC),
	}
	for index, name := range names {
		letter := string(rune('A' + index))
		uid, identity := string(rune('a'+index))+"1", "id"+letter
		test.wl.group.Pods = append(test.wl.group.Pods, workload.Pod{
			Name: name, UID: uid, IP: fmt.Sprintf("10.0.0.%d", index), Index: index, Identity: identity,
			Node: fmt.Sprintf("gpu-node-%d", index+1), State: workload.PodReady,
		})
		test.eng.members = append(test.eng.members, engine.Member{
			Pod: workload.PodRef{Name: name, UID: uid}, PodIdentity: identity, Name: fmt.Sprintf("dp%d", index), Serving: true,
		})
	}
	return test
}

func (test *harness) load() {
	test.t.Helper()
	fsm, err := load(test.t.Context(), test.wl, test.eng, test.store, test.rec, test.config, func() time.Time { return test.clock })
	if err != nil {
		test.t.Fatal(err)
	}
	test.fsm = fsm
}

func (test *harness) pollErr() error {
	test.clock = test.clock.Add(test.config.PollInterval)
	return test.fsm.Step(test.t.Context())
}

func (test *harness) pollAnyway() {
	if err := test.pollErr(); err != nil {
		test.t.Log(err)
	}
}

func (test *harness) poll() {
	test.t.Helper()
	if err := test.pollErr(); err != nil {
		test.t.Fatal(err)
	}
}

func (test *harness) serving() {
	test.t.Helper()
	test.load()
	test.poll()
	if record := test.record(); record.Phase != Serving || len(record.Members) != 4 {
		test.t.Fatalf("not serving: %+v", record)
	}
	test.rec.calls = nil
}

func (test *harness) record() Record {
	test.t.Helper()
	var record Record
	if err := json.Unmarshal(test.store.data, &record); err != nil {
		test.t.Fatal(err)
	}
	return record
}

func (test *harness) member(name string) *engine.Member {
	index := slices.IndexFunc(test.eng.members, func(member engine.Member) bool { return member.Name == name })
	return &test.eng.members[index]
}

func (test *harness) dead() {
	for index := range test.eng.members {
		test.eng.members[index].Serving = false
	}
	test.member("dp3").Terminal = true
}

func (test *harness) shrinkPlan() engine.Action {
	return engine.Action{
		Kind: engine.Change, Participants: ids("dp0@idA", "dp1@idB", "dp2@idC"), Remove: ids("dp3@idD"), Reason: "dp3 is dead",
	}
}

func (test *harness) wantCalls(want ...string) {
	test.t.Helper()
	if !slices.Equal(test.rec.calls, want) {
		test.t.Fatalf("recorder calls:\n got %q\nwant %q", test.rec.calls, want)
	}
	test.rec.calls = nil
}
