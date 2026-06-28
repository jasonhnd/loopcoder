# loopcoder Usage

loopcoder turns one delivery need into a small GitHub-issue batch, Codex worker
PRs, Opus review, chat progress, and user-directed merges.

Use it when you want one Claude Code chat to plan, dispatch, review, and merge a
small batch of repository work without manually relaying every step between
GitHub, git worktrees, Codex, and PR review.

## Prerequisites

- Claude Code, because loopcoder is a Claude Code skill.
- `git` on `PATH`.
- `gh` on `PATH`, authenticated for the target GitHub repository.
- `codex` on `PATH`.
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
worktrees, the Codex worker adapter, GitHub PRs/checks/merges, Opus review,
human merge gating, and chat reporting.

The current example is:

```yaml
version: 1
adapters:
  work_items: github      # Work items are GitHub issues.
  workspace: git-worktree # Work happens in git worktrees.
  worker: codex           # Codex implements each issue.
  vcs: github             # GitHub hosts PRs and checks.
  verifier: opus          # Opus reviews worker output.
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
   creates a fresh git worktree, runs headless `codex exec`, commits the
   resulting changes, pushes the branch, opens a PR, and cleans up.

5. loopcoder reviews each PR in the Opus chat session, checks the diff and
   `gh pr checks`, and reports progress, failures, risks, and final status in
   chat.

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

loopcoder state push --repo .
loopcoder state pull --repo .
loopcoder lease acquire --repo .
loopcoder lease release --repo .
```

## Limits

loopcoder v1 is intentionally small-batch and single-session. It is meant for a
handful of issues in one open Opus chat session, not large unattended roadmaps.

State lives in GitHub plus the conductor's in-chat state table and dependency
DAG. If the session ends, a later session can re-read GitHub state, but v1 does
not provide a fully stateless background conductor that automatically adopts
orphaned workers. See [`architecture.md`](architecture.md) for the current
architecture and limits.
