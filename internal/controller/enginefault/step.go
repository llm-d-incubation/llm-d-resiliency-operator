// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

package enginefault

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/llm-d/llm-d-resiliency-operator/internal/engine"
	"github.com/llm-d/llm-d-resiliency-operator/internal/journal"
	"github.com/llm-d/llm-d-resiliency-operator/internal/workload"
)

type controller struct {
	wl             workload.Workload
	eng            engine.Engine
	store          journal.Journal
	recorder       Recorder
	config         Config
	now            func() time.Time
	saved          []byte
	version        string
	pending        []func()
	lastStepFailed bool
	wouldDo        string
}

type view struct {
	group               *workload.Group
	obs                 engine.Observation
	admitted            []engine.MemberID
	candidates          []engine.MemberID
	members             map[engine.MemberID]*engine.Member
	unknown             []engine.MemberID
	read                int
	observationTimedOut bool
}

//nolint:gocritic // Configuration is copied into each controller.
func New(ctx context.Context, wl workload.Workload, eng engine.Engine, store journal.Journal,
	recorder Recorder, config Config,
) (Controller, error) {
	fsm, err := load(ctx, wl, eng, store, recorder, config, time.Now)
	if err != nil {
		return nil, err
	}
	return fsm, nil
}

//nolint:gocritic // Configuration is copied into each controller.
func load(ctx context.Context, wl workload.Workload, eng engine.Engine, store journal.Journal,
	recorder Recorder, config Config, now func() time.Time,
) (*controller, error) {
	data, version, err := store.Load(ctx)
	if err != nil {
		return nil, err
	}
	record, err := decodeRecord(data, now().UTC())
	if err != nil {
		return nil, err
	}

	record.Budget.StableSince = time.Time{}
	saved, err := json.Marshal(&record)
	if err != nil {
		return nil, err
	}
	return &controller{
		wl: wl, eng: eng, store: store, recorder: recorder, config: config, now: now, saved: saved, version: version,
	}, nil
}

func decodeRecord(data []byte, now time.Time) (Record, error) {
	if len(data) == 0 {
		return Record{Version: 2, Phase: Starting, Since: now, Epoch: now, Reason: "new group"}, nil
	}
	var record Record
	if err := json.Unmarshal(data, &record); err != nil {
		return Record{}, fmt.Errorf("invalid journal record: %w", err)
	}
	if record.Version != 2 {
		return Record{}, fmt.Errorf("unsupported journal version %d; refusing to discard recovery state", record.Version)
	}
	if !slices.Contains([]Phase{Starting, Serving, Recovering}, record.Phase) || record.Epoch.IsZero() {
		return Record{}, errors.New("invalid journal phase or epoch")
	}
	if record.Operation != nil && (record.Operation.ID == "" || record.OperationSince.IsZero()) {
		return Record{}, errors.New("invalid saved operation identity or start time")
	}
	if record.Workload != nil && (record.Operation == nil || !externalAction(record.Operation.Action.Kind)) {
		return Record{}, errors.New("workload action has no matching engine requirement")
	}
	if record.Operation == nil && (record.OperationBlocked != "" || !record.OperationSince.IsZero()) {
		return Record{}, errors.New("operation state has no saved operation")
	}
	budget := record.Budget
	if budget.Actions < 0 ||
		budget.Started.IsZero() && (budget.Actions != 0 || !budget.StableSince.IsZero() || budget.Blocked != "") ||
		!budget.StableSince.IsZero() && budget.StableSince.Before(budget.Started) {
		return Record{}, errors.New("invalid recovery budget")
	}
	return record, nil
}

func (fsm *controller) Step(ctx context.Context) error {
	fsm.pending, fsm.wouldDo = nil, ""
	next := fsm.record()
	if fsm.lastStepFailed {
		fsm.interruptStability(&next)
	}
	err := fsm.step(ctx, &next)
	if err == nil {
		next.WouldDo = fsm.wouldDo
		err = fsm.save(ctx, &next)
	}
	fsm.lastStepFailed = err != nil
	current := fsm.record()
	fsm.recorder.Stepped(&current, err)
	return err
}

func (fsm *controller) record() Record {
	var record Record
	if err := json.Unmarshal(fsm.saved, &record); err != nil {
		panic(err)
	}
	return record
}

