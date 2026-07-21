# Effective Policy Snapshot (V090-085)

This living reference describes the v0.9.0 configuration authority resolver
implemented in [`internal/effectivepolicy`](../../internal/effectivepolicy).
Issue: [#1094](https://github.com/jasonhnd/loopcoder/issues/1094).

## Purpose

Before provider launch, worktree creation, UI bridge start, or GitHub side
effects, LoopCoder freezes one **effective-policy snapshot** with:

- normalized values for route pins and resource limits;
- per-field **provenance** (where the value came from);
- a stable **digest** over schema + values + sources (not wall-clock time);
- human and JSON **explain** output that redacts secrets and local paths.

Credentials and provider authentication material are **never** configuration
fields.

## Precedence (high wins)

| Rank | Source id | Surface |
| ---: | --- | --- |
| 50 | `explicit_cli` | Explicit operator CLI / host pin for this run |
| 40 | `approved_run_request` | Approved run request record |
| 30 | `project_policy` | Optional reviewed project policy file (repo-relative) |
| 20 | `user_local` | User-local configuration under `$LOOPCODER_HOME` (not the repo) |
| 10 | `compiled_default` | Safe compiled defaults in the binary |
| 5 | `compatibility` | Compatibility-derived value (explicitly marked) |

Environment variables such as `LOOPCODER_PROVIDER` **do not** contribute values
and cannot override pins. When present and conflicting, Resolve records a
warning that the environment value was ignored.

## Schema

Policy files use YAML with `schema_version: 1` and only these keys:

```yaml
schema_version: 1
provider: grok
model: grok-4.5
effort: high
permission: read_only   # read_only | bounded_write | orchestrate
report_client: terminal
base_branch: pre-prod
project_policy_path: docs/loopcoder-policy.yml   # repo-relative only
max_child_processes: 8
max_rss_mib: 2048
retention_days: 14
native_subagents: false
```

Unknown keys, missing/incompatible `schema_version`, absolute or `..` policy
paths, and non-positive limits fail closed before side effects.

## Immutability

A snapshot is a value object. Approved configuration changes produce a
**successor** snapshot via `Successor(prev, inputs)` / a new `Resolve` — never
an in-place rewrite of a prior digest. Later run/attempt writers must store the
new digest rather than mutate history.

## Security vocabulary

Reading a frozen snapshot requires `cap.config_freeze` from
[`internal/securitypolicy`](../../internal/securitypolicy). Untrusted
issue/provider/UI content must not appear as a configuration source.

## Consumers

- Direct-run admission (`loopcoder run`) must call `Resolve` once and pass the
  snapshot digest into durable events.
- `V090-003` fixtures should freeze synthetic inputs and assert digests.
- Explain surfaces use `Snapshot.Explain` / `ExplainJSON` only.

## Non-goals

- Routing scores, provider probes, report transport, or credential management.
- Replacing `.delivery.yml` for v0.8 orchestration in this issue.
- Writing runtime state into the repository.
