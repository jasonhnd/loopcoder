---
id: 403
title: E2 — Auto-Promote to Production (default-on)
status: draft
date: 2026-07-03
issue: 403
pr: null
supersedes: [161]
superseded_by: []
---

# E2 — Auto-Promote to Production (default-on)

This is a design-only spec for loopcoder 0.4.1. This PR must add only this
document: no Go code, no `.delivery.yml` change, no command behavior change, and
no new runtime dependency. Code slices are filed only AFTER this spec merges, per
[`docs/PROCESS.md`](../PROCESS.md).

0.4.1 realizes the **E2 — Auto-promote to production** seam that spec
[`0161`](0161-autonomous-delivery-loop.md) reserved for a later release. It is a
deliberate, pre-approved change to loopcoder's safety keystone: it inverts the
DEFAULT of production promotion from human-gated to automatic, and it does so ONLY
by adding the safety machinery (deterministic gate, production auto-rollback,
evidence-based decision) that makes an irreversible auto-advance defensible. Every
failsafe beneath the gate is preserved verbatim.

## Supersession scope (partial amendment of 0161)

This spec is a **scoped amendment**, not a wholesale replacement. It changes
exactly one clause of spec 0161's "Seams reserved for 0.5.0" chapter:

- **Amended — F3 authorization clause.** 0161 F3 reads "promotion to production is
  a distinct step callable only by a human; tick MUST NOT invoke it." 0.4.1
  supersedes ONLY the "callable only by a human" half. The new rule: promotion is a
  distinct step callable by the **auto gate OR a human**. F3's structural half —
  promotion is a *distinct step* and *tick MUST NOT invoke it* — is preserved
  unchanged.