func (fsm *controller) save(ctx context.Context, next *Record) error {
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	changed, version := !bytes.Equal(data, fsm.saved), fsm.version
	if changed && !fsm.config.DryRun {
		version, err = fsm.store.Save(ctx, data, fsm.version)
		if err != nil {
			fsm.pending = nil
			fsm.recorder.SaveFailed(err)
			return fmt.Errorf("save journal: %w", err)
		}
	}
	if changed {
		previous := fsm.record()
		fsm.saved, fsm.version = data, version
		if previous.Phase != next.Phase {
			fsm.recorder.PhaseChanged(previous.Phase, next.Phase, next.Reason)
		}
		if previous.WouldDo != next.WouldDo {
			fsm.recorder.WouldDo(previous.WouldDo, next.WouldDo, next.Reason)
		}
	}
	for _, emit := range fsm.pending {
		emit()
	}
	fsm.pending = nil
	return nil
}

func (fsm *controller) later(emit func()) { fsm.pending = append(fsm.pending, emit) }

func (fsm *controller) step(ctx context.Context, next *Record) error {
	now := fsm.now().UTC()
	group, err := fsm.wl.Discover(ctx)
	if err != nil {
		return fsm.unseen(ctx, next, now, true, fmt.Errorf("discover: %w", err))
	}
	next.RecreateStopped = group.RecreateStopped
	if next.Operation != nil && next.Workload != nil {
		return fsm.watchPods(ctx, next, &group, now)
	}
	if group.Recreating || group.RecreateStopped {
		fsm.interruptStability(next)
		return nil
	}
	if next.Phase != Starting && !sameGroup(next, &group) {
		if fsm.config.DryRun && next.Operation != nil {
			fsm.wouldDo = describeAction(&next.Operation.Action, nil)
			return nil
		}
		fsm.startOver(next, "new group", now)
	}
	if next.Phase == Starting && next.Operation == nil {
		return fsm.start(ctx, next, &group, now)
	}
	fsm.comparePods(next, &group)
	current := fsm.connect(ctx, next, &group)
	if len(current.unknown) > 0 && current.read == 0 {
		if next.UnseenSince.IsZero() {
			next.UnseenSince = now
		}
		if fsm.config.UnseenTimeout <= 0 || now.Sub(next.UnseenSince) < fsm.config.UnseenTimeout {
			return fsm.unseen(ctx, next, now, false, errors.New("observe: no admitted member could be read"))
		}
		current.observationTimedOut = true
	} else {
		next.UnseenSince, next.UnseenByDiscover = time.Time{}, false
	}
	if reason := violation(next, current); reason != "" {
		next.Unsafe = reason
	}
	if next.Operation != nil {
		return fsm.execute(ctx, next, current, now)
	}
	return fsm.decide(ctx, next, current, now)
}

func (fsm *controller) unseen(ctx context.Context, next *Record, now time.Time, byDiscover bool, err error) error {
	fsm.interruptStability(next)
	if next.UnseenSince.IsZero() {
		next.UnseenSince = now
	}
	next.UnseenByDiscover = next.UnseenByDiscover || byDiscover
	if saveErr := fsm.save(ctx, next); saveErr != nil {
		return saveErr
	}
	return err
}

func sameGroup(next *Record, group *workload.Group) bool {
	for uid, saved := range next.Pods {
		if pod := find(group, uid); pod != nil && pod.Identity == saved.Identity && pod.State != workload.PodTerminating {
			return true
		}
	}
	return false
}

func (fsm *controller) startOver(next *Record, reason string, now time.Time) {
	if next.Operation != nil {
		fsm.finish(next, engine.Failed, engine.Group, reason, now)
	}
	fsm.closeIncident(next, Recreated, now)
	epoch, state, result, budget := next.Epoch, next.PolicyState, next.Result, next.Budget
	budget.StableSince = time.Time{}
	*next = Record{
		Version: 2, Phase: Starting, Since: now, Reason: reason, Epoch: epoch, PolicyState: state, Result: result, Budget: budget,
	}
}

