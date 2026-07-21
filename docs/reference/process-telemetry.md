# Process Telemetry (V090-015)

Package: [`internal/proctelemetry`](../../internal/proctelemetry)
Issue: [#1107](https://github.com/jasonhnd/loopcoder/issues/1107)

## Contract for V090-016

| Field | Meaning |
| --- | --- |
| `Quality` | `full` / `partial` / `unavailable` / `stale` |
| `ProcessCount` | Owned, successfully sampled, non-zombie PIDs |
| `CPUTimeSecs` | Aggregate cumulative CPU |
| `CPURate` + `HasCPURate` | ΔCPU/Δwall; first sample has no rate |
| `RSSBytes` | Aggregate resident set |
| `Crossings` | Edge-triggered threshold events (once per transition) |

Rules:

1. Sample **owned PIDs only** from `processtree` (exclude escaped / reused).
2. `unavailable` / `stale` never mean “idle zero use” as success evidence.
3. Bound reads to `MaxPIDs` (default 64); no full-system scan when identities known.
4. Darwin reader uses `ps -p <list>` only.

## API

```go
s := &proctelemetry.Sampler{Thresholds: proctelemetry.Thresholds{RSSBytes: 1 << 30}}
sample := s.SampleFromAssessment(tracker.Observe())
```

## Tests

```bash
go test ./internal/proctelemetry -count=1
```
