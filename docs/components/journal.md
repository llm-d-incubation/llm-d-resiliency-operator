# Journal

## What it does

The journal keeps the controller's state for one engine group, so a new leader can pick up where the last one
stopped. It records the pending operation, policy checkpoint and last result. The
[workload](workloads/index.md) reports which Pods exist.

It stores bytes and never looks inside them. The schema is the controller's [`Record`](controller.md#objects).
It is its own component, apart from the workload: the workload keeps no memory and the journal is all memory, a
set with several engine groups needs one record per group, and tests can fake it alone.

## What it talks to

| With | Calls | When | Returns |
|---|---|---|---|
| Controller → journal | `Load` | when it becomes the leader, and after a conflict | the saved record and its version |
| | `Save` | before every action, and after each change | the new version, or a conflict |
| Journal → storage | the store's own API, such as a ConfigMap owned by the workload object | inside the calls above | |

## When the controller uses it

The controller saves the exact action and policy checkpoint before dispatch. It saves the completed result
before asking the policy for the next action. The policy interprets its checkpoint; the journal and controller
store its JSON bytes without interpreting them. A failed save prevents the action or next decision.

An operation keeps its ID, payload and original start time across leadership changes. The saved policy state
also preserves retry history and diagnosis or verification start times, so a reload does not restart a strategy.

The record has an explicit schema version. Loading an unversioned record, an unknown version, an invalid
phase or a missing epoch fails. IRO keeps that record and retries loading; it does not erase pending work or
reset the group to make an incompatible record load. Deployments with an existing journal must explicitly
migrate its operation and recovery state, or arrange a fresh group and record after resolving the old work.
Deleting an active record discards its operation and retry history.

## Reading the record

`Load` reads the saved record.

| | |
|---|---|
| **Input** | none |
| **Output** | the record bytes and their version, or `nil, ""` when there is no record yet |
| **Errors** | the store cannot be read |
| **Side effects** | none |

```go
Load(ctx) // first run of IRO for this group: nil, "", nil. The controller starts a new group.
Load(ctx) // after a leader change: {"phase":"recovering","operation":{"ID":"iro-ft-7c1e","Action":{...}},...}, "v42", nil
```

A bad record does not stop the manager: if `Load` fails or the bytes do not parse, the runner logs it and tries
again on the next poll.

## Saving the record

`Save` writes the record, unless someone else wrote it first.

| | |
|---|---|
| **Input** | the record bytes, and the version they replace: the one from the last `Load` or `Save`, or `""` when `Load` found no record |
| **Output** | the new version. `ErrConflict` when the record is not at that version: changed or deleted by someone else, or, for `""`, already there. Another error otherwise. |
| **Side effects** | the write only |

Two IRO processes that both think they lead:

```go
// process A and process B both loaded version 41.
A.Save(ctx, recordA, "41") // "42", nil.
B.Save(ctx, recordB, "41") // ErrConflict. B's state is not saved, so B does not act on it. B loads again.
B.Save(ctx, recordB, "")   // ErrConflict: the record exists. "" only creates.
```

After `ErrConflict` the controller drops its state and loads again, and continues from the saved record as after
a leader change, deadlines included. If the record was deleted, it starts over as on IRO's first run.

[example journal-writers]: ../scenarios/journal-writers.json

| Rule | Why |
|---|---|
| `Save` is compare-and-swap on the version it is given. The journal keeps no version of its own. | two leaders can never both win, and a writer that lost one race is not stuck: it loads the current version and goes on |
| A change that leaves the record alone, such as a new annotation, is not a conflict | metadata edits must not stop recovery |
| `Save` never recreates a deleted record. With a version it returns `ErrConflict`; `""` only creates. | a writer that cannot prove its state is current must not write it back |
| The bytes are opaque | the journal never interprets recovery state |

## Lifetime

The record lives and dies with the workload object, so a new workload with the same name starts clean.
Deleting the record is how a person makes IRO forget the group: the next leader starts over, and admits a group
that serves without resetting it.

## API reference

### Interface

```go
type Journal interface {
    Load(ctx context.Context) (data []byte, version string, err error)
    Save(ctx context.Context, data []byte, version string) (string, error)
}

// ErrConflict means the record is not at the version Save was given: another writer changed or deleted it,
// or, for "", it already exists.
var ErrConflict = errors.New("journal conflict")
```

The record itself is opaque `[]byte`; its content is the controller's [`Record`](controller.md#objects).