func (fsm *controller) start(ctx context.Context, next *Record, group *workload.Group, now time.Time) error {
	next.UnseenSince, next.UnseenByDiscover = time.Time{}, false
	next.Pods = map[string]PodRecord{}
	for index := range group.Pods {
		if pod := &group.Pods[index]; pod.State != workload.PodTerminating {
			next.Pods[pod.UID] = PodRecord{Name: pod.Name, Identity: pod.Identity, Node: pod.Node}
		}
	}
	obs := fsm.eng.Observe(ctx, group.Pods)
	for index := range group.Pods {
		pod := &group.Pods[index]
		hasMember := slices.ContainsFunc(obs.Members, func(member engine.Member) bool { return member.Pod.UID == pod.UID })
		if pod.State == workload.PodStarting || (pod.State == workload.PodReady && !hasMember) {
			return nil
		}
	}
	if slices.ContainsFunc(obs.Members, func(member engine.Member) bool { return member.Unknown }) {
		return nil
	}
	var reported, live []engine.MemberID
	for index := range obs.Members {
		member := &obs.Members[index]
		if member.Replaces != "" && !member.Serving {
			continue
		}
		reported = append(reported, member.ID())
		if !member.Terminal {
			live = append(live, member.ID())
		}
	}
	slices.Sort(live)
	next.Members = map[engine.MemberID]MemberState{}
	for _, id := range reported {
		next.Members[id] = Admitted
	}
	current := &view{group: group, obs: obs, admitted: live, members: map[engine.MemberID]*engine.Member{}}
	for index := range obs.Members {
		member := &obs.Members[index]
		current.members[member.ID()] = member
	}
	return fsm.decide(ctx, next, current, now)
}

func (fsm *controller) comparePods(next *Record, group *workload.Group) {
	for _, uid := range slices.Sorted(maps.Keys(next.Pods)) {
		saved, pod := next.Pods[uid], find(group, uid)
		switch {
		case saved.Removing && pod == nil:
			forget(next, uid)
		case saved.Removing:
		case pod == nil && !tracks(next, saved.Identity):
			delete(next.Pods, uid)
		case pod != nil && pod.Identity != saved.Identity && !tracks(next, saved.Identity):
			next.Pods[uid] = PodRecord{Name: pod.Name, Identity: pod.Identity, Node: pod.Node}
		}
	}
	for index := range group.Pods {
		pod := &group.Pods[index]
		if _, ok := next.Pods[pod.UID]; !ok && pod.State != workload.PodTerminating {
			next.Pods[pod.UID] = PodRecord{Name: pod.Name, Identity: pod.Identity, Node: pod.Node}
		}
	}
}

func (fsm *controller) connect(ctx context.Context, next *Record, group *workload.Group) *view {
	current := &view{group: group, obs: fsm.eng.Observe(ctx, group.Pods), members: map[engine.MemberID]*engine.Member{}}
	if next.Members == nil {
		next.Members = map[engine.MemberID]MemberState{}
	}
	reported := map[engine.MemberID]bool{}
	for index := range current.obs.Members {
		member := &current.obs.Members[index]
		id := member.ID()
		if member.Unknown {
			member.Serving, member.Terminal, member.Joinable = false, false, false
		}
		state, tracked := next.Members[id]
		lost := tracked && isLost(next, group, id)
		reported[id] = !lost && !member.Unknown
		switch {
		case lost && state == Candidate:
			next.Members[id] = Rejected
		case lost:
			member.Terminal, member.Serving, member.Unknown = true, false, false
		case tracked && state == Candidate && member.Terminal:
			next.Members[id] = Rejected
		case !tracked && joinable(next, group, member) && replacementAllowed(next, member):
			next.Members[id] = Candidate
		}
		current.members[id] = member
	}
	for _, id := range slices.Sorted(maps.Keys(next.Members)) {
		if _, found := current.members[id]; found {
			continue
		}
		lost := isLost(next, group, id)
		switch {
		case lost && next.Members[id] == Admitted:
			current.obs.Members = append(current.obs.Members, lostMember(next, id))
		case lost && next.Members[id] == Candidate:
			next.Members[id] = Rejected
		case !lost && next.Members[id] == Admitted:
			current.obs.Members = append(current.obs.Members, unknownMember(next, id))
		}
	}
	for index := range current.obs.Members {
		member := &current.obs.Members[index]
		current.members[member.ID()] = member
	}
	for _, id := range slices.Sorted(maps.Keys(next.Members)) {
		switch next.Members[id] {
		case Admitted:
			current.admitted = append(current.admitted, id)
			if current.members[id].Unknown {
				current.unknown = append(current.unknown, id)
			} else if reported[id] {
				current.read++
			}
		case Candidate:
			current.candidates = append(current.candidates, id)
		}
	}
	return current
}

