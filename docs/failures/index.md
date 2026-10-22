# Failures

Recovery examples for the supported action contract, one failure per page. Each page shows the failure step
by step: the group's Pods and members, the controller's phase, and every call IRO makes with its input and output.

## How IRO recovers

![Controller phases: starting, serving, recovering](../figures/phases.svg)

The engine policy chooses actions from observations, its saved checkpoint and completed results. The driver
resumes each engine action through `Apply`. The controller selects workload operations for replacement
requirements, persists execution and enforces shared safety and recovery limits.

These examples use policies that restore serving before adding capacity. Confirmed failures return to the
policy for another decision. Unknown operation outcomes stay pending; reaching a deadline fences additional
effects. Every new mutation must fit the controller's durable recovery budget.

### Engine members

| Failure | IRO sees | Ends with |
|---|---|---|
| [A member dies](member-dies.md) | the engine reports the member terminal; the others pause | full again, without a restart |
| [Several members die at once](several-members-die.md) | several members terminal at once | full again, without a restart |
| [Another member dies after a shrink](another-member-dies.md) | a new terminal member while others are already excluded | serving with fewer members, then full again as the Pods are reset |
| [A member dies inside a shared Pod](member-dies-in-shared-pod.md) | a terminal member and a prepared replacement candidate | full again after replacement admission |
| [A transient fault](transient-fault.md) | every member paused, none terminal | the same members, serving |
| [A member hangs](member-hangs.md) | paused members, none terminal; the policy says `Wait` | a new group |
| [A member cannot be read](member-unreadable.md) | an unknown member next to readable ones | a new group |

### Pods

| Failure | IRO sees | Ends with |
|---|---|---|
| [A Pod is lost](pod-lost.md) | `Discover` no longer lists the Pod; its members are added as terminal, `pod lost` | full again, or a new group if the workload recreates the whole group |
| [The failed Pod cannot come back](capacity-not-restored.md) | `Reset` is unsupported, or the new member cannot join | serving below full until the next group, or a reset with `--ft-capacity-restore=reset-all` |

### Recovery itself

| Failure | IRO sees | Ends with |
|---|---|---|
| [A replacement cannot join](member-rejoin-fails.md) | confirmed joiner-only failure with survivors unchanged | the example policy chooses reduced capacity after one attempt |
| [A reconfig fails](reconfig-fails.md) | `Apply` reports `Failed` with scope `Group` | a new group |
| [A reconfig never finishes](reconfig-hangs.md) | `Apply` reports `Running` past `--ft-apply-timeout` | pending operation; further effects fenced |
| [Recovery keeps repeating](recovery-repeats.md) | recovery count or time limit reached | new mutations blocked |
| [The group cannot generate after a recovery](verify-fails.md) | `Verify` keeps failing after a reconfig | a new group |
| [The engine cannot recover in place](unrecoverable.md) | the policy says `RecreateGroup` | a new group |

### IRO itself

| Failure | IRO sees | Ends with |
|---|---|---|
| [The IRO leader dies mid-reconfig](leader-dies.md) | an operation, attempted marker and checkpoint in the journal | the saved operation resumes |
| [IRO cannot see the group](iro-blind.md) | `Observe` reads no admitted member | nothing, if reads come back in time; otherwise a new group |
| [The journal is lost](journal-lost.md) | an empty journal | the same group, never reset |

The workload can also give up recreating the group (`RecreateStopped`). IRO then waits and acts on
nothing until the workload recreates the group; see [resetting the group](../components/controller.md#resetting-the-group).

## Adding a failure

A failure's stepper is a JSON file in `docs/scenarios`, shown on any page by one line:

```markdown
[example my-failure]: ../scenarios/my-failure.json
```

GitHub hides that line; the site replaces it with the stepper, and with it any figure right above it. The file
lists the Pods once, then the steps; each step names only what changed.

```json
{
  "title": "m0 dies",
  "page": "failures/my-failure",
  "pods": [{"name": "pod-0", "uid": "a1", "members": {"m0": "serving"}}],
  "steps": [
    {"title": "m0 dies", "phase": "recovering",
     "pods": {"pod-0": {"m0": "dead"}},
     "calls": [{"to": "policy", "call": "Decide(...)", "out": "RecreateGroup"}]}
  ]
}
```

| Field | Values |
|---|---|
| `phase` | `starting`, `serving`, `recovering` |
| member state | `serving`, `paused`, `dead`, `excluded`, `starting`, `joining`, `unknown` |
| Pod change | member states, a new `uid`, or `"gone": true` |
| `calls[].to` | `workload`, `driver`, `policy`, `journal`, `telemetry` |
| optional | `page` (where the title links), `note` and `journal` on a step, `in` and `out` on a call |
