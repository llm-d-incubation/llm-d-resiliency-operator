// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

package lws_test

import (
	"context"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d/llm-d-resiliency-operator/internal/engine"
	"github.com/llm-d/llm-d-resiliency-operator/internal/workload/lws"
)

func fixture(t *testing.T) (client.Client, lws.Config) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	set := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "leaderworkerset.x-k8s.io/v1", "kind": "LeaderWorkerSet",
		"metadata": map[string]any{"namespace": "test", "name": "model", "uid": "set-uid"},
		"spec": map[string]any{
			"replicas": int64(1), "leaderWorkerTemplate": map[string]any{
				"size": int64(2), "restartPolicy": "RecreateGroupOnPodRestart",
			},
		},
	}}
	objects := make([]client.Object, 0, 3)
	objects = append(objects, set)
	for index, name := range []string{"model-0", "model-0-1"} {
		objects = append(objects, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: "test", UID: types.UID(name + "-uid"),
				Labels: map[string]string{
					"leaderworkerset.sigs.k8s.io/name":         "model",
					"leaderworkerset.sigs.k8s.io/group-index":  "0",
					"leaderworkerset.sigs.k8s.io/worker-index": fmt.Sprint(index),
				},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "engine"}}},
			Status: corev1.PodStatus{
				PodIP: fmt.Sprintf("10.0.0.%d", index+1),
				ContainerStatuses: []corev1.ContainerStatus{{
					Name: "engine", ContainerID: name + "-container",
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				}},
			},
		})
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(), lws.Config{
		Namespace: "test", Name: "model", LocalRanks: 2, GroupPods: 2,
	}
}

func newWorkload(t *testing.T, api client.Client, config lws.Config) *lws.Client {
	t.Helper()
	workload, err := lws.New(api, api, config)
	if err != nil {
		t.Fatal(err)
	}
	return workload
}

func discover(t *testing.T, workload *lws.Client) engine.Group {
	t.Helper()
	group, err := workload.Discover(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return group
}

func TestDiscover(t *testing.T) {
	t.Parallel()
	api, config := fixture(t)
	workload := newWorkload(t, api, config)
	group := discover(t, workload)
	if group.ID == "" || len(group.Endpoints) != 4 {
		t.Fatalf("unexpected group: %+v", group)
	}
	for rank, endpoint := range group.Endpoints {
		wantURL := fmt.Sprintf("http://10.0.0.%d:%d", rank/2+1, 8000+rank%2)
		if endpoint.Rank != rank || endpoint.URL != wantURL || endpoint.PodUID == "" || endpoint.DiagnosticsURL == "" {
			t.Fatalf("unexpected endpoint: %+v", endpoint)
		}
	}
	if next := discover(t, workload); next.ID != group.ID {
		t.Fatal("unchanged workload changed identity")
	}
	var pod corev1.Pod
	if err := api.Get(t.Context(), client.ObjectKey{Namespace: "test", Name: "model-0-1"}, &pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.ContainerStatuses[0].RestartCount++
	if err := api.Status().Update(t.Context(), &pod); err != nil {
		t.Fatal(err)
	}
	if next := discover(t, workload); next.ID == group.ID {
		t.Fatal("container restart did not change group identity")
	}
}

func TestConfiguration(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		mutate func(*lws.Config)
	}{
		{"namespace", func(config *lws.Config) { config.Namespace = "" }},
		{"no ranks", func(config *lws.Config) { config.LocalRanks = 0 }},
		{"no Pods", func(config *lws.Config) { config.GroupPods = 0 }},
		{"port range", func(config *lws.Config) { config.BasePort = 65535 }},
		{"observer port", func(config *lws.Config) { config.ObserverPort = -1 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			api, config := fixture(t)
			test.mutate(&config)
			if _, err := lws.New(api, api, config); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}

func TestDiscoverRejectsInvalidTopology(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		field []string
		value any
	}{
		{"replicas", []string{"spec", "replicas"}, int64(2)},
		{"size", []string{"spec", "leaderWorkerTemplate", "size"}, int64(3)},
		{"restart policy", []string{"spec", "leaderWorkerTemplate", "restartPolicy"}, "None"},
		{"hash identity", []string{"spec", "groupIdentity"}, "Hash"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			api, config := fixture(t)
			set := &unstructured.Unstructured{}
			set.SetGroupVersionKind(schema.GroupVersionKind{Group: "leaderworkerset.x-k8s.io", Version: "v1", Kind: "LeaderWorkerSet"})
			if err := api.Get(t.Context(), client.ObjectKey{Namespace: "test", Name: "model"}, set); err != nil {
				t.Fatal(err)
			}
			if err := unstructured.SetNestedField(set.Object, test.value, test.field...); err != nil {
				t.Fatal(err)
			}
			if err := api.Update(t.Context(), set); err != nil {
				t.Fatal(err)
			}
			if _, err := newWorkload(t, api, config).Discover(t.Context()); err == nil {
				t.Fatal("invalid topology accepted")
			}
		})
	}
}

func TestDiscoveryRequiresNativeIdentity(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		mutate func(*corev1.Pod)
	}{
		{"other set", func(pod *corev1.Pod) { pod.Labels["leaderworkerset.sigs.k8s.io/name"] = "other" }},
		{"other group", func(pod *corev1.Pod) { pod.Labels["leaderworkerset.sigs.k8s.io/group-index"] = "1" }},
		{"duplicate rank", func(pod *corev1.Pod) { pod.Labels["leaderworkerset.sigs.k8s.io/worker-index"] = "0" }},
		{"missing container", func(pod *corev1.Pod) { pod.Status.ContainerStatuses = nil }},
		{"exited container", func(pod *corev1.Pod) { pod.Status.ContainerStatuses[0].State = corev1.ContainerState{} }},
		{"missing address", func(pod *corev1.Pod) { pod.Status.PodIP = "" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			api, config := fixture(t)
			var pod corev1.Pod
			if err := api.Get(t.Context(), client.ObjectKey{Namespace: "test", Name: "model-0-1"}, &pod); err != nil {
				t.Fatal(err)
			}
			test.mutate(&pod)
			status := pod.Status.DeepCopy()
			if err := api.Update(t.Context(), &pod); err != nil {
				t.Fatal(err)
			}
			pod.Status = *status
			if err := api.Status().Update(t.Context(), &pod); err != nil {
				t.Fatal(err)
			}
			if _, err := newWorkload(t, api, config).Discover(t.Context()); err == nil {
				t.Fatal("invalid Pod accepted")
			}
		})
	}
}

type guardedClient struct {
	client.Client
	uid   types.UID
	calls int
}

func (api *guardedClient) Delete(ctx context.Context, object client.Object, options ...client.DeleteOption) error {
	settings := (&client.DeleteOptions{}).ApplyOptions(options)
	if settings.Preconditions == nil || settings.Preconditions.UID == nil {
		return fmt.Errorf("missing UID precondition")
	}
	api.calls++
	if *settings.Preconditions.UID != api.uid {
		return apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, object.GetName(), fmt.Errorf("UID changed"))
	}
	return api.Client.Delete(ctx, object, options...)
}

