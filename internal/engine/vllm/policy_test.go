// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

//nolint:testpackage // Tests pass the driver-private observation to the policy.
package vllm

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/llm-d/llm-d-resiliency-operator/internal/engine"
)

func policyInput(admitted []engine.MemberID) engine.DecisionInput {
	return engine.DecisionInput{
		Admitted: admitted, DiagnosisTimeout: time.Minute, VerifyTimeout: time.Minute,
		FlapWindow: time.Minute,
	}
}

func resultOf(id string, plan *engine.Action, state engine.Status) *engine.Result {
	return &engine.Result{Operation: engine.Operation{ID: id, Action: *plan}, State: state}
}

func TestPolicyOwnsRemovalSequence(t *testing.T) {
	t.Parallel()
	test := newFleet(t, 4, 1)
	input := policyInput(four)
	for _, fake := range test.ranks {
		fake.unhealthy()
	}
	test.ranks[1].set("dead", nil)
	observation := test.observe(t)
	input.Observation = observation
	probe := test.eng.Decide(input)
	if probe.Kind != engine.Probe {
		t.Fatalf("first action %+v", probe)
	}
	input.State, input.LastResult = probe.State, resultOf("probe", &probe, engine.Succeeded)
	input.Observation = observation
	remove := test.eng.Decide(input)
	if remove.Kind != engine.Change || !slices.Equal(remove.Remove, ids("dp1@id1")) {
		t.Fatalf("after probe %+v", remove)
	}
	input.State, input.LastResult = remove.State, resultOf("remove", &remove, engine.Succeeded)
	input.Admitted, input.Excluded = ids("dp0@id0", "dp2@id2", "dp3@id3"), ids("dp1@id1")
	for _, index := range []int{0, 2, 3} {
		test.ranks[index].set("healthy", nil)
	}
	observation = test.observe(t)
	input.Observation = observation
	verify := test.eng.Decide(input)
	if verify.Kind != engine.Verify || !slices.Equal(verify.Participants, input.Admitted) {
		t.Fatalf("after removal %+v", verify)
	}
	input.State, input.LastResult = verify.State, resultOf("verify", &verify, engine.Succeeded)
	input.Observation = observation
	replacement := test.eng.Decide(input)
	if replacement.Kind != engine.ReplaceMembers || !slices.Equal(replacement.Members, input.Excluded) {
		t.Fatalf("after verification %+v", replacement)
	}
	input.State, input.LastResult = replacement.State, resultOf("replacement", &replacement, engine.Failed)
	input.Observation = observation
	if plan := test.eng.Decide(input); plan.Kind != engine.Healthy {
		t.Fatalf("completed result processed twice: %+v", plan)
	}
}

func TestPolicyVerificationDeadlineSurvivesReload(t *testing.T) {
	t.Parallel()
	test := newFleet(t, 4, 1)
	input := policyInput(four)
	input.Starting, input.Elapsed = true, time.Second
	input.Observation = test.observe(t)
	verify := test.eng.Decide(input)
	if verify.Kind != engine.Verify {
		t.Fatalf("startup action %+v", verify)
	}
	input.State, input.LastResult = verify.State, resultOf("verify-1", &verify, engine.Failed)
	input.Elapsed = 2 * time.Second
	input.Observation = test.observe(t)
	retry := test.eng.Decide(input)
	if retry.Kind != engine.Verify {
		t.Fatalf("first failed verification %+v", retry)
	}
	input.State = slices.Clone(retry.State)
	input.Elapsed = time.Minute
	input.Observation = test.observe(t)
	second := test.eng.Decide(input)
	if second.Kind != engine.Verify {
		t.Fatalf("within verification deadline %+v", second)
	}
	input.State, input.LastResult = second.State, resultOf("verify-2", &second, engine.Failed)
	input.Elapsed = time.Minute + 2*time.Second
	reloaded := &Engine{}
	input.Observation = test.observe(t)
	if plan := reloaded.Decide(input); plan.Kind != engine.RecreateGroup {
		t.Fatalf("expired deadline after reload %+v", plan)
	}
}

func TestPolicyStartupVerificationCompletes(t *testing.T) {
	t.Parallel()
	test := newFleet(t, 4, 1)
	input := policyInput(four)
	input.Starting = true
	input.Observation = test.observe(t)
	verify := test.eng.Decide(input)
	input.State, input.LastResult = verify.State, resultOf("verify", &verify, engine.Succeeded)
	input.Observation = test.observe(t)
	if plan := test.eng.Decide(input); plan.Kind != engine.Healthy {
		t.Fatalf("verified startup %+v", plan)
	}
}

func TestPolicyDiagnosisDeadlineSurvivesReload(t *testing.T) {
	t.Parallel()
	test := newFleet(t, 4, 1)
	test.ranks[1].set("dead", nil)
	input := policyInput(four)
	input.Observation = test.observe(t)
	probe := test.eng.Decide(input)
	input.State, input.LastResult = probe.State, resultOf("probe", &probe, engine.Failed)
	input.Elapsed = time.Second
	input.Observation = test.observe(t)
	wait := test.eng.Decide(input)
	if wait.Kind != engine.Wait {
		t.Fatalf("failed probe must permit diagnosis: %+v", wait)
	}
	input.State, input.Elapsed = slices.Clone(wait.State), time.Minute
	input.Observation = test.observe(t)
	if plan := (&Engine{}).Decide(input); plan.Kind != engine.RecreateGroup {
		t.Fatalf("diagnosis deadline %+v", plan)
	}
}

