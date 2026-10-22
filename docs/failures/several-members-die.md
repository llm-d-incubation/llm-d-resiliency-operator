# Several members die at once

Two members die together. One reconfig removes both. Capacity restoration follows the configured policy.

- **IRO sees:** several members terminal at once
- **IRO does:** one reconfig for all of them; with `reset-pod` and an engine that can grow, one `Reset` names
  every failed Pod
- **Ends with:** full again without a restart if the engine can grow; otherwise serving with fewer members
- **Rules:** [one reconfig at a time, naming only members IRO observes (C7)](../components/controller.md#safety-checks),
  [restoring capacity](../components/controller.md#getting-back-to-full)

[example two-members-die]: ../scenarios/two-members-die.json
