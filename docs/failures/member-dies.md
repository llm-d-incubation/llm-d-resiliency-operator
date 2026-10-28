# A member dies

One member's process exits. IRO removes it in place so the survivors can resume serving.

Getting back to full needs an engine that can [grow](../components/engines/index.md#growing-back). With one that
cannot, the group keeps serving with fewer members until the next group, or IRO resets the group with
`--ft-capacity-restore=reset-all`.

- **IRO sees:** the engine reports the member terminal; the others pause
- **IRO does:** removes it in one reconfig, verifies the survivors, then restores capacity according to the
  configured policy
- **Ends with:** full again without a restart if the engine can grow; otherwise serving with fewer members
- **Rules:** [only members the engine reports dead are removed (C1)](../components/controller.md#safety-checks), [a Pod
  is reset only after all its members are removed (C8)](../components/controller.md#safety-checks), [restoring
  capacity](../components/controller.md#getting-back-to-full)

![One member dies and the group goes 4, 3, 4 while serving](../figures/recovery-timeline.svg)

![The same recovery as calls between the controller and its plugins](../figures/recovery-sequence.svg)

[example member-dies]: ../scenarios/member-dies.json

## How it works

1. **Remove the dead member.** The driver reports `m3` Terminal. The policy proposes `Remove`, the reconfig
   runs, and `Progress` reports `Succeeded`. Only then does the controller record `m3@identity-d1` as excluded and
   verify the survivors.
2. **Reset the failed Pod when the engine can grow.** Under `reset-pod`, each member names its Pod, so the
   controller knows `pod-3` holds only excluded members. It calls `Reset` when the engine supports growth.
   The rest of the group keeps serving. There is no deadline; scheduling and model loading take as long as they take.
3. **Admit the new member.** The new Pod's members show up as candidates. The policy plans `Add` when the
   engine can admit them, the reconfig runs like any other, and the reconfig's members are verified.

A fault at any point comes first: the group goes to `recovering`, and the capacity work continues once the
group serves again.

Capacity never comes from new Pods elsewhere: it comes back by resetting, with one try. If resetting the
Pod fails in any way (the workload answers `ErrUnsupported`, the engine cannot admit members, or the new Pod
fails to join), the group keeps serving with fewer members until the next group. With
`--ft-capacity-restore=reset-all`, IRO resets the whole group instead to restore full capacity. With
`release`, it frees the failed Pod's GPUs and serves below full.

## Poll by poll

[example controller-polls]: ../scenarios/controller-polls.json
