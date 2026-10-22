// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

//nolint:testpackage // Tests reach the driver-private observation.
package vllm

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/llm-d/llm-d-resiliency-operator/internal/engine"
	"github.com/llm-d/llm-d-resiliency-operator/internal/workload"
)

type fakeRank struct {
	mu      sync.Mutex
	rank    int
	status  map[string]any
	health  int
	text    string
	tokens  int
	applied []map[string]any
	prompts int
	down    bool
	onApply func(*fakeRank, map[string]any)
}

func (fake *fakeRank) get() map[string]any {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.status
}

func (fake *fakeRank) put(status map[string]any) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.status = status
}

func (fake *fakeRank) set(status string, extra map[string]any) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.status = map[string]any{"id": fake.rank, "status": status}
	for key, value := range extra {
		fake.status[key] = value
	}
}

func (fake *fakeRank) unhealthy() {
	fake.set("unhealthy", map[string]any{"fault_info": "RuntimeError"})
}

func (fake *fakeRank) isDown() bool {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.down
}

func reply(writer http.ResponseWriter, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := writer.Write(data); err != nil {
		return
	}
}

func (fake *fakeRank) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path == "/v1/fault_tolerance/apply" {
		fake.apply(writer, request)
		return
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	switch request.URL.Path {
	case "/v1/fault_tolerance/status":
		reply(writer, map[string]any{"schema_version": 1, "total_engines": 1, "engines": []any{fake.status}})
	case "/health":
		writer.WriteHeader(fake.health)
	case "/v1/completions":
		fake.prompts++
		reply(writer, map[string]any{
			"choices": []any{map[string]any{"text": fake.text}},
			"usage":   map[string]any{"completion_tokens": fake.tokens},
		})
	default:
		writer.WriteHeader(http.StatusNotFound)
	}
}

func (fake *fakeRank) apply(writer http.ResponseWriter, request *http.Request) {
	var payload map[string]any
	if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	fake.mu.Lock()
	fake.applied = append(fake.applied, payload)
	hook := fake.onApply
	fake.mu.Unlock()
	writer.WriteHeader(http.StatusAccepted)
	if hook != nil {
		hook(fake, payload)
	}
}

type fleet struct {
	eng   *Engine
	pods  []workload.Pod
	ranks []*fakeRank
}

func newFleet(t *testing.T, pods, perPod int) *fleet {
	t.Helper()
	test := &fleet{}
	routes := map[string]*fakeRank{}
	servers := map[*fakeRank]string{}
	for index := range pods {
		ip := fmt.Sprintf("10.0.0.%d", index+1)
		test.pods = append(test.pods, workload.Pod{
			Name: fmt.Sprintf("lws-%d", index), UID: fmt.Sprintf("uid%d", index), IP: ip, Index: index,
			Identity: fmt.Sprintf("id%d", index), State: workload.PodReady,
		})
		for local := range perPod {
			fake := &fakeRank{rank: index*perPod + local, health: http.StatusOK, text: " two", tokens: 8}
			fake.set("healthy", nil)
			server := httptest.NewServer(fake)
			t.Cleanup(server.Close)
			routes[net.JoinHostPort(ip, fmt.Sprint(8000+local))] = fake
			servers[fake] = server.Listener.Addr().String()
			test.ranks = append(test.ranks, fake)
		}
	}
	dialer := &net.Dialer{}
	options := Options{APIPort: 8000, RanksPerPod: perPod, StorePort: 29600, Timeout: time.Second}
	test.eng = &Engine{options: options, client: &http.Client{Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			fake := routes[address]
			if fake == nil || fake.isDown() {
				return nil, errors.New("connection refused")
			}
			return dialer.DialContext(ctx, network, servers[fake])
		},
	}}}
	return test
}

func (test *fleet) observe(t *testing.T) engine.Observation {
	t.Helper()
	return test.eng.Observe(t.Context(), test.pods)
}

