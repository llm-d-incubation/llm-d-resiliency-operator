# Workload

## What it does

The workload plugin owns the Pods of one engine group. It does four things:

- **reports** the group's Pods as they are right now,
- **replaces** named Pods with new Pods in the same place, while the rest of the group keeps running,
- **recreates** the whole group,
- **releases** Pods: removes them and frees their GPUs. It never adds Pods.

It remembers nothing: no earlier calls, no earlier groups. Every answer comes from the cluster as it is now, so a
restarted IRO or a new leader gets the same answers. Deciding whether this is still the same group is the
[controller's](../controller.md#same-group-or-a-new-one) job. It knows nothing about engines beyond which of
its containers run one: no ranks, ports or processes. Each workload type brings its own plugin.

## What it talks to

| With | Calls | When | Returns |
|---|---|---|---|
| Controller → workload | `Discover` | every poll | the group's Pods as they are now |
| | `Reset` | when Pods hold only removed members | nothing; the next `Discover` shows the new Pods |
| | `ResetAll` | when the whole group must be recreated | nothing; the next `Discover` shows the new group |
| | `Release` | when failed Pods are given back | nothing; the next `Discover` shows them gone |
| Workload → Kubernetes | its own Kubernetes objects | inside the calls above | |

It never talks to the engine. How often a group may be recreated, and when to stop, is the workload's own
restart policy: IRO has no reset budget of its own.

## Pods

| Field | Means |
|---|---|
| `Name`, `UID` | the Pod. A new Pod with the same name is a different Pod: it has a new UID. |
| `Index` | its position in the group, fixed for its life, so the engine driver can address it |
| `Identity` | the UID plus the engine containers' IDs and restart counts |
| `Node` | where it runs, empty until scheduled |
| `State` | `Starting`, `Ready` (every container runs and it has an IP) or `Terminating` |

Once its engine containers run, a Pod's `Identity` changes only when the Pod is replaced or an engine container
restarts. A restarted engine is never mistaken for the old one, and a sidecar restart does not look like lost
members.

So the workload must know exactly which containers run the engine. If it also counts a sidecar, a sidecar
restart looks like an engine restart: the members are lost, their new IDs serve without being admitted, and the
group is reset. If it leaves an engine container out, a restarted engine passes for the old process.

Besides the Pods, the workload reports two facts about the group:

| | Means | The controller |
|---|---|---|
| `Recreating` | the workload is replacing the whole group right now | waits for the new group |
| `RecreateStopped` | the workload gave up recreating the group, for example its restart budget is used up | waits, and telemetry reports it |

A workload that will recreate the group because of what it sees reports `Recreating` right away, not only once
the leader is deleted. Otherwise the controller may plan a reconfig for a group that is about to go.
`Recreating` follows the workload's own restart policy, which may count every container.

## Every poll

The controller calls `Discover` every poll. It calls `Reset`, `ResetAll` or `Release` only when it decided Pods
must go, saves that first, and repeats the call every poll until `Discover` shows the result.

[example workload-calls]: ../../scenarios/workload-calls.json

## Reporting the group

`Discover` reports the group as it is now.

| | |
|---|---|
| **Input** | none |
| **Output** | a [`Group`](#objects), or an error |
| **Errors** | only when the workload cannot be read, or its spec is not supported. Starting, terminating and missing Pods are facts, not errors: the controller must see them, not lose sight of the group. |
| **Side effects** | none |

While `pod-3` is reset in [a member dies](../../failures/member-dies.md):

```yaml
Recreating: false
RecreateStopped: false
Pods:
- {Name: pod-0, UID: a1, Index: 0, Identity: identity-a1,  Node: gpu-node-1, State: Ready}
- {Name: pod-1, UID: b1, Index: 1, Identity: identity-b1,  Node: gpu-node-2, State: Ready}
- {Name: pod-2, UID: c1, Index: 2, Identity: identity-c1,  Node: gpu-node-3, State: Ready}
- {Name: pod-3, UID: d2, Index: 3, Identity: identity-d2, Node: gpu-node-9, State: Starting}
```

`pod-3` has a new UID: it is a new Pod in the same place. `Discover` does not say that the old one was reset on
purpose; the controller knows that from its journal. Pods are sorted by `Index`.

## Replacing failed Pods

`Reset` replaces each named Pod with a new Pod in the same place. The rest of the group keeps running.

| | |
|---|---|
| **Input** | the Pods to replace, as the controller last saw them |
| **Output** | `nil` once every replacement is triggered, or once none of the named Pods (by UID) exist. `ErrUnsupported` when the workload cannot replace one of them without recreating the group: it decides that before touching any Pod, and changes none of them. Another error otherwise. |
| **Side effects** | each named Pod goes, and a new Pod with the same `Index` and a new UID starts in its place. The replacements start on their own and may become `Ready` at different times. |

```go
Reset(ctx, []PodRef{{"pod-2", "c1"}, {"pod-3", "d1"}})
// a workload that supports it: both are deleted and come back with new UIDs. Returns nil.
// the same call again: no named Pod exists. Returns nil, changes nothing.
// the list names a Pod the workload cannot replace alone, such as its leader: ErrUnsupported, changes nothing.
```

| Rule | Why |
|---|---|
| Touch only the Pods named, by UID | a late call can never hit a new Pod |
| Never recreate the rest of the group; return `ErrUnsupported` instead | the healthy members keep serving |
| `ErrUnsupported` changes nothing, for the whole list | the controller then asks for each Pod alone, and keeps the ones the workload cannot replace |

Some workloads cannot replace their leader without recreating the group, so they return `ErrUnsupported` for
any list that names it. `Reset` never names every Pod of the group: the controller names only Pods whose
members a reconfig removed, and some members always stay. The new Pod keeps its `Index`, but the scheduler may
put it on the same node. If the node is the problem, the new Pod fails too, and the group serves with fewer
members until the next group.

## Recreating the group

`ResetAll` recreates the group that the named Pods belong to.

| | |
|---|---|
| **Input** | the Pods of the group, as the controller last saw them |
| **Output** | `nil` once the recreation is triggered, or once none of the named Pods exist. An error otherwise. |
| **Side effects** | the whole group is replaced by new Pods, when the workload's own restart policy allows it |

```go
ResetAll(ctx, []PodRef{{"pod-0", "a1"}, {"pod-1", "b1"}, {"pod-2", "c1"}, {"pod-3", "d1"}})
// first call: the workload deletes pod-0 (UID a1), and the whole group is recreated. Returns nil.
// after the new group exists: pod-0 now has UID f1, so no named Pod exists. Returns nil, changes nothing.
```

The second call is why the controller can repeat `ResetAll` until the old Pods are gone: it can never touch the
new group.

## Giving Pods back

`Release` removes the named Pods and frees their GPUs. Nothing is added in their place: capacity comes back
only through a reset.

| | |
|---|---|
| **Input** | the Pods to remove |
| **Output** | `nil` once the removal is triggered, or once none of the named Pods exist. `ErrUnsupported` when the workload cannot remove Pods from a running group, with nothing changed. Another error otherwise. |
| **Side effects** | the named Pods go, and the group gets smaller |

```go
Release(ctx, []PodRef{{"pod-3", "d1"}})
// a workload that supports it: pod-3 starts terminating and is not replaced. Returns nil.
// the same call again: no Pod with UID d1 exists. Returns nil, changes nothing.
// a workload that cannot: returns ErrUnsupported, changes nothing.
```

## Later: disaggregated sets

A prefill/decode set holds several engine groups with different roles. Proposal: each engine group is one
recovery unit with its own `Group`, journal record and controller loop, and the workload lists the units. This
is open decision 1 in the [overview](../index.md#open-decisions).

## API reference

### Interface

```go
type Workload interface {
    Discover(ctx context.Context) (Group, error)
    Reset(ctx context.Context, pods []PodRef) error
    ResetAll(ctx context.Context, pods []PodRef) error
    Release(ctx context.Context, pods []PodRef) error
}
```

### Objects

```go
// Group is the group as it is now.
type Group struct {
    Pods            []Pod // every Pod, sorted by Index, including starting and terminating ones
    Recreating      bool
    RecreateStopped bool
}

type Pod struct {
    Name     string
    UID      string
    IP       string
    Index    int
    Identity string
    Node     string
    State    PodState
}

type PodState string

const (
    PodStarting    PodState = "Starting"
    PodReady       PodState = "Ready"
    PodTerminating PodState = "Terminating"
)

// PodRef names one Pod. The UID makes it exact.
type PodRef struct {
    Name string
    UID  string
}

// ErrUnsupported means the workload cannot do this at all. Nothing was changed.
var ErrUnsupported = errors.New("unsupported")
```
