// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

package configmap_test

import (
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d/llm-d-resiliency-operator/internal/journal"
	"github.com/llm-d/llm-d-resiliency-operator/internal/journal/configmap"
)

func TestSaveUnchangedRecord(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"none", "annotation", "record", "deleted"} {
		t.Run(change, func(t *testing.T) {
			t.Parallel()
			scheme := runtime.NewScheme()
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "group", Namespace: "test", UID: "owner"}}
			storage := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner).Build()
			ref := &unstructured.Unstructured{}
			ref.SetAPIVersion("v1")
			ref.SetKind("ConfigMap")
			ref.SetName(owner.Name)
			ref.SetNamespace(owner.Namespace)
			store := configmap.New(storage, storage, ref)
			data := []byte(`{"phase":"serving"}`)
			version, err := store.Save(t.Context(), data, "")
			if err != nil {
				t.Fatal(err)
			}
			var before corev1.ConfigMap
			name := client.ObjectKey{Namespace: owner.Namespace, Name: owner.Name + "-ft-state"}
			if err := storage.Get(t.Context(), name, &before); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "annotation", "record":
				before.Annotations = map[string]string{"test": "metadata edit"}
				if change == "record" {
					before.Data["state.json"] = `{"phase":"recovering"}`
				}
				if err := storage.Update(t.Context(), &before); err != nil {
					t.Fatal(err)
				}
			case "deleted":
				if err := storage.Delete(t.Context(), &before); err != nil {
					t.Fatal(err)
				}
			}
			nextVersion, err := store.Save(t.Context(), data, version)
			if change == "record" || change == "deleted" {
				if !errors.Is(err, journal.ErrConflict) {
					t.Fatalf("got %v, want conflict", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			var after corev1.ConfigMap
			err = storage.Get(t.Context(), name, &after)
			if change == "deleted" {
				if !apierrors.IsNotFound(err) {
					t.Fatalf("deleted record was recreated: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if after.ResourceVersion != before.ResourceVersion || after.Data["state.json"] != before.Data["state.json"] {
				t.Fatalf("record was written: before %+v, after %+v", before, after)
			}
			if change == "none" && nextVersion != version {
				t.Fatalf("unchanged version: got %q, want %q", nextVersion, version)
			}
		})
	}
}
