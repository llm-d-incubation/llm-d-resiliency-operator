# vLLM runtime bridge

Mount this directory in the vLLM container and install `requirements.txt` in its Python environment. The runtime must provide fault-tolerant multi-port serving, original DP rank IDs, native `vllm:engine_healthy`, and a supervisor that preserves surviving ranks during diagnosis and FT. This bridge currently supports TP=1.

`launch.sh MODEL [vLLM options]` starts the process observer and executes vLLM. Supply the model's communication backend, expert redundancy, and FT timeout as vLLM options. Configure:

| Variable | Value |
| --- | --- |
| `POD_UID` | Downward API `metadata.uid` |
| `IRO_RANK_START` | First original global DP rank in this Pod |
| `IRO_LOCAL_RANKS` | Number of local ranks, at least 2 |
| `IRO_DP_SIZE` | Original group size |
| `IRO_DP_ADDRESS` | Initial DP coordinator address |
| `IRO_OBSERVER_PORT` | Diagnostic port; default 9257 |
| `IRO_SUPERVISOR_PORT` | Supervisor port; default 9256 |

For LWS, compute the start rank as worker index × local rank count in the workload launcher. Keep it unchanged after exclusion. Use supervisor `/health` and `/ready` for container probes, with LWS `RecreateGroupOnPodRestart`.

`rank_observer.py --start-rank N --local-ranks N` exposes `/status`. It retains the first observed API, EngineCore, and worker PID/creation time. Confirmed EngineCore/worker exit or replacement records process-loss evidence; API-only exit and unreadable processes do not. IRO preserves the engine's native serving state separately. Run it in the engine's PID namespace and keep the diagnostic port internal. Evidence is scoped to the Pod UID.

EPP selects each API port using native `vllm:engine_healthy`. The engine rejects inference while its native engine status is not healthy. There is no IRO middleware, admission metric, shared verification token, or request-time call to the operator. Healthy inference continues through an IRO outage; recovery coordination resumes when the elected operator returns.

IRO's ConfigMap journal records recovery progress and verified ranks. This is an observation, not an admission gate: native health can restore routing before IRO completes verification, and replacement endpoint discovery can delay gateway availability after verification. Failed verification requests an LWS reset.

Safe exclusion requires the failed endpoint to report native terminal `dead`. Process-exit evidence prevents a stale healthy endpoint from being treated as recovered, but it does not change the metric exposed by that endpoint. Mask-only stalls and observer-only failures take bounded diagnosis and reset unless native terminal death is established. The runtime must not restart excluded processes in the same group. If an excluded endpoint reports a live state, IRO requests reset; this is detection of a runtime contract violation, not an instantaneous routing fence.

`epp-config.yaml` shows the llm-d v0.10.0 native-health filter. Enable experimental plugins and include every per-rank API port in the InferencePool. Missing metrics and empty candidate sets fail closed. Detection and metrics propagation are asynchronous, and requests already in flight can fail. Do not use the supervisor's Pod-level `/ready` or liveness `/health` as a per-rank serving signal.

Run tests with `python3 -m pytest runtime/vllm` after installing `requirements-test.txt`.
