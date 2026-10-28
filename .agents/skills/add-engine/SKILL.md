---
name: add-engine
description: Add support for an inference engine, as a new engine plugin (driver and policy). Use when IRO must recover a new engine, or when changing how an engine is read or reconfigured.
---

# Add an engine

An engine plugin is everything IRO knows about one inference engine. The controller never changes for a new
engine. Read `internal/engine/engine.go` and `docs/components/engines/index.md` first, then the closest existing
engine under `internal/engine/<name>/` and its page, and follow their structure.

## Before writing code

Find out, from the engine's own code or docs, how it:

1. reports each member's state: serving, dead for good, still deciding, unreadable,
2. tells a member to retry, to drop dead peers, or to admit new ones,
3. reports the operation each member is executing and its committed effects,
4. deduplicates repeated requests, or establishes their outcome after a response is lost,
5. names its members within a Pod.

If the engine cannot report a member as dead for good, IRO can never remove it in place: only recreate the group.

## Write it

Under `internal/engine/<name>/`:

- **Driver**: `Observe`, `Apply`. Every call bounded by a timeout. No memory between calls: execution uses the
  latest `Observation`, saved `Operation`, and its opaque `Checkpoint`. `Apply` returns a `Report` with `Running`,
  `Succeeded`, or `Failed` and an optional checkpoint. It receives the original action and stable operation ID.
  It may contain several engine-specific stages. `MayAdvance: false` permits observation only. `Attempted` is
  saved before the first execution call, so it means a request may have been submitted. A lost response remains
  pending until the driver establishes the outcome. Replay an effect only when the runtime can deduplicate it
  or observation proves replay safe. A member the driver cannot read is `Unknown`, never guessed `Terminal`.
- **Policy**: `Decide(DecisionInput) Action`. Pure: no I/O, no clock, no globals. Decisions use current observation,
  membership, saved policy state, the last terminal result, and the remaining controller budget. Process each
  result once using its operation ID. Choose the next action after the active operation finishes. `Add` only if
  `Capabilities().Grow`. `ReplaceMembers` identifies excluded or rejected members needing replacement resources;
  `RecreateGroup` reports that the group needs recreation. The controller selects workload operations and enforces
  its recovery limits.
- **Options**: the engine's own flags, registered through its factory.

Register the factory in `cmd/main.go`.

## Test it

- Driver tests against a fake engine API: each member state, refusal before side effects, repeated Apply calls,
  lost responses, restarts with only journal state, and read-only Apply after the execution deadline.
- Policy tests from recorded observations: each action, saved result consumption, and retry limits.
- A flow test that drives a full recovery through the controller with the fake engine, including a failed attempt.

## Document it

In the docs PR: a page `docs/components/engines/<name>.md` (what the engine gives, driver, policy, flags, how to
deploy it for IRO, known engine issues). Explain what proves completion and how ambiguous outcomes are resolved.
The general engine page stays engine-neutral.
