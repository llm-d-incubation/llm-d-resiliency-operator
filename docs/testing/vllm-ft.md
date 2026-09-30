# vLLM fault recovery validation

Run `make test` for the Go race suite and `python -m pytest runtime/vllm -q` after installing `runtime/vllm/requirements-test.txt`. Tests cover coordinated dispatch, partial acceptance, native status and identity checks, terminal-only exclusion, bounded reset, journal conflicts, restart without replay and operator shutdown. Render the example with `kubectl kustomize deploy/vllm-ft`.

## Cluster fixture
Four H200 GPUs, two LWS Pods, two local ranks per Pod, DP4/EP4/TP1. The compatible runtime is `vllm/vllm-openai@sha256:5f1142f7ceea906a61bc46c76b1f1d562c2d4898f604e1f6cd3620ceafd9ce93` with FT/supervisor/native-health patches. This validates that runtime contract, not an arbitrary released vLLM image.

| Model | Revision | Redundant experts | Minimum survivors |
| --- | --- | --- | --- |
| DeepSeek-V2-Lite | 604d5664dddd88a0433dbae533b7fe9472482de0 | 64 | 2 |
| Qwen3-30B-A3B | ad44e777bcd18fa416d9da3bd8f70d33ebb85d39 | 64 | 3 |

## Earlier measurements
September 29–30 single-trial measurements used native EPP health filtering. Under four continuous clients requesting up to 32 output tokens, DeepSeek worker-rank-1 exclusion restored sustained gateway responses in 31.9 s. Its 225.5 s reset result used a different live-stall fault after exclusion, including IRO's diagnosis wait; it is not a matched no-FT baseline.

Qwen's matched worker-rank-1 SIGKILL trial restored sustained gateway responses in 35.5 s with FT/IRO, versus 143.6 s with FT disabled and normal LWS recreation. Surviving identities were preserved with FT; both Pods were replaced without FT. Four deterministic completion checks matched in both runs. The no-FT EPP configuration omitted the FT-only health filter because that runtime does not export the metric when FT is disabled. These are individual observations, not a recovery SLA.

Healthy throughput at concurrency 16 was 517.88 output tokens/s for DeepSeek and 347.41 for Qwen (64 requests, 128 input/output tokens, warmed repeated prefix). DeepSeek with three survivors measured 533.86 tokens/s at the same offered concurrency. This load did not establish maximum capacity or equal capacity for three and four ranks.

## Reduced implementation: September 30 repeat

Linux/amd64 manager SHA256: `aba3ae13a376b0afb8ed7a5b5a6e43d69e2dbc6b5f64491bd56bc12fac34a07d` (Go 1.25). Go race tests, lint for all new Go packages, four observer tests, Ruff and Kustomize passed. Full-repository lint reports 18 issues in unchanged base files. The binary ran inside the pinned fixture image; a new operator container image was not built locally.

| Fault | Sustained gateway recovery | Result |
| --- | --- | --- |
| One-shot RuntimeError on worker rank 1 | 31.8 s | Retry all four; identities preserved; 4/4 deterministic outputs matched. |
| SIGKILL worker rank 1 | 32.2 s | Exclude 1; verify 0/2/3; survivor identities preserved; 4/4 outputs matched. |
| SIGSTOP worker rank 3 after exclusion | 234.9 s | Request reset at 105.7 s; replace both Pods; verify all four; 4/4 outputs matched. The live stalled rank was never excluded. |
| Qwen worker rank 1 SIGKILL | 34.8 s | Exclude 1; verify 0/2/3; survivor identities preserved; 4/4 outputs matched. |

These single-trial recovery timings use four continuous clients, temperature 0, a fixed France-capital prompt and up to 32 output tokens. Recovery is the first valid response after the final observed failure, followed by at least 30 seconds and 60 valid completions. Requests failed during every fault. The stall/reset timing includes diagnosis and model startup; it is not a no-FT baseline.

At concurrency 16, DeepSeek completed 64/64 requests at 517.52 output tokens/s while healthy and 537.32 with three survivors while IRO was stopped. Each request reused a warmed 128-token prefix and generated exactly 128 output tokens. Restarting IRO preserved the exclusion and journal round. Surviving Pod/container/process identities stayed intact; the excluded rank's API and EngineCore exited later without replacement. Qwen also completed 64/64 healthy requests at concurrency 16, measuring 367.00 output tokens/s under the same prompt/token settings. These are offered-load checks, not capacity measurements.

## Repeat validation
Record the exact operator binary/runtime, model revision, topology and offered load. Confirm valid gateway completions and native health, compare deterministic outputs, and preserve Pod/container/surviving-process identities for in-place recovery. A reset must replace the complete old group. Check healthy serving with IRO stopped and restart reconciliation without command replay.

Exercise retry, terminal worker exclusion and live-stall reset separately. Missing APIs, masks and process observations alone cannot authorize exclusion. Check that excluded incarnations remain non-serving, and validate capacity for every supported rank combination. Native-routing API-only/EngineCore loss, idle faults, multiple-rank backend recovery, store-master exclusion, wider TP, multiple groups and node failures still need cluster coverage. Full-repository lint has 18 pre-existing issues in the `initialcontroller` CRD and RecoveryRequest code; report those separately from feature checks.
