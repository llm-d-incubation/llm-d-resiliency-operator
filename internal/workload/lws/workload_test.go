// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

package lws_test

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/llm-d/llm-d-resiliency-operator/internal/workload"
	"github.com/llm-d/llm-d-resiliency-operator/internal/workload/lws"
)

var created = metav1.NewTime(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))

type deletion struct {
	name        string
	propagation metav1.DeletionPropagation
}

type cluster struct {
	api     client.Client
	deletes []deletion
}

func newCluster(t *testing.T, set *unstructured.Unstructured, pods ...*corev1.Pod) *cluster {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	world := &cluster{}
	objects := []client.Object{}
	if set != nil {
		objects = append(objects, set)
	}
	for _, pod := range pods {
		objects = append(objects, pod)
	}
	world.api = fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(ctx context.Context, api client.WithWatch, object client.Object, options ...client.DeleteOption) error {
			settings := (&client.DeleteOptions{}).ApplyOptions(options)
			if settings.Preconditions == nil || settings.Preconditions.UID == nil {
				t.Errorf("delete of %s without a UID precondition", object.GetName())
				return errors.New("missing UID precondition")
			}
			var current corev1.Pod
			if err := api.Get(ctx, client.ObjectKeyFromObject(object), &current); err != nil {
				return err
			}
			if current.UID != *settings.Preconditions.UID {
				return apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, object.GetName(), errors.New("UID changed"))
			}
			propagation := metav1.DeletionPropagation("")
			if settings.PropagationPolicy != nil {
				propagation = *settings.PropagationPolicy
			}
			world.deletes = append(world.deletes, deletion{object.GetName(), propagation})
			return api.Delete(ctx, object, options...)
		},
	}).Build()
	return world
}

func leaderWorkerSet(policy string) *unstructured.Unstructured {
	return specified(1, 2, "", policy)
}

func specified(replicas, size int64, identity, policy string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "leaderworkerset.x-k8s.io/v1", "kind": "LeaderWorkerSet",
		"metadata": map[string]any{"namespace": "test", "name": "model", "uid": "set-uid"},
		"spec": map[string]any{
			"replicas": replicas, "groupIdentity": identity,
			"leaderWorkerTemplate": map[string]any{"size": size, "restartPolicy": policy},
		},
	}}
}

func readyPod(index int) *corev1.Pod {
	name := "model-0"
	if index > 0 {
		name = fmt.Sprintf("model-0-%d", index)
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "test", UID: types.UID(name + "-uid"), CreationTimestamp: created,
			Labels: map[string]string{
				"leaderworkerset.sigs.k8s.io/name":         "model",
				"leaderworkerset.sigs.k8s.io/group-index":  "0",
				"leaderworkerset.sigs.k8s.io/worker-index": fmt.Sprint(index),
			},
		},
		Spec: corev1.PodSpec{NodeName: fmt.Sprintf("node-%d", index), Containers: []corev1.Container{{Name: "engine"}}},
		Status: corev1.PodStatus{
			PodIP: fmt.Sprintf("10.0.0.%d", index+1),
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "engine", ContainerID: name + "-container",
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}
}

func terminating(pod *corev1.Pod) {
	pod.Finalizers = []string{"test/hold"}
	pod.DeletionTimestamp = &created
}

func newWorkload(t *testing.T, world *cluster) workload.Workload {
	t.Helper()
	options := &lws.Options{Namespace: "test", Name: "model"}
	instance, _, err := options.New(world.api, world.api)
	if err != nil {
		t.Fatal(err)
	}
	return instance
}