func ids(list ...string) []engine.MemberID {
	result := make([]engine.MemberID, 0, len(list))
	for _, id := range list {
		result = append(result, engine.MemberID(id))
	}
	return result
}

func params(t *testing.T, payload map[string]any) map[string]any {
	t.Helper()
	found, ok := payload["params"].(map[string]any)
	if !ok {
		t.Fatalf("payload %v has no params", payload)
	}
	return found
}

var four = ids("dp0@id0", "dp1@id1", "dp2@id2", "dp3@id3")

func terminal(obs engine.Observation, ids ...string) engine.Observation {
	for index := range obs.Members {
		if member := &obs.Members[index]; slices.Contains(ids, string(member.ID())) {
			member.Terminal, member.Serving = true, false
		}
	}
	return obs
}

func lost(pod *workload.Pod, name string) engine.Member {
	return engine.Member{
		Pod: workload.PodRef{Name: pod.Name, UID: pod.UID}, PodIdentity: pod.Identity, Name: name, Terminal: true, Detail: "pod lost",
	}
}

func TestObserve(t *testing.T) {
	t.Parallel()
	test := newFleet(t, 4, 1)
	test.ranks[1].set("dead", nil)
	test.ranks[2].health = http.StatusServiceUnavailable
	test.ranks[3].down = true
	obs := test.observe(t)
	want := map[engine.MemberID][2]bool{
		"dp0@id0": {true, false}, "dp1@id1": {false, true}, "dp2@id2": {false, true}, "dp3@id3": {false, false},
	}
	if len(obs.Members) != 4 {
		t.Fatalf("members %+v", obs.Members)
	}
	for _, member := range obs.Members {
		if got := [2]bool{member.Serving, member.Terminal}; got != want[member.ID()] {
			t.Errorf("%s serving, terminal = %v, want %v (%s)", member.ID(), got, want[member.ID()], member.Detail)
		}
		if member.Unknown != (member.ID() == "dp3@id3") {
			t.Errorf("%s unknown = %v", member.ID(), member.Unknown)
		}
	}
}

func TestObserveSkipsPodsWithoutAddress(t *testing.T) {
	t.Parallel()
	test := newFleet(t, 2, 1)
	test.pods[0].State = workload.PodTerminating
	test.pods[1].IP = ""
	if obs := test.observe(t); len(obs.Members) != 0 {
		t.Fatalf("members %+v", obs.Members)
	}
}

func TestObserveRanksPerPod(t *testing.T) {
	t.Parallel()
	test := newFleet(t, 2, 2)
	test.ranks[3].set("unhealthy", map[string]any{"fault_info": "RuntimeError", "mask": []int{0, 0, 1, 0}})
	obs := test.observe(t)
	names := make([]string, 0, len(obs.Members))
	for _, member := range obs.Members {
		names = append(names, string(member.ID()))
	}
	if !slices.Equal(names, []string{"dp0@id0", "dp1@id0", "dp2@id1", "dp3@id1"}) || obs.Members[3].Serving {
		t.Fatalf("members %+v", obs.Members)
	}
}

func TestObserveRejectsWrongRank(t *testing.T) {
	t.Parallel()
	test := newFleet(t, 2, 1)
	test.ranks[1].status["id"] = 5
	obs := test.observe(t)
	if member := obs.Members[1]; member.Serving || member.Terminal || !strings.Contains(member.Detail, "rank 5") {
		t.Fatalf("member %+v", member)
	}
}