- **Preserved — F1 in full.** 0161 F1 ("tick has NO capability to merge into
  main/production") is **kept verbatim**. 0.4.1 does NOT teach tick to merge main
  and does NOT give tick a `ProductionWriter`. Auto-promotion runs as the existing
  separate `promote` step, invoked by an automation trigger — never by tick. (This
  is a deliberate strengthening over a naive "supersede F1/F3": full autonomy is
  achieved while the strongest structural failsafe survives.)
- **Preserved — everything else in 0161.** Tick, compile, ready-set, guard,
  dispatch, risk gate, pre-production model, F2, F4, F5, all four M-invariants
  (M1–M4, minus F3's amended clause), and the E1 / multi-project-scheduler seams
  remain accepted and in force. The E2 seam contracts (gate is replaceable policy;
  red lines are a floor beneath the gate; promote is the only path into production;
  promote is idempotent and ledgered; auto-merge targets base by parameter) are not
  weakened — 0.4.1 **activates** them.

Lifecycle note (for the merging conductor): because this is a partial amendment,
0161 is NOT marked `status: superseded`; it stays `accepted`. The frontmatter
`supersedes: [161]` records the relationship for discoverability; the precise scope
is this section. Whether to add a back-pointer to 0161's frontmatter is a lifecycle
decision deferred to merge time (0161's body stays immutable regardless).

## Goal

Make promotion to production automatic **by default**, with no human on the safe
path, while keeping every deterministic failsafe intact. Today promotion is
exclusively a human action (`promote.go:259` enforces `gate == "human-merge"`).
0.4.1 inverts this default so a verified pre-production batch advances to
production on its own — gated by a deterministic floor, backed by an automatic
production rollback, and decided from evidence + CI + the verifier verdict rather
than a human review.

The human is not removed from the system; the human is moved to **intent** (writing
`ROADMAP.md`), to the **opt-out** (`gate: human-merge`), and to the **audited
escape hatch** (a human can still promote or roll back manually). This mirrors the
convergent industry pattern (Argo Rollouts, Flagger, GitHub Environments,
Spinnaker/Kayenta): automate the common path, keep the human bypass.

## Design basis (four-system convergence)

The gate design is grounded in the 0.5.0 advance-prep research. Every mature
automated-promotion system studied uses the same two-layer shape:

- A **deterministic floor** that must all-pass (conjunctive), evaluated
  independently of any policy.
- A **policy layer that can only VETO, never force** a promotion the floor blocked.

Argo Rollouts (analysis → continue/abort, failed post-promotion analysis switches
traffic back to the **retained** stable revision), Flagger (veto-only 2xx/non-2xx
gates that compose), GitHub Environments (conjunctive protection rules — "the job
won't start until all rules pass"), and Kayenta (deterministic aggregate score
evaluated before/independent of manual judgment; automatic failures force score 0)
all converge on this. 0.4.1 adopts it directly.

## The change (normative)

### C1 — Gate gains an `auto` value and it becomes the default

- `adapters.gate` (`config.go:33`) accepts a new value `auto` in addition to
  `human-merge`. The DEFAULT flips from `human-merge` to `auto`.
- A **closed gate enum** is introduced. Today there is no enum — only a hardcoded
  `opts.Gate != "human-merge"` check (`promote.go:259`). 0.4.1 validates gate
  against the closed set `{human-merge, auto}`; an unknown value is a hard error
  (**fail-closed**, never silently treated as a permissive default).
- `human-merge` is preserved verbatim as the explicit **opt-out**: with it set,
  promotion requires explicit human invocation exactly as in 0.4.0.

### C2 — Deterministic auto-promote gate (floor + veto-only policy)

Under `gate: auto`, an item/batch is auto-promoted to production ONLY when ALL of
the following are simultaneously true (conjunctive):

1. **CI green** — every required check passes (`checkPassed`, `risk_gate.go:260`;
   `PRChecks`, `github.go:316`).
2. **Verifier pass** — the loopreview verdict is `pass` (`Verdict.Verdict`,
   `loopreview.go:58`; constants `loopreview.go:26`). `fail` or `needs-human`
   never auto-promotes.
3. **Evidence present** — the project's configured evidence precondition is
   satisfied (see C5).
4. **No red line** — the deterministic risk-gate floor (`EvaluateRiskGate`,
   `risk_gate.go:72`) reports clean: no destructive/data-loss op, no build break,
   no loopcoder-core change.

Rules binding the gate:

- **The policy may only VETO.** The `auto` policy MUST NOT authorize a promotion
  the deterministic floor blocked. It may only ADD a veto on items the floor
  permits. An LLM or policy may RAISE risk, never lower the floor (re-affirms 0161
  E2 seam "red lines are a floor beneath any gate").
- **Red lines are evaluated BEFORE and INDEPENDENT of the gate.** Red-line-blocked
  items route to the existing `needs-human` sink (`scaffold.go:90`) before the gate
  is consulted.
- **Fail-closed.** If any input is missing, unknown, or errored — CI status
  unresolved, verdict absent, evidence unresolved — the item is NOT auto-promoted;
  it routes to `needs-human`. Absence is never treated as permission.

### C3 — Production auto-rollback (retained prior-stable + deterministic revert)

An irreversible target (production/`main`) MUST NOT be auto-promoted without a
recorded, deterministic way back.

- **Record the revert target (E2-G1).** The promote ledger event MUST record two
  new optional `omitempty` snake_case fields on `state.Event` (`state.go:69`):
  `merge_commit` (the SHA the promotion produced) and `prior_stable_commit` (the
  production SHA it advanced from). Today `state.Event` has NO SHA fields; the
  promote SHAs live only inside the `Details` JSON (`promote.go:412`). These become
  first-class, byte-additive, M3-safe fields.
- **Post-promote check.** After an auto-promote, a deterministic post-promote check
  runs against production (mirroring the pre-production keeps-green check,
  `runTickPreProdKeepsGreen`, `tick.go:1027`).
- **Deterministic revert on failure.** If the post-promote check fails, production
  is reverted to `prior_stable_commit` by a deterministic revert-merge. The rollback
  is **never LLM-driven**. It is a NEW, separately-gated production path — per the
  0161 E2 seam, auto-promote "MUST be a NEW, separately-gated method or command —
  not a relaxation of the target parameter of any existing automated merge path."
  `github.Writer` (the tick/worker surface) MUST NOT gain a merge-to-production or
  revert-production method; the new path lives on the human-only
  `ProductionWriter`-class surface (`github.go:42`).
- **Rollback is ledgered.** The rollback records its own append-only event carrying
  the same `merge_commit` / `prior_stable_commit` pair.
- **Scope.** Auto-rollback fires only on auto-promoted merges. Under
  `gate: human-merge`, rollback stays a human action.

### C4 — Who triggers auto-promote (F1 preserved)

This clause is the crux of preserving F1 while inverting the default.

- Promotion remains the existing `promote` step — the ONLY code path into
  production (0161 E2 seam "promote is the only path into production"). It is NOT
  moved into `tick`.
- Under `gate: auto`, the `promote` step no longer requires human confirmation: an
  **automation trigger** (the 0.4.0 automation trigger — cron / goal-loop / hook,
  0161 slice 9) may invoke `promote` as a **separate step**, the same way it invokes
  `tick`. `tick` and `promote` stay separate processes/invocations.
- **`tick` still has NO capability to merge main.** It is not injected with a
  `ProductionWriter` (`cli.go:1070` unchanged); `github.Writer` gains no
  merge-to-main method (F1, `github.go:23`). The trigger — not tick — invokes the
  separate promote step.
- Therefore F3's structural half is intact (promote is a distinct step; tick does
  not invoke it); only F3's authorization half is inverted (auto-or-human instead of
  human-only).

### C5 — Evidence-for-auto-decision (honest scope)

The `auto` gate consumes evidence as part of its go/no-go basis. 0.4.1 uses the
evidence that already exists and does NOT invent a new evidence system.

- Today `Evidence` (`config.go:108`) is **static** per-project configuration in
  `.delivery.yml` (`preview_url`, `example_output`, `test_results`,
  `preview_build`), surfaced in tick reports (`ConfiguredEvidence`), not a computed
  per-PR runtime artifact.
- "Evidence present" in 0.4.1 therefore means: **the project's configured evidence
  precondition for its project type resolves** (the required configured artifacts
  are present / non-empty). It is a configured go/no-go precondition, not a promise
  of dynamic evidence production.
- Fail-closed: if a project declares evidence as required and none resolves, the
  item does NOT auto-promote (→ `needs-human`).
- A dynamic per-PR evidence-production pipeline is explicitly **out of scope** for
  0.4.1 (it is larger than E2, and belongs to a future release if wanted).

### C6 — Authorizing-gate identity (E2-G2, optional companion)

The promote (and rollback) ledger event SHOULD carry an optional `omitempty`
snake_case `authorized_by` field, set to the `cfg.Adapters.Gate` value in force at
invocation (`human-merge` or `auto`). This gives a uniform audit trail across the
human/auto boundary. Reserving a field, not building policy.

## Preserved failsafes (binding table)

0.4.1 MUST NOT weaken any of these. Each is loopcoder core; any change to the code
enforcing it is itself a red-line item (self-hosting guard).

| Failsafe | Status in 0.4.1 | Anchor |
|---|---|---|
| **F1** — tick has no merge-to-main capability | **Preserved verbatim** | `github.go:23` (Writer has no merge-to-main); tick injected `PreProdWriter` only (`cli.go:1070`) |
| **F2** — verifier hardcoded `ReadOnly: true` + `PermissionReadOnly` | Preserved | `loopreview.go:214`, `loopreview.go:261` |
| **F3** — promotion is a distinct step; tick never invokes it | **Structural half preserved**; human-only half amended to auto-or-human | `promote.go:198` (separate command), `promote.go:259` (gate check) |
| **F4** — guardrail budget/circuit-breaker gates every wave | Preserved | `guardrails/budget.go:152,272`; `dispatch_wave.go:156,184` |
| **F5** — attestation closed permission enum `{read-only, write, orchestrate}` | Preserved | `attestation.go:273` |
| **Red-line floor** — destructive/build/core → needs-human | Preserved; is a floor BENEATH the gate | `EvaluateRiskGate`, `risk_gate.go:72`; categories `risk_gate.go:23` |
| **Self-hosting guard** — loopcoder-core changes → needs-human, effective only after rebuild | **Preserved and survives full autonomy** | `isLoopcoderCorePath`, `risk_gate.go:311` |

**Critical self-hosting invariant (G5 below):** even under `gate: auto`, a change to
loopcoder's own core (tick / worker / loopreview / risk-gate / promote / config /
attestation / guardrails …) routes `needs-human` and takes effect only after a human
rebuild. loopcoder MUST NOT auto-ship a change to its own safety core. The auto gate
sits ABOVE the red-line floor and can only tighten it.

