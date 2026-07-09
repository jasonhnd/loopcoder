# Run Lifecycle State

loopcoder derives durable run lifecycle state from repo-local run records under
`.loopcoder/runs/<run_id>/`. The event stream remains append-only, so a fresh
process can replay it after restart and recover the current state plus the
transition history.

## States

The durable states are:

| State | Meaning |
| --- | --- |
| `planned` | The run exists but has not started executable work. |
| `queued` | The run is ready to start or retry. |
| `running` | A worker, verifier, promotion, or orchestration step is active. |
| `waiting` | The run is paused for a child run, dependency, idle adapter, or other wait. |
| `succeeded` | The run completed its current lifecycle successfully. |
| `failed` | The run hit a failure that may need recovery or retry. |
| `cancelled` | The run was intentionally stopped before completion. |
| `abandoned` | The run should not be resumed automatically. |
| `needs-human` | A human decision is required before the run can proceed. |

Terminal states are `succeeded`, `cancelled`, and `abandoned`. `failed` and
`needs-human` can move back to `queued`, `running`, or `waiting` when recovery
or a human decision explicitly restarts work.

## Transition Records

New lifecycle transitions are persisted as compact JSON lines in
`events.jsonl`:

```json
{"ts":"2026-07-09T00:00:00Z","run_id":"run-...","from":"planned","to":"queued","event":"run.lifecycle.transition","source":"explicit"}
```

Each transition is validated against the current replayed state before it is
written. Invalid moves, such as `succeeded` back to `running`, are rejected.

Parent and child runs are ordinary runs linked by optional `parent_run_id` and
`child_run_id` fields on lifecycle transition records. The replayed lifecycle
exposes the parent run ID and a stable sorted list of child run IDs.

## Legacy Mapping

v0.6.x worker and orchestration events are still readable. During replay,
legacy `status` or `outcome` values are conservatively mapped into lifecycle
states:

- `running` and similar in-progress values map to `running`.
- `waiting`, `idle`, and `blocked` map to `waiting`.
- `succeeded`, `success`, `pass`, `done`, and `promoted` map to `succeeded`.
- `failed`, `error`, `hung`, and timeout values map to `failed`.
- `cancelled`/`canceled`, `abandoned`, and `needs-human` map directly.

Unknown legacy values are ignored rather than treated as successful progress.
Repeated same-state legacy events are ignored, which keeps lifecycle history
focused on actual state changes.

## Inspection

`loopcoder status --repo . --run <run_id>` renders the current lifecycle state
and transition count alongside worker and verifier status. `loopcoder state
push --repo . --run-id <run_id>` includes the replayed lifecycle object in the
synthesized `state.json` snapshot when a run does not already have a local
snapshot file.