func discover(t *testing.T, instance workload.Workload) workload.Group {
	t.Helper()
	group, err := instance.Discover(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return group
}

func TestNewNeedsNamespaceAndName(t *testing.T) {
	t.Parallel()
	world := newCluster(t, leaderWorkerSet(""))
	for _, options := range []lws.Options{{Namespace: "test"}, {Name: "model"}} {
		if _, _, err := options.New(world.api, world.api); err == nil {
			t.Fatalf("%+v was accepted", options)
		}
	}
}

func TestDiscover(t *testing.T) {
	t.Parallel()
	world := newCluster(t, leaderWorkerSet("RecreateGroupOnPodRestart"), readyPod(0), readyPod(1))
	instance := newWorkload(t, world)
	group := discover(t, instance)
	if len(group.Pods) != 2 || group.Recreating || group.RecreateStopped {
		t.Fatalf("group: %+v", group)
	}
	for index, pod := range group.Pods {
		want := readyPod(index)
		if pod.Name != want.Name || pod.UID != string(want.UID) || pod.IP != want.Status.PodIP || pod.Index != index ||
			pod.Node != want.Spec.NodeName || pod.State != workload.PodReady || pod.Identity == "" {
			t.Fatalf("pod %d: %+v", index, pod)
		}
	}
	if again := discover(t, instance); again.Pods[0].Identity != group.Pods[0].Identity {
		t.Fatal("identity is not stable")
	}
}

func TestDiscoverPodState(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		mutate func(*corev1.Pod)
		want   workload.PodState
	}{
		"no IP":     {func(pod *corev1.Pod) { pod.Status.PodIP = "" }, workload.PodStarting},
		"no status": {func(pod *corev1.Pod) { pod.Status.ContainerStatuses = nil }, workload.PodStarting},
		"waiting container": {
			func(pod *corev1.Pod) { pod.Status.ContainerStatuses[0].State = corev1.ContainerState{} }, workload.PodStarting,
		},
		"no container ID": {func(pod *corev1.Pod) { pod.Status.ContainerStatuses[0].ContainerID = "" }, workload.PodStarting},
		"init done": {func(pod *corev1.Pod) {
			pod.Spec.InitContainers = []corev1.Container{{Name: "init"}}
			pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
				Name: "init", ContainerID: "init-container",
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}},
			}}
		}, workload.PodReady},
		"terminating": {terminating, workload.PodTerminating},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			worker := readyPod(1)
			test.mutate(worker)
			world := newCluster(t, leaderWorkerSet("None"), readyPod(0), worker)
			if got := discover(t, newWorkload(t, world)).Pods[1].State; got != test.want {
				t.Fatalf("state %s, want %s", got, test.want)
			}
		})
	}
}

func TestDiscoverIdentity(t *testing.T) {
	t.Parallel()
	base := readyPod(1)
	world := newCluster(t, leaderWorkerSet("None"), readyPod(0), base)
	before := discover(t, newWorkload(t, world)).Pods[1].Identity
	for name, test := range map[string]struct {
		mutate  func(*corev1.Pod)
		changed bool
	}{
		"new UID":       {func(pod *corev1.Pod) { pod.UID = "other-uid" }, true},
		"new container": {func(pod *corev1.Pod) { pod.Status.ContainerStatuses[0].ContainerID = "other" }, true},
		"restart":       {func(pod *corev1.Pod) { pod.Status.ContainerStatuses[0].RestartCount = 1 }, true},
		"new IP":        {func(pod *corev1.Pod) { pod.Status.PodIP = "10.0.0.9" }, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			worker := readyPod(1)
			test.mutate(worker)
			got := discover(t, newWorkload(t, newCluster(t, leaderWorkerSet("None"), readyPod(0), worker))).Pods[1].Identity
			if (got != before) != test.changed {
				t.Fatalf("identity %s, before %s", got, before)
			}
		})
	}
}

func withSidecars(pod *corev1.Pod) *corev1.Pod {
	always := corev1.ContainerRestartPolicyAlways
	pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{Name: "proxy"})
	pod.Spec.InitContainers = []corev1.Container{{Name: "setup"}, {Name: "routing-proxy", RestartPolicy: &always}}
	pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, corev1.ContainerStatus{
		Name: "proxy", ContainerID: pod.Name + "-proxy", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	})
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{
		{Name: "setup", ContainerID: pod.Name + "-setup", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}},
		{
			Name: "routing-proxy", ContainerID: pod.Name + "-routing",
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		},
	}
	return pod
}