func joinable(next *Record, group *workload.Group, member *engine.Member) bool {
	saved, ok := next.Pods[member.Pod.UID]
	pod := find(group, member.Pod.UID)
	return ok && !saved.Removing && saved.Identity == member.PodIdentity && pod != nil &&
		pod.State == workload.PodReady && pod.Identity == member.PodIdentity
}

func replacementAllowed(next *Record, member *engine.Member) bool {
	if member.Replaces == "" {
		return true
	}
	uid, _, found := podOf(next, member.Replaces)
	return found && uid == member.Pod.UID && member.ID() != member.Replaces &&
		next.Members[member.Replaces] == Excluded
}

func isLost(next *Record, group *workload.Group, id engine.MemberID) bool {
	uid, saved, ok := podOf(next, id)
	if !ok {
		return true
	}
	pod := find(group, uid)
	return pod == nil || pod.State == workload.PodTerminating || pod.Identity != saved.Identity
}

func lostMember(next *Record, id engine.MemberID) engine.Member {
	name, identity := nameOf(id), identityOf(id)
	uid, saved, _ := podOf(next, id)
	return engine.Member{
		Pod: workload.PodRef{Name: saved.Name, UID: uid}, PodIdentity: identity, Name: name,
		Terminal: true, Detail: "pod lost",
	}
}

func unknownMember(next *Record, id engine.MemberID) engine.Member {
	uid, saved, _ := podOf(next, id)
	return engine.Member{
		Pod: workload.PodRef{Name: saved.Name, UID: uid}, PodIdentity: identityOf(id), Name: nameOf(id),
		Unknown: true, Detail: "not reported",
	}
}

func violation(next *Record, current *view) string {
	for id, member := range current.members {
		if member.Unknown {
			continue
		}
		state := next.Members[id]
		switch {
		case state == Excluded && !member.Terminal:
			return fmt.Sprintf("C4: excluded member %s is not terminal", id)
		case state != Admitted && member.Serving && (next.Operation == nil || !slices.Contains(next.Operation.Action.Add, id)):
			return fmt.Sprintf("C5: member %s serves before it is admitted", id)
		case next.Operation != nil && slices.Contains(next.Operation.Action.Remove, id) && !member.Terminal:
			return fmt.Sprintf("C1: member %s left the reconfig but is not terminal", id)
		}
	}
	return ""
}

