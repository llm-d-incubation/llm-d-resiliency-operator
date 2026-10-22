# Engine

## What it does

The engine plugin reads an inference engine and chooses and executes its recovery actions. The controller
sees members and operation reports. Engine ranks, ports, status strings and execution stages stay in the plugin.

![The engine plugin: driver and policy](../../figures/engine.svg)

### Driver

The driver performs I/O through `Observe` and resumable `Apply`. Each call receives the current observation
and the saved operation. It must work after a leader change without any process-local recovery memory.
The runtime and the journal hold the facts needed to resume. Calls are bounded so a hung engine cannot
stall the controller.

### Policy

The policy implements `Decide`: it chooses one next action from observation, recorded membership, the last
result and its saved checkpoint. It owns engine recovery ordering and engine-specific retry decisions.
It is pure: no I/O, no clock, no globals. The same inputs produce the same action on every leader.

## What it talks to

| With | Calls | When | Returns |
|---|---|---|---|
| Controller to driver | `Observe` | every poll | members and engine-private observation data |
| | `Apply` | while an engine action is pending | `Running`, `Succeeded` or `Failed`, with an execution checkpoint |
| Controller to policy | `Decide` | when no operation is pending | one action and a policy checkpoint |
| Controller | `Capabilities` | once | whether the engine can admit new members |
| Driver to runtime | engine-specific APIs | inside driver calls | observable operation state and execution results |

The policy can request replacement capacity or whole-group recreation. The controller selects and executes
workload operations according to configuration and Pod safety. The policy never selects Pod UIDs or calls
Kubernetes. The traffic router reads the engine's own health directly.

## Members

A member is one engine process that serves: a rank, a worker. The driver reports each member with the Pod it
runs on.

| Field | Means | The driver must |
|---|---|---|
| `Name` | the process instance name within its Pod, such as `m3` or `m3-process2`; excludes `@` | keep it stable across observations and use a new name for a replacement in the same Pod |
| `Replaces` | the excluded member replaced by this candidate, or empty | keep the association stable for the candidate |
| `Serving` | the router may send it traffic: the engine's own health | report what the router uses, and never `Serving` for a candidate (checks [C1 and C5](../controller.md#safety-checks)) |
| `Terminal` | it will never serve again | never report it `Serving` again, only `Terminal` or `Unknown` (check C4) |
| `Unknown` | the driver could not read it now. `Serving`, `Terminal` and `Joinable` are then false. | use it whenever it cannot read a member, never guess `Terminal`: only a process known to be gone may be removed (check C1) |
| `Joinable` | for a candidate only: the driver can start admission now, including a coordinated launch | report it false while the Pod is not `Ready`; keep launch descriptors fenced to the reported member ID |

A Pod gives all of its members, or none when the driver cannot list them. At the end of `starting`, the
controller adopts the reported group and skips unlaunched replacement descriptors.

A member's ID is `Name@PodIdentity`, such as `m3@idD`. Engines that replace individual processes within a
running container supply a new name for every replacement, such as `m3-process2@idD`. The adapter maps that
name to the engine's logical member. Pod replacement or engine-container restart changes the Pod identity.
An excluded member remains terminal after its replacement joins.

`Serving` is not "healthy". `Serving` is what the router sees. "Healthy" is the policy's `Healthy` action, which
may also use what the router does not see, such as a process that already exited.

**Lost members.** When a Pod is gone, `Terminating`, or has a new identity, the driver no longer sees the
members that ran on it. Before it calls `Decide` or `Apply`, the controller adds each one to
the observation with its Pod, identity and name, `Terminal: true` and `Detail: "pod lost"`. The policy plans
them like any terminal member, so a shrink removes them. A container probe that restarts an engine container
loses its members the same way, so each engine page says which probes are safe while a member is paused for
recovery.

## Every poll

The controller observes the group. With an operation pending, it calls `Apply` with that same operation.
With no operation pending, it calls `Decide`. The chosen action and its ID are saved before execution; the
completed result is saved before another decision.

A recovery can contain several actions: remove a member, verify survivors, request replacement capacity,
then admit a candidate. Each action can contain several internal driver stages. The controller resumes it
without interpreting those stages.

## Reading members

`Observe` reports the members running on the given Pods, and their state.

