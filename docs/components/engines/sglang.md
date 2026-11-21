# SGLang engine

How SGLang implements the [engine contract](index.md), on top of the SGLang fault tolerance API from
[sgl-project/sglang#40078](https://github.com/sgl-project/sglang/pull/40078), which is not merged yet.
Code: `internal/engine/sglang`. Select it with `--engine=sglang`.

## What SGLang gives

One SGLang server runs every data parallel rank of the group, across Pods with `--nnodes` and `--node-rank`.
Only the first Pod serves HTTP. On `--enable-fault-tolerance`, a central FT manager on that Pod tracks every DP
rank and pauses admission when one faults (`--fault-tolerance-on-error-strategy pause`, the default).

| Endpoint | Use |
|---|---|
| `GET /v1/fault_tolerance/status` | each DP rank's `status`: `healthy`, `unhealthy` or `dead`, and for the last finished instruction `last_ft_request_id` and `ft_error` |
| `POST /v1/fault_tolerance/apply` | `retry` or `scale_down` with `removed_dp_ranks`, for every rank at once; `202` means accepted, `409` that another instruction runs |

Rules that shape the driver:

- An instruction is accepted only while a rank is `unhealthy` or its process is gone.
- The status shows an instruction's result only after it finishes. While it runs, the status still shows the
  previous one.
- A rank removed by `scale_down` reports `dead`.

## Driver

| Contract | SGLang |
|---|---|
| Members per Pod | `--sglang-ranks-per-pod` ranks; rank `i` of Pod `Index` is `dp<Index × ranks + i>` |
| `Observe` | one status read from the first Pod on `--sglang-api-port` |
| `Serving` | status `healthy` |
| `Terminal` | status `dead` |
| `Unknown` | the first Pod is not running or does not answer, so every member is unknown |
| `Probe` | one short completion, so the ranks run a step and notice a dead peer |
| `Apply` | one request for all participants: `retry` when nothing is removed, else `scale_down`; `request_id` is the reconfig ID. Not sent again when the status already shows that ID. |
| `Verify` | one completion per member, routed to it with `routed_dp_rank` |
| Capabilities | `Grow: false`. SGLang re-admits a removed rank by itself once a replacement process joins; IRO does not drive that yet. |

## Policy

| Observation | Plan |
|---|---|
| every admitted rank healthy | `Healthy` |
| a rank unknown | `Wait` |
| no rank survives | `Unrecoverable` |
| a rank dead | `Change`: `scale_down` the dead ranks |
| a rank unhealthy, none dead | `Change`: `retry` |

Unlike vLLM, the policy does not wait for healthy ranks to notice a fault: the FT manager already saw it.

`Progress` is `Running` until the status shows the reconfig's ID, then `Failed` if it has an `ft_error` or a
participant is not healthy, else `Succeeded`.

## Not supported yet

- The first Pod failing: every member becomes unknown and IRO resets the group after `--ft-unseen-timeout`.
- API keys and HTTPS.
- A leader that takes over while an instruction runs gets `409` on its resend and resets the group.
