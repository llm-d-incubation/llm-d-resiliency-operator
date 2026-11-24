# The group cannot generate after a recovery

The engine reports every member serving, but one cannot answer a request.

- **IRO sees:** `Verify` keeps failing after a reconfig
- **IRO does:** stays `recovering`, retries for `--ft-verify-timeout`, then resets the whole group
- **Ends with:** a new group
- **Rules:** [reconfigs](../components/controller.md#repairing-a-fault)

[example verify-fails]: ../scenarios/verify-fails.json