func TestDecide(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		setup  func([]*fakeRank)
		kind   engine.ActionKind
		remove []engine.MemberID
		joined int
	}{
		"healthy": {setup: func([]*fakeRank) {}, kind: engine.Healthy},
		"dead rank": {
			setup: func(r []*fakeRank) { r[1].set("dead", nil); r[0].unhealthy(); r[2].unhealthy(); r[3].unhealthy() },
			kind:  engine.Change, remove: ids("dp1@id1"), joined: 3,
		},
		"survivor healthy": {
			setup: func(r []*fakeRank) { r[1].set("dead", nil); r[0].unhealthy(); r[2].unhealthy() },
			kind:  engine.Wait,
		},
		"survivor diagnoses": {
			setup: func(r []*fakeRank) {
				r[1].set("dead", nil)
				r[0].unhealthy()
				r[2].unhealthy()
				r[3].set("diagnosing", nil)
			},
			kind: engine.Wait,
		},
		"retry": {
			setup: func(r []*fakeRank) { r[0].unhealthy(); r[1].unhealthy(); r[2].unhealthy(); r[3].unhealthy() },
			kind:  engine.Change, joined: 4,
		},
		"unreachable": {
			setup: func(r []*fakeRank) { r[2].down = true; r[0].unhealthy(); r[1].unhealthy(); r[3].unhealthy() },
			kind:  engine.Wait,
		},
		"reconfig in flight": {
			setup: func(r []*fakeRank) {
				r[0].unhealthy()
				r[1].unhealthy()
				r[2].unhealthy()
				r[3].set("unhealthy", map[string]any{"ft_state": "recovering", "last_ft_request_id": "x"})
			},
			kind: engine.Wait,
		},
		"process exited": {
			setup: func(r []*fakeRank) {
				r[0].health = http.StatusServiceUnavailable
				r[1].unhealthy()
				r[2].unhealthy()
				r[3].unhealthy()
			},
			kind: engine.Change, remove: ids("dp0@id0"), joined: 3,
		},
		"no survivors": {
			setup: func(r []*fakeRank) {
				for _, fake := range r {
					fake.set("dead", nil)
				}
			},
			kind: engine.RecreateGroup,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fleet := newFleet(t, 4, 1)
			test.setup(fleet.ranks)
			input := policyInput(four)
			input.State = json.RawMessage(`{"stage":"diagnosing"}`)
			input.Observation = fleet.observe(t)
			plan := fleet.eng.Decide(input)
			if plan.Kind != test.kind || !slices.Equal(plan.Remove, test.remove) || len(plan.Participants) != test.joined {
				t.Fatalf("plan %+v", plan)
			}
			if name == "survivor healthy" && !strings.HasPrefix(plan.Reason, "dp1@id1 (dead)") {
				t.Fatalf("the reason does not name the fault: %s", plan.Reason)
			}
		})
	}
}

func TestDecideSeesControllerLostMembers(t *testing.T) {
	t.Parallel()
	test := newFleet(t, 4, 1)
	for _, fake := range test.ranks[:3] {
		fake.unhealthy()
	}
	obs := test.eng.Observe(t.Context(), test.pods[:3])
	obs.Members = append(obs.Members, engine.Member{Name: "dp3", PodIdentity: "id3", Terminal: true, Detail: "pod lost"})
	input := policyInput(four)
	input.State = json.RawMessage(`{"stage":"diagnosing"}`)
	input.Observation = obs
	if plan := test.eng.Decide(input); plan.Kind != engine.Change || !slices.Equal(plan.Remove, ids("dp3@id3")) {
		t.Fatalf("plan %+v", plan)
	}
}

