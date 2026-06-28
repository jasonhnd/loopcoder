# loopcoder Usage

loopcoder turns one delivery need into a small GitHub-issue batch,
provider-pluggable worker PRs, independent `loopreview` verification, chat
progress, and user-directed merges.

Use it when you want one conductor chat to plan, dispatch, review, and merge a
small batch of repository work without manually relaying every step between
GitHub, git worktrees, worker providers, and PR review.

## Prerequisites

- A conductor host session that follows `SKILL.md`, `AGENTS.md`, or `GEMINI.md`.
- `git` on `PATH`.
- `gh` on `PATH`, authenticated for the target GitHub repository.
- At least one supported provider CLI on `PATH`. `codex` is the default worker,
  `codex` and `claude` are verified worker and verifier providers, and
  `gemini` is experimental and unverified end-to-end.
- Go, for installing the native `loopcoder` binary with `go install`.
- A GitHub repository with a configured remote.

## Install

Install the native helper binary:

```text
go install github.com/jasonhnd/loopcoder/cmd/loopcoder@latest
```

From a source checkout, you can also build it locally:

```text
go build ./cmd/loopcoder
```

Make sure the installed or built binary is on `PATH`, or set `LOOPCODER_BIN` to
its full path. The conductor uses this binary as its only mechanical backend.

loopcoder is also installed as a Claude Code skill. Invoke it from Claude Code
with:

```text
/loopcoder <need>
```

You can also just state a delivery need in chat; the skill is meant to activate
when the request should be planned, split into GitHub issues, dispatched to
workers, reviewed, and reported in chat.

For a global install, copy or symlink this repository to:

```text
~/.claude/skills/loopcoder/
```

On Windows, a directory junction is also fine. Install automation is on the
roadmap.

## Backend Selection

The conductor resolves the `loopcoder` binary before running mechanical work:

1. `LOOPCODER_BIN` when set.
2. `loopcoder` found on `PATH`.
3. Otherwise, `loopcoder` is required on all platforms.

Use the resolved binary for dispatch, ready-set scheduling, resume, recovery,
local verification, state, and lease operations.

## Per-Repo Setup

Optionally add a `.delivery.yml` file at the repository root. If it is absent,
loopcoder uses the v1 defaults from the current design: GitHub issues, git
worktrees, the Codex worker adapter, GitHub PRs/checks/merges, independent
`loopreview` verification, human merge gating, and chat reporting.

The current example is:

```yaml
version: 1
adapters:
  work_items: github      # Work items are GitHub issues.
  workspace: git-worktree # Work happens in git worktrees.
  conductor: codex-cli    # Transparency only: the human session that conducts.
  worker: codex           # Default worker provider; claude is also verified.
  vcs: github             # GitHub hosts PRs and checks.
  verifier: claude        # Verified loopreview provider; should differ from worker.
  gate: human-merge       # Humans choose what merges.
worker:
  # Optional. Absent = inherit your codex global config (~/.codex/config.toml). loopcoder never sets these on its own; they appear only when you state a permanent preference.
  # model:
  # Optional. Absent = inherit your codex global config (~/.codex/config.toml). loopcoder never sets these on its own; they appear only when you state a permanent preference.
  # reasoning_effort:
  base_branch: main
  command_hint: "implement the issue, run relevant checks, commit"
ci:
  checks: []
report:
  channel: chat
```

## End-To-End Use

1. State a delivery need, for example:

   ```text
   /loopcoder add usage docs for the project
   ```

2. loopcoder inspects the repo context and drafts GitHub issues plus a
   dependency DAG. It shows the proposed issues, blocked-by relationships, and
   worker setting in chat.

3. You approve the plan before anything is published.

4. loopcoder creates the approved GitHub issues and dispatches ready issues to
   workers through `loopcoder dispatch` / `loopcoder dispatch-wave`. The binary
   creates a fresh git worktree, runs the selected provider, commits the
   resulting changes, pushes the branch, opens a PR, and cleans up.

5. loopcoder runs `loopcoder loopreview` for each PR, checks the diff and
   `gh pr checks`, and reports progress, failures, risks, and final status in
   chat. `codex` and `claude` have real verifier smoke proof; ambiguous,
   malformed, timed-out, or incomplete verifier output is still reported as
   `needs-human`.

6. You name which PRs to merge. loopcoder merges only those named PRs by running
   `gh pr merge`, following `.delivery.yml` merge settings when present.

## Model And Speed

By default, loopcoder passes no model or reasoning-effort flags to Codex. It
inherits your global Codex configuration from `~/.codex/config.toml` and never
chooses a model or effort level for you.

For a single run, say what you want in chat, such as:

```text
run faster
#B use max
```

