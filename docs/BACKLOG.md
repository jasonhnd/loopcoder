# loopcoder — Backlog (deferred)

Items intentionally deferred to keep M1 focused on getting the full loop running. Nothing here blocks M1.

## Principle: inherit by default, never pester per-run
loopcoder must not make the user choose routine settings (model, speed, etc.) on every run. By default it passes no model or reasoning-effort flags and inherits the underlying tool's global config. The user can configure a preference explicitly; the loop must never choose one on its own.

## Deferred items

### B1 — Worker model & speed selection
Raised 2026-06-26. Implemented in the native binary: when `loopcoder dispatch` passes no `--model` or `--effort` flags, the codex worker follows codex's global `~/.codex/config.toml`. That inheritance remains the default.

The binary exposes knobs so model + reasoning effort are configurable only when the user explicitly asks:
- `loopcoder dispatch`: optional `--model` and `--effort` flags pass `codex exec -m <model> -c model_reasoning_effort=<effort>` only when provided.
- `.delivery.yml`: document optional `worker.model` and `worker.reasoning_effort` as commented keys only; absent means inherit codex's global default.
- No automatic per-issue effort routing. The config and command line must only reflect what the user has said.

This is the first concrete knob of the provider-pluggable Worker port. Multi-provider adapters (gemini / claude / openai) remain M3.
