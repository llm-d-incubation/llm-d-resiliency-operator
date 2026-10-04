// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

package lws

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"slices"
	"strconv"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-resiliency-operator/internal/engine"
)

const labelPrefix = "leaderworkerset.sigs.k8s.io/"

type Config struct {
	Namespace, Name                    string
	LocalRanks, BasePort, ObserverPort int
}

type Client struct {
	reader client.Reader
	writer client.Client
	config Config
	mu     sync.Mutex
	state  *corev1.ConfigMap
}

func New(reader client.Reader, writer client.Client, config Config) (*Client, error) {
	if config.BasePort == 0 {
		config.BasePort = 8000
	}
	if config.ObserverPort == 0 {
		config.ObserverPort = 9257
	}
	if reader == nil || writer == nil || config.Namespace == "" || config.Name == "" ||
		config.LocalRanks < 1 || config.BasePort < 1 ||
		config.BasePort > 65535 || config.LocalRanks > 65536-config.BasePort ||
		config.ObserverPort < 1 || config.ObserverPort > 65535 {
		return nil, fmt.Errorf("invalid LWS configuration")
	}
	return &Client{reader: reader, writer: writer, config: config}, nil
}

func (workload *Client) Discover(ctx context.Context) (engine.Group, error) {
	set := &unstructured.Unstructured{}
	set.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "leaderworkerset.x-k8s.io", Version: "v1", Kind: "LeaderWorkerSet",
	})
	if err := workload.reader.Get(ctx, client.ObjectKey{
		Namespace: workload.config.Namespace, Name: workload.config.Name,
	}, set); err != nil {
		return engine.Group{}, err
	}
	groupPods, err := workload.validate(set)
	if err != nil {
		return engine.Group{}, err
	}
	var pods corev1.PodList
	if err := workload.reader.List(ctx, &pods, client.InNamespace(workload.config.Namespace), client.MatchingLabels{
		labelPrefix + "name": workload.config.Name, labelPrefix + "group-index": "0",
	}); err != nil {
		return engine.Group{}, err
	}
	if len(pods.Items) != groupPods {
		return engine.Group{}, fmt.Errorf("expected %d group Pods from LWS size, found %d", groupPods, len(pods.Items))
	}
	group := engine.Group{}
	identities := []string{string(set.GetUID())}
	seen := make(map[int]bool, len(pods.Items))
	for index := range pods.Items {
		pod := &pods.Items[index]
		worker, err := strconv.Atoi(pod.Labels[labelPrefix+"worker-index"])
		if err != nil || worker < 0 || worker >= groupPods || seen[worker] ||
			pod.Name != workload.podName(worker) {
			return engine.Group{}, fmt.Errorf("invalid worker identity for Pod %s", pod.Name)
		}
		seen[worker] = true
		identity, err := podIdentity(pod)
		if err != nil {
			return engine.Group{}, err
		}
		identities = append(identities, identity...)
		for local := 0; local < workload.config.LocalRanks; local++ {
			group.Endpoints = append(group.Endpoints, engine.Endpoint{
				Rank: worker*workload.config.LocalRanks + local,
				URL:  "http://" + net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(workload.config.BasePort+local)),
				Pod:  pod.Name, PodUID: string(pod.UID),
				DiagnosticsURL: "http://" + net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(workload.config.ObserverPort)) + "/status",
			})
		}
	}
	slices.Sort(identities)
	slices.SortFunc(group.Endpoints, func(first, second engine.Endpoint) int { return first.Rank - second.Rank })
	identity, err := json.Marshal(identities)
	if err != nil {
		return engine.Group{}, err
	}
	group.ID = fmt.Sprintf("%x", sha256.Sum256(identity))
	return group, nil
}

