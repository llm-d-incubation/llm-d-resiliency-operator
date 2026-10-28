# Another member dies after a shrink

A second member dies while the group serves below full. It is removed like the first.

- **IRO sees:** a new terminal member while others are already excluded
- **IRO does:** a second reconfig over the members still admitted
- **Ends with:** serving with fewer members, then full again if the engine can grow; otherwise below full until
  the next group
- **Rules:** [removed members must stay dead (C4)](../components/controller.md#safety-checks),
  [reconfigs](../components/controller.md#repairing-a-fault)

[example another-member-dies]: ../scenarios/another-member-dies.json
