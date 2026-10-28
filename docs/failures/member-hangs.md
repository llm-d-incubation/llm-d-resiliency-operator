# A member hangs

A member stops making progress without dying. Nothing proves it is gone, so nothing can be removed.

- **IRO sees:** members stop making progress, none terminal; the policy says `Wait`
- **IRO does:** waits for the engine, then resets the group when `--ft-diagnosis-timeout` passes
- **Ends with:** a new group
- **Rules:** [only members the engine reports dead are removed (C1)](../components/controller.md#safety-checks),
  [reconfigs](../components/controller.md#repairing-a-fault)

[example member-hangs]: ../scenarios/member-hangs.json