func TestResetUsesOldLeaderUID(t *testing.T) {
	t.Parallel()
	base, config := fixture(t)
	api := &guardedClient{Client: base, uid: "model-0-uid"}
	workload := newWorkload(t, api, config)
	group := discover(t, workload)
	group.Endpoints[0].PodUID = "previous-uid"
	if err := workload.Reset(t.Context(), group); err != nil {
		t.Fatal(err)
	}
	if next := discover(t, workload); next.ID == "" {
		t.Fatal("stale reset deleted replacement")
	}
	group.Endpoints[0].PodUID = string(api.uid)
	if err := workload.Reset(t.Context(), group); err != nil {
		t.Fatal(err)
	}
	if err := workload.Reset(t.Context(), group); err != nil {
		t.Fatalf("missing leader should be a no-op: %v", err)
	}
	group.Endpoints[0].Pod = "other"
	if err := workload.Reset(t.Context(), group); err == nil || api.calls != 3 {
		t.Fatal("unexpected leader was not rejected before deletion")
	}
}

func TestStateConflicts(t *testing.T) {
	t.Parallel()
	api, config := fixture(t)
	first, second := newWorkload(t, api, config), newWorkload(t, api, config)
	for _, workload := range []*lws.Client{first, second} {
		if data, err := workload.Load(t.Context()); err != nil || len(data) != 0 {
			t.Fatalf("initial load: %s, %v", data, err)
		}
	}
	if err := first.Save(t.Context(), []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := second.Save(t.Context(), []byte("stale create")); !apierrors.IsAlreadyExists(err) {
		t.Fatalf("expected create conflict, got %v", err)
	}
	if data, err := second.Load(t.Context()); err != nil || string(data) != "first" {
		t.Fatalf("load: %s, %v", data, err)
	}
	if err := first.Save(t.Context(), []byte("newer")); err != nil {
		t.Fatal(err)
	}
	if err := second.Save(t.Context(), []byte("stale update")); !apierrors.IsConflict(err) {
		t.Fatalf("expected version conflict, got %v", err)
	}
	if data, err := second.Load(t.Context()); err != nil || string(data) != "newer" {
		t.Fatalf("newer state overwritten: %s, %v", data, err)
	}
}
