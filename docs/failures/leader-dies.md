# The IRO leader dies mid-reconfig

The leader dies while a reconfig runs. The new leader finishes it from the journal.

- **IRO sees:** an operation, attempted marker and checkpoint in the journal
- **IRO does:** resumes `Apply` with the saved operation and a fresh observation
- **Ends with:** the saved operation resumes
- **Rules:** [reconfigs](../components/controller.md#repairing-a-fault), [journal](../components/journal.md)

[example leader-change]: ../scenarios/leader-change.json
