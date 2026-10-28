# A Pod is lost

A Pod disappears: its node failed or it was deleted. Its members are lost.

- **IRO sees:** `Discover` no longer lists the Pod; its members are added as terminal, `pod lost`
- **IRO does:** removes them in one reconfig; admits replacement members when the engine supports growth
- **Ends with:** full again if the engine can grow; otherwise serving with fewer members, or a new group if the
  workload recreates the whole group
- **Rules:** [reading Discover](../components/controller.md#same-group-or-a-new-one), [connecting Pods and
  members](../components/controller.md#pods-and-members)

[example pod-lost]: ../scenarios/pod-lost.json

A replacement must not serve before it is admitted ([C5](../components/controller.md#safety-checks)): if it does,
IRO resets the group. A new Pod UID alone does not establish membership.
