# Remove autonomous compile/tick/trigger/promotion entry points (V090-076)

Package: [`internal/noauton`](../../internal/noauton)  
Issue: [#1190](https://github.com/jasonhnd/loopcoder/issues/1190)

## Purpose

Remove v0.8 paths that compile ROADMAP markers, synthesize epics, continuously
tick/trigger, or promote/merge without explicit v0.9 direct-run/workflow and
human gate.

## Denied

`compile`, `tick`, `trigger`, `promote`, conductor auto, orchestration loop,
risk-gate auto, issue synthesis, autonomous schedules.

## Preserved

- Explicit **bounded wave** scheduler (requires workflow definition)
- Explicit **human/release** gate
- Zero-model watcher **facade** only

Roadmap markers are inert documentation (`RoadmapMarkerInert`).

## Verification

```bash
go test ./internal/noauton/
```
