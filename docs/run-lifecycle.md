# Durable Run Lifecycle

Loopcoder v0.7 stores run lifecycle state in internal storage. The current
state lives on the `runs.status` column and every state change is appended to
`run_events` with event type `run_lifecycle_transition`. Imported v0.6.x
events are appended with event type `legacy_lifecycle_import`; import is
additive and does not rewrite or delete repo-local `.loopcoder/` event files.

## States

The durable lifecycle states are:

- `planned`
- `queued`
- `running`
- `waiting`
- `succeeded`
- `failed`
- `cancelled`
- `abandoned`
- `needs-human`

Valid explicit transitions are:

| From | To |
| --- | --- |
| `planned` | `queued`, `running`, `cancelled`, `abandoned` |
| `queued` | `running`, `cancelled`, `abandoned` |
| `running` | `waiting`, `succeeded`, `failed`, `cancelled`, `abandoned`, `needs-human` |
| `waiting` | `queued`, `running`, `failed`, `cancelled`, `abandoned`, `needs-human` |
| `needs-human` | `queued`, `running`, `failed`, `cancelled`, `abandoned` |

`succeeded`, `failed`, `cancelled`, and `abandoned` are terminal states for
explicit transitions.

## Parent And Child Runs

Runs carry `parent_run_id`, `root_run_id`, `depth`, and `origin` metadata.
Root runs use their own ID as `root_run_id` and depth `0`. Child runs inherit
their parent's `root_run_id`, increment depth by one, and update their
`run_edges` status whenever their lifecycle changes.

For required child aggregation, intervention states win over active states:
if any required child is `failed`, `cancelled`, `abandoned`, or
`needs-human`, the parent aggregate is `needs-human`, even when another
required child is still `running`. If every required child is `succeeded`, the
aggregate is `succeeded`; otherwise it is `running`.