## New invariants introduced by 0.4.1

- **G1** — Production auto-promote MUST NOT proceed without a recorded revert target
  (`merge_commit` + `prior_stable_commit`). No auto-promote without rollback
  capability.
- **G2** — The `auto` gate is conjunctive over {CI green, verdict pass, evidence
  present, no red line} and is fail-closed on any missing/unknown/errored input.
- **G3** — The `auto` gate may only VETO; it may never lower the deterministic
  red-line floor.
- **G4** — F1 preserved: tick has no merge-to-main. Auto-promote runs as the
  separate `promote` step invoked by an automation trigger, never by tick.
- **G5** — loopcoder-core changes route `needs-human` even under `gate: auto` (the
  self-hosting red line survives full autonomy); such changes take effect only after
  a human rebuild.
- **G6** — Production auto-rollback is deterministic (revert-merge to
  `prior_stable_commit`), never LLM-driven, and is a NEW separately-gated path — not
  a relaxation of any existing merge.
- **G7** — 0.4.1 itself ships **human-gated**: a human reviews and promotes 0.4.1 to
  production. Auto-promote is available for USE from that point on. (loopcoder must
  not auto-ship the change that makes it auto-ship.)

## Follow-up code slices (filed after this spec merges, in dependency order)

Each slice brief MUST carry the invariant→slice bindings below as hard constraints,
not just the issue's acceptance criteria.