func TestPolicyRetryFlapSurvivesReload(t *testing.T) {
	t.Parallel()
	test := newFleet(t, 4, 1)
	for _, fake := range test.ranks {
		fake.unhealthy()
	}
	input := policyInput(four)
	input.State = json.RawMessage(`{"stage":"diagnosing"}`)
	input.Observation = test.observe(t)
	retry := test.eng.Decide(input)
	if retry.Kind != engine.Change || len(retry.Remove) > 0 {
		t.Fatalf("initial retry %+v", retry)
	}
	input.State, input.LastResult = retry.State, resultOf("retry", &retry, engine.Succeeded)
	for _, fake := range test.ranks {
		fake.set("healthy", nil)
	}
	input.Observation = test.observe(t)
	verify := test.eng.Decide(input)
	input.State, input.LastResult = verify.State, resultOf("verify", &verify, engine.Succeeded)
	input.Observation = test.observe(t)
	healthy := test.eng.Decide(input)
	input.State = healthy.State
	for _, fake := range test.ranks {
		fake.unhealthy()
	}
	input.Elapsed = time.Second
	input.Observation = test.observe(t)
	probe := test.eng.Decide(input)
	input.State, input.LastResult = probe.State, resultOf("probe-2", &probe, engine.Succeeded)
	input.Observation = test.observe(t)
	if plan := (&Engine{}).Decide(input); plan.Kind != engine.RecreateGroup {
		t.Fatalf("repeated retry %+v", plan)
	}
}

func TestPolicyRequestsReplacementOncePerMember(t *testing.T) {
	t.Parallel()
	for _, status := range []engine.Status{engine.Succeeded, engine.Failed} {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			test := newFleet(t, 4, 1)
			input := policyInput(ids("dp0@id0", "dp2@id2", "dp3@id3"))
			input.Excluded = ids("dp1@id1")
			input.Observation = test.observe(t)
			action := test.eng.Decide(input)
			if action.Kind != engine.ReplaceMembers || !slices.Equal(action.Members, input.Excluded) {
				t.Fatalf("replacement action %+v", action)
			}
			input.State, input.LastResult = action.State, resultOf("replacement", &action, status)
			next := (&Engine{}).Decide(input)
			if next.Kind != engine.Healthy {
				t.Fatalf("repeats completed replacement request: %+v", next)
			}
			input.State = next.State
			input.Rejected = ids("dp1-replacement@id1")
			if next = (&Engine{}).Decide(input); next.Kind != engine.ReplaceMembers || !slices.Equal(next.Members, input.Rejected) {
				t.Fatalf("new failed member ignored: %+v", next)
			}
		})
	}
}

func TestPolicyFailureRequestsReset(t *testing.T) {
	t.Parallel()
	for _, input := range []engine.DecisionInput{
		{Admitted: four, Fault: "membership is uncertain"},
		{Admitted: four, ObservationTimedOut: true},
		{Admitted: four, State: json.RawMessage(`invalid`)},
		{Admitted: four, LastResult: resultOf("change", &engine.Action{Kind: engine.Change}, engine.Failed)},
	} {
		test := newFleet(t, 4, 1)
		input.Observation = test.observe(t)
		if plan := test.eng.Decide(input); plan.Kind != engine.RecreateGroup {
			t.Fatalf("failure %+v returned %+v", input, plan)
		}
	}
}

func TestPolicyIsPure(t *testing.T) {
	t.Parallel()
	test := newFleet(t, 4, 1)
	test.ranks[1].set("dead", nil)
	input := policyInput(four)
	observation := test.observe(t)
	input.Observation = observation
	first := test.eng.Decide(input)
	input.Observation = observation
	second := test.eng.Decide(input)
	if !reflect.DeepEqual(first, second) || len(input.State) != 0 {
		t.Fatalf("non-deterministic plans: %+v, %+v; state %s", first, second, input.State)
	}
}

func TestPolicyResetRecoversMalformedCheckpoint(t *testing.T) {
	t.Parallel()
	test := newFleet(t, 4, 1)
	input := policyInput(four)
	input.State = json.RawMessage(`invalid`)
	input.Observation = test.observe(t)
	reset := test.eng.Decide(input)
	if reset.Kind != engine.RecreateGroup || !json.Valid(reset.State) {
		t.Fatalf("malformed checkpoint action %+v", reset)
	}
	input.State, input.LastResult = reset.State, resultOf("reset", &reset, engine.Succeeded)
	input.Starting, input.Elapsed = true, time.Minute
	input.Observation = test.observe(t)
	verify := (&Engine{}).Decide(input)
	if verify.Kind != engine.Verify {
		t.Fatalf("recreated group action %+v", verify)
	}
}

func TestPolicyVerifiesRecreatedGroup(t *testing.T) {
	t.Parallel()
	test := newFleet(t, 4, 1)
	input := policyInput(four)
	input.Starting = true
	input.Observation = test.observe(t)
	verify := test.eng.Decide(input)
	input.State, input.LastResult = verify.State, resultOf("verify", &verify, engine.Succeeded)
	input.Observation = test.observe(t)
	healthy := test.eng.Decide(input)
	input.State = healthy.State
	for index := range test.pods {
		test.pods[index].Identity += "-new"
	}
	observation := test.observe(t)
	input.Admitted = nil
	for _, member := range observation.Members {
		input.Admitted = append(input.Admitted, member.ID())
	}
	input.Observation = observation
	if plan := (&Engine{}).Decide(input); plan.Kind != engine.Verify {
		t.Fatalf("recreated group reused earlier verification: %+v", plan)
	}
}
