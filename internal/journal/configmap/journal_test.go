// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

package configmap_test

import (
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d/llm-d-resiliency-operator/internal/journal"
	"github.com/llm-d/llm-d-resiliency-operator/internal/journal/configmap"
)

var stateKey = client.ObjectKey{Namespace: "test", Name: "model-ft-state"}

func owner() *unstructured.Unstructured {
	set := &unstructured.Unstructured{}
	set.SetAPIVersion("leaderworkerset.x-k8s.io/v1")
	set.SetKind("LeaderWorkerSet")
	set.SetNamespace("test")
	set.SetName("model")
	return set
}

func newClient(t *testing.T, withOwner bool, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if withOwner {
		set := owner()
		set.SetUID("set-uid")
		objects = append(objects, set)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

//nolint:gocritic // The record and its version.
func load(t *testing.T, store *configmap.Journal) (string, string) {
	t.Helper()
	bytes, version, err := store.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return string(bytes), version
}

func save(t *testing.T, store *configmap.Journal, data, version string) string {
	t.Helper()
	next, err := store.Save(t.Context(), []byte(data), version)
	if err != nil {
		t.Fatal(err)
	}
	if next == "" || next == version {
		t.Fatalf("save %q over %q returned version %q", data, version, next)
	}
	return next
}

func stored(t *testing.T, api client.Client) *corev1.ConfigMap {
	t.Helper()
	var state corev1.ConfigMap
	if err := api.Get(t.Context(), stateKey, &state); err != nil {
		t.Fatal(err)
	}
	return &state
}

func TestLoadEmpty(t *testing.T) {
	t.Parallel()
	api := newClient(t, true)
	if data, version := load(t, configmap.New(api, api, owner())); data != "" || version != "" {
		t.Fatalf("load: %q, version %q", data, version)
	}
	empty := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "model-ft-state"}}
	api = newClient(t, true, empty)
	store := configmap.New(api, api, owner())
	data, version := load(t, store)
	if data != "" || version == "" {
		t.Fatalf("load: %q, version %q", data, version)
	}
	save(t, store, "first", version)
}

func TestSaveConflicts(t *testing.T) {
	t.Parallel()
	api := newClient(t, true)
	first, second := configmap.New(api, api, owner()), configmap.New(api, api, owner())
	version := save(t, first, "first", "")
	if _, err := second.Save(t.Context(), []byte("stale create"), ""); !errors.Is(err, journal.ErrConflict) {
		t.Fatalf("stale create: %v", err)
	}
	data, loaded := load(t, second)
	if data != "first" || loaded != version {
		t.Fatalf("load: %q, version %q, want %q", data, loaded, version)
	}
	save(t, first, "newer", version)
	if _, err := second.Save(t.Context(), []byte("stale update"), loaded); !errors.Is(err, journal.ErrConflict) {
		t.Fatalf("stale update: %v", err)
	}
	if data, _ := load(t, second); data != "newer" {
		t.Fatalf("newer record overwritten: %q", data)
	}
}

func TestSaveAfterConflictWithReloadedVersion(t *testing.T) {
	t.Parallel()
	api := newClient(t, true)
	first, second := configmap.New(api, api, owner()), configmap.New(api, api, owner())
	version := save(t, first, "first", "")
	save(t, first, "newer", version)
	if _, err := second.Save(t.Context(), []byte("stale"), version); !errors.Is(err, journal.ErrConflict) {
		t.Fatalf("stale update: %v", err)
	}
	_, reloaded := load(t, second)
	save(t, second, "after reload", reloaded)
	if data, _ := load(t, first); data != "after reload" {
		t.Fatalf("load: %q", data)
	}
}

func TestSaveSurvivesMetadataEdit(t *testing.T) {
	t.Parallel()
	api := newClient(t, true)
	store := configmap.New(api, api, owner())
	version := save(t, store, "first", "")
	state := stored(t, api)
	state.Annotations = map[string]string{"example.com/owner": "platform"}
	if err := api.Update(t.Context(), state); err != nil {
		t.Fatal(err)
	}
	save(t, store, "second", version)
	if state = stored(t, api); state.Data["state.json"] != "second" || state.Annotations["example.com/owner"] != "platform" {
		t.Fatalf("stored %v, annotations %v", state.Data, state.Annotations)
	}
}

func TestSaveConflictsOnDataEdit(t *testing.T) {
	t.Parallel()
	api := newClient(t, true)
	store := configmap.New(api, api, owner())
	version := save(t, store, "first", "")
	state := stored(t, api)
	state.Data["state.json"] = "edited"
	if err := api.Update(t.Context(), state); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(t.Context(), []byte("second"), version); !errors.Is(err, journal.ErrConflict) {
		t.Fatalf("save over another writer: %v", err)
	}
	if got := stored(t, api).Data["state.json"]; got != "edited" {
		t.Fatalf("stored %q", got)
	}
}

func TestSaveDoesNotRecreateDeleted(t *testing.T) {
	t.Parallel()
	api := newClient(t, true)
	store := configmap.New(api, api, owner())
	version := save(t, store, "first", "")
	if err := api.Delete(t.Context(), stored(t, api)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(t.Context(), []byte("second"), version); !errors.Is(err, journal.ErrConflict) {
		t.Fatalf("save after deletion: %v", err)
	}
	data, reloaded := load(t, store)
	if data != "" || reloaded != "" {
		t.Fatalf("recreated: %q, version %q", data, reloaded)
	}
	save(t, store, "started over", "")
	if state := stored(t, api); state.Data["state.json"] != "started over" || len(state.OwnerReferences) != 1 {
		t.Fatalf("created: %v, owners %v", state.Data, state.OwnerReferences)
	}
}

func TestOwnedByWorkloadObject(t *testing.T) {
	t.Parallel()
	api := newClient(t, true)
	save(t, configmap.New(api, api, owner()), "state", "")
	owners := stored(t, api).OwnerReferences
	if len(owners) != 1 || owners[0].APIVersion != "leaderworkerset.x-k8s.io/v1" ||
		owners[0].Kind != "LeaderWorkerSet" || owners[0].Name != "model" || owners[0].UID != "set-uid" {
		t.Fatalf("owners: %+v", owners)
	}
}

func TestSaveWithoutOwner(t *testing.T) {
	t.Parallel()
	api := newClient(t, false)
	store := configmap.New(api, api, owner())
	if _, err := store.Save(t.Context(), []byte("state"), ""); err == nil || errors.Is(err, journal.ErrConflict) {
		t.Fatalf("save without the owner: %v", err)
	}
}
