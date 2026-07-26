# Redacted diagnostic support bundle and no-telemetry default (V090-101)

Package: [`internal/supportbundle`](../../internal/supportbundle)  
Issue: [#1195](https://github.com/jasonhnd/loopcoder/issues/1195)

## Purpose

Produce a bounded, reviewable diagnostic bundle when a run or UI integration
fails **without** uploading private code, prompts, credentials, raw provider
output, or machine identity. Telemetry remains **disabled by default**.

## Modes

1. **Dry-run plan** — owner inspects include/exclude, size estimate, destination
   basename, telemetry=`disabled`, `network_upload=false`.
2. **Local archive** — builds redacted JSON bundle under a local destination;
   this package performs **no** network upload.

## Included (default)

versions, capability matrix ids, schema/integrity summaries, redacted
event/report/ack transitions, process terminal evidence, check names, typed
diagnostics.

## Excluded (default)

source, issue/PR body, prompt, auth files, environment, absolute home paths,
raw logs, tokens, provider responses.

## Privacy

All string fields pass `privacy.RedactFor(host_diagnostics)` and a leak scan
before success. Secrets never appear in findings messages.

## Verification

```bash
go test ./internal/supportbundle/
```
