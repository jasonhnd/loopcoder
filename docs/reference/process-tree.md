# Process Tree Identity and Liveness (V090-013)

Package: [`internal/processtree`](../../internal/processtree)
Issue: [#1105](https://github.com/jasonhnd/loopcoder/issues/1105)

## Model

| Concept | Meaning |
| --- | --- |
| `LaunchEvidence` | Root PID + process birth identity + PGID at start |
| `Snapshot` | Bounded, PID-ordered nodes; **comm only** (no argv/env) |
| `Assessment` | Liveness + confidence + attention flags |

### Liveness

| State | Meaning |
| --- | --- |
| `not_started` | No launch evidence |
| `starting` | Reserved for future launch phases |
| `alive` | Owned tree has live non-zombie members (incl. after wrapper exit) |
| `exited` | No live owned members; terminal |
| `unknown` | Observation failed, PID reuse, or ambiguity — **never** success/takeover |

### Rules

1. **PID reuse**: alive PID whose birth identity ≠ launch evidence → `unknown` + `pid_reuse`.
2. **Wrapper exit**: root dead, owned descendants live → `alive`, **not** terminal.
3. **Escape**: child of owned parent outside PGID → `AttentionRequired`, no silent cleanup.
4. Snapshots redact command names that look like secrets; never store argv.

## API

```go
ev, err := processtree.RecordLaunch(pid, attemptID, time.Now())
tr := &processtree.Tracker{Evidence: ev} // Observer defaults to DarwinPS
a := tr.Observe()
```

## Tests

```bash
go test ./internal/processtree -count=1
```