func identityWith(t *testing.T, engines []string, mutate func(*corev1.Pod)) string {
	t.Helper()
	worker := withSidecars(readyPod(1))
	mutate(worker)
	world := newCluster(t, leaderWorkerSet("None"), withSidecars(readyPod(0)), worker)
	options := &lws.Options{Namespace: "test", Name: "model", EngineContainers: engines}
	instance, _, err := options.New(world.api, world.api)
	if err != nil {
		t.Fatal(err)
	}
	return discover(t, instance).Pods[1].Identity
}

func TestIdentityCoversEngineContainers(t *testing.T) {
	t.Parallel()
	restartRegular := func(name string) func(*corev1.Pod) {
		return func(pod *corev1.Pod) {
			for index := range pod.Status.ContainerStatuses {
				if pod.Status.ContainerStatuses[index].Name == name {
					pod.Status.ContainerStatuses[index].RestartCount++
				}
			}
		}
	}
	restartInit := func(name string) func(*corev1.Pod) {
		return func(pod *corev1.Pod) {
			for index := range pod.Status.InitContainerStatuses {
				if status := &pod.Status.InitContainerStatuses[index]; status.Name == name {
					status.RestartCount++
					status.ContainerID += "-again"
				}
			}
		}
	}
	for name, test := range map[string]struct {
		engines []string
		mutate  func(*corev1.Pod)
		changed bool
	}{
		"native sidecar restart":         {mutate: restartInit("routing-proxy")},
		"init container status":          {mutate: restartInit("setup")},
		"regular sidecar restart":        {mutate: restartRegular("proxy"), changed: true},
		"engine restart":                 {mutate: restartRegular("engine"), changed: true},
		"named: regular sidecar restart": {engines: []string{"engine"}, mutate: restartRegular("proxy")},
		"named: engine restart":          {engines: []string{"engine"}, mutate: restartRegular("engine"), changed: true},
		"named: native sidecar as engine": {
			engines: []string{"engine", "routing-proxy"}, mutate: restartInit("routing-proxy"), changed: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			before := identityWith(t, test.engines, func(*corev1.Pod) {})
			if got := identityWith(t, test.engines, test.mutate); (got != before) != test.changed {
				t.Fatalf("identity %s, before %s", got, before)
			}
		})
	}
	t.Run("leader and workers name their engine containers differently", func(t *testing.T) {
		t.Parallel()
		leader, worker := readyPod(0), readyPod(1)
		leader.Spec.Containers[0].Name, leader.Status.ContainerStatuses[0].Name = "vllm-leader", "vllm-leader"
		worker.Spec.Containers[0].Name, worker.Status.ContainerStatuses[0].Name = "vllm-worker", "vllm-worker"
		world := newCluster(t, leaderWorkerSet("None"), leader, worker)
		options := &lws.Options{Namespace: "test", Name: "model", EngineContainers: []string{"vllm-leader", "vllm-worker"}}
		instance, _, err := options.New(world.api, world.api)
		if err != nil {
			t.Fatal(err)
		}
		if group := discover(t, instance); len(group.Pods) != 2 {
			t.Fatalf("group %+v", group)
		}
	})
	t.Run("a Pod with none of the named containers", func(t *testing.T) {
		t.Parallel()
		world := newCluster(t, leaderWorkerSet("None"), readyPod(0), readyPod(1))
		options := &lws.Options{Namespace: "test", Name: "model", EngineContainers: []string{"vllm"}}
		instance, _, err := options.New(world.api, world.api)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := instance.Discover(t.Context()); err == nil {
			t.Fatal("Discover accepted a Pod without the engine container")
		}
	})
	t.Run("flag", func(t *testing.T) {
		t.Parallel()
		var options lws.Options
		flags := flag.NewFlagSet("test", flag.ContinueOnError)
		options.BindFlags(flags)
		if err := flags.Parse([]string{"--lws-engine-containers=vllm, other,"}); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(options.EngineContainers, []string{"vllm", "other"}) {
			t.Fatalf("containers %q", options.EngineContainers)
		}
	})
}

