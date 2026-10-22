# Controller

## What it does

The controller runs one recovery loop per engine group on the elected leader. The engine policy chooses
an action from observations and saved results. The controller enforces shared safety rules and recovery
limits, persists the action, executes it through the engine or workload, and saves the outcome.

It owns Pod and member identity, durable operations, deadlines and workload action selection. The engine
owns native recovery decisions and execution stages. The controller stores engine checkpoints opaquely.

## What it talks to

![IRO manager, its plugins and what they talk to](../figures/architecture.svg)

| With | Calls | When | Gets back |
|---|---|---|---|
| Leader election | | always | the right to run the loop |
| [Journal](journal.md) | `Load` | on leadership and after a conflict | saved [`Record`](#objects) |
| | `Save` | before effects and before exposing results | new version or conflict |
| [Workload](workloads/index.md) | `Discover` | every poll | current Pods |
| | `Reset`, `ResetAll`, `Release` | for a saved workload action | acceptance or error |
| [Engine driver](engines/index.md#driver) | `Observe` | every poll | members and state |
| | `Apply` | with an engine action pending | report and execution checkpoint |
| [Engine policy](engines/index.md#policy) | `Decide` | with no operation pending | next action and policy checkpoint |
| [Telemetry](telemetry.md) | `Recorder` | when something happens | best effort; never read back |

The router follows the engine's own health. A member the engine reports out of service stops receiving
traffic without IRO.

## The life of a group

![Controller phases: starting, serving, recovering](../figures/phases.svg)

| Phase | Meaning |
|---|---|
| `starting` | the group is coming up; policy can request verification before adoption |
| `serving` | policy has reported healthy serving, possibly below full capacity |
| `recovering` | a fault or recovery action is being handled |

Phases summarize status. A new workload group clears recorded membership. Policy state, the previous
result, elapsed-time epoch and recovery budget survive recreation. A brief healthy observation does not
reset the recovery budget.

## Every poll

![One controller step: discover, decide, save and execute](../figures/controller-step.svg)

1. Discover Pods, observe members and add lost members as terminal facts.
2. Validate membership and save any safety fault.
3. Resume the saved operation, or call `Decide` when none is pending.
4. Check and save a new action, its identity, policy checkpoint and any budget charge before execution.
5. Save the execution checkpoint or terminal result before continuing.

A row in this example may span several polls:

| Observation or result | Engine decision | Execution |
|---|---|---|
| all members serving | `Healthy` | record serving |
| `m3` terminal | `Probe` | `Apply` checks the engine |
| survivors ready to reconfigure | `Change`, remove `m3` | repeat `Apply` with one saved operation |
| removal succeeded | `Verify` | `Apply` verifies survivors |
| native recovery needs replacement resources | `ReplaceMembers(m3)` | resolve configured workload action and safe Pod UIDs |
| replacement member joinable | `Change`, add replacement | repeat `Apply` through admission stages |
| expanded group verified | `Healthy` | record serving and begin the stable-health interval |

[example controller-polls]: ../scenarios/controller-polls.json

## Taking over

A new leader loads and resumes the saved operation. Its ID, action, execution checkpoint, attempted marker
and original deadline survive reload. Policy checkpoint and recovery budget survive too.

| The journal holds | The new leader |
|---|---|
| no record | starts in `starting`; a serving group can be adopted |
| policy state and completed result | supplies both to `Decide` |
| engine operation | calls `Apply` with the same saved operation and fresh observation |
| workload operation | continues its saved selection and explicit Pod UID targets |
| unknown operation past its deadline | permits read-only operation reporting; blocks conflicting actions |
| unreadable bytes | returns the load error and tries again next poll |

Every save compares the loaded journal version. A conflict discards unsaved work and reloads the winning
record before any effect. [Safe driver resumption](engines/index.md#safe-resumption) covers a crash between
an effect and its checkpoint save.

[example leader-change]: ../scenarios/leader-change.json

## Seeing the group

### Same group or a new one?

The workload reports current Pods. The controller compares their UIDs and engine-container identities with
the journal to determine whether members belong to the same group.

![Deciding on Discover whether this is the same group or a new one](../figures/adopt.svg)

| Pod compared with the journal | Meaning |
|---|---|
| same UID and identity, not `Terminating` | unchanged |
| recorded as `Removing`, then gone | the saved Pod action completed |
| unexpectedly gone or `Terminating` | its admitted members are lost |
| same UID with a new engine-container identity | old members are lost; members of the restarted container cannot join this group |
| new Pod, `Starting` | a replacement is loading |
| new Pod, `Ready` | its observed members can become candidates |

Lost members are added to the observation as `Terminal`, with `Detail: "pod lost"`. A process replaced within
an unchanged container has a new member ID; the driver reports its `Replaces` association.

A saved `RecreateGroup` operation continues against the old Pod UIDs until they are gone. Workload recreation
or a stopped restart policy suspends recovery actions. `Starting` tells the policy to evaluate the new
membership alongside its saved checkpoint and last result.

### Pods and members

`Discover` reports Pods. `Observe` reports members. The controller joins them using Pod identity and the
engine's member ID.

| State | Meaning |
|---|---|
| candidate | an eligible observed member awaiting admission |
| admitted | part of the group |
| excluded | removed by a successful reconfig |
| rejected | an identity that failed admission and cannot rejoin this group |

The controller exposes these lists to the policy. It resolves a replacement request to Pods whose current identity
matches the recorded identity and whose recorded members are all excluded or rejected. A shared Pod with admitted
survivors cannot be reset or released individually.

### Members it cannot read

An unreadable member is `Unknown`; it cannot be removed by a new action. The policy receives this observation
and decides whether to wait or reset the group. The controller refuses a `Healthy` result that overlooks an
unreadable admitted member, and refuses a change naming an unreadable participant.

An unknown member never becomes terminal by timeout. Only the driver or a lost Pod identity can prove the
old process is gone.

### When it cannot see the group

A failed `Discover` prevents every engine and workload action. When no admitted member can be read, the
controller waits for `--ft-unseen-timeout` before setting `DecisionInput.ObservationTimedOut`. With workload
visibility available, the policy can request `RecreateGroup` or keep waiting. The flag clears when reads recover.
If a membership operation was pending, it remains pending and further execution is fenced.
Restored visibility alone does not establish its outcome; the driver must report an established result.

Elapsed time and saved operation deadlines continue across visibility gaps. The controller can evaluate
the observation timeout only while `Discover` succeeds. An established completion can be accepted after
the deadline; unresolved work remains pending without further effects.

[example controller-blind]: ../scenarios/controller-blind.json

## Deciding

The [engine contract](engines/index.md#deciding) defines `DecisionInput` and action kinds. The controller
provides membership, observation, saved policy state, the last result, elapsed time and remaining recovery
limits. Engine policy receives no list of resettable Pods and no workload capacity preference.

`Healthy` and `Wait` save state without dispatch. `Probe`, `Verify` and `Change` create engine operations.
`ReplaceMembers` requests replacement resources; the controller maps that request to a workload action.
`RecreateGroup` requests whole-group recreation because in-place recovery cannot continue.

### Safety checks

An unsafe observation or established group-scoped membership failure is persisted in `DecisionInput.Fault`.
It permits only waiting or requesting whole-group recreation once no operation remains unresolved. A pending
operation always takes precedence over a new decision.

| # | Rule | Why |
|---|---|---|
| C1 | `Remove` names only admitted, terminal members | removed processes must be proven gone |
| C2 | `Add` names eligible, joinable candidates on ready, unchanged Pods | stale or rejected identities cannot join |
| C3 | participants and removals are admitted; additions are candidates; lists are disjoint | membership changes must be consistent |
| C4 | an excluded member stays terminal whenever readable | an excluded process must not resume serving |
| C5 | an unadmitted member cannot serve, except a joiner in the pending operation | admission must precede serving |
| C7 | one operation at a time; named members exist in the observation, including lost members | stale or conflicting actions must not overlap |
| C8 | individual Pod reset or release uses current UIDs and no admitted members | serving members must retain their Pod |

### Bounding repeated recovery

The durable `RecoveryBudget` is independent of the telemetry incident. It counts new mutating actions and
tracks a recovery deadline for the logical group. Engine policy receives these facts through `Budget` and
can stop sooner using its own checkpointed retry policy.

A mutation consumes one count when its intent is saved, before execution. Successful actions count too.
Repeated `Apply` calls, checkpoint saves, leader reloads and UID-scoped workload retries do not consume
additional counts. `Probe`, `Change` and workload-changing actions consume the budget; `Verify` does not.
Pod replacement and group recreation retain the budget.

When the count or elapsed-time limit is reached, the controller records a blocked reason and refuses new
mutating actions. It continues observation and can finish an existing operation within that operation's
original deadline. Reaching the episode limit does not cancel already authorized remote work.

The budget clears only after the policy reports healthy serving continuously for the configured stable
interval and the controller confirms admitted members are serving. A brief `Healthy` decision, a new
candidate identity or a recreated Pod does not open a new budget. A lost observation, failed controller step
or leader reload restarts the stable interval.

[example recovery-budget]: ../scenarios/recovery-budget.json

## Repairing a fault

An operation contains its unique ID and immutable `Action`. Policy state is saved separately. Execution
state is stored in `Operation.Checkpoint`. Before the first driver call, the controller durably records
`Attempted`; the first call alone receives the previous false value.

| `Apply` report | Controller behavior |
|---|---|
| `Running` | save checkpoint; resume the same operation |
| `Succeeded` | record membership effects and terminal result |
| `Failed`, scope `Joiners` | for a pure add, reject candidates after the driver proves survivors unchanged and candidates fenced |
| `Failed`, scope `Group` | record failure and fence conflicting membership actions |
| Go error | retain the pending operation and report the call error |

The completed `Result` is saved before another `Decide` call. The controller never guesses a terminal
outcome from a missing response.

### Deadlines

`--ft-apply-timeout` is measured from the saved operation start. Leadership changes, retries and visibility
gaps do not extend it. After the deadline, an engine operation remains pending and receives
`MayAdvance: false`. The driver may read and report an established result but cannot issue further effects.
A deadline does not prove failure, rollback or cancellation; an unresolved operation blocks a conflicting
reset.

Diagnosis and verification intervals are supplied to the policy. It records the relevant start time in its
checkpoint and chooses whether to wait, retry or request recreation. Controller recovery limits apply
independently.

## Getting back to full

`ReplaceMembers` identifies excluded or rejected members whose replacement resources are needed. The
controller resolves those identities to current Pods and selects a workload action using its configured
capacity preference and the engine's `Grow` capability.

![Selecting a workload action for replacement capacity](../figures/capacity.svg)

| Capacity preference | Controller selection |
|---|---|
| `reset-pod`, engine can grow | reset eligible Pods by UID; observe replacement candidates |
| `reset-pod`, engine cannot grow | keep reduced capacity |
| `release` | release eligible Pods by UID and accept reduced capacity |
| `reset-all` | recreate the group |

Individual reset or release never deletes a shared Pod containing admitted survivors. The selection and
exact targets are persisted before execution. Replays use the saved selection and targets. Workload
`ErrUnsupported` becomes a failed result; the engine policy decides whether to issue another request.
Transport errors retain the pending action within its original deadline.

[example two-members-die]: ../scenarios/two-members-die.json

### Replacement admission

A replacement inside an unchanged Pod has a distinct member ID and names its excluded predecessor in
`Replaces`. Policy chooses admission using observation and saved results. The driver executes native
preparation, cleanup, launch and membership commit through resumable `Apply` calls.

After a proven joiner-only failure, the candidate stays rejected. A fresh candidate for the same original
member can appear, and the policy decides whether another attempt is worthwhile. Its saved result ID and
attempt count prevent repeated polling or leader changes from forgetting previous failures. The controller
also enforces its [generic recovery budget](#bounding-repeated-recovery).

See [shared-Pod admission](../failures/member-dies-in-shared-pod.md) and
[failed replacement admission](../failures/member-rejoin-fails.md).

## Resetting the group

`RecreateGroup`, or `ReplaceMembers` under `reset-all`, selects workload `ResetAll` with the recorded Pod
UIDs. Execution continues until the old Pods disappear. Workload restart policy controls recreation;
`RecreateStopped` suspends further recovery actions.

Whole-group recreation consumes a recovery action and preserves the episode budget. It cannot bypass an
unresolved engine operation or an exhausted mutation budget.

## Dry run

With `--ft-dry-run`, the controller loads the journal read-only, discovers Pods and observes the engine.
It calls the pure `Decide` policy when no operation is pending, records the proposed action in memory and
logs changes through `WouldDo`. It never calls `Apply`, changes workload resources or writes the persistent
journal. Saved operations are previewed without resuming execution.

| Poll | Observation | Logged proposal |
|---|---|---|
| 1 | a member is terminal | remove the member |
| 2 | the same member is terminal | unchanged; no new log |
| 3 | the engine reports recovery impossible | recreate the group |
| 4 | the engine recovers without intervention | healthy |

Dry run shows the action selected from current observations. Later execution stages and results require
an action to run, so dry run cannot simulate a complete recovery. Proposed mutations do not consume the
recovery budget or advance the saved action. Deadlines and the loaded recovery budget still constrain the
proposal. The in-memory record is discarded when the controller stops.

## Guarantees

| Guarantee | How |
|---|---|
| One writer per group | leader election and versioned journal saves |
| Intent saved before effect | action, targets, attempted marker and budget charge are durable |
| Result saved before next decision | `LastResult` is a journaled completion |
| One operation at a time | resume pending work before calling `Decide` |
| Resumption after restart | ID, action, checkpoints and deadline survive reload |
| Bounded repeated recovery | durable count and time limits survive recreation |
| Repeatable workload calls | saved selection and explicit Pod UIDs |
| No effect after failed save | unsaved changes never become the active record |
| No action when discovery fails | safe workload targets cannot be established |

## Configuration

| Flag | Default | Meaning |
|---|---|---|
| `--enable-engine-ft` | `false` | enable the controller; requires leader election |
| `--ft-dry-run` | `false` | observe and log proposed actions without effects; see [dry run](#dry-run) |
| `--engine` | | engine implementation |
| `--workload` | | workload implementation |
| `--ft-poll-interval` | 1s | leader poll interval |
| `--ft-diagnosis-timeout` | 75s | diagnosis interval supplied to policy |
| `--ft-apply-timeout` | 90s | pending action's mutation deadline |
| `--ft-verify-timeout` | 2m | verification retry interval supplied to policy |
| `--ft-unseen-timeout` | 10m | unreadable-engine interval before `ObservationTimedOut`; 0 waits forever |
| `--ft-capacity-restore` | `reset-pod` | workload choice: `reset-pod`, `reset-all`, or `release` |
| `--ft-recovery-max-actions` | 8 | maximum new mutations in one recovery episode |
| `--ft-recovery-timeout` | 15m | maximum episode duration before new mutations are blocked |
| `--ft-recovery-stable-period` | 5m | continuous confirmed healthy interval required to clear the budget |
| no flag | 2m | `FlapWindow`, supplied to policy |

Recovery-budget settings must be positive. Each engine and workload registers its own flags.

## API reference

### Interface

```go
func New(ctx context.Context, workload Workload, engine Engine, journal Journal, recorder Recorder, config Config) (Controller, error)

type Controller interface {
    Step(ctx context.Context) error
}
```

The runner calls `New` until the journal loads, then `Step` every `PollInterval` while this process leads.
After `ErrConflict`, it reloads the saved record through `New`.

### Objects

```go
type Config struct {
    DryRun               bool
    PollInterval         time.Duration
    DiagnosisTimeout     time.Duration
    ApplyTimeout         time.Duration
    VerifyTimeout        time.Duration
    UnseenTimeout        time.Duration
    FlapWindow           time.Duration
    CapacityRestore      CapacityRestore
    RecoveryMaxActions   int
    RecoveryTimeout      time.Duration
    RecoveryStablePeriod time.Duration
}

type CapacityRestore string // reset-pod | reset-all | release

type RecoveryBudget struct {
    Started     time.Time
    Actions     int
    StableSince time.Time
    Blocked     string
}

type WorkloadAction struct {
    Kind CapacityRestore
    Pods []PodRef
}

type Phase string // starting | serving | recovering

type MemberState string // candidate | admitted | excluded | rejected

type PodRecord struct {
    Name     string
    Identity string
    Node     string
    Removing bool
}

type Record struct {
    Version int
    Phase  Phase
    Since  time.Time
    Reason string
    WouldDo string

    Pods    map[string]PodRecord
    Members map[MemberID]MemberState

    Epoch            time.Time
    PolicyState      json.RawMessage
    Operation        *Operation
    OperationSince   time.Time
    OperationBlocked string
    Workload         *WorkloadAction
    Result           *Result
    Unsafe           string
    Budget           RecoveryBudget

    UnseenSince      time.Time
    UnseenByDiscover bool
    RecreateStopped  bool
    Incident         *Incident
}
```

`Version` identifies journal schema 2. Records with other versions are rejected without overwriting them.
An existing installation requires an explicit migration or a fresh journal after resolving old operations.
`Epoch` anchors policy elapsed time and survives group replacement. `PolicyState` is the policy checkpoint.
`OperationBlocked` records why a pending action cannot advance. `Workload` stores the controller's selected
workload action separately from the original engine action.

`Operation`, `Result` and `MemberID` come from the [engine](engines/index.md#objects); `PodRef` comes from the
[workload](workloads/index.md#objects). `Incident` comes from [telemetry](telemetry.md#objects).

### New

Builds the controller and loads the saved record through `Journal.Load`. A missing record starts in
`starting`. Unreadable data produces an error; the runner tries again next poll. A saved operation retains
its original start time.

### Step

Runs [one poll](#every-poll). It can call `Journal.Save`, the selected engine or workload operation, and
telemetry. An error before saving leaves the last saved state intact. A journal conflict requires reload.
