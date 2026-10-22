# A transient fault

A step fails but no member dies. IRO retries in place and nobody is lost.

- **IRO sees:** every member paused, none terminal
- **IRO does:** a reconfig that removes nobody: a retry
- **Ends with:** the same members, serving
- **Rules:** [a reconfig that changes no members cannot repeat within 2 minutes
  (C6)](../components/controller.md#safety-checks)

[example transient-fault]: ../scenarios/transient-fault.json