| | |
|---|---|
| **Input** | the Pods from [`Discover`](../workloads/index.md#reporting-the-group) |
| **Output** | an [`Observation`](#objects). Never fails: a member the driver cannot read is `Unknown`, and a Pod whose members it cannot list gives none. |
| **Side effects** | none |

Right after `m3` dies:

```yaml
Members:
- {Pod: pod-0/a1, PodIdentity: idA, Name: m0, Serving: false, Terminal: false, Detail: "paused, waiting for instruction"}
- {Pod: pod-1/b1, PodIdentity: idB, Name: m1, Serving: false, Terminal: false, Detail: "paused, waiting for instruction"}
- {Pod: pod-2/c1, PodIdentity: idC, Name: m2, Serving: false, Terminal: false, Detail: "paused, waiting for instruction"}
- {Pod: pod-3/d1, PodIdentity: idD, Name: m3, Serving: false, Terminal: true,  Detail: "dead"}
Engine: <driver-private: API addresses, raw engine status>
```

`Engine` holds whatever the same engine's driver and policy need later, such as addresses. The driver may cache
internally, keyed by Pod identity, but a new leader must need no warm-up. If `pod-3` is deleted, `Observe`
returns `m0`-`m2` and the controller adds `{Pod: pod-3/d1, PodIdentity: idD, Name: m3, Terminal: true,
Detail: "pod lost"}`.

## Deciding

`Decide` receives a `DecisionInput` assembled from current observations and journaled facts.

| Input | Meaning |
|---|---|
| `Observation` | current engine state, including controller-supplied lost and unknown members |
| `Admitted`, `Candidates`, `Excluded`, `Rejected` | membership facts; rejected identities cannot rejoin |
| `State` | policy checkpoint from the last saved decision |
| `LastResult` | most recently completed operation, status, scope and reason |
| `Starting` | the group is awaiting adoption |
| `ObservationTimedOut` | no admitted member was readable within the observation interval; clears when reads recover |
| `Fault` | persisted membership safety fault |
| `Elapsed` | time since the saved epoch, measured by the controller |
| `DiagnosisTimeout`, `VerifyTimeout`, `FlapWindow` | policy timing settings |
| `Budget` | controller recovery activity, limits and any blocked reason |

The policy returns one `Action`:

| Kind | Meaning |
|---|---|
| `Healthy` | the admitted group can serve, possibly at reduced capacity |
| `Wait` | more observation is needed |
| `Probe` | run an engine check to expose a fault |
| `Verify` | verify the named `Participants` can serve |
| `Change` | perform the membership operation described by `Participants`, `Remove`, `Add` and `Payload` |
| `ReplaceMembers` | native recovery needs replacement resources for the excluded or rejected `Members` |
| `RecreateGroup` | recovery in place cannot continue; request group recreation |

`Healthy` and `Wait` save policy state without starting an operation. `Probe`, `Verify` and `Change` go through
`Apply`. The controller maps replacement requests to [workload actions](../controller.md#getting-back-to-full).
Every action passes the controller's [safety and budget checks](../controller.md#safety-checks).

`Action.State` is the policy's opaque JSON checkpoint. A nil value retains the saved checkpoint; a supplied
JSON value replaces it. The controller stores it separately and strips it from the saved action. The
execution driver receives the action's `Payload` and its own `Operation.Checkpoint`.

`LastResult` remains available until another operation completes. The policy can save the consumed result ID
so repeated observations do not count the same failure twice. Its checkpoint can track replacement attempts
for an original member even when each candidate has a new identity.

Every `Change` has one of these shapes:

| Change | Remove | Add |
|---|---|---|
| retry | nobody | nobody |
| shrink | terminal members | nobody |
| grow | nobody | joinable candidates |
| replace | terminal members | joinable candidates |

[example engine-plans]: ../../scenarios/engine-plans.json

## Applying an action

```go
Apply(ctx, ApplyInput{
    Observation: observation,
    Operation:   savedOperation,
    MayAdvance:  true,
})
```

`Apply` first establishes what happened to this operation, then waits, advances it, or reports completion.
Its `Report` supplies an updated opaque execution checkpoint. The controller saves that checkpoint before
the next call. A nil checkpoint retains the current value.

| Report | Controller behavior |
|---|---|
| `Running` | save checkpoint and resume the same operation on a later observation |
| `Succeeded` | save the result and committed membership, then call `Decide` |
| `Failed`, scope `Joiners` | for a pure add, reject the failed candidates and retain admitted survivors |
| `Failed`, scope `Group` | record the established failure and membership safety fault |
| Go error | retain the pending operation; the error does not establish its outcome |

An accepted request is still `Running`. A lost response or network timeout does not establish failure or
prove that remote work stopped. A driver must establish terminal effects before returning `Failed`.

### Safe resumption

An operation has a stable ID, immutable action, execution checkpoint and an `Attempted` marker. The
controller saves `Attempted` before entering `Apply`; only the first call receives `Attempted: false`.
After a crash, `Attempted: true` means the call may have reached the runtime. The driver must use remote
readback or deduplication to avoid starting it again blindly.

A stage checkpoint alone cannot close the crash window. For example, the runtime might launch a replacement
and the controller might crash before saving that stage. The next call must discover the launch or repeat a
request deduplicated by operation and stage identity. A plugin requiring local memory cannot satisfy this
contract.

When `MayAdvance` is false, `Apply` may inspect and report the operation but must cause no additional effects.
The controller uses this mode after the original deadline or while safety is uncertain. It accepts an
established terminal report; it keeps an unknown operation pending. The deadline never proves cancellation.

### Probing and verification

`Probe` and `Verify` are actions executed through `Apply`. A probe can make idle peers notice a failed
collective. Verification can send bounded inference requests through `Participants`. Their results feed the
next decision. The driver defines the engine checks and their execution; there are no separate public
probe, verification or progress methods.

## Growing back

`Capabilities.Grow` states whether the driver can admit new members into a running group. The controller
uses it with the capacity configuration when handling `ReplaceMembers`. An engine without admission support
can keep serving at reduced capacity or have its eligible resources released or its group recreated,
according to operator configuration.

## Replacing a process in a shared Pod

`Observe` may report a replacement descriptor before its process starts. Its `Name` is a new process identity
and `Replaces` names the excluded predecessor. The runtime must reserve that identity durably. Reading it
performs no launch. The old member and candidate can appear in the same observation.

`Decide` selects `Change` with `Add` when admission is appropriate. Repeated `Apply` calls can prepare the
survivors, clean up old resources, launch the reserved process and commit membership. Those stages stay in
the driver. Each call uses the same saved operation, fresh observation and persisted execution checkpoint.

`Succeeded` requires the reserved added identities to be observed and membership to have committed. A
joiner-scoped failure requires proof that admitted members are unchanged and the candidates are stopped or
fenced. It is valid only for a pure add. An uncertain commit remains pending.

[The shared-Pod example](../../failures/member-dies-in-shared-pod.md) shows admission across several calls and
a leader restart. [The failed admission example](../../failures/member-rejoin-fails.md) shows saved policy
retry history and the independent controller budget.

A driver advertises `Grow` only for paths it can execute with its configured runtime or launcher. Native
topology expansion alone does not establish failed-member replacement support.

## API reference

### Interface

```go
type Engine interface {
    Capabilities() Capabilities
    Driver
    Policy
}

type Driver interface {
    Observe(ctx context.Context, pods []Pod) Observation
    Apply(ctx context.Context, input ApplyInput) (Report, error)
}

type Policy interface {
    Decide(input DecisionInput) Action
}
```

`Pod` and `PodRef` come from the [workload](../workloads/index.md#objects).

### Objects

```go
type Capabilities struct {
    Grow bool
}

type Member struct {
    Pod         PodRef
    PodIdentity string
    Name        string
    Replaces    MemberID
    Serving     bool
    Terminal    bool
    Unknown     bool
    Joinable    bool
    Detail      string
}

type MemberID string // Name@PodIdentity

type Observation struct {
    Members []Member
    Engine  any
}

type DecisionInput struct {
    Observation         Observation
    Admitted            []MemberID
    Candidates          []MemberID
    Excluded            []MemberID
    Rejected            []MemberID
    State               json.RawMessage
    LastResult          *Result
    Starting            bool
    ObservationTimedOut bool
    Fault               string
    Elapsed             time.Duration
    DiagnosisTimeout    time.Duration
    VerifyTimeout       time.Duration
    FlapWindow          time.Duration
    Budget              RecoveryBudget
}

type RecoveryBudget struct {
    Actions    int
    MaxActions int
    Elapsed    time.Duration
    Timeout    time.Duration
    Blocked    string
}

type Action struct {
    Kind         ActionKind
    Participants []MemberID
    Remove       []MemberID
    Add          []MemberID
    Members      []MemberID
    Payload      json.RawMessage
    State        json.RawMessage
    Reason       string
}

type ActionKind string // Healthy | Wait | Probe | Verify | Change | ReplaceMembers | RecreateGroup

type Operation struct {
    ID         string
    Action     Action
    Checkpoint json.RawMessage
    Attempted  bool
}

type ApplyInput struct {
    Observation Observation
    Operation   Operation
    MayAdvance  bool
}

type Report struct {
    State      Status
    Scope      FailureScope
    Reason     string
    Checkpoint json.RawMessage
}

type Result struct {
    Operation Operation
    State     Status
    Scope     FailureScope
    Reason    string
}

type Status string // Running | Succeeded | Failed

type FailureScope string // Joiners | Group
```
