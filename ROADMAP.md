# ROADMAP

<!--
Format for loopcoder work units:
- Each ## heading is one topic or unit.
- Each "- doc:" or "- code:" list item is one slice and becomes one issue.
- code slices depend on the doc slices in the same unit unless "(needs: ...)" is set.
- Slice refs are <unit-slug>/<kind>-<n>; within the same unit, <kind>-<n> works.
- Use "## [epic] ..." for a slice DAG; add "- doc:" / "- code:" lines for explicit slices.
-->

## 0.5.0 — Generalize loopcoder beyond code (domain profiles)

Turn loopcoder from a code-delivery loop into a general autonomous-delivery engine for
ANY verifiable, repo-based, AI-doable work (documents, content, data…), WITHOUT changing
the core engine (tick / dispatch / loopreview / risk-gate / promote / guardrails /
watchdog / relay). Code becomes the first of several domains.

Mechanism: a **domain profile** declared in `.delivery.yml` (a new, purely-additive
`domain` section) supplies the plug points the unchanged core consumes:

- **Skill sources configurable** — today skill discovery is hardcoded to
  `.claude/skills/*/SKILL.md` (`internal/skills/skills.go`) and finds nothing elsewhere
  (e.g. a domain skill at `.../governance/skill.md`); add configurable repo paths + an
  optional machine skill library + selection.
- **Verification rubric injectable** — loopreview's verdict criteria are hardcoded
  code-specific (`internal/loopreview`); inject a project-supplied checklist so a
  non-code artifact is judged by its domain's rubric. Keep the verdict enum and the
  0.4.2 H5 exit-code contract.
- **Evidence producer, fed to the verifier** — today evidence is configured paths
  (`config.Evidence`) shown only in the report; add a producer (a command that renders
  the artifact) and feed the produced artifact into loopreview's packet. Generalize the
  `browser` special-case toward a generic rendered-artifact.
- **Domain red-lines (append-only)** — risk-gate red lines are hardcoded
  (`internal/orchestration/risk_gate.go`) with an unused `AdditionalRedLines` param;
  wire a `.delivery.yml` domain red-lines list that may only tighten, never lower the
  floor, and never touch the self-hosting-guard core red line.
- **MCP (new)** — add `.delivery.yml mcp.servers[]`, a pure-append optional MCP field on
  `agent.Invocation`, and per-provider plumbing (claude/codex/gemini support MCP
  natively); support BOTH local stdio and external/remote HTTP servers. External MCP
  needs auth from env/secrets (never hardcoded) and read-only classification for the
  verifier (never trust a server's self-reported read-only hint — the local sandbox does
  not neutralize a remote server's side-effects). ReadOnly (spec 0161 E1) stays the one
  permission boundary.
- **Fold in 0.4.2 pluggability** — make configurable per domain: partial-work acceptance
  (H1 harvest opens a needs-human PR with salvaged work), liveness mode (H2 worktree
  file-mtime is code-shaped; API/remote-effect domains need log-only or custom), and
  review-packet section ordering (H3 source-first is code-specific).

Constraints: doc-first; self-hosting guard (spec 0161 M2 — this machinery is
loopcoder-core, so building it routes needs-human + rebuild and the 0.5.0 build is
semi-attended); additive & byte-stable config (M3); ReadOnly boundary (E1) unchanged.
Out of scope: E2 auto-promote (shipped 0.4.1); multi-project scheduler (dropped —
per-instance robustness already covers the real need). Validation target: a `docs`
domain profile proven on the private repo SB_Glome (corporate IR document production) —
governance = spec, `qa-checklist.md` = rubric, `check_tokens.py`/`verify_deck.py` = CI,
rendered PDF = evidence (via producer), securities-law/TDnet = red lines, human approval
= promote. Its skill lives at `.../governance/skill.md` (NOT `.claude/skills/`), which is
exactly why skill sources must become configurable.

- doc: Write the 0.5.0 domain-profile spec — define the `domain` profile bundle and the
  five plug points (skill sources, rubric injection, evidence producer, domain red-lines,
  MCP local+external) as purely-additive `.delivery.yml` sections plus the
  `agent.Invocation` MCP append, grounded in the current code anchors; fold in the 0.4.2
  pluggability points; enumerate the follow-up code slices; preserve the self-hosting
  guard, M1/M3, the ReadOnly boundary, and the 0.4.2 H5 exit-code contract.
