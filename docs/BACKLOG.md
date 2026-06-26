# loopcoder — Backlog (deferred)

Items intentionally deferred to keep M1 focused on getting the full loop running. Nothing here blocks M1.

## Principle: sensible defaults, never pester per-run
loopcoder must not make the user choose routine settings (model, speed, etc.) on every run. Every such choice has a sensible default — from `.delivery.yml`, falling back to the underlying tool's global config. The user configures once; the loop runs without nagging.

## Deferred items

### B1 — Worker model & speed selection
Raised 2026-06-26. Today `scripts/dispatch-worker.ps1` passes no model flags, so the codex worker follows codex's global `~/.codex/config.toml` (currently `model = "gpt-5.5"`, `model_reasoning_effort = "xhigh"` — smartest but slowest). That default is fine for now.

Add knobs so model + reasoning effort are configurable **as defaults**, not per-run prompts:
- `dispatch-worker.ps1`: add `-Model` and `-Effort` params → pass `codex exec -m <model> -c model_reasoning_effort=<effort>`.
- `.delivery.yml`: add `worker.model` and `worker.reasoning_effort` (optional; fall back to codex's global default).
- Optional later: per-issue effort (docs → low, hard code → xhigh) — but only if the loop decides automatically, never by asking the user.

This is the first concrete knob of the provider-pluggable Worker port. Multi-provider adapters (gemini / claude / openai) remain M3.
