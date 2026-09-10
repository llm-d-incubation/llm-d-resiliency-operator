# vLLM rank recovery

## Summary

We propose contributing the recovery flow from
[llm-d-resiliency-manager][prototype] to IRO, building on the
[IRO proposal][iro] and [initial controller][controller]. It removes one failed
rank and resumes serving on the survivors.

## Motivation (Goals/Non-Goals)

A worker process can fail while its container stays alive. The group needs
engine recovery and routing exclusion, even without an infrastructure
`RecoveryRequest`. Treating every such event as transient is insufficient.

The initial scope is one EP group with DP=EP, TP=1, redundant experts and one
failed non-master rank. A supervisor must keep the container alive without
relaunching failed children. LWS keeps `RecreateGroupOnPodRestart`. Infrastructure
repair, rank rejoin and LWS changes are outside this contribution.

## Proposal

Add an opt-in recovery flow within IRO using a vLLM adapter, persistent operation
state and a routing adapter:

1. Confirm the same failed rank through repeated peer reports, with all survivors
   at the FT barrier.
2. Quarantine the group and wait for routing acknowledgement.
3. Persist the dispatch intent, then send `scale_down` to all survivors with one
   operation ID.
4. Verify survivor inference, then admit only surviving ranks after routing
   acknowledgement.

## Design Details

Bind each operation to Pod UIDs, container IDs and endpoints. Recheck membership
before dispatch and inference immediately before admission. An interrupted
dispatch is not replayed automatically; changed membership or another failure
leaves the group quarantined for operator recovery.

Routing must acknowledge the exact membership and revision after every serving
router enforces the change. Pod labels and successful HTTP submission alone do
not establish this. The prototype provides a client; the llm-d routing adapter
still needs implementation.

Two contracts need agreement:

- How IRO classifies and persists engine-only failures without assigning an
  infrastructure action or conflicting with an incoming `RecoveryRequest`.
- How rank exclusions and their acknowledgement integrate with llm-d routing.

The prototype has unit and simulated HTTP coverage. The integrated controller
still needs GPU validation with real routing, including worker failure,
survivor inference and unchanged LWS fallback behavior.

## Alternatives

Keeping a separate manager duplicates IRO's coordination role. Retrying the engine
alone does not remove a persistently failed rank or enforce routing exclusions.

[prototype]: https://github.com/Etelis/llm-d-resiliency-manager
[iro]: https://github.com/llm-d/llm-d/blob/main/proposals/inference-resilience-operator.md
[controller]: https://github.com/llm-d-incubation/llm-d-resiliency-operator/tree/initialcontroller
