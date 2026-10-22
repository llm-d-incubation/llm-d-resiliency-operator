# A reconfig never finishes

- **IRO sees:** `Apply` keeps reporting `Running` past the operation's original deadline.
- **IRO does:** retains the pending action and calls `Apply` with `MayAdvance: false`.
- **Ends with:** the action unresolved until its outcome can be established; conflicting mutations stay blocked.
- **Rules:** [operation deadlines](../components/controller.md#deadlines).

The deadline stops additional effects from the driver. Remote work may still be running, so the controller
cannot infer failure, cancellation or permission to reset the group. A later established terminal report is
saved before another decision. Reloading the journal preserves the same operation and deadline.

[example reconfig-hangs]: ../scenarios/reconfig-hangs.json
