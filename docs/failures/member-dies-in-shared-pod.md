# A member dies inside a shared Pod

A Pod holds several members. One member dies while its container and the other processes remain alive.

- **IRO sees:** a terminal member, followed by a durably reserved replacement identity.
- **IRO does:** removes the old member, verifies survivors and starts one admission action.
- **Ends with:** the replacement admitted and verified, with the Pod UID and surviving processes unchanged.
- **Rules:** [replacement admission](../components/controller.md#replacement-admission).

`Decide` selects each action from observations and saved results. Admission itself spans several `Apply`
calls with the same operation ID. The driver prepares survivors, launches the candidate and commits
membership, returning `Running` until the result is established. The example includes a leader restart.

| Point | Saved facts and next call |
|---|---|
| before admission | save action `add-1`, policy attempt count, mutation count and attempted marker |
| preparation confirmed | `Apply(add-1)` advances the native launch and returns an execution checkpoint |
| leader changes | load `add-1`; fresh observation establishes whether the launch happened |
| commit confirmed | `Apply(add-1)` returns `Succeeded`; save membership before the next decision |
| verification completed | `Decide` returns `Healthy`; stable health can eventually clear the controller budget |

[example member-dies-in-shared-pod]: ../scenarios/member-dies-in-shared-pod.json

The runtime must provide readback or deduplication for a launch whose response or checkpoint was lost.
These are engine contract examples; each driver must implement its own native admission protocol.