func TestDiscoverReportsPodsAsTheyAre(t *testing.T) {
	t.Parallel()
	other := readyPod(1)
	other.Labels["leaderworkerset.sigs.k8s.io/name"] = "other"
	tenth, second := readyPod(10), readyPod(2)
	world := newCluster(t, leaderWorkerSet("None"), readyPod(0), other, tenth, second)
	group := discover(t, newWorkload(t, world))
	if len(group.Pods) != 3 || group.Pods[1].Index != 2 || group.Pods[2].Index != 10 {
		t.Fatalf("pods: %+v", group.Pods)
	}
}

func TestDiscoverRejectsUnsupportedSpec(t *testing.T) {
	t.Parallel()
	deleted := leaderWorkerSet("None")
	deleted.SetDeletionTimestamp(&created)
	deleted.SetFinalizers([]string{"test/hold"})
	for name, set := range map[string]*unstructured.Unstructured{
		"two replicas":    specified(2, 2, "", "None"),
		"zero size":       specified(1, 0, "", "None"),
		"hash identity":   specified(1, 2, "Hash", "None"),
		"after start":     specified(1, 2, "", "RecreateGroupAfterStart"),
		"being deleted":   deleted,
		"missing the LWS": nil,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := newWorkload(t, newCluster(t, set, readyPod(0))).Discover(t.Context()); err == nil {
				t.Fatal("spec accepted")
			}
		})
	}
}

func TestDiscoverRejectsMismatchedIndex(t *testing.T) {
	t.Parallel()
	worker := readyPod(1)
	worker.Labels["leaderworkerset.sigs.k8s.io/worker-index"] = "0"
	if _, err := newWorkload(t, newCluster(t, leaderWorkerSet("None"), readyPod(0), worker)).Discover(t.Context()); err == nil {
		t.Fatal("mismatched index accepted")
	}
}

func TestDiscoverRecreating(t *testing.T) {
	t.Parallel()
	older := metav1.NewTime(created.Add(-time.Minute))
	for name, test := range map[string]struct {
		policy  string
		leader  func(*corev1.Pod)
		worker  func(*corev1.Pod)
		want    bool
		stopped bool
	}{
		"steady":             {policy: "RecreateGroupOnPodRestart"},
		"leader terminating": {policy: "None", leader: terminating, want: true},
		"recreate pending":   {policy: "None", leader: annotate("leaderworkerset.sigs.k8s.io/group-recreate-pending"), want: true},
		"worker older":       {policy: "None", worker: func(pod *corev1.Pod) { pod.CreationTimestamp = older }, want: true},
		"restart recreates":  {policy: "RecreateGroupOnPodRestart", worker: restart, want: true},
		"restart under None": {policy: "None", worker: restart},
		"sidecar restart recreates": {policy: "RecreateGroupOnPodRestart", worker: func(pod *corev1.Pod) {
			withSidecars(pod).Status.InitContainerStatuses[1].RestartCount = 1
		}, want: true},
		"terminating recreates":  {policy: "RecreateGroupOnPodRestart", worker: terminating, want: true},
		"terminating under None": {policy: "None", worker: terminating},
		"budget used up": {
			policy: "RecreateGroupOnPodRestart", worker: restart,
			leader: annotate("leaderworkerset.sigs.k8s.io/group-restart-budget-exhausted"), stopped: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			leader, worker := readyPod(0), readyPod(1)
			if test.leader != nil {
				test.leader(leader)
			}
			if test.worker != nil {
				test.worker(worker)
			}
			group := discover(t, newWorkload(t, newCluster(t, leaderWorkerSet(test.policy), leader, worker)))
			if group.Recreating != test.want || group.RecreateStopped != test.stopped {
				t.Fatalf("recreating %v, stopped %v", group.Recreating, group.RecreateStopped)
			}
		})
	}
	t.Run("leader missing", func(t *testing.T) {
		t.Parallel()
		if !discover(t, newWorkload(t, newCluster(t, leaderWorkerSet("None"), readyPod(1)))).Recreating {
			t.Fatal("missing leader is not Recreating")
		}
	})
}

func annotate(key string) func(*corev1.Pod) {
	return func(pod *corev1.Pod) { pod.Annotations = map[string]string{key: "true"} }
}

