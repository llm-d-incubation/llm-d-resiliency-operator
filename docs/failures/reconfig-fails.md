# A reconfig fails

The engine cannot complete a reconfig and cannot promise the survivors are consistent.

- **IRO sees:** `Apply` reports `Failed` with scope `Group`
- **IRO does:** resets the whole group
- **Ends with:** a new group
- **Rules:** [reconfigs](../components/controller.md#repairing-a-fault)

[example reconfig-fails]: ../scenarios/reconfig-fails.json
