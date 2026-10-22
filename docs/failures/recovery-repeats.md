# Recovery keeps repeating

An engine policy continues proposing recovery actions after earlier attempts fail.

- **IRO sees:** new mutating actions for the same logical group, including actions with new candidate identities.
- **IRO does:** charges each saved new mutation once against a durable count and time budget.
- **Ends with:** new mutations blocked at the limit, while observation continues.
- **Rules:** [bounding repeated recovery](../components/controller.md#bounding-repeated-recovery).

The example uses a three-action limit. Removal succeeds, then two replacement admissions fail safely. The
controller blocks the next admission. Polls and resumptions of an existing operation do not consume extra
counts. Engine policy can stop earlier using its own saved attempt history.

| New mutation | Count |
|---|---|
| remove original member | 1 |
| admit first replacement | 2 |
| admit second replacement | 3 |
| propose another replacement | blocked; no execution |

[example recovery-budget]: ../scenarios/recovery-budget.json

Controller restarts and Pod or group recreation preserve the budget. Only sustained confirmed healthy
serving clears it. The telemetry incident can close earlier and does not govern these limits.
