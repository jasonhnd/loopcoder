# Compatibility shims and old/new writer isolation (V090-071)

Package: [`internal/compatshim`](../../internal/compatshim)  
Issue: [#1185](https://github.com/jasonhnd/loopcoder/issues/1185)

## Purpose

Keep only narrowly required v0.8 read/view/export compatibility for **one
release** while guaranteeing legacy and v0.9 paths cannot both mutate the same
project or present conflicting authority.

## Command classes

| Class | Behavior |
| --- | --- |
| `removed` | fail with replacement guidance (compile/dispatch/tick/…) |
| `read_only_compat` | status/show/history; prefixed output; excluded from v0.9 gates |
| `explicit_exporter` | export-v08 only |
| `unsupported` | mutating legacy writers refused |
| `v09_only` | doctor/import-v09/rehydrate → v0.9 stores exclusively |

## Writer isolation

- `BeginWrite(project, cmd, GenV09)` marks the project as v0.9 authority.
- Subsequent `GenV08` mutation attempts fail closed.
- Removed/unsupported mutating commands never write even before v0.9.

## Compatibility output

All compat surfaces use prefix:

```text
[loopcoder-compat-v0.8]
```

`IncludeInV09Status` is true only for `v09_only` commands.

## Deprecation schedule

| Surface | Until |
| --- | --- |
| read-only compat | 0.9.x |
| explicit exporter | 0.9.x |
| removed commands | effective 0.9.0 |

## Verification

```bash
go test ./internal/compatshim/
```
