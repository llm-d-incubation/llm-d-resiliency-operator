# The engine cannot recover in place

Too much is lost for the engine to recover in place.

- **IRO sees:** the policy says `RecreateGroup`
- **IRO does:** resets the whole group
- **Ends with:** a new group
- **Rules:** [state machine](../components/controller.md#the-life-of-a-group)

[example unrecoverable]: ../scenarios/unrecoverable.json
