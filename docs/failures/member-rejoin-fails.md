# A replacement cannot join

A Pod holds several members. One member dies while its container and the other members stay alive.

- **IRO sees:** a terminal member, followed by a replacement descriptor with a distinct incarnation.
- **IRO does:** removes the old incarnation, verifies survivors, saves one replacement attempt and sends `Add`.
- **Ends with:** the candidate rejected and survivors serving at reduced capacity after a confirmed joiner-only failure.
- **Rules:** [replacement admission](../components/controller.md#replacement-admission).

A launch failure can return a joiner-scoped `ApplyError`. A failed join can report `Failed`, scope `Joiners`,
through `Progress`. Both require cleanup or fencing of the candidate and proof that admitted members are
unchanged. The recorded attempt survives leader changes and prevents automatic relaunch with a new candidate.
An uncertain commit, apply deadline, survivor fault or failed post-commit verification uses the group-reset
rules. [Member identity](../components/engines/index.md#members) distinguishes old and replacement processes.

[example member-rejoin-fails]: ../scenarios/member-rejoin-fails.json
