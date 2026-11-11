# End-to-end deployment

The real-cluster stack for fault-injection testing: a LeaderWorkerSet holding one vLLM group with fault
tolerance enabled, and IRO watching it.

| File | What it is |
|---|---|
| `lws.yaml` | the LWS and the engine Pod |
| `iro.yaml` | IRO, its ServiceAccount, Role and RoleBinding |

## Variables

Both files are rendered with `envsubst`; a variable with no default must be set:

| Variable | Used by | Required | Default | Meaning |
|---|---|---|---|---|
| `NAMESPACE` | both | yes | | namespace of the LWS, the journal and IRO |
| `VLLM_IMAGE` | lws | yes | | vLLM image with the fault-tolerance API |
| `IRO_IMAGE` | iro | yes | | operator image |
| `LWS_NAME` | both | no | `dsv2lite` | LeaderWorkerSet name and Pod prefix |
| `RANKS` | both | no | `4` | data parallel ranks, one GPU each |
| `API_PORT` | both | no | `8000` | first rank's HTTP port; rank `i` listens on this port plus `i` |
| `RPC_PORT` | lws | no | `5555` | data parallel RPC port |
| `MODEL` | lws | no | `deepseek-ai/DeepSeek-V2-Lite` | Hugging Face model id |
| `IMAGE_PULL_SECRET` | both | no | `ghcr-pull-secret` | secret holding registry credentials |
| `MODEL_CACHE_PVC` | lws | no | `model-cache-pvc` | claim that holds the weights between Pod recreations |
| `SRIOV_NETWORK` | lws | no | `default/compute-p1v0` | network-attachment-definition for the RDMA VF |
| `GPU_RESOURCE` | lws | no | `nvidia.com/gpu` | GPU resource name |
| `RDMA_RESOURCE` | lws | no | `rdma/roce_gdr` | RDMA resource name |
| `SRIOV_RESOURCE` | lws | no | `openshift.io/p1_sriov_nodepolicy` | SR-IOV VF resource name |

## Apply

Export the values and apply:

```sh
export CTX=...   # kubectl context

envsubst '${NAMESPACE} ${VLLM_IMAGE} ${LWS_NAME} ${RANKS} ${API_PORT} ${RPC_PORT} ${MODEL} ${IMAGE_PULL_SECRET} ${MODEL_CACHE_PVC} ${SRIOV_NETWORK} ${GPU_RESOURCE} ${RDMA_RESOURCE} ${SRIOV_RESOURCE}' \
  < hack/e2e/lws.yaml | kubectl --context "$CTX" apply -f -
envsubst '${NAMESPACE} ${IRO_IMAGE} ${LWS_NAME} ${RANKS} ${API_PORT} ${IMAGE_PULL_SECRET}' \
  < hack/e2e/iro.yaml | kubectl --context "$CTX" apply -f -
```

Keep the allowlist as written. The container command in `lws.yaml` contains shell variables of its own
(`${i}`, `${RAILS:+$RAILS,}`, `${LWS_LEADER_ADDRESS}`), which a bare `envsubst` would replace.

The order does not matter: IRO starts and waits for the LeaderWorkerSet to appear.

## Prerequisites

- The namespace exists, holding the image pull secret and the model cache claim named by `IMAGE_PULL_SECRET`
  and `MODEL_CACHE_PVC`.
- The LeaderWorkerSet controller is installed cluster-wide.
- One node has `RANKS` GPUs, one device from `RDMA_RESOURCE` and one from `SRIOV_RESOURCE` free.
- The SR-IOV device plugin advertises those resources, and `SRIOV_NETWORK` names a
  network-attachment-definition in the namespace.
- `VLLM_IMAGE` serves the fault-tolerance API (`/v1/fault_tolerance/status`); `IRO_IMAGE` is the operator built
  from this repository.

## What it creates

| Object | Name | Notes |
|---|---|---|
| LeaderWorkerSet | `LWS_NAME` | one Pod, `LWS_NAME-0`, running `RANKS` vLLM servers |
| Deployment, ServiceAccount, Role, RoleBinding | `iro` | in `NAMESPACE`, leader-elected |
| ConfigMap | `LWS_NAME-ft-state` | the journal, owned by the LeaderWorkerSet |

IRO and the LWS share the namespace. IRO derives the members from `RANKS` and `API_PORT`: rank `n` of the Pod
at index `i` is `dp<i*RANKS+n>` at `pod-ip:API_PORT+n`. Use the same `NAMESPACE`, `LWS_NAME`, `RANKS` and
`API_PORT` in both files.

The IRO objects are named `iro`. Two groups in one namespace need those names changed, or separate namespaces.

If the engine Pod has extra containers, add `--lws-engine-containers` to IRO's args naming the containers that
run the engine. By default every container counts, and a restart of any of them changes the Pod's identity.

## Wait for the group

```sh
kubectl --context "$CTX" -n "$NAMESPACE" get cm "${LWS_NAME}-ft-state" \
  -o jsonpath='{.data.state\.json}' | python3 -m json.tool
```

The group is ready when `phase` is `serving` and every member is `admitted`. IRO's log is at
`kubectl --context "$CTX" -n "$NAMESPACE" logs deploy/iro`.

## Remove

```sh
kubectl --context "$CTX" -n "$NAMESPACE" delete leaderworkerset "$LWS_NAME"
kubectl --context "$CTX" -n "$NAMESPACE" delete deployment/iro serviceaccount/iro role/iro rolebinding/iro
```

Deleting the LeaderWorkerSet removes its Pod and the journal ConfigMap through the owner reference.
