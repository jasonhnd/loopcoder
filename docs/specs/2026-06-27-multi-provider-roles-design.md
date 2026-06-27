# loopcoder 0.3.0 — Multi-Provider Roles (Conductor / Worker / Verifier)

Status: DRAFT (design-first; not yet built). Owner: jasonhnd. Date: 2026-06-27.
Target version: 0.3.0 (minor — new feature; not a patch).
Relates to: [`DESIGN.md`](../../DESIGN.md) (roles, "reviewer ≠ author"),
[`docs/worker.md`](../worker.md), [`SKILL.md`](../../SKILL.md), [`docs/PROCESS.md`](../PROCESS.md).

## 1. Purpose

Today loopcoder hardwires two roles to two specific LLMs:

- The **Conductor** is framed throughout `SKILL.md` as "the Opus session".
- The **Worker** is hardwired to `codex`: `internal/worker/worker.go` rejects any
  `--provider` other than `codex`, and the runner (`ExecCodexRunner`,
  `BuildCodexArgs`, the `-o` summary file) is codex-specific.

This is wrong for a tool that several agent ecosystems want to use. **Conductor and
Worker should each be any capable LLM, freely composable**, plus an **independent
Verifier** of a contrasting provider. The four canonical combinations:

1. Opus conducts → Opus-side worker
2. Codex conducts → Codex worker
3. Opus conducts → Codex worker (today's only working path)
4. Any conductor → Gemini worker

## 2. Roles and the core asymmetry

The reason the two roles were hardwired differently is that **they are different kinds
of thing**:

- **Conductor** = a human-launched *interactive agent session* (host runtime + a
  playbook). The binary does not spawn it; a human opens Claude Code / Codex CLI /
  Gemini CLI, the host reads its playbook, and the session drives the loop by calling
  the `loopcoder` binary for mechanical work.
- **Worker** = a *headless, binary-spawned CLI invocation* in an isolated git
  worktree. The binary fully controls it: build prompt → spawn → wait → capture
  summary → commit → push → open PR.
- **Verifier** = a *headless, binary-spawned CLI invocation*, **read-only**, of a
  provider that differs from the worker; reviews a PR adversarially and emits a
  structured verdict.

**Key consequence:** the binary already does not assume who the Conductor is — it only
exposes subcommands (`dispatch`, `ready-set`, `verify-local`, …) that any host can
call. "Conductor = Opus" lives entirely in **docs/config** (`SKILL.md` prose,
`.delivery.yml adapters.verifier: opus`). So:

- **"Worker = any LLM"** and **"Verifier = any LLM"** are *code* work (a provider
  abstraction inside the binary).
- **"Conductor = any LLM"** is *docs/config* work (de-Opus-ify the playbook, give each
  host an entrypoint). The binary needs no conductor-runner. (A binary that *drives*
  the conductor itself is the autonomous v2/cloud tick — explicitly a non-goal here.)

## 3. Provider abstraction — `AgentRunner`

Both Worker and Verifier are "one agent invocation". They share a single abstraction
and a provider registry; they differ only in (prompt, write vs read-only, how the
result is interpreted).

```go
// internal/agent (new package) — provider-neutral.
type Invocation struct {
    WorktreePath string   // working root for the agent process (cmd.Dir)
    Prompt       string   // task or review instructions
    Model        string   // optional; provider-specific meaning
    Effort       string   // optional; ignored by providers without the knob
    ReadOnly     bool     // true for Verifier: select each provider's read-only mode
    OutputSchema string   // optional JSON Schema path/string for structured output
    LogPath      string   // combined stdout+stderr transcript
}

type Result struct {
    ExitCode int
    Summary  string // captured final agent message (raw text or structured JSON)
}

type Runner interface {
    Run(ctx context.Context, inv Invocation) (Result, error)
}

// Registry resolves a provider name to its Runner.
func Lookup(provider string) (Runner, error) // unknown -> actionable error
```

`internal/worker` keeps owning worktree → commit → push → PR, but delegates the agent
step to `agent.Lookup(provider).Run(...)`. The existing codex-specific names
(`CodexRunner`, `CodexInvocation`, `ExecCodexRunner`, `BuildCodexArgs`) are renamed /
moved into the codex adapter under the new abstraction. The hard reject at
`worker.go` (`if opts.Provider != "codex"`) is removed in favor of the registry.

### 3a. Per-provider adapter facts (detected 2026-06-27 on this host)

| capability | codex `exec` (0.142.2) | claude `--print` (2.1.170) | gemini `--prompt` (0.45.2) |
|---|---|---|---|
| headless trigger | `codex exec` | `-p`/`--print` | `-p`/`--prompt` |
| prompt delivery | positional or stdin (`-`) | positional or stdin | `-p` arg or stdin |
| working dir | `-C`/`--cd <dir>` | process cwd + `--add-dir` | process cwd + `--include-directories` |
| write/yolo | `--dangerously-bypass-approvals-and-sandbox` | `--dangerously-skip-permissions` | `-y`/`--yolo` (or `--approval-mode yolo`) |
| **read-only** | `-s read-only` | `--allowedTools "Read Grep Glob"` | `tools.core: []` settings override + `--extensions none` |
| model | `-m` | `--model` | `-m` |
| reasoning effort | `-c model_reasoning_effort=<x>` | `--effort <low..max>` | (none — ignored) |
| final-message capture | `-o <file>` | `--output-format json` (stdout) | `--output-format json` (stdout) |
| structured output | `--output-schema <file>` | `--json-schema <schema>` | `--output-format json` |

Only two axes actually differ: the write/read-only flag name, and where the final
message comes from (codex writes a file; claude/gemini emit JSON on stdout). Everything
else is uniform. Each adapter therefore implements three things: argv construction,
prompt delivery, and summary capture.

### 3b. Auth, config, secrets

Each CLI uses its own local auth/config (`~/.codex/config.toml`, Claude Code's own
auth, Gemini's own auth). loopcoder shells out and inherits the environment; it stores
and manages **no** secrets. Model/effort defaults continue to be inherited from each
provider's own config unless the user explicitly overrides per the existing
"never chosen for the user" rule.

### 3c. Windows invocation

- `codex` resolves to `codex.cmd`, `claude` to `claude.exe`, `gemini` to `gemini.cmd`
  (npm installs `gemini`, `gemini.cmd`, `gemini.ps1` side-by-side). Go's `exec`
  resolves `.cmd`/`.exe` directly via PATHEXT — **no `pwsh -File` wrapper is needed**
  for gemini.
- Preserve the existing codex stdin handling (real file-handle stdin, no `cmd /c`).
  Where a provider accepts the prompt as an argument (`-p`/positional), prefer that to
  sidestep `.cmd` stdin quirks; fall back to file-handle stdin otherwise.

## 4. Worker path (generalized)

- `worker.Options.Provider` selects the adapter via the registry; `codex` stays the
  default. `claude` and `gemini` become first-class.
- The existing implementation prompt (`BuildPrompt`) is provider-neutral already (it
  tells the agent to edit files and NOT commit/push — the harness does that). Keep it.
- `--model` / `--effort` become provider-specific: codex and claude honor effort,
  gemini ignores it (logged once, not an error). Help text stops calling these
  "Codex" pass-throughs.
- Summary capture moves into the adapter: codex reads its `-o` file; claude/gemini
  parse the final message from `--output-format json` on stdout.

## 5. Verifier path (new)

A new command runs an **independent** review agent whose provider differs from the
worker that produced the PR.

- **Command:** `loopcoder loopreview --repo <path> --pr-number <n> --provider <V>
  [--base-branch main]`.
- **Execution model: read-only worktree checkout** of the PR branch (reuse the
  worktree machinery in `internal/worker` / `internal/gitutil`). Providers with
  read-only tools can inspect the full post-change tree, but every verifier runs
  in a **headless-safe read-only mode** (`-s read-only` / read-only tool allowlist /
  disabled tools) so it cannot mutate or push. Do not use provider plan modes
  for headless verifier runs when they wait for interactive approval. The Gemini
  fallback is prompt-only because its tool-restricted path was not headless-safe.
- **Inputs to the review prompt:** the PR diff (`gh pr diff <n>` and `--name-only`),
  the issue title/body + acceptance criteria, and the merged design doc referenced by
  the code issue (read from the base branch).
- **Output: a structured verdict** captured via each provider's structured-output
  mechanism against a shared schema:

  ```json
  {
    "verdict": "pass | fail | needs-human",
    "findings": [{ "severity": "...", "file": "...", "note": "..." }],
    "evidence": "spec-conformance, changed-files, and check reasoning",
    "spec_conformance": "pass | fail | not-applicable"
  }
  ```

- **Relationship to existing gates:** `loopcoder verify-local` (deterministic command
  gates: tests/typecheck/build) is unchanged. The conductor folds three signals into
  one merge-eligibility decision: hosted CI checks (`gh pr checks`), `verify-local`,
  and the independent Verifier verdict. A parse failure or unreadable spec yields
  `needs-human`, never a silent pass.

## 6. Configuration — role slots

`.delivery.yml adapters` gains an explicit, freely-composable set of role slots:

```yaml
adapters:
  conductor: opus      # transparency only: who is expected to conduct (a human session)
  worker:    codex     # default worker provider; overridable per dispatch via --provider
  verifier:  claude    # default verifier provider; SHOULD differ from worker
```

- **reviewer ≠ worker:** the verifier provider defaults to a value different from the
  worker provider. If a run is configured or invoked with `verifier == worker`, the
  binary and the playbook **warn** but do not block (the human-merge gate is the final
  backstop). This honors `DESIGN.md`'s "reviewer ≠ author" intent without removing
  flexibility for same-family setups (combos 1 and 2).
- **Per-issue routing is a non-goal for 0.3.0** (see §12). The existing per-dispatch
  `--provider` override already lets the conductor send different issues to different
  workers within one run.

## 7. Conductor playbook portability (docs)

- **De-Opus-ify `SKILL.md`:** replace "the Opus session" / "Opus chat session" with
  "the conductor session" / "a sufficiently capable agent session". Replace the
  hardwired `worker: codex` transparency line with the configured `adapters.worker`.
- **Per-host entrypoints** carrying the same host-neutral procedure:
  - `SKILL.md` — Claude Code (existing).
  - `AGENTS.md` — Codex CLI.
  - `GEMINI.md` — Gemini CLI.
  Keep the procedure in one canonical place and have each entrypoint reference it, so
  the playbook does not fork.
- The conductor still owns planning, doc-first ordering, dispatch, folding the
  Verifier verdict + CI + local gates, and the human-merge gate. It no longer performs
  the *primary* adversarial review itself — it delegates that to the independent
  Verifier and folds the verdict in (revise `SKILL.md` step 5 accordingly).

## 8. Error handling

- Unknown provider → registry miss returns an actionable error listing supported
  providers.
- Provider CLI not installed / not on PATH → clear "install/auth <provider>" error;
  do not fall back silently to another provider.
- gemini effort knob absent → ignore `--effort` for gemini with a single advisory log,
  not a failure.
- `verifier == worker` → advisory warning, proceed.
- Structured-verdict parse failure or unreadable referenced spec → `needs-human`.
- Verifier worktree is always read-only; a verifier that somehow mutates the tree is a
  defect, not a deliverable.

## 9. Testing

- **Unit (table-driven):** registry resolution; each adapter's argv builder for
  worker (write) and verifier (read-only) modes; summary/verdict parsing for codex
  (file), claude (json), gemini (json).
- **Integration (this host has all three CLIs):** a real `dispatch` per provider
  (codex/claude/gemini) producing a branch + PR; a real independent `loopreview` per
  provider producing a structured verdict; confirm read-only mode makes no commits.
- **Reviewer ≠ worker:** a test that same-provider verifier+worker warns but proceeds.
- **CI gate unchanged:** every PR still needs `verify` + `go` green
  (`.delivery.yml ci.checks: [verify, go]`).

## 10. Build slices (issue DAG — doc-first; NOT to be filed until approved)

Per [`docs/PROCESS.md`](../PROCESS.md): this design doc merges first; each code issue
below is separate and references the merged doc. Suggested DAG:

- **D1 (doc):** merge this design doc. *(blocks all code issues)*
- **C1:** `internal/agent` abstraction + registry; refactor codex into an adapter
  behind it; remove the hard reject. No behavior change; codex path stays green.
- **C2:** claude worker adapter + integration proof (Opus→claude, codex→claude). *(blocked-by C1)*
- **C3:** gemini worker adapter (Windows `.cmd` exec, effort-ignored) + proof. *(blocked-by C1)*
- **C4:** independent Verifier command `loopreview` (read-only PR-branch worktree +
  structured verdict) + proof per provider. *(blocked-by C1)*
- **C5:** `.delivery.yml` role slots (conductor/worker/verifier) + reviewer≠worker
  warning. *(blocked-by C1)*
- **C6 (doc):** de-Opus-ify `SKILL.md` + add `AGENTS.md` / `GEMINI.md` entrypoints +
  revise step 5 to delegate to the Verifier. *(blocked-by C4, C5)*
- **C7 (doc):** README / CHANGELOG / `docs/worker.md` sweep for multi-provider.

## 11. Non-goals (0.3.0)

- No binary-driven / autonomous conductor (no background tick). The Conductor stays a
  human-launched session; only the playbook becomes host-portable. (That is v2/cloud.)
- No per-issue label-based worker routing (per-dispatch `--provider` already suffices).
- No providers beyond codex / claude / gemini.
- No secret or auth management inside loopcoder.

## 12. Open questions / risks

- Stability of claude/gemini `--output-format json` shapes across versions; pin the
  fields the parser depends on and degrade to `needs-human` on mismatch.
- Verifier read-only worktree cost vs value; acceptable given existing worktree
  machinery and read-only sandbox.
- Effort-knob asymmetry (gemini lacks it) — documented, ignored, not mapped.
