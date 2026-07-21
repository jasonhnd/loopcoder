# Bounded Output Capture (V090-014)

Package: [`internal/outputcap`](../../internal/outputcap)
Issue: [#1106](https://github.com/jasonhnd/loopcoder/issues/1106)

## Behavior

| Concern | Policy |
| --- | --- |
| Drain | `Write` always consumes the full buffer for pipe progress |
| Display | Byte, line, and rate limits; tail kept; truncation/drop markers |
| Disk | Owner-only files under `{payloadRoot}/logs/{attemptID}/` |
| Excerpts | Valid UTF-8 + `sanitize.Text` redaction before events |
| Faults | `ErrLogWrite` → `FullyObserved=false` |

Paths must resolve under the project payload root; `../` and absolute escapes
fail with `ErrOutsidePayloadRoot`.

## Terminal evidence

Per stream: `BytesIn`, `BytesWrittenLog`, `DroppedBytes`, `Truncated`, `Digest`
(`sha256:` of raw bytes).

## Tests

```bash
go test ./internal/outputcap -count=1
```
