# A replacement cannot join

A replacement process fails during admission while the surviving members can still serve.

- **IRO sees:** an admission action followed by an established joiner-only failure.
- **IRO does:** rejects the failed candidate and supplies the result and saved policy state to `Decide`.
- **Ends with:** survivors serving at reduced capacity when the policy stops retrying.
- **Rules:** [replacement admission](../components/controller.md#replacement-admission).

`Apply` returns `Failed`, scope `Joiners`, only after proving admitted members unchanged and the candidate
stopped or fenced. The result and checkpoints survive leader changes. A fresh candidate has a different
identity but replaces the same original member, so the policy can apply its existing retry history.

| Observation or outcome | This example policy chooses |
|---|---|
| old member terminal | remove it, then verify survivors |
| replacement B2 joinable | admit B2 and save attempt 1 |
| B2 fails safely | consume that result once; retain original B as excluded |
| B3 appears after reload | `Healthy` at reduced capacity because the policy allows one attempt |

[example member-rejoin-fails]: ../scenarios/member-rejoin-fails.json

An unknown commit remains pending. A transport error or elapsed deadline does not establish failure and
cannot authorize a conflicting reset. The independent [controller recovery budget](recovery-repeats.md)
bounds new mutations even when an engine policy continues proposing attempts.
