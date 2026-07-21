# v0.9 Acceptance Fixture Harness (V090-003)

Package: [`internal/acceptharness`](../../internal/acceptharness)  
Issue: [#1095](https://github.com/jasonhnd/loopcoder/issues/1095)

## Purpose

Provide deterministic disposable fixtures so later v0.9 issues can prove store,
process, host, Git, and GitHub behavior **without** real providers, network,
keychain, or browser access, and without using LoopCoder to develop itself.

## Public surfaces

| Helper | Role |
| --- | --- |
| `ManualClock` / `Clock` | Injected time; tests advance time explicitly |
| `Barrier` | Explicit synchronization without correctness sleeps |
| `CleanProcessEnv` | Strips `GIT_*` scoping and common credential env vars |
| `CreateRepo` | Disposable `docs-only` or `small-go` git repositories |
| `FakeProvider` | Subprocess modes: emit, silent, spawn_child, nonzero, hang, flood, complete |
| `FakeGitHub` | In-memory issues/PRs/checks with push timeout and duplicate create |
| `FakeUI` | Deliver/ack with disconnect, reconnect, duplicate ack, replay cursor |
| `ProcessObserver` | Tracks live fixture PIDs; scenarios require zero survivors |
| `RunGoldenScenario` | Full happy path + evidence manifest |
| `RunFaultScenario` | Injected failure + optional resume |
| `Manifest` | Bounded evidence record (`loopcoder.acceptance_manifest.v1`) |

## Scenario contracts

### Golden

1. Freeze synthetic effective policy via `effectivepolicy.Resolve`.
2. Create disposable repo and run fake provider to a completion record.
3. Commit a synthetic change on a fixture branch.
4. Open PR, mark required checks green, deliver UI message, ack `rendered`.
5. Assert zero surviving children.
6. Write `acceptance-manifest.json` with tested SHA, events, side effects.

### Fault / resume

Supported failure points: `push_timeout`, `ui_disconnect`, `provider_nonzero`,
`duplicate_pr`, `duplicate_ack`, `provider_hang` (cancelled via context).

## Privacy

- Synthetic owner/repo/issue/token shapes only.
- Manifests fail closed if they contain `/Users/...`, `HOME=`, or secret-shaped tokens.
- Fixture subprocesses inherit `CleanProcessEnv` only.

## Resource ceiling

One test process, at most a handful of fixture children, focused package tests
only. Remote CI owns repository-wide suites (see evidence tiers).

## Non-goals

- Production process supervisor.
- Real provider auth/quota.
- Machine/project schema implementation.
- Second orchestration framework.