func TestApplyReportsObservedOutcome(t *testing.T) {
	t.Parallel()
	reconfig := engine.Reconfig{ID: "iro-ft-1", Participants: ids("dp0@id0", "dp2@id2", "dp3@id3"), Remove: ids("dp1@id1")}
	ft := func(state, request string) map[string]any {
		return map[string]any{"ft_state": state, "last_ft_request_id": request, "ft_error": "status is HEALTHY"}
	}
	for name, test := range map[string]struct {
		setup func([]*fakeRank)
		state engine.Status
	}{
		"succeeded":     {setup: func(r []*fakeRank) { r[1].set("dead", nil) }, state: engine.Succeeded},
		"running":       {setup: func(r []*fakeRank) { r[2].set("unhealthy", ft("recovering", "iro-ft-1")) }, state: engine.Running},
		"rejected":      {setup: func(r []*fakeRank) { r[2].set("healthy", ft("failed", "iro-ft-1")) }, state: engine.Failed},
		"other request": {setup: func(r []*fakeRank) { r[3].set("unhealthy", ft("recovering", "other")) }, state: engine.Running},
		"other completed request": {setup: func(r []*fakeRank) {
			r[3].set("healthy", ft("succeeded", "other"))
		}, state: engine.Running},
		"stale failure": {setup: func(r []*fakeRank) { r[3].set("unhealthy", ft("failed", "older")) }, state: engine.Running},
		"vote failed":   {setup: func(r []*fakeRank) { r[0].set("dead", nil) }, state: engine.Failed},
		"unreachable":   {setup: func(r []*fakeRank) { r[3].down = true }, state: engine.Running},
		"faulted again": {setup: func(r []*fakeRank) { r[0].unhealthy() }, state: engine.Running},
		"never sent": {setup: func(r []*fakeRank) {
			for _, rank := range r {
				rank.unhealthy()
			}
		}, state: engine.Running},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fleet := newFleet(t, 4, 1)
			test.setup(fleet.ranks)
			progress, err := fleet.eng.Apply(t.Context(), engine.ApplyInput{
				Observation: fleet.observe(t), Operation: operation(&reconfig, true), MayAdvance: false,
			})
			if err != nil || progress.State != test.state || (progress.State == engine.Failed) != (progress.Scope == engine.Group) {
				t.Fatalf("progress %+v", progress)
			}
		})
	}
}

