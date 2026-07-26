# Remove repository-local runtime fallbacks and sidecars (V090-072)

Package: [`internal/nosidecar`](../../internal/nosidecar)  
Issue: [#1186](https://github.com/jasonhnd/loopcoder/issues/1186)

## Purpose

Delete every **production** fallback that writes `.loopcoder`, run sidecars,
relay, recovery, log, or temporary payloads inside customer repositories.
Retain read-only migration discovery for V090-069 only. Never auto-delete
existing repo-local files.

## Manifest dispositions

| Pattern | Disposition |
| --- | --- |
| `.loopcoder`, runs/relay/recovery/logs/tmp | `removed_writer` |
| `.loopcoder/state`, `.loopcoder/db` | `read_only_export` |
| `.delivery.yml`, `.loopcoder.yml` | `policy_file_readonly` |

## Rules

1. Worker code changes and Git metadata writes remain allowed.
2. Unregistered project identity never chooses `<repo>/.loopcoder`.
3. Registration errors never fall back to repo-local production paths.
4. Read-only exporter may open export-disposition paths only.
5. `ScanCanaryPaths` lists patterns that must stay free of new production writes.

## Verification

```bash
go test ./internal/nosidecar/
```
