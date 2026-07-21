# v0.9 Project Authority Schema (V090-008 / V090-009 / V090-010)

Package: [`internal/projectschema`](../../internal/projectschema)
Issues: [#1100](https://github.com/jasonhnd/loopcoder/issues/1100),
[#1101](https://github.com/jasonhnd/loopcoder/issues/1101),
[#1102](https://github.com/jasonhnd/loopcoder/issues/1102)

## Tables

| Table | Role |
| --- | --- |
| `project_meta` | Singleton project identity row |
| `work_items` | WorkItem projections |
| `jobs` | Local execution jobs |
| `attempts` | Provider/model attempts |
| `events` | Append-only lifecycle truth |
| `projection_checkpoints` | Append-side last-sequence hint (same txn as event) |
| `projection_state` | Rebuildable reducer checkpoints with generations (V090-010) |
| `external_evidence_refs` | GitHub/provider/local evidence pointers |
| `ui_client_cursors` | UI follow cursors |
| `ui_acknowledgements` | Final-mile acks |

## Event envelope

Required fields: `event_id`, `project_id`, `aggregate_kind`, `aggregate_id`,
`kind`, `envelope_version`, `sequence`, `recorded_at`, `idempotency_key`,
bounded `payload_json`, optional `causal_event_id` / `evidence_ref_id`.

Immutability is enforced by SQL triggers; corrections append a new event.
`Append` is atomic with sequence assignment and idempotency (V090-009).

## Isolation

Each project uses its own `project.db` under `$LOOPCODER_HOME/projects/<id>/`.
Overlapping issue numbers across projects do not share sequence space.

## Cursor replay (V090-010)

Opaque cursors encode only `format_version`, `project_id`, and last accepted
`sequence`. Encoding is base64url(JSON); paths and payloads are never included.

| API | Role |
| --- | --- |
| `EncodeCursor` / `DecodeCursor` | Opaque cursor codec |
| `ValidateCursorForProject` | Reject wrong project / future format |
| `ZeroCursor` | Position before sequence 1 |
| `Replay` | Bounded page after cursor (default 100, max 500), stable order |
| `ReplayAll` | Multi-page helper for rebuild/tests |

Invalid, future-version, and wrong-project cursors return `ErrInvalidCursor`
without silently falling back to sequence zero.

## Projection checkpoints (V090-010)

`projection_state` stores compact, disposable reducer output. Events remain the
sole source of truth.

| Field | Meaning |
| --- | --- |
| `generation` | Rebuild generation; only one row may be `is_current=1` |
| `reducer_version` | Reducer identity/version string |
| `input_sequence` | Last event sequence incorporated |
| `output_digest` | `sha256:` digest of payload |
| `payload_json` | Compact payload (≤ 8192 bytes) |

| API | Role |
| --- | --- |
| `GetCurrentCheckpoint` | Read current generation; corrupt payload → `ErrRebuildRequired` |
| `AdvanceCheckpoint` | CAS advance from expected input sequence |
| `BeginRebuild` | Stage a non-current generation from sequence 0 |
| `WriteRebuildCheckpoint` | Update staging generation during rebuild |
| `SwapCurrent` | Atomically promote staging generation |
| `RebuildProjection` | Full rebuild helper (replay → reduce → swap) |

Rebuild writes a **new** generation, then swaps. Event append is not blocked for
the duration of rebuild. Corrupt or missing projection data never mutates
`events`; callers get `ErrRebuildRequired`.

## Tests

```bash
go test ./internal/projectschema -count=1
```
