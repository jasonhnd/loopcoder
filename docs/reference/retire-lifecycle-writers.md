# Retire parallel progress/report/relay/outbox writers (V090-074)

Package: [`internal/nolifecycle`](../../internal/nolifecycle)  
Issue: [#1188](https://github.com/jasonhnd/loopcoder/issues/1188)

## Purpose

Remove v0.8 progress/report/relay/outbox code that writes lifecycle truth in
**parallel** with project events. Project events are sole lifecycle truth.

## Allowed

- Pure projection from event ids → `loopcoder.ui.v1` (`ProjectFromEvents`)
- `reportquery` / `reporter` projection actions only when `fromEvents=true`

## Denied

- create / flush / close / ack / write on progress, report, relay, outbox, claims
- compat commands: `progress`, `report-write`, `relay-flush`, `outbox-*`, `ack-progress`

## Verification

```bash
go test ./internal/nolifecycle/
```
