# A member dies inside a shared Pod

A Pod holds several members. One member dies while its container and the other members stay alive.

- **IRO sees:** a terminal member, followed by a replacement descriptor with a distinct incarnation.
- **IRO does:** removes the old incarnation, verifies survivors, saves one replacement attempt and sends `Add`.
- **Ends with:** the replacement admitted after verification, with the Pod UID and healthy processes unchanged.
- **Rules:** [replacement admission](../components/controller.md#replacement-admission).

[example member-dies-in-shared-pod]: ../scenarios/member-dies-in-shared-pod.json
