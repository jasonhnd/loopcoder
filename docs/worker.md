# Worker Adapter

This document describes the worker adapter as built in `loopcoder dispatch`,
the native `loopcoder` binary subcommand. It covers the v1 `codex` adapter
only.

## Purpose

The worker adapter turns one approved GitHub issue into a pull request. The
subcommand owns the mechanical delivery steps: repository resolution, worktree
creation, Codex invocation, change detection, commit, push, PR creation,
attempt state, recovery briefs, and cleanup.

`codex` only edits files in the fresh worktree. It does not commit, push, or
open the PR.

## Flow

For one issue, `loopcoder dispatch` runs these steps in order:

1. Resolve `--repo` to a local path and resolve the GitHub `owner/name` slug
   with `gh repo view`.
2. Create a scratch directory and a fresh git worktree from
   `origin/<base-branch>`.
3. Write a self-contained prompt file containing the issue number, title, body,
   branch, current working directory, and rules.
4. Run headless `codex exec` in the worktree with stdin redirected from the
   prompt file.
5. Verify that file changes exist with `git status --porcelain`.
6. Commit all changes with the issue title and `closes #<IssueNumber>` in the
   commit message.
7. Push the branch to `origin`.
8. Open a pull request with `gh pr create`.
9. Remove the worktree, delete the local branch, and remove the scratch
   directory unless `--keep-worktree` is set.

If Codex exits non-zero, makes no file changes, commit fails, push fails, or PR
creation fails, the subcommand returns a failed result instead of producing a
successful result.

## Codex Invocation

The adapter runs:

```text
codex exec --cd <worktree> --dangerously-bypass-approvals-and-sandbox --skip-git-repo-check ... -o <summaryFile> -
```

The prompt must be fed from a file with stdin closed:

```text
codex exec ... - < promptfile
```

This is required because headless `codex exec` hangs forever waiting on stdin if
it is not given a closed stdin stream. The adapter writes the prompt to
`prompt.txt`, then runs the command through `cmd /c` with stdin redirected from
that file and stdout/stderr redirected to `codex.log`.

The adapter passes `--dangerously-bypass-approvals-and-sandbox` so Codex can run
unattended without approval prompts. The fresh worktree is the intended blast
radius. The current invocation also passes `--skip-git-repo-check`.

## Why VCS Stays In The Adapter

The adapter, not Codex, commits, pushes, and opens the PR. This keeps VCS state
deterministic and in the conductor's hands:

- Codex edits files only.
- The adapter checks whether changes exist before committing.
- The adapter controls the commit message, branch push, PR title, PR body, and
  cleanup.

## Parameters

| Parameter | Required | Default | Description |
| --- | --- | --- | --- |
| `--repo` | Yes | none | Local repository path. The subcommand resolves it and runs `gh repo view` there. |
| `--issue-number` | Yes | none | GitHub issue number used in the prompt, branch default, commit message, PR body, and JSON output. |
| `--issue-title` | Yes | none | Issue title used in the prompt, commit message, and PR title. |
| `--issue-body` | No | empty string | Issue body included in the self-contained Codex prompt. |
| `--base-branch` | No | `main` | Base branch fetched from `origin` and used for the new worktree and PR base. |
| `--branch` | No | `loop/issue-<issue-number>` | Branch created for the worktree and pushed for the PR. |
| `--run-id` | No | generated | Run id used for attempt state and recovery context. |
| `--attempt` | No | `1` | Attempt number recorded in state and recovery output. |
| `--recovery-context` | No | unset | Prior recovery context to append to the worker prompt. |
| `--provider` | No | `codex` | Worker provider. v1 validates this to `codex` only. |
| `--model` | No | unset | Optional Codex model override. Passed as `-m <model>` only when set. |
| `--effort` | No | unset | Optional Codex reasoning effort override. Passed as `-c model_reasoning_effort=<effort>` only when set. |
| `--keep-worktree` | No | false | Keeps the worktree and scratch directory for inspection instead of cleaning them up. |

## Model And Speed

`--model` and `--effort` are optional knobs. The adapter passes them to
`codex exec` only when the caller provides non-empty values:

- `--model` becomes `-m <model>`.
- `--effort` becomes `-c model_reasoning_effort=<effort>`.

When both are absent, loopcoder passes no model or reasoning-effort flags and
Codex inherits the user's global Codex configuration from
`~/.codex/config.toml`.

loopcoder never chooses a model or reasoning effort on its own. The conductor
playbook in [`../SKILL.md`](../SKILL.md) says to inherit the user's global Codex
setting by default, and
[`BACKLOG.md` B1](BACKLOG.md#b1--worker-model--speed-selection) records the same
principle: configuration and command-line overrides should reflect only what the
user has explicitly requested.

## Output

On success, `loopcoder dispatch` prints a compact JSON object:

```json
{"ok":true,"issue":24,"branch":"loop/issue-24","pr":"https://github.com/owner/repo/pull/123","summary":"Codex summary text"}
```

The fields are:

- `ok`: `true` on success.
- `issue`: the issue number.
- `branch`: the branch pushed for the PR.
- `pr`: the PR URL returned by `gh pr create`.
- `summary`: Codex's summary from the `-o <summaryFile>` output, or a fallback
  message if no summary file exists.