func TestApply(t *testing.T) {
	t.Parallel()
	t.Run("scale down", func(t *testing.T) {
		t.Parallel()
		test := newFleet(t, 4, 1)
		for _, rank := range test.ranks {
			rank.unhealthy()
		}
		reconfig := engine.Reconfig{ID: "r1", Participants: ids("dp0@id0", "dp2@id2", "dp3@id3"), Remove: ids("dp1@id1")}
		report, err := test.apply(t, terminal(test.observe(t), "dp1@id1"), &reconfig)
		if err != nil || report.State != engine.Running || len(test.ranks[1].applied) != 0 {
			t.Fatalf("report %+v, err %v", report, err)
		}
		payload := test.ranks[0].applied[0]
		found := params(t, payload)
		if payload["instruction"] != "scale_down" || payload["request_id"] != "r1" ||
			fmt.Sprint(found["removed_dp_ranks"]) != "[1]" || found["dp_master_ip"] != nil {
			t.Fatalf("payload %v", payload)
		}
	})
	t.Run("store host removed", func(t *testing.T) {
		t.Parallel()
		test := newFleet(t, 4, 1)
		for _, rank := range test.ranks {
			rank.unhealthy()
		}
		reconfig := engine.Reconfig{ID: "r2", Participants: ids("dp2@id2", "dp1@id1"), Remove: ids("dp3@id3", "dp0@id0")}
		if _, err := test.apply(t, terminal(test.observe(t), "dp3@id3", "dp0@id0"), &reconfig); err != nil {
			t.Fatal(err)
		}
		found := params(t, test.ranks[2].applied[0])
		if fmt.Sprint(found["removed_dp_ranks"]) != "[0 3]" || found["dp_master_ip"] != "10.0.0.2" ||
			found["dp_store_port"] != 29601.0 {
			t.Fatalf("params %v", found)
		}
	})
	t.Run("lost member removed", func(t *testing.T) {
		t.Parallel()
		test := newFleet(t, 4, 1)
		for _, rank := range test.ranks {
			rank.unhealthy()
		}
		obs := test.eng.Observe(t.Context(), test.pods[:3])
		obs.Members = append(obs.Members, lost(&test.pods[3], "dp3"))
		reconfig := engine.Reconfig{ID: "r6", Participants: ids("dp0@id0", "dp1@id1", "dp2@id2"), Remove: ids("dp3@id3")}
		if _, err := test.apply(t, obs, &reconfig); err != nil {
			t.Fatal(err)
		}
		for _, rank := range test.ranks[:3] {
			found := params(t, rank.applied[0])
			if fmt.Sprint(found["removed_dp_ranks"]) != "[3]" || found["dp_master_ip"] != nil {
				t.Fatalf("params %v", found)
			}
		}
	})
	t.Run("lost store host", func(t *testing.T) {
		t.Parallel()
		test := newFleet(t, 4, 1)
		for _, rank := range test.ranks {
			rank.unhealthy()
		}
		obs := test.eng.Observe(t.Context(), test.pods[1:])
		obs.Members = append(obs.Members, lost(&test.pods[0], "dp0"))
		reconfig := engine.Reconfig{ID: "r7", Participants: ids("dp1@id1", "dp2@id2", "dp3@id3"), Remove: ids("dp0@id0")}
		if _, err := test.apply(t, obs, &reconfig); err != nil {
			t.Fatal(err)
		}
		if found := params(t, test.ranks[1].applied[0]); found["dp_master_ip"] != "10.0.0.2" || found["dp_store_port"] != 29601.0 {
			t.Fatalf("params %v", found)
		}
	})
	for name, reconfig := range map[string]engine.Reconfig{
		"unobserved participant":       {ID: "r8", Participants: ids("dp0@id0", "dp9@id9"), Remove: ids("dp1@id1")},
		"unobserved removal":           {ID: "r9", Participants: ids("dp0@id0"), Remove: ids("dp9@id9")},
		"removal that is not terminal": {ID: "r10", Participants: ids("dp0@id0"), Remove: ids("dp2@id2")},
	} {
		t.Run(name+" is refused before sending", func(t *testing.T) {
			t.Parallel()
			test := newFleet(t, 3, 1)
			for _, rank := range test.ranks {
				rank.unhealthy()
			}
			if _, err := test.apply(t, terminal(test.observe(t), "dp1@id1"), &reconfig); err == nil {
				t.Fatal("Apply sent a reconfig it should refuse")
			}
			for _, rank := range test.ranks {
				if len(rank.applied) != 0 {
					t.Fatalf("rank %d got %v", rank.rank, rank.applied)
				}
			}
		})
	}
	t.Run("an unreachable participant is refused before sending", func(t *testing.T) {
		t.Parallel()
		test := newFleet(t, 2, 1)
		for _, rank := range test.ranks {
			rank.unhealthy()
		}
		test.ranks[1].down = true
		obs := test.observe(t)
		test.ranks[1].down = false
		if _, err := test.apply(t, obs, &engine.Reconfig{ID: "r11", Participants: ids("dp0@id0", "dp1@id1")}); err == nil {
			t.Fatal("Apply sent to a rank it could not read")
		}
		if len(test.ranks[0].applied) != 0 {
			t.Fatalf("rank 0 got %v", test.ranks[0].applied)
		}
	})
	t.Run("a second store move in one Pod uses another port", func(t *testing.T) {
		t.Parallel()
		test := newFleet(t, 1, 4)
		for _, rank := range test.ranks {
			rank.unhealthy()
		}
		first := engine.Reconfig{ID: "r12", Participants: ids("dp1@id0", "dp2@id0", "dp3@id0"), Remove: ids("dp0@id0")}
		if _, err := test.apply(t, terminal(test.observe(t), "dp0@id0"), &first); err != nil {
			t.Fatal(err)
		}
		second := engine.Reconfig{ID: "r13", Participants: ids("dp2@id0", "dp3@id0"), Remove: ids("dp1@id0")}
		if _, err := test.apply(t, terminal(test.observe(t), "dp0@id0", "dp1@id0"), &second); err != nil {
			t.Fatal(err)
		}
		before, after := params(t, test.ranks[1].applied[0]), params(t, test.ranks[2].applied[1])
		if before["dp_store_port"] != 29601.0 || after["dp_store_port"] != 29602.0 || before["dp_master_ip"] != after["dp_master_ip"] {
			t.Fatalf("first %v, second %v", before, after)
		}
	})
	t.Run("retry", func(t *testing.T) {
		t.Parallel()
		test := newFleet(t, 2, 1)
		for _, rank := range test.ranks {
			rank.unhealthy()
		}
		reconfig := engine.Reconfig{ID: "r3", Participants: ids("dp0@id0", "dp1@id1")}
		if _, err := test.apply(t, test.observe(t), &reconfig); err != nil {
			t.Fatal(err)
		}
		if payload := test.ranks[1].applied[0]; payload["instruction"] != "retry" || len(params(t, payload)) != 0 {
			t.Fatalf("payload %v", payload)
		}
	})
	t.Run("one rank does not accept", func(t *testing.T) {
		t.Parallel()
		test := newFleet(t, 2, 1)
		for _, rank := range test.ranks {
			rank.unhealthy()
		}
		obs := test.observe(t)
		test.ranks[1].down = true
		report, err := test.apply(t, obs, &engine.Reconfig{ID: "r4", Participants: ids("dp0@id0", "dp1@id1")})
		if err == nil || report.State != engine.Running || len(test.ranks[0].applied) != 1 || len(test.ranks[1].applied) != 0 {
			t.Fatalf("report %+v, err %v", report, err)
		}
	})
	t.Run("add", func(t *testing.T) {
		t.Parallel()
		test := newFleet(t, 2, 1)
		for _, rank := range test.ranks {
			rank.unhealthy()
		}
		reconfig := engine.Reconfig{ID: "r5", Participants: ids("dp0@id0"), Add: ids("dp1@id9")}
		if _, err := test.apply(t, test.observe(t), &reconfig); err == nil {
			t.Fatal("vLLM cannot add members")
		}
	})
}