loopcoder then passes the requested one-off override to the worker for that run
only. Natural-language effort mapping is defined in `SKILL.md`: `fast` or
`quick` maps to `low`, `balanced` maps to `medium`, `high` maps to `high`, and
`thorough`, `max`, or `highest` maps to `xhigh`.

For a permanent default, say so explicitly, for example:

```text
from now on default to high
```

Only then should loopcoder write `worker.reasoning_effort` or `worker.model`
into `.delivery.yml`.

## Doc-First Process

New work is documented first, coded second, and verified last. The mandatory
workflow is described in [`PROCESS.md`](../PROCESS.md):

1. Write and merge the design or spec under `docs/`.
2. Open separate code issues only after the relevant document is merged.
3. Verify the implementation against the merged document and working behavior.

Documentation and code are intentionally not bundled in the same issue or PR.

## Binary Commands

Use the native `loopcoder` commands as the helper interface:

```text
loopcoder ready-set --repo . --base-branch main --format text

loopcoder dispatch \
  --repo . \
  --issue-number <number> \
  --issue-title "<title>" \
  --issue-body "<body>" \
  --base-branch main \
  --provider codex

loopcoder dispatch-wave --repo . --base-branch main --issue-numbers <n1>,<n2>

loopcoder resume --repo . --run-id <run-id>

loopcoder recover \
  --repo . \
  --issue-number <number> \
  --issue-title "<title>" \
  --issue-body "<body>" \
  --run-id <run-id>

loopcoder verify-local --repo . --pr-number <pr>

loopcoder loopreview --repo . --pr-number <pr> --provider claude

loopcoder attest \
  --role conductor \
  --provider <host-provider> \
  --model <host-model> \
  --permission orchestrate \
  --action "<delivery action>" \
  --duration-ms <milliseconds> \
  --total-tokens <tokens>

loopcoder state push --repo .
loopcoder state pull --repo .
loopcoder lease acquire --repo .
loopcoder lease release --repo .
```

## Verifier Provider Status

`loopcoder loopreview` has 0.3.x smoke proof for the `claude` and `codex`
verifier mechanism: both providers reliably returned a valid structured
verdict plus attestation within the timeout. This proof does not make the
LLM verdict itself deterministic; `pass` and `fail` remain model judgments that
can vary across otherwise valid runs.

One representative point-in-time run used merged PR #202 (`0.3.x: loopreview
bounded review packet`) with the command:

```text
loopcoder loopreview --repo . --pr-number 202 --provider <provider> --timeout 180s
```

The review packet was not truncated: 2 changed files, 73 changed-file bytes
of 8,192, 29,730 diff bytes of 81,920, max per-file patch 18,925 bytes of
24,576, 541 issue-body bytes of 12,288, and 13,769 spec bytes of 40,960.

| Provider | Wall elapsed | Parsed model / effort | Token usage | Verdict in this run | Inputs truncated |
| --- | ---: | --- | --- | --- | --- |
| `codex` | 77.398s | `gpt-5.5` / `xhigh` | `18,266` total | `pass` | no |
| `claude` | 70.686s | `claude-haiku-4-5-20251001` / unset | `2,447` input / `4,947` output | `pass` | no |

The `codex` verifier run emits the expected reviewer-not-worker advisory when
the repository worker default is also `codex`; that warning does not block the
run, but `claude` remains the preferred verifier for default Codex-worker
repos. `gemini` remains unverified for `loopreview` until issue #188 resolves
headless authentication.

## Attestation

Worker and verifier invocations carry binary-stamped attestation records with
`verified: true`, `model_source: parsed`, provider, real parsed model, effort,
permission, action, exit code, timing, and token usage. Missing required
identity or usage fails closed: `dispatch` opens no PR, and `loopreview`
returns `needs-human` with the incomplete-attestation finding.

`loopcoder attest` is for Conductor self-attestation. It emits canonical JSON
followed by the one-line `[attestation] ...` header, forces `model_source` to
`self-reported`, and forces `verified` to `false` even if flags try to set other
values. It exits non-zero when required fields are missing or invalid,
including provider, model, action, timing, and usage. Provide either
`--total-tokens` or both `--input-tokens` and `--output-tokens`.

Design rationale: [`../specs/0146-attestation.md`](../specs/0146-attestation.md).

## Limits

loopcoder v1 is intentionally small-batch and single-session. It is meant for a
handful of issues in one open conductor session, not large unattended roadmaps.

State lives in GitHub plus the conductor's in-chat state table and dependency
DAG. If the session ends, a later session can re-read GitHub state, but v1 does
not provide a fully stateless background conductor that automatically adopts
orphaned workers. See [`architecture.md`](architecture.md) for the current
architecture and limits.
