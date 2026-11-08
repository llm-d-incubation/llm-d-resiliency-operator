# Telemetry

## What it does

Telemetry turns what the controller does into things people can see:

- **logs**: one structured line per event,
- **metrics**: Prometheus, for dashboards and alerts,
- **Kubernetes Events** on the workload object, one series per incident,
- **incident records**: what failed, what IRO decided, how long it took, how it ended,
- **readiness**: whether the leader's loop is still making progress.

It is read by people, Prometheus and `kubectl`, never by IRO, and it never blocks or fails recovery. It is not
the [journal](journal.md): the journal is the controller's current state and must be saved before IRO acts;
telemetry is history and best-effort. It covers issues
[#25](https://github.com/llm-d-incubation/llm-d-resiliency-operator/issues/25) (Events and metrics) and
[#30](https://github.com/llm-d-incubation/llm-d-resiliency-operator/issues/30) (incident records).

## What it talks to

| With | Calls | When | Emits |
|---|---|---|---|
| Controller → recorder | `Stepped` | at the end of every poll | gauges, the time for readiness, the step's error |
| | `PhaseChanged` | on a phase change | log, `PhaseChanged` Event, transition counter |
| | `IncidentOpened`, `IncidentClosed` | when a fault starts and ends | logs, Events, incident counters and durations, history |
| | `ReconfigStarted`, `ReconfigFinished` | when a reconfig is sent and ends | logs, Events, reconfig counter |
| | `PodResetRequested`, `ResetAllRequested`, `ReleaseRequested` | when IRO asks the workload for Pods | logs, Events, reset counters |
| | `SaveFailed` | when a journal save fails | log, save-failure counter |
| Manager → recorder | `Ready` | every readiness check | whether the leader is still making progress |
| Recorder → outside | logger, Prometheus registry, Kubernetes Events, a ConfigMap | asynchronously | |

The controller calls the recorder after each thing happens, once. The recorder decides which signals to emit.

## Incidents

An incident is one fault, from the moment IRO sees it until the group serves again or is reset. It opens when the
engine reports a fault, when some admitted members cannot be read, or when the workload starts or stops
recreating a serving group.

| It records | |
|---|---|
| `ID` | such as `inc-7c1e`. Every Event of the incident starts with it. |
| `Cause` | the reason the controller gave when it opened the incident |
| `Affected`, `Pods`, `Nodes` | the terminal members when it opened (if none, those not serving), plus every member a reconfig removes, and where they ran |
| `Reconfigs` | every reconfig sent for it, with its result |
| `Outcome` | `Recovered` (same members), `Shrunk` (fewer members), `Reset` (IRO recreated the group), or `Recreated` (the workload did, on its own) |
| `Durations` | diagnose (opened → first reconfig or reset), apply (reconfigs running), verify (checking the result), total |

The controller keeps the open incident in its [`Record`](controller.md#objects), so a new leader continues the
same incident and its timings. Closed incidents leave the record and go to telemetry.

## What a recovery looks like

What a person sees while [a member dies](../failures/member-dies.md), in order:

| When | Event | Metric |
|---|---|---|
| `m3` dies | `IncidentOpened`: `inc-7c1e: m3@identity-d1 on pod-3 (node gpu-node-7) is dead; recovering` | |
| | `PhaseChanged`: serving → recovering | `llm_d_iro_phase_transitions_total{from="serving",to="recovering"}` +1 |
| the reconfig is sent | `ReconfigStarted`: `inc-7c1e: reconfig iro-ft-7c1e started: shrink, remove m3@identity-d1` | |
| it succeeds | `ReconfigSucceeded` | `llm_d_iro_reconfigs_total{change="shrink",outcome="succeeded"}` +1 |
| | `PhaseChanged`: recovering → serving | `llm_d_iro_members{state="admitted"} 3`, `{state="excluded"} 1` |
| `pod-3` is reset | `PodReset`: `resetting pod-3 to bring m3 back` | `llm_d_iro_pod_resets_total` +1 |
| verified | `IncidentClosed`: `inc-7c1e: shrunk in 41s (diagnose 4s, apply 22s, verify 15s)` | `llm_d_iro_incidents_total{outcome="Shrunk"}` +1 |

## Logs

One structured line per event, from the controller-runtime logger, with `group`, `incident`, `reconfig` and
`phase` keys. The step's error, if any, is logged at the end of every poll.

## Metrics

All metrics carry `group` (`<namespace>/<workload name>`), are served on the manager's `/metrics`, and take label
values from fixed sets only. Names follow llm-d: `llm_d_iro_*`, like `llm_d_epp_*`.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `llm_d_iro_leader` | gauge | | 1 on the leader, 0 elsewhere |
| `llm_d_iro_phase` | gauge | `phase` | 1 for the current phase |
| `llm_d_iro_phase_transitions_total` | counter | `from`, `to` | phase changes |
| `llm_d_iro_members` | gauge | `state` | members by state: candidate, admitted, excluded, rejected |
| `llm_d_iro_pod_resets_total` | counter | | Pods IRO reset to bring members back, from calls that returned nil |
| `llm_d_iro_reconfigs_total` | counter | `change`, `outcome` | reconfigs by shape (retry, shrink, grow, replace) and result (succeeded, failed_joiners, failed_group, deadline, dropped) |
| `llm_d_iro_unseen_seconds` | gauge | | how long the controller has not seen the group; 0 when it sees it |
| `llm_d_iro_resets_total` | counter | `reason` | resets IRO asked for, by [reason](controller.md#resetting-the-group). Recreations the workload did on its own are not counted here. |
| `llm_d_iro_incidents_total` | counter | `outcome` | closed incidents by outcome |
| `llm_d_iro_incident_duration_seconds` | histogram | `stage` | diagnose, apply, verify, total |
| `llm_d_iro_journal_save_failures_total` | counter | | failed journal saves |
| `llm_d_iro_last_verified_timestamp_seconds` | gauge | | when the last recovery's members passed `Verify` |

Alerts this allows: recovering for more than N minutes, below full for more than N minutes, the group unseen
for more than N minutes, journal saves failing, no leader.

## Events

On the workload object, through the manager's `EventRecorder`. Every Event of an incident starts with its ID.

| Reason | Type | When |
|---|---|---|
| `IncidentOpened` | Warning | a fault opens an incident |
| `PhaseChanged` | Normal | any phase change |
| `ReconfigStarted` | Normal | a reconfig is sent, and again when it is resent (`resent: no member got it`) |
| `ReconfigSucceeded` / `ReconfigFailed` | Normal / Warning | a reconfig finishes |
| `PodReset` | Normal | IRO resets Pods to bring their members back, once per call |
| `PodsReleased` | Normal | IRO releases Pods and serves below full, once per call |
| `GroupReset` | Warning | IRO asks the workload to recreate the group |
| `RecreateStopped` | Warning | the workload reports it gave up recreating the group |
| `GroupUnseen` | Warning | the controller can no longer see the group |
| `IncidentClosed` | Normal | the group serves again, or was reset |

A workload call answered with `ErrUnsupported` emits only a log line. A list the workload cannot handle as a
whole is not recorded; each Pod is then asked for, and recorded, alone.

## Incident history

The last 20 closed incidents, newest first, in ConfigMap `<workload-name>-ft-incidents`, owned by the workload
object. Written best-effort: a failed write is logged and dropped. It is separate from the journal, so the
journal stays small and its saves stay on the critical path only.

## Readiness

The manager's `readyz` check calls `Ready`. It passes when this process is not the leader, or when the leader
completed a step recently. With a 1s poll and no completed step for 45s it fails with
`no completed step for 45s`, so Kubernetes marks the Pod not ready and a stuck leader is visible.

## Guarantees

| Guarantee | Why |
|---|---|
| Never returns an error to the controller, except `Ready` | telemetry must not change what IRO does |
| Never blocks: metrics are in memory; Events and history are written asynchronously | a slow API server must not delay a reconfig or a reset |
| IRO never reads telemetry back | decisions come from the workload, the engine and the journal only |
| No member IDs, Pod names or free text in metric labels | bounded cardinality. IDs and names go in Events and logs. |

## Not built yet

- **Today** IRO logs errors only. It has no IRO metrics, no Events, and its readiness check is a plain ping.
- **Detection time.** Measuring when the engine saw the fault needs a timestamp on `Member`. Until then,
  `diagnose` starts when IRO opens the incident.
- **Fault kinds.** An incident's `Cause` is the policy's free-text reason. A fixed fault kind from the engine
  would let incidents be counted by kind, such as member death, collective stall or frontend loss.
- **Tracing.** An OpenTelemetry trace per incident, following llm-d's conventions, is proposed on
  [#30](https://github.com/llm-d-incubation/llm-d-resiliency-operator/issues/30), with the same boundaries.

## API reference

### Interface

```go
type Recorder interface {
    Stepped(record *Record, err error)
    PhaseChanged(from, to Phase, reason string)
    IncidentOpened(incident *Incident)
    ReconfigStarted(reconfig *Reconfig, plan *Plan)
    ReconfigFinished(reconfig *Reconfig, progress Progress)
    PodResetRequested(pods []PodRef, err error)
    ResetAllRequested(reason ResetReason, pods []PodRef)
    ReleaseRequested(pods []PodRef, err error)
    SaveFailed(err error)
    IncidentClosed(incident *Incident)
    WouldDo(from, to, reason string)
    Ready() error
}
```

The code is in `internal/controller/enginefault/recorder.go`, next to the controller whose state it reports.
`Record` and `Phase` come from the [controller](controller.md#objects); `Reconfig`, `Plan`, `Progress` and
`MemberID` from the [engine](engines/index.md#objects); `PodRef` from the [workload](workloads/index.md#objects).

`ReconfigFinished` with `NotStarted` is a reconfig dropped because no member got it and it no longer passed the
checks (outcome `dropped`). A reconfig ended by a reset or a new group finishes `Running`, with that as its
reason.

### Objects

```go
type Incident struct {
    ID        string
    Opened    time.Time
    Closed    time.Time // zero while open
    Cause     string
    Affected  []MemberID
    Pods      []PodRef
    Nodes     []string
    Reconfigs []ReconfigResult
    Outcome   Outcome // set when closed
    Durations Durations
}

type ReconfigResult struct {
    Reconfig Reconfig
    Progress Progress
}

type Outcome string // Recovered | Shrunk | Reset | Recreated

type Durations struct {
    Diagnose time.Duration
    Apply    time.Duration
    Verify   time.Duration
    Total    time.Duration
}

// ResetReason is a fixed set, so it can be a metric label.
type ResetReason string // unrecoverable | reconfig_failed | diagnosis_deadline | apply_deadline | flap | verify_failed | exclusion_broken | capacity | unseen
```