func TestVerify(t *testing.T) {
	t.Parallel()
	test := newFleet(t, 2, 1)
	obs := test.observe(t)
	if err := test.eng.verify(t.Context(), obs, ids("dp0@id0", "dp1@id1")); err != nil {
		t.Fatal(err)
	}
	test.ranks[1].text = ""
	if err := test.eng.verify(t.Context(), obs, ids("dp0@id0", "dp1@id1")); err != nil {
		t.Fatal(err)
	}
	test.ranks[1].tokens = 0
	if err := test.eng.verify(t.Context(), obs, ids("dp0@id0", "dp1@id1")); err == nil || !strings.Contains(err.Error(), "dp1") {
		t.Fatalf("err %v", err)
	}
}

func TestProbeWakesServingRanks(t *testing.T) {
	t.Parallel()
	test := newFleet(t, 3, 1)
	test.ranks[1].set("dead", nil)
	if err := test.eng.probe(t.Context(), test.observe(t)); err != nil {
		t.Fatal(err)
	}
	if test.ranks[0].prompts != 1 || test.ranks[1].prompts != 0 || test.ranks[2].prompts != 1 {
		t.Fatalf("prompts %d %d %d", test.ranks[0].prompts, test.ranks[1].prompts, test.ranks[2].prompts)
	}
}

func TestOptions(t *testing.T) {
	t.Parallel()
	if _, err := (&Options{APIPort: 8000, RanksPerPod: 0, StorePort: 29600, Timeout: time.Second}).New(); err == nil {
		t.Fatal("zero ranks per Pod accepted")
	}
	eng, err := (&Options{APIPort: 8000, RanksPerPod: 1, StorePort: 29600, Timeout: time.Second}).New()
	if err != nil || eng.Capabilities().Grow {
		t.Fatalf("engine %v, err %v", eng, err)
	}
}

