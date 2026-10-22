# A member dies

One member's process exits. The rest keep serving, and the group is full again once the failed Pod is reset.

Getting back to full needs an engine that can [grow](../components/engines/index.md#growing-back). With one that
cannot, the group keeps serving with fewer members until the next group, or IRO resets the group with
`--ft-capacity-restore=reset-all`.

- **IRO sees:** the engine reports the member terminal; the others pause
- **IRO does:** removes it in one reconfig, verifies the survivors, then resets its Pod and admits the new member
- **Ends with:** full again without a restart if the engine can grow; otherwise serving with fewer members
- **Rules:** [C1](../components/controller.md#safety-checks), [C8](../components/controller.md#safety-checks),
  [restoring capacity](../components/controller.md#getting-back-to-full)

![One member dies and the group goes 4, 3, 4 while serving](../figures/recovery-timeline.svg)

![The same recovery as calls between the controller and its plugins](../figures/recovery-sequence.svg)

[example member-dies]: ../scenarios/member-dies.json

## How it works

1. **Remove the dead member.** The driver reports `m3` terminal. The policy selects a `Change` to remove it.
   `Apply` reports success and the controller records `m3@idD` excluded. The next decision requests `Verify`
   for the survivors.
2. **Reset the failed Pod.** After verification, the policy requests replacement resources
   with `ReplaceMembers` for the excluded member. With `reset-pod` and an engine that can grow, the controller saves
   the selected `Reset` and calls the workload with the Pod UID.
3. **Admit the new member.** The replacement Pod's members become candidates. The policy selects `Change`
   with `Add`, then a separate `Verify` after admission succeeds.

This example policy restores capacity after survivors are verified. Its checkpoint and each completed
result determine the next action. For an engine that cannot admit replacement members, the controller can
retain reduced capacity, release eligible Pods or recreate the group according to operator configuration.