func (fsm *controller) decide(ctx context.Context, next *Record, current *view, now time.Time) error {
	unhealthy := current.pick(func(member *engine.Member) bool { return member == nil || !member.Serving || member.Unknown })
	if len(unhealthy) > 0 {
		fsm.beginRecovery(next, now)
	}
	budget := fsm.budgetInput(next, now)
	if err := fsm.save(ctx, next); err != nil {
		return err
	}
	input := engine.DecisionInput{
		Observation: current.obs, ObservationTimedOut: current.observationTimedOut,
		Admitted: current.admitted, Candidates: current.candidates,
		State: slices.Clone(next.PolicyState), LastResult: next.Result, Starting: next.Phase == Starting,
		Fault: next.Unsafe, Elapsed: now.Sub(next.Epoch), Budget: budget,
		DiagnosisTimeout: fsm.config.DiagnosisTimeout, VerifyTimeout: fsm.config.VerifyTimeout, FlapWindow: fsm.config.FlapWindow,
	}
	for _, id := range slices.Sorted(maps.Keys(next.Members)) {
		switch next.Members[id] {
		case Excluded:
			input.Excluded = append(input.Excluded, id)
		case Rejected:
			input.Rejected = append(input.Rejected, id)
		}
	}
	action := fsm.eng.Decide(input)
	if reason := checkAction(next, current, &action); reason != "" {
		return fmt.Errorf("invalid engine action: %s", reason)
	}
	if len(action.Add) > 0 && !fsm.eng.Capabilities().Grow {
		return errors.New("engine cannot admit replacements")
	}
	next.Reason = action.Reason
	if next.Phase != Starting && len(unhealthy) > 0 {
		enter(next, Recovering, action.Reason, now)
		if next.Incident == nil {
			fsm.openIncident(next, action.Reason, current, now)
		}
	}
	if action.Kind == engine.Healthy || action.Kind == engine.Wait {
		if action.State != nil {
			next.PolicyState = slices.Clone(action.State)
		}
		if action.Kind == engine.Healthy {
			enter(next, Serving, action.Reason, now)
			fsm.closeIncident(next, outcome(next.Incident), now)
			fsm.stableRecovery(next, current, now)
		} else {
			fsm.interruptStability(next)
		}
		if next.Budget.Blocked != "" {
			next.Reason = next.Budget.Blocked
		}
		return nil
	}
	fsm.interruptStability(next)
	var selected *WorkloadAction
	var result engine.Report
	if externalAction(action.Kind) {
		selected, result = fsm.selectWorkload(next, current, &action)
	}
	mutating := action.Kind == engine.Change || action.Kind == engine.Probe || selected != nil
	if fsm.config.DryRun {
		if mutating && budget.Blocked != "" {
			next.Reason = budget.Blocked
			return nil
		}
		fsm.wouldDo = describeAction(&action, selected)
		return nil
	}
	if mutating && !fsm.authorizeAction(next, now) {
		next.Reason = next.Budget.Blocked
		return nil
	}
	if action.State != nil {
		next.PolicyState = slices.Clone(action.State)
	}
	if inc := next.Incident; inc != nil && (action.Kind == engine.Change || selected != nil && selected.Kind == RestoreResetAll) {
		if len(inc.Reconfigs) == 0 && inc.Durations.Diagnose == 0 {
			inc.Durations.Diagnose = now.Sub(inc.Opened)
		}
		affect(next, inc, action.Remove)
	}
	action.State = nil
	next.Operation = &engine.Operation{ID: newID("iro-ft-", 16), Action: action}
	next.OperationSince, next.Workload = now, selected
	if selected != nil {
		for _, target := range selected.Pods {
			pod := next.Pods[target.UID]
			pod.Removing = true
			next.Pods[target.UID] = pod
		}
	}
	if err := fsm.save(ctx, next); err != nil {
		return err
	}
	if externalAction(action.Kind) && selected == nil {
		fsm.finish(next, result.State, result.Scope, result.Reason, now)
		return nil
	}
	return fsm.execute(ctx, next, current, now)
}

func enter(next *Record, phase Phase, reason string, now time.Time) {
	if next.Phase != phase {
		next.Since = now
	}
	next.Phase, next.Reason = phase, reason
}

func find(group *workload.Group, uid string) *workload.Pod {
	index := slices.IndexFunc(group.Pods, func(pod workload.Pod) bool { return pod.UID == uid })
	if index < 0 {
		return nil
	}
	return &group.Pods[index]
}

func podOf(next *Record, id engine.MemberID) (string, PodRecord, bool) {
	identity := identityOf(id)
	for uid, saved := range next.Pods {
		if saved.Identity == identity {
			return uid, saved, true
		}
	}
	return "", PodRecord{}, false
}

func identityOf(id engine.MemberID) string {
	return string(id)[strings.LastIndex(string(id), "@")+1:]
}

func nameOf(id engine.MemberID) string {
	return strings.TrimSuffix(string(id), "@"+identityOf(id))
}

func joined(ids []engine.MemberID) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, string(id))
	}
	return strings.Join(parts, ", ")
}

func tracks(next *Record, identity string) bool {
	for id := range next.Members {
		if identityOf(id) == identity {
			return true
		}
	}
	return false
}

func forget(next *Record, uid string) {
	identity := next.Pods[uid].Identity
	delete(next.Pods, uid)
	for id := range next.Members {
		if identityOf(id) == identity {
			delete(next.Members, id)
		}
	}
}

func refs(next *Record) []workload.PodRef {
	pods := make([]workload.PodRef, 0, len(next.Pods))
	for _, uid := range slices.Sorted(maps.Keys(next.Pods)) {
		saved := next.Pods[uid]
		pods = append(pods, podRef(uid, &saved))
	}
	slices.SortFunc(pods, func(left, right workload.PodRef) int { return strings.Compare(left.Name, right.Name) })
	return pods
}

func podRef(uid string, saved *PodRecord) workload.PodRef {
	return workload.PodRef{Name: saved.Name, UID: uid}
}

func newID(prefix string, size int) string {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(value)
}