func TestAuthenticatedHTTPS(t *testing.T) {
	t.Parallel()
	fake := &fakeRank{rank: 0, health: http.StatusOK, tokens: 8}
	fake.set("healthy", nil)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/v1/completions" {
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			if payload["max_tokens"] == float64(8) && payload["ignore_eos"] != true {
				t.Error("verification must ignore EOS")
			}
		}
		fake.ServeHTTP(w, r)
	}))
	defer server.Close()
	keyFile := filepath.Join(t.TempDir(), "key")
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(keyFile, []byte("test-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(caFile, cert, 0o600); err != nil {
		t.Fatal(err)
	}
	host, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	var apiPort int
	if _, err := fmt.Sscan(port, &apiPort); err != nil {
		t.Fatal(err)
	}
	options := Options{
		APIPort: apiPort, RanksPerPod: 1, StorePort: 29600, Timeout: 5 * time.Second,
		APIKeyFile: keyFile, HTTPS: true, CAFile: caFile,
	}
	driver, err := options.New()
	if err != nil {
		t.Fatal(err)
	}
	observation := driver.Observe(t.Context(), []workload.Pod{{Name: "rank", IP: host, Identity: "id0"}})
	if len(observation.Members) != 1 || !observation.Members[0].Serving {
		t.Fatalf("observation %+v", observation)
	}
	input := engine.ApplyInput{
		Observation: observation, MayAdvance: true,
		Operation: engine.Operation{ID: "probe", Action: engine.Action{Kind: engine.Probe}},
	}
	if report, err := driver.Apply(t.Context(), input); err != nil || report.State != engine.Succeeded {
		t.Fatal(err)
	}
	fake.unhealthy()
	observation = driver.Observe(t.Context(), []workload.Pod{{Name: "rank", IP: host, Identity: "id0"}})
	input.Observation = observation
	input.Operation = operation(&engine.Reconfig{ID: "secure", Participants: ids("dp0@id0")}, false)
	if _, err := driver.Apply(t.Context(), input); err != nil {
		t.Fatal(err)
	}
	input.Operation = engine.Operation{ID: "verify", Action: engine.Action{Kind: engine.Verify, Participants: ids("dp0@id0")}}
	if report, err := driver.Apply(t.Context(), input); err != nil || report.State != engine.Succeeded {
		t.Fatal(err)
	}
	options.CAFile = ""
	untrusted, err := options.New()
	if err != nil {
		t.Fatal(err)
	}
	if obs := untrusted.Observe(t.Context(), []workload.Pod{{IP: host}}); !obs.Members[0].Unknown {
		t.Fatal("untrusted certificate accepted")
	}
}

func TestInvalidCredentials(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"", "first\nsecond"} {
		t.Run(value, func(t *testing.T) {
			t.Parallel()
			file := filepath.Join(t.TempDir(), "key")
			if err := os.WriteFile(file, []byte(value), 0o600); err != nil {
				t.Fatal(err)
			}
			options := Options{APIPort: 8000, RanksPerPod: 1, StorePort: 29600, Timeout: time.Second, APIKeyFile: file}
			if _, err := options.New(); err == nil {
				t.Fatal("invalid key accepted")
			}
		})
	}
}

func operation(reconfig *engine.Reconfig, attempted bool) engine.Operation {
	return engine.Operation{ID: reconfig.ID, Attempted: attempted, Action: engine.Action{
		Kind: engine.Change, Participants: reconfig.Participants, Remove: reconfig.Remove, Add: reconfig.Add, Payload: reconfig.Payload,
	}}
}

func (test *fleet) apply(t *testing.T, observation engine.Observation, reconfig *engine.Reconfig) (engine.Report, error) {
	t.Helper()
	return test.eng.Apply(t.Context(), engine.ApplyInput{
		Observation: observation, Operation: operation(reconfig, false), MayAdvance: true,
	})
}