func restart(pod *corev1.Pod) { pod.Status.ContainerStatuses[0].RestartCount = 1 }

var errNoUID = errors.New("no UID")

func TestReset(t *testing.T) {
	t.Parallel()
	worker := workload.PodRef{Name: "model-0-1", UID: "model-0-1-uid"}
	other := workload.PodRef{Name: "model-0-2", UID: "model-0-2-uid"}
	leader := workload.PodRef{Name: "model-0", UID: "model-0-uid"}
	for name, test := range map[string]struct {
		policy  string
		pods    []workload.PodRef
		deletes int
		err     error
	}{
		"recreates group":  {policy: "RecreateGroupOnPodRestart", pods: []workload.PodRef{worker}, err: workload.ErrUnsupported},
		"leader":           {policy: "None", pods: []workload.PodRef{leader}, err: workload.ErrUnsupported},
		"leader in a list": {policy: "None", pods: []workload.PodRef{worker, leader}, err: workload.ErrUnsupported},
		"worker":           {policy: "None", pods: []workload.PodRef{worker}, deletes: 1},
		"two workers":      {policy: "None", pods: []workload.PodRef{worker, other}, deletes: 2},
		"stale UID":        {policy: "None", pods: []workload.PodRef{{Name: "model-0-1", UID: "old"}}},
		"no UID":           {policy: "None", pods: []workload.PodRef{worker, {Name: "model-0-2"}}, err: errNoUID},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			world := newCluster(t, leaderWorkerSet(test.policy), readyPod(0), readyPod(1), readyPod(2))
			instance := newWorkload(t, world)
			err := instance.Reset(t.Context(), test.pods)
			if errors.Is(test.err, errNoUID) {
				if err == nil || errors.Is(err, workload.ErrUnsupported) {
					t.Fatalf("error %v, want a missing UID", err)
				}
			} else if !errors.Is(err, test.err) {
				t.Fatalf("error %v, want %v", err, test.err)
			}
			if len(world.deletes) != test.deletes {
				t.Fatalf("deletes: %+v", world.deletes)
			}
			if test.err == nil {
				if err := instance.Reset(t.Context(), test.pods); err != nil {
					t.Fatalf("repeat: %v", err)
				}
			}
		})
	}
}

func TestResetAll(t *testing.T) {
	t.Parallel()
	world := newCluster(t, leaderWorkerSet("RecreateGroupOnPodRestart"), readyPod(0), readyPod(1))
	instance := newWorkload(t, world)
	stale := []workload.PodRef{{Name: "model-0", UID: "old"}, {Name: "model-0-1", UID: "model-0-1-uid"}}
	if err := instance.ResetAll(t.Context(), stale); err != nil || len(world.deletes) != 0 {
		t.Fatalf("stale leader: %v, %+v", err, world.deletes)
	}
	current := []workload.PodRef{{Name: "model-0-1", UID: "model-0-1-uid"}, {Name: "model-0", UID: "model-0-uid"}}
	if err := instance.ResetAll(t.Context(), current); err != nil {
		t.Fatal(err)
	}
	if len(world.deletes) != 1 || world.deletes[0] != (deletion{"model-0", metav1.DeletePropagationForeground}) {
		t.Fatalf("deletes: %+v", world.deletes)
	}
	if err := instance.ResetAll(t.Context(), current); err != nil {
		t.Fatalf("repeat: %v", err)
	}
	if err := instance.ResetAll(t.Context(), current[:1]); err == nil {
		t.Fatal("ResetAll without the leader was accepted")
	}
}

func TestRelease(t *testing.T) {
	t.Parallel()
	world := newCluster(t, leaderWorkerSet("None"), readyPod(0), readyPod(1))
	err := newWorkload(t, world).Release(t.Context(), []workload.PodRef{{Name: "model-0-1", UID: "model-0-1-uid"}})
	if !errors.Is(err, workload.ErrUnsupported) || len(world.deletes) != 0 {
		t.Fatalf("release: %v, %+v", err, world.deletes)
	}
}
