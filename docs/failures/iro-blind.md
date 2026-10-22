# IRO cannot see the group

IRO cannot read any member. The group may be fine; IRO may be the one cut off.

- **IRO sees:** `Observe` reads no admitted member
- **IRO does:** waits, then sets `ObservationTimedOut` after the unseen interval while `Discover` succeeds. The policy
  chooses whether to request recreation when no operation is pending. Saved deadlines retain their original start times.
- **Ends with:** normal observation when reads recover, or the selected recreation when no operation remains unresolved
- **Rules:** [unknown members](../components/controller.md#members-it-cannot-read)

[example controller-blind]: ../scenarios/controller-blind.json

A pending operation retains its identity and uncertainty. The controller cannot infer its failure from
unreadability and cannot start a conflicting reset until the operation outcome is established.
