# Runtime Facade (V090-012)

Package: [`internal/runtimefacade`](../../internal/runtimefacade)
Issue: [#1104](https://github.com/jasonhnd/loopcoder/issues/1104)

## Port

| Type | Role |
| --- | --- |
| `Runtime.Launch` | Start one attempt; immutable `LaunchRequest` snapshot |
| `Handle.Observe` | OS-backed liveness (`alive` / `exited` / `unknown`) |
| `Handle.Signal` | term / kill / interrupt |
| `Handle.Join` | Wait for terminal process evidence |

Success requires `TerminalEvidence.Exited`. Provider stdout/stderr and prose are
**not** completion authority. Output sinks are optional and diagnostic only.

## Adapters in this PR

| Adapter | Backing | Use |
| --- | --- | --- |
| `FixtureRuntime` | `os/exec` + process-group signals | Tests and synthetic fixtures |
| `SupervisedRuntime` | `internal/supervisedexec.Run` | One generic local command path |

No second process-group or PTY supervisor is introduced. Resource policy,
report scheduling, and CLI surfaces are out of scope.

## Direct-launch disposition inventory

Remaining callers that still launch processes outside this facade (migrate later
or keep as compatibility):

| Location | Current mechanism | Disposition |
| --- | --- | --- |
| `internal/agent/*` provider runners | `supervisedexec.Run` via `agent.Runner` | **later migration** — keep until workflow path uses facade |
| `internal/worker` | `agent.Lookup` + `Runner.Run` | **later migration** (V090-030+) |
| `internal/cli` delivery / nested | `agent.Runner` / `supervisedexec.Run` / `exec.Command` | **later migration** / nested compatibility |
| `internal/cli/detached.go` | `exec.CommandContext` spawn | **later migration** (V090-094 detach) |
| `internal/supervisedexec` | kill-group, guardian, stall | **keep** — low-level mechanics owned here |
| `internal/process` | PID identity snapshots | **keep** — used by supervisedexec / V090-013 |
| `internal/acceptharness.FakeProvider` | synthetic provider helper | **compatibility-only** for acceptance fixtures |
| `internal/runtimefacade` | fixture + supervised adapters | **keep** — v0.9 product port |

This issue does **not** migrate the inventory rows; it establishes the only new
P2 entry point so later issues do not invent parallel launch APIs.

## Tests

```bash
go test ./internal/runtimefacade -count=1
```
