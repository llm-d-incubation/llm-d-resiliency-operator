# A member cannot be read

One member's API stops answering. Its engine may still serve, so IRO never removes it.

- **IRO sees:** an unknown member next to readable ones
- **IRO does:** treats it as a fault, never sends a plan that names it, and resets the group at the diagnosis deadline
- **Ends with:** a new group
- **Rules:** [unknown members](../components/controller.md#members-it-cannot-read), [only members the engine reports
  dead are removed (C1)](../components/controller.md#safety-checks)

[example member-unreadable]: ../scenarios/member-unreadable.json
