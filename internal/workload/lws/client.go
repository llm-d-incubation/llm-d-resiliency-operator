// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

package lws

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

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
	Namespace, Name        string
	BasePort, ObserverPort int
}

type Client struct {
	HTTPClient *http.Client
	reader     client.Reader
	writer     client.Client
	config     Config
	mu         sync.Mutex
	state      *corev1.ConfigMap
	group      engine.Group
	layoutID   string
}

func New(reader client.Reader, writer client.Client, config Config) (*Client, error) {
	if config.BasePort == 0 {
		config.BasePort = 8000
	}
	if config.ObserverPort == 0 {
		config.ObserverPort = 9257
	}
	if reader == nil || writer == nil || config.Namespace == "" || config.Name == "" ||
		config.BasePort < 1 || config.BasePort > 65535 ||
		config.ObserverPort < 1 || config.ObserverPort > 65535 {
		return nil, fmt.Errorf("invalid LWS configuration")
	}
	return &Client{
		reader: reader, writer: writer, config: config,
		HTTPClient: &http.Client{Timeout: 2 * time.Second},
	}, nil
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
	identities := []string{string(set.GetUID())}
	addresses := []string{"size=" + strconv.Itoa(groupPods)}
	ordered := make([]*corev1.Pod, groupPods)
	seen := make(map[int]bool, len(pods.Items))
	for index := range pods.Items {
		pod := &pods.Items[index]
		worker, err := strconv.Atoi(pod.Labels[labelPrefix+"worker-index"])
		if err != nil || worker < 0 || worker >= groupPods || seen[worker] ||
			pod.Name != workload.podName(worker) {
			return engine.Group{}, fmt.Errorf("invalid worker identity for Pod %s", pod.Name)
		}
		seen[worker] = true
		ordered[worker] = pod
		identity, err := podIdentity(pod)
		if err != nil {
			return engine.Group{}, err
		}
		identities = append(identities, identity...)
		addresses = append(addresses, string(pod.UID)+"/"+pod.Status.PodIP)
	}
	slices.Sort(identities)
	identity, err := json.Marshal(identities)
	if err != nil {
		return engine.Group{}, err
	}
	group := engine.Group{ID: fmt.Sprintf("%x", sha256.Sum256(identity))}
	slices.Sort(addresses)
	// Keep the persisted group identity independent of the topology cache key.
	// Existing journals identify the same Pod/container incarnations on upgrade.
	cacheIdentity, err := json.Marshal(append(slices.Clone(identities), addresses...))
	if err != nil {
		return engine.Group{}, err
	}
	layoutID := fmt.Sprintf("%x", sha256.Sum256(cacheIdentity))
	workload.mu.Lock()
	if workload.layoutID == layoutID {
		group.Endpoints = slices.Clone(workload.group.Endpoints)
		workload.mu.Unlock()
		return group, nil
	}
	workload.mu.Unlock()

	localRanks := 0
	for worker, pod := range ordered {
		diagnosticsURL := "http://" + net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(workload.config.ObserverPort)) + "/status"
		ranks, err := workload.ranks(ctx, diagnosticsURL, string(pod.UID))
		if err != nil {
			return engine.Group{}, fmt.Errorf("discover original ranks for Pod %s: %w", pod.Name, err)
		}
		if worker == 0 {
			localRanks = len(ranks)
		}
		if len(ranks) != localRanks || localRanks > 65536-workload.config.BasePort ||
			groupPods > int(^uint(0)>>1)/localRanks {
			return engine.Group{}, fmt.Errorf("invalid or nonuniform original rank count for Pod %s", pod.Name)
		}
		for local, rank := range ranks {
			if rank != worker*localRanks+local {
				return engine.Group{}, fmt.Errorf("unexpected original rank %d for Pod %s", rank, pod.Name)
			}
			group.Endpoints = append(group.Endpoints, engine.Endpoint{
				Rank: rank, URL: "http://" + net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(workload.config.BasePort+local)),
				Pod: pod.Name, PodUID: string(pod.UID), DiagnosticsURL: diagnosticsURL,
			})
		}
	}
	// Original membership is immutable for this Pod/container incarnation. Keep
	// discovery usable if its observer disappears during fault recovery.
	workload.mu.Lock()
	workload.group = group
	workload.group.Endpoints = slices.Clone(group.Endpoints)
	workload.layoutID = layoutID
	workload.mu.Unlock()
	return group, nil
}

func (workload *Client) ranks(ctx context.Context, url, podUID string) ([]int, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, err
	}
	response, err := workload.HTTPClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("observer returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(body) > 1<<20 {
		return nil, fmt.Errorf("observer response exceeds size limit")
	}
	var payload struct {
		SchemaVersion int    `json:"schema_version"`
		PodUID        string `json:"pod_uid"`
		Ranks         []struct {
			ID *int `json:"id"`
		} `json:"ranks"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	if payload.SchemaVersion != 1 || payload.PodUID != podUID || len(payload.Ranks) == 0 {
		return nil, fmt.Errorf("invalid observer schema, Pod identity or original rank list")
	}
	ranks := make([]int, len(payload.Ranks))
	for index, rank := range payload.Ranks {
		if rank.ID == nil || *rank.ID < 0 {
			return nil, fmt.Errorf("missing or negative original rank ID")
		}
		ranks[index] = *rank.ID
	}
	slices.Sort(ranks)
	return ranks, nil
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
