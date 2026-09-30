# vLLM engine fault recovery

## Summary
Implement the engine recovery tracks in the [IRO proposal](https://docs.google.com/document/d/1Jx2Y3bwwoemy1wk9ZLykPYvPYpVpKDpn/edit) as an opt-in controller inside the existing operator manager. Retry usable engines or exclude terminally dead ranks; fall back to LWS group recreation when recovery cannot restore inference.

## Motivation
LWS already recreates workloads after Pod/container failure. Engine faults can leave those containers alive, so engine recovery can preserve surviving processes and loaded model state.

Goals: coordinate vLLM recovery, verify inference and bound unsuccessful attempts. Non-goals: process repair/rejoin, capacity restoration, request replay, infrastructure repair and changes to RecoveryRequest or LWS restart policy.

## Proposal
Enable with `--enable-vllm-ft --leader-elect`. Only the elected leader coordinates recovery. IRO stays outside the request path: EPP uses native per-rank `vllm:engine_healthy`, and healthy inference continues while IRO is unavailable.

The initial scope is one ordinal LWS group (`replicas: 1`), uniform local DP ranks, TP=1 and `RecreateGroupOnPodRestart`. The runtime must provide multi-port FT APIs, terminal `DEAD` fencing and a supervisor that preserves usable ranks during recovery. Expert redundancy and the minimum survivor count require validation for each model/backend.

## Design details
The [adapter](../../internal/engine/vllm) polls each original rank's `GET /v1/fault_tolerance/status` and concurrently sends `POST /v1/fault_tolerance/apply` to participants with one shared request ID. `ResumeEngine` maps to `retry`; `ScaleDown` maps to `scale_down`. Polling supplies fault observations; unused proposal capabilities are deferred.

| Engine state | Action |
| --- | --- |
| All active ranks healthy, no FT in progress | Verify inference after startup/recovery. |
| All active ranks unhealthy, no confirmed process loss or FT in progress | Retry all active original ranks. |
| Native terminal `DEAD`, eligible unhealthy survivors, sufficient validated capacity | Exclude dead original ranks and verify survivors. Replace the FT store address when excluding its current master. |
| API-only loss with healthy peers | Verify peer inference without changing EP membership. |
| Live stall, ambiguous diagnosis, insufficient capacity or unsuccessful FT | Bounded diagnosis/recovery, then LWS reset. |

Process observations and survivor masks cannot authorize exclusion: an unreachable or stalled rank may still advertise healthy when it returns. The Pod-scoped [observer](../../runtime/vllm) only prevents confirmed process loss from being mistaken for healthy or retryable state. An internal inference probe lets idle peers enter collective diagnosis. Native terminal death must fence the excluded incarnation; revival triggers reset rather than rejoin.

The [coordinator](../../internal/controller/enginefault) persists intent in `<lws>-ft-state` before dispatch, then reconciles native status and errors. HTTP 202 means acceptance, not completion. Operator restart reconciles the recorded round without replay. Completion requires direct inference probes followed by another workload-identity and native-health check.

The [LWS client](../../internal/workload/lws) includes Pod/container identities in discovery, uses optimistic concurrency for journal writes, and requests recreation by deleting the leader with a UID precondition. Partial replacement cannot rejoin the old group. Startup deadlines and a reset budget surface unresolved failures instead of resetting indefinitely.

`verified` records successful probes; it does not admit requests. Native health may restore routing before verification, and endpoint discovery may delay gateway recovery. In-flight requests can fail. Deployment instructions are in [deploy/vllm-ft](../../deploy/vllm-ft); validation and remaining coverage are in [testing](../testing/vllm-ft.md).

## Alternatives
Always recreate LWS: simpler, but discards usable ranks and model state. Process repair, scale-up and infrastructure recovery need separate lifecycle work.

## References
- [vLLM FT API](https://github.com/vllm-project/vllm/pull/46370)
- [Multi-port supervisor recovery](https://github.com/vllm-project/vllm/pull/54963)
- [Native per-rank health](https://github.com/vllm-project/vllm/pull/55915)
