# Failures

Every failure IRO handles, one page each. Each page shows the failure step by step: the group's Pods and
members, the controller's phase, and every call IRO makes with its input and output.

## How IRO recovers

![Controller phases: starting, serving, recovering](../figures/phases.svg)

1. **Remove in place.** When the engine proves members are gone, one reconfig removes them. The rest keep serving.
2. **Restore capacity.** Once the group serves, IRO admits replacement members or resets failed Pods.
3. **Reset as the last resort.** When nothing can be removed safely, or a recovery itself fails, IRO resets the
   whole group. It never gives up on its own: the workload's restart policy paces resets.
4. **Never act blind.** IRO never removes a member it cannot see, and never resets a group it cannot list.

A fault always comes first: capacity work waits until the group serves again.

### Engine members

| Failure | IRO sees | Ends with |
|---|---|---|
| [A member dies](member-dies.md) | the engine reports the member terminal; the others pause | full again, without a restart |
| [Several members die at once](several-members-die.md) | several members terminal at once | full again, without a restart |
| [Another member dies after a shrink](another-member-dies.md) | a new terminal member while others are already excluded | serving with fewer members, then full again as the Pods are reset |
| [A member dies inside a shared Pod](member-dies-in-shared-pod.md) | a terminal member and a prepared replacement incarnation | full again after replacement admission |
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
| [A replacement cannot join](member-rejoin-fails.md) | confirmed joiner-only failure with survivors unchanged | reduced capacity, with no second replacement attempt |
| [A reconfig fails](reconfig-fails.md) | `Progress` says `Failed` with scope `Group` | a new group |
| [A reconfig never finishes](reconfig-hangs.md) | `Progress` says `Running` past `--ft-apply-timeout` | a new group |
| [The group cannot generate after a recovery](verify-fails.md) | `Verify` keeps failing after a reconfig | a new group |
| [The engine cannot recover in place](unrecoverable.md) | the policy says `Unrecoverable` | a new group |

### IRO itself

| Failure | IRO sees | Ends with |
|---|---|---|
| [The IRO leader dies mid-reconfig](leader-dies.md) | a reconfig in the journal, with or without saved acceptances | the recovery finishes, never sent twice |
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
     "calls": [{"to": "policy", "call": "Plan(...)", "out": "Unrecoverable"}]}
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