1. **Slice A — promote ledger records revert SHAs (E2-G1).** Add optional
   `omitempty` snake_case `merge_commit` + `prior_stable_commit` to `state.Event`;
   populate in the promote event and in the pre-production auto-revert event (shared
   shape). Byte-additive, no rollback logic. Binds G1, and 0161 M3.
2. **Slice B — auto-promote gate policy + deterministic floor.** Add the `auto` gate
   value + closed gate enum + conjunctive fail-closed evaluation + veto-only policy
   above the red-line floor. Binds G2, G3, and 0161 E2 "gate is replaceable policy"
   / "red lines are a floor".
3. **Slice C — production auto-rollback.** Post-promote check + deterministic
   revert-merge to `prior_stable_commit` as a NEW separately-gated production path +
   rollback ledger event. Binds G1, G6, and 0161 E2 "promote is the only path" /
   "auto-merge targets base by parameter".
4. **Slice D — evidence-driven auto-decision.** Wire configured evidence + CI +
   verdict as the gate's go/no-go basis; fail-closed on unresolved required
   evidence. Binds G2, C5.
5. **Slice E — flip gate default to `auto` + docs + CHANGELOG.** Default gate
   `human-merge` → `auto` in `normalizePromotionGate` (`promote.go:835`),
   `.delivery.yml`, `scaffold.go:197`; update README / CHANGELOG. Preserve
   `human-merge` as opt-out. Binds C1, G7.

Dependency rule: every slice is blocked-by this merged spec. Slice C is blocked-by
Slice A (needs the SHA fields). Slice E (default flip) lands last, after B/C/D make
`auto` safe. The IRON RULE stands: `auto` promotion MUST never run without the
deterministic gate (B), the recorded revert target (A), and the rollback path (C)
all in place.

## Scope / non-goals

- 0.4.1 is ONLY E2 auto-promote to production (default-on) + its gate + rollback +
  evidence consumption.
- NOT E1 MCP connectors. NOT the multi-project scheduler. NOT a dynamic per-PR
  evidence-production pipeline. Those remain future / 0.5.0+.
- No change to the tick/worker VCS surface (F1). No widening of `github.Writer`.

## Relationship to existing specs

- [`0161-autonomous-delivery-loop.md`](0161-autonomous-delivery-loop.md) — parent;
  0.4.1 amends F3's human-only clause and activates the E2 seam. All other 0161
  invariants preserved.
- [`0192-delivery-guardrails.md`](0192-delivery-guardrails.md) — F4 budget /
  circuit-breaker, preserved.
- [`0146-attestation.md`](0146-attestation.md) / [`0306-local-only-attestation.md`](0306-local-only-attestation.md)
  — F5 attestation, preserved.
- [`0194-reliable-loopreview-verifier.md`](0194-reliable-loopreview-verifier.md) —
  F2 read-only verifier; the verdict the auto gate consumes.
- [`0390-process-watchdog.md`](0390-process-watchdog.md) — process supervision;
  unaffected.
