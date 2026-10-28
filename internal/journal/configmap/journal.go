// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

// Package configmap stores the journal in a ConfigMap owned by the workload object.
package configmap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-resiliency-operator/internal/journal"
)

const key = "state.json"

type Journal struct {
	reader client.Reader
	writer client.Client
	owner  *unstructured.Unstructured
	name   types.NamespacedName
}

func New(reader client.Reader, writer client.Client, owner *unstructured.Unstructured) *Journal {
	return &Journal{
		reader: reader, writer: writer, owner: owner,
		name: types.NamespacedName{Namespace: owner.GetNamespace(), Name: owner.GetName() + "-ft-state"},
	}
}

//nolint:gocritic // The contract returns the record, its version and an error.
func (store *Journal) Load(ctx context.Context) ([]byte, string, error) {
	var current corev1.ConfigMap
	err := store.reader.Get(ctx, store.name, &current)
	if apierrors.IsNotFound(err) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	var data []byte
	if value := current.Data[key]; value != "" {
		data = []byte(value)
	}
	return data, versionOf(&current), nil
}

func (store *Journal) Save(ctx context.Context, data []byte, previous string) (string, error) {
	if previous == "" {
		return store.create(ctx, data)
	}
	resourceVersion, digest, _ := strings.Cut(previous, "/")
	var current corev1.ConfigMap
	err := store.reader.Get(ctx, store.name, &current)
	switch {
	case apierrors.IsNotFound(err):
		return "", fmt.Errorf("%w: the record was deleted", journal.ErrConflict)
	case err != nil:
		return "", err
	case current.ResourceVersion != resourceVersion && hash(current.Data[key]) != digest:
		return "", fmt.Errorf("%w: the record changed", journal.ErrConflict)
	}
	if current.Data[key] == string(data) {
		return versionOf(&current), nil
	}
	// An edit that leaves the record alone, such as a new annotation, is not another writer.
	if current.Data == nil {
		current.Data = map[string]string{}
	}
	current.Data[key] = string(data)
	if err := store.writer.Update(ctx, &current); err != nil {
		return "", conflict(err)
	}
	return versionOf(&current), nil
}

func (store *Journal) create(ctx context.Context, data []byte) (string, error) {
	owner := store.owner.DeepCopy()
	if err := store.reader.Get(ctx, client.ObjectKeyFromObject(owner), owner); err != nil {
		return "", err
	}
	next := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: store.name.Namespace, Name: store.name.Name,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: owner.GetAPIVersion(), Kind: owner.GetKind(), Name: owner.GetName(), UID: owner.GetUID(),
			}},
		},
		Data: map[string]string{key: string(data)},
	}
	if err := store.writer.Create(ctx, next); err != nil {
		return "", conflict(err)
	}
	return versionOf(next), nil
}

func conflict(err error) error {
	if apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) || apierrors.IsNotFound(err) {
		return fmt.Errorf("%w: %w", journal.ErrConflict, err)
	}
	return err
}

func versionOf(state *corev1.ConfigMap) string {
	return state.ResourceVersion + "/" + hash(state.Data[key])
}

func hash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:8])
}