func TestApplyResumesAfterLostReport(t *testing.T) {
	t.Parallel()
	test := newFleet(t, 2, 1)
	for _, rank := range test.ranks {
		rank.unhealthy()
		rank.onApply = func(fake *fakeRank, payload map[string]any) {
			fake.set("unhealthy", map[string]any{
				"ft_state": "recovering", "last_ft_request_id": payload["request_id"],
			})
		}
	}
	op := operation(&engine.Reconfig{ID: "resume", Participants: ids("dp0@id0", "dp1@id1")}, false)
	input := engine.ApplyInput{Observation: test.observe(t), Operation: op, MayAdvance: true}
	if report, err := test.eng.Apply(t.Context(), input); err != nil || report.State != engine.Running {
		t.Fatalf("first Apply: %+v, %v", report, err)
	}
	// The submitted call's report was lost; only the pre-call journal survives.
	op.Attempted = true
	reloaded := &Engine{options: test.eng.options, client: test.eng.client}
	for range 3 {
		input = engine.ApplyInput{Observation: test.observe(t), Operation: op, MayAdvance: true}
		if report, err := reloaded.Apply(t.Context(), input); err != nil || report.State != engine.Running {
			t.Fatalf("resumed Apply: %+v, %v", report, err)
		}
	}
	for _, rank := range test.ranks {
		rank.set("healthy", nil)
	}
	input.Observation, input.MayAdvance = test.observe(t), false
	if report, err := reloaded.Apply(t.Context(), input); err != nil || report.State != engine.Succeeded {
		t.Fatalf("read-only completion: %+v, %v", report, err)
	}
	for _, rank := range test.ranks {
		if len(rank.applied) != 1 {
			t.Fatalf("rank %d received %d submissions", rank.rank, len(rank.applied))
		}
	}
}

func TestApplyPreservesSubmissionUncertainty(t *testing.T) {
	t.Parallel()
	test := newFleet(t, 2, 1)
	for _, rank := range test.ranks {
		rank.unhealthy()
	}
	input := engine.ApplyInput{
		Observation: test.observe(t), MayAdvance: true,
		Operation: operation(&engine.Reconfig{ID: "uncertain", Participants: ids("dp0@id0", "dp1@id1")}, false),
	}
	test.ranks[1].down = true
	if report, err := test.eng.Apply(t.Context(), input); err == nil || report.State != engine.Running {
		t.Fatalf("partial submission: %+v, %v", report, err)
	}
	test.ranks[1].down = false
	input.Operation.Attempted = true
	input.Observation = test.observe(t)
	reloaded := &Engine{options: test.eng.options, client: test.eng.client}
	if report, err := reloaded.Apply(t.Context(), input); err != nil || report.State != engine.Running {
		t.Fatalf("uncertain outcome: %+v, %v", report, err)
	}
	if len(test.ranks[0].applied) != 1 || len(test.ranks[1].applied) != 0 {
		t.Fatalf("unsafe resend: first %d, second %d", len(test.ranks[0].applied), len(test.ranks[1].applied))
	}
}

func TestApplyDoesNotAdvanceWithoutAuthorization(t *testing.T) {
	t.Parallel()
	for _, kind := range []engine.ActionKind{engine.Change, engine.Probe, engine.Verify} {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			test := newFleet(t, 1, 1)
			input := engine.ApplyInput{
				Observation: test.observe(t), MayAdvance: false,
				Operation: engine.Operation{ID: "expired", Action: engine.Action{Kind: kind, Participants: ids("dp0@id0")}},
			}
			if report, err := test.eng.Apply(t.Context(), input); err != nil || report.State != engine.Running {
				t.Fatalf("read-only Apply: %+v, %v", report, err)
			}
			if len(test.ranks[0].applied) != 0 || test.ranks[0].prompts != 0 {
				t.Fatal("read-only Apply performed an effect")
			}
		})
	}
}

func TestApplyDoesNotResendWithoutRequestEvidence(t *testing.T) {
	t.Parallel()
	test := newFleet(t, 1, 1)
	test.ranks[0].unhealthy()
	input := engine.ApplyInput{
		Observation: test.observe(t), MayAdvance: true,
		Operation: operation(&engine.Reconfig{ID: "saved-before-crash", Participants: ids("dp0@id0")}, true),
	}
	if report, err := test.eng.Apply(t.Context(), input); err != nil || report.State != engine.Running {
		t.Fatalf("unknown submission: %+v, %v", report, err)
	}
	if len(test.ranks[0].applied) != 0 {
		t.Fatal("resent a request whose prior effects cannot be established")
	}
}
