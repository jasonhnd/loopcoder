# v0.9 Project Authority Schema (V090-008)

Package: `internal/projectschema`  
Issue: #1100

## Tables

| Table | Role |
| --- | --- |
| `project_meta` | Singleton project identity row |
| `work_items` | WorkItem projections |
| `jobs` | Local execution jobs |
| `attempts` | Provider/model attempts |
| `events` | Append-only lifecycle truth |
| `projection_checkpoints` | Reader checkpoints (V090-010) |
| `external_evidence_refs` | GitHub/provider/local evidence pointers |
| `ui_client_cursors` | UI follow cursors |
| `ui_acknowledgements` | Final-mile acks |

## Event envelope

Required fields: `event_id`, `project_id`, `aggregate_kind`, `aggregate_id`,
`kind`, `envelope_version`, `sequence`, `recorded_at`, `idempotency_key`,
bounded `payload_json`, optional `causal_event_id` / `evidence_ref_id`.

Immutability is enforced by SQL triggers; corrections append a new event.
Production append/idempotency is **V090-009**.

## Isolation

Each project uses its own `project.db` under `$LOOPCODER_HOME/projects/<id>/`.
Overlapping issue numbers across projects do not share sequence space.
