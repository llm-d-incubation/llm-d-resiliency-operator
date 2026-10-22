# vLLM engine

How vLLM implements the [engine contract](index.md), on top of the vLLM fault tolerance API
from [vllm-project/vllm#46370](https://github.com/vllm-project/vllm/pull/46370). Code: `internal/engine/vllm`.

## What vLLM gives

Each data parallel rank runs its own API server (external load balancing, `--data-parallel-size-local 1`).
On `--enable-fault-tolerance`, a rank that hits a fault stops serving and waits for an instruction:

![A vLLM rank's fault-tolerance states](../../figures/vllm-states.svg)

| Endpoint | Use |
|---|---|
| `GET /v1/fault_tolerance/status` | `status`, `fault_info`, `mask`, and for the last instruction `ft_state`, `last_ft_request_id`, `ft_error` |
| `POST /v1/fault_tolerance/apply` | `retry` or `scale_down`; `202` means queued, and the result, including a refusal, shows in the status |
| `GET /health` | `503` once the engine process is gone |

Rules that shape the driver:

- An instruction is accepted only while the rank is `unhealthy`.
- Every alive rank must get the same `request_id` within 10 seconds, or the reconfig fails.
- `scale_down` keeps the original rank numbers. Removing the lowest alive rank also moves the store it
  hosts, so the request carries the next lowest rank's address and a port.
- A failed `scale_down` is final: the ranks report `dead`.
- The status is cached by the API server. If the engine process is killed, it can still say `healthy`;
  `/health` is what notices.

## Driver

| Contract | vLLM |
|---|---|
| Members per Pod | `--vllm-ranks-per-pod` ranks; rank `i` of Pod `Index` is `dp<Index × ranks + i>` on port `--vllm-api-port + i` |
| `Observe` | status and `/health` of every rank of every Pod that has an IP and is not terminating |
| `Serving` | status `healthy` and `/health` 200 |
| `Terminal` | status `dead`, or `/health` 503 |
| `Unknown` | the API does not answer, or answers for another rank. If IRO can read no admitted rank, it waits until `ObservationTimedOut` is set, then the policy requests `RecreateGroup`; if it can read some, the unknown ranks are a fault. |
| `Apply` for `Probe` | one short completion to each serving rank, so idle peers run a step and notice a dead rank |
| `Apply` for `Change` | `retry` when nothing is removed, else `scale_down` with `removed_dp_ranks`; `request_id` is the reconfig ID. Refused before anything is sent when a participant cannot be reached or a removed rank is not terminal in the observation. A lost rank needs no address: its number comes from its name. |
| Store host | when the reconfig removes the lowest alive rank: `dp_master_ip` of the lowest participant and `--vllm-store-port` plus its rank, so a second move inside one Pod does not reuse the port the previous host still holds. Ports from `--vllm-store-port` up to it plus the group size must be free. |
| `Apply` for `Verify` | an 8-token completion on each member; the error names every rank that failed |
| `Grow` | no: the fault tolerance API cannot add ranks |
| Same reconfig sent twice | `Apply` sends only on its first authorized attempt. Resumed calls inspect status. The API does not safely deduplicate a second send after a completed request or later fault. |

Probe and Verify use ordinary completion requests and count toward vLLM request and token metrics.
The API provides no option to exclude them; dashboards and traffic accounting must allow for this traffic.
Verify sets `ignore_eos` and requires 8 completion tokens, including tokens hidden from the response text.

## Policy

The vLLM policy stores its recovery checkpoint in the controller journal. It chooses the next action from
that checkpoint, the latest observation and the last completed operation.

| Observation or result | Action |
|---|---|
| initial ranks all serving | `Verify`, then `Healthy` after success |
| a new fault | `Probe` once, recording the fault start in policy state |
| none left alive | `RecreateGroup` |
| one unknown, `diagnosing`, or running an instruction | `Wait` until the diagnosis interval expires, then `RecreateGroup` |
| some faulted, some still `healthy` | `Wait`: vLLM rejects instructions until every survivor reports `unhealthy` |
| some terminal, every survivor `unhealthy` | `Change`: remove the terminal ranks |
| none terminal, all `unhealthy` | `Change`: retry, unless a previous retry falls within `FlapWindow` |
| reconfig succeeded | `Verify` its participants |
| verification failed | retry `Verify` within `VerifyTimeout`, measured from the first failure; otherwise `RecreateGroup` |
| group-scoped failure or unsafe outcome | `RecreateGroup` |
| ranks serving and required verification passed | `Healthy`, or a configured capacity action |

The checkpoint preserves consumed result IDs, fault and verification start times, and the most recent
membership-preserving retry. A repeated observation or leader change does not restart those limits.

vLLM cannot admit replacement ranks. Its policy requests `ReplaceMembers` for excluded or rejected members
after verification, and records that request in its checkpoint. The controller maps it using capacity
configuration: `reset-pod` keeps reduced capacity, `release` releases eligible Pods, and `reset-all` recreates
the group. A shared Pod containing admitted ranks is never eligible for individual release.

A rank that stays `healthy` while its peers fault never accepts an instruction. The policy waits for diagnosis
and chooses `RecreateGroup` when its saved diagnosis interval expires.

| During a reconfig | `Apply` report |
|---|---|
| a participant is terminal | `Failed`, scope `Group` |
| `ft_state` is `failed` for this reconfig | `Failed`, scope `Group` |
| a participant runs another `request_id` | `Running`; the saved operation outcome remains unknown |
| a participant is `unhealthy` with no `request_id`: it finished the reconfig and faulted after it, or never got it | `Running`; no blind resubmission |
| every participant serving, with no conflicting request ID or recovery in progress | `Succeeded` |
| otherwise | `Running` |
| resumed call with an ambiguous request outcome | `Running`; preserve the operation. The status drops `last_ft_request_id` at the next state report, so an unsent request can resemble one that finished before another fault. The driver does not resend without proof. |

## Flags

| Flag | Default |
|---|---|
| `--vllm-api-key-file` | unset; file containing the API key, read at startup |
| `--vllm-https` | false; enable TLS with certificate verification |
| `--vllm-ca-file` | unset; PEM CA bundle added to system roots, requires HTTPS |
| `--vllm-api-port` | 8000 |
| `--vllm-ranks-per-pod` | 1 |
| `--vllm-store-port` | 29600 |
| `--vllm-timeout` | 5s per status, health or apply request |

## Deploying vLLM for IRO

For vLLM with `--api-key`, mount the key as a Secret file and pass its path with
`--vllm-api-key-file`. IRO sends a Bearer header on every request. Restart IRO after changing the key.
For TLS, set `--vllm-https` and optionally `--vllm-ca-file` for a private CA.
The server certificate must cover the Pod IP used by IRO.

- After `scale_down`, a removed rank stays dead. Its Pod remains while it hosts surviving ranks;
  the removed rank cannot rejoin and keeps its GPU until the group is recreated.
- Set `VLLM_KEEP_ALIVE_ON_ENGINE_DEATH=1`. Otherwise the API server exits with its engine and its container
  restarts: under `RecreateGroupOnPodRestart` the workload recreates the whole group and IRO never gets to
  shrink it; under `None` IRO removes the rank only once the restart changes its Pod's identity, and the
  restarted rank cannot rejoin.
- No liveness probe on vLLM containers. A probe on the status or on a completion fails on every rank paused
  for recovery, and the restart loses that rank: under `RecreateGroupOnPodRestart` the group is recreated;
  under `None` the rank is removed with the dead one, or the reconfig in flight fails and the group is reset.
  A restarted rank never rejoins and holds its GPU until the next group. Under `None`, `GET /health` is the
  only safe target: it stays 200 while a rank is paused and turns 503 only when the engine process is gone.
- With sidecars in the Pod, name the vLLM container in `--lws-engine-containers`, and point
  `--vllm-api-port` at vLLM's own port, not a proxy's.
- `/health` stays 200 for a rank that is `dead` while its engine process lives, so a `/health` readiness
  probe keeps sending traffic to it, and it answers 503. Readiness that routes well checks the status
  endpoint for `healthy`.
- `engine_recovery_timeout_sec` must leave room for detection plus IRO's diagnosis deadline.
- `scale_down` needs EPLB with redundant experts and an all2all backend with timeouts
  (`nixl_ep` or `deepep_low_latency`).
- Size the redundant experts for the ranks you want to lose in place: the survivors' slots must hold every
  logical expert. With E experts, R redundant and N ranks, losing k ranks needs (N - k)(E + R)/N >= E.
  Otherwise the reconfig fails ("no redundant slot for missing expert") and the group is reset.
- `nixl_ep` from NIXL 1.4 on uses GPU-initiated RDMA across Pods. Restrict `UCX_NET_DEVICES` to the RDMA
  devices the Pod can use for GPU memory, and make sure the nodes support it; within one Pod it runs over NVLink.

## Known engine issues

Found with vllm#46370 (`384ba19`, then `fcfbd37`) on two setups: 4 H200 Pods of one rank each over InfiniBand
(DeepSeek-V2-Lite, `nixl_ep` from NIXL 1.3.2), and one Pod of 4 H100 ranks over NVLink (Qwen3-30B-A3B,
NIXL 1.3.2 and 1.5.0).

| Issue | What IRO does |
|---|---|
| With NIXL before 1.4, removing the highest rank succeeds, then the next step fails `x.size(1) == active_rank_bound * num_max_dispatch_tokens_per_rank` in `nixl_ep`. NIXL 1.5.0, which vLLM pins, removes it in place. For NIXL 1.3, `Etelis/vllm` branch `ft-nixl-ep-highest-rank` trims the expert output to the dispatched slots. | `Apply` reports the established reconfig failure on the new fault and the group is reset |
| A Pod that disappears (not just its processes) can wedge the survivors: their next step fails on a Gloo connection reset, but the `nixl_ep` fault of the step in flight is never raised, so the EngineCore never learns of it and blocks on the shared-memory broadcast with status `healthy`. Seen in 1 of 2 Pod deletions at `384ba19`, none of 11 at `fcfbd37`. | the policy waits; the diagnosis deadline resets the group |
| The DP coordinator (rank 0) dying goes unnoticed under load; the first request after an idle period faults the group | the probe wakes the idle rank, then a retry; a second one within `FlapWindow` resets the group |
| NIXL 1.4 and later fail across Pods on nodes without GPU-initiated RDMA: `invalid memh` on unusable devices, then `lane not found` | none: the group does not start |