func (workload *Client) validate(set *unstructured.Unstructured) (int, error) {
	var value struct {
		Spec struct {
			Replicas             *int64 `json:"replicas"`
			GroupIdentity        string `json:"groupIdentity"`
			LeaderWorkerTemplate struct {
				Size          *int64 `json:"size"`
				RestartPolicy string `json:"restartPolicy"`
			} `json:"leaderWorkerTemplate"`
		} `json:"spec"`
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(set.Object, &value); err != nil {
		return 0, err
	}
	if !set.GetDeletionTimestamp().IsZero() || value.Spec.Replicas == nil || *value.Spec.Replicas != 1 ||
		value.Spec.LeaderWorkerTemplate.Size == nil || *value.Spec.LeaderWorkerTemplate.Size < 1 ||
		value.Spec.LeaderWorkerTemplate.RestartPolicy != "RecreateGroupOnPodRestart" ||
		(value.Spec.GroupIdentity != "" && value.Spec.GroupIdentity != "Ordinal") {
		return 0, fmt.Errorf("requires one ordinal LWS group with positive size and RecreateGroupOnPodRestart")
	}
	groupPods := int(*value.Spec.LeaderWorkerTemplate.Size)
	if int64(groupPods) != *value.Spec.LeaderWorkerTemplate.Size {
		return 0, fmt.Errorf("LWS group size exceeds supported integer range")
	}
	return groupPods, nil
}

func podIdentity(pod *corev1.Pod) ([]string, error) {
	if pod.UID == "" || !pod.DeletionTimestamp.IsZero() || net.ParseIP(pod.Status.PodIP) == nil ||
		len(pod.Spec.Containers) == 0 || len(pod.Status.ContainerStatuses) != len(pod.Spec.Containers) ||
		len(pod.Status.InitContainerStatuses) != len(pod.Spec.InitContainers) {
		return nil, fmt.Errorf("pod %s is starting or terminating", pod.Name)
	}
	identities := []string{string(pod.UID)}
	for _, item := range []struct {
		containers []corev1.Container
		statuses   []corev1.ContainerStatus
		init       bool
	}{
		{pod.Spec.Containers, pod.Status.ContainerStatuses, false},
		{pod.Spec.InitContainers, pod.Status.InitContainerStatuses, true},
	} {
		for containerIndex := range item.containers {
			name := item.containers[containerIndex].Name
			index := slices.IndexFunc(item.statuses, func(status corev1.ContainerStatus) bool { return status.Name == name })
			if index < 0 {
				return nil, fmt.Errorf("pod %s has no status for container %s", pod.Name, name)
			}
			status := item.statuses[index]
			completed := item.init && status.State.Terminated != nil && status.State.Terminated.ExitCode == 0
			if status.ContainerID == "" || (status.State.Running == nil && !completed) {
				return nil, fmt.Errorf("pod %s container %s is not running", pod.Name, name)
			}
			identities = append(identities, fmt.Sprintf("%s/%s/%s/%d", pod.UID, status.Name, status.ContainerID, status.RestartCount))
		}
	}
	return identities, nil
}

func (workload *Client) podName(index int) string {
	name := workload.config.Name + "-0"
	if index > 0 {
		name += "-" + strconv.Itoa(index)
	}
	return name
}

// Reset fences the old leader; LWS recreates the group.
func (workload *Client) Reset(ctx context.Context, group engine.Group) error {
	if len(group.Endpoints) == 0 {
		return fmt.Errorf("missing leader identity")
	}
	leader := group.Endpoints[0]
	if leader.Rank != 0 || leader.Pod != workload.podName(0) || leader.PodUID == "" {
		return fmt.Errorf("unexpected leader identity")
	}
	uid := types.UID(leader.PodUID)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: workload.config.Namespace, Name: leader.Pod}}
	err := workload.writer.Delete(ctx, pod, client.Preconditions{UID: &uid})
	if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
		return nil
	}
	return err
}

func (workload *Client) Load(ctx context.Context) ([]byte, error) {
	workload.mu.Lock()
	defer workload.mu.Unlock()
	var state corev1.ConfigMap
	key := client.ObjectKey{Namespace: workload.config.Namespace, Name: workload.config.Name + "-ft-state"}
	err := workload.reader.Get(ctx, key, &state)
	if apierrors.IsNotFound(err) {
		workload.state = nil
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	workload.state = &state
	return []byte(state.Data["state.json"]), nil
}

// Save uses the version loaded by this coordinator, never overwriting a newer writer.
func (workload *Client) Save(ctx context.Context, data []byte) error {
	workload.mu.Lock()
	defer workload.mu.Unlock()
	state := workload.state.DeepCopy()
	if state == nil {
		state = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
			Namespace: workload.config.Namespace, Name: workload.config.Name + "-ft-state",
		}}
	}
	if state.Data == nil {
		state.Data = make(map[string]string)
	}
	state.Data["state.json"] = string(data)
	var err error
	if workload.state == nil {
		err = workload.writer.Create(ctx, state)
	} else {
		err = workload.writer.Update(ctx, state)
	}
	if err == nil {
		workload.state = state
	}
	return err
}
