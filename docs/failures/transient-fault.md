# A transient fault

A step fails but no member dies. IRO retries in place and nobody is lost.

- **IRO sees:** every member paused, none terminal
- **IRO does:** a reconfig that removes nobody: a retry
- **Ends with:** the same members, serving
- **Rules:** [policy decisions and checkpoints](../components/engines/index.md#deciding)

[example transient-fault]: ../scenarios/transient-fault.json
