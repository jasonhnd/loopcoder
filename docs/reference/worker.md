# Worker Adapter

This document describes the worker adapter as built in `loopcoder dispatch`,
the native `loopcoder` binary subcommand. As of 0.3.3 the adapter is
provider-pluggable and uses the shared provider registry.

## Purpose

The worker adapter turns one approved GitHub issue into a pull request. The
subcommand owns the mechanical delivery steps: repository resolution, worktree
creation, provider invocation, change detection, commit, push, PR creation,
attempt state, recovery briefs, and cleanup.

The selected provider only edits files in the fresh worktree. It does not
commit, push, or open the PR; loopcoder owns those VCS steps. The worker prompt
requires the provider's final summary to be in English.

## Supported worker providers

As of 0.3.3 the worker is provider-pluggable. The adapter step is delegated to a
provider registry, and three providers are registered:

- `codex` (default; verified)
- `claude` (verified)
- `gemini` (experimental/unverified)

The `gemini` worker adapter code is present and registered, but it has not been
verified end-to-end because the Gemini CLI was not usable in the development
environment due to missing authentication.

The provider is selected per dispatch with the `--provider` flag and defaults to
`codex`. The registry rejects any unknown provider with an actionable error.

`--model` and `--effort` are provider-specific. `codex` and `claude` honor the
reasoning-effort knob; `gemini` has no effort knob, so `--effort` is ignored for
it (logged once, not an error). As always, loopcoder passes these only when the
caller sets them and otherwise inherits each provider's own configuration.

For the full design — roles, the provider abstraction, and per-provider adapter
facts — see
[`../specs/0131-multi-provider-roles.md`](../specs/0131-multi-provider-roles.md).

## Flow

For one issue, `loopcoder dispatch` runs these steps in order:

1. Resolve `--repo` to a local path and resolve the GitHub `owner/name` slug
   with `gh repo view`.
2. Create a scratch directory and a fresh git worktree from
   `origin/<base-branch>`.
3. Write a self-contained prompt file containing the issue number, title, body,
   branch, current working directory, and rules.
4. Run the selected provider through the registry in the worktree, capturing its
   log and summary with provider-specific adapter behavior.
5. Build and validate the Worker `AttestationRecord` from parsed provider
   output.
6. Verify that file changes exist with `git status --porcelain`.
7. Commit all changes with the issue title and `closes #<IssueNumber>` in the
   commit message.
8. Push the branch to `origin`.
9. Open a pull request with `gh pr create`.
10. Remove the worktree, delete the local branch, and remove the scratch
   directory unless `--keep-worktree` is set.

If the provider exits non-zero, makes no file changes, commit fails, push fails,
or PR creation fails, the subcommand returns a failed result instead of
producing a successful result.

## Attestation

A successful dispatch stamps the Worker `AttestationRecord` after the provider
exits and before commit, push, or PR creation. The record is binary-stamped from
the provider output with `role: worker`, the selected provider, the real parsed
model and effort, `model_source: parsed`, `permission: write`, the issue action,
exit code, timestamps, duration, token usage, and `verified: true`.

For Claude invocations with an explicit configured model, the attested model is
the pinned/configured model when that exact model appears in Claude's reported
model usage. Auxiliary models can still appear in provider usage, but a larger
auxiliary token count does not relabel the Worker or Verifier attestation away
from the configured model that the provider reported.

The PR body carries the one-line attestation header plus a fenced canonical JSON
block:

````text
Closes #<issue>

<provider summary>

[attestation] role=worker provider=<provider> model=<model>(parsed) effort=<effort> perm=write action="implement issue #<issue>" exit=0 dur=<duration> tokens=<usage> verified=true

```json
{"role":"worker","provider":"codex","model":"gpt-5","model_source":"parsed","effort":"high","permission":"write","action":"implement issue #185","exit_code":0,"started_at":"...","ended_at":"...","duration_ms":120000,"usage":{"total_tokens":12345},"verified":true}
```
````

This replaces the older bare `worker: <provider>` line. If attestation
validation fails, including missing model identity or token usage, dispatch
hard-fails before delivery and opens no PR. `codex` and `claude` are the
verified worker providers; `gemini` remains experimental/unverified end-to-end,
and the same validation still applies to it.

Design rationale: [`../specs/0146-attestation.md`](../specs/0146-attestation.md).

## Provider Invocation

All providers share the same contract: edit files only, do not commit or push,
and return a final summary for the harness to include in the PR body and JSON
result.

The `codex` adapter runs:

```text
codex exec --cd <worktree> --dangerously-bypass-approvals-and-sandbox --skip-git-repo-check ... -o <summaryFile> -
```

The prompt must be fed from a file with stdin closed:

```text
codex exec ... - < promptfile
```

This is required because headless `codex exec` hangs forever waiting on stdin if
it is not given a closed stdin stream. The adapter writes the prompt to a
`prompt.txt` file, opens it as stdin, and writes stdout/stderr to `codex.log`.

The adapter passes `--dangerously-bypass-approvals-and-sandbox` so Codex can run
unattended without approval prompts. The fresh worktree is the intended blast
radius. The current invocation also passes `--skip-git-repo-check`.

The `claude` adapter runs `claude --print --dangerously-skip-permissions
--output-format json ...` with the prompt on stdin, and parses the JSON `result`
field as the summary.

The `gemini` adapter runs `gemini --prompt <prompt> --yolo --output-format json
...` and parses the JSON response fields, falling back to raw stdout when
needed. Gemini has no reasoning-effort flag, so a supplied `--effort` is logged
as an advisory and otherwise ignored.

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
| `--provider` | No | `codex` | Worker provider registered in the provider registry: `codex`, `claude`, or experimental/unverified `gemini`. |
| `--model` | No | unset | Optional provider-specific model override. Passed only when set. |
| `--effort` | No | unset | Optional provider-specific reasoning effort override. `codex` and `claude` honor it; `gemini` logs an advisory and ignores it. |
| `--keep-worktree` | No | false | Keeps the worktree and scratch directory for inspection instead of cleaning them up. |
| `--pretty` | No | false | Forces the human-readable pretty attestation block to stderr in emoji form, even on non-TTY output; absent still uses the default pretty behavior. |
| `--no-pretty` | No | false | Suppresses the human-readable pretty attestation block. This wins over `--pretty` and `LOOPCODER_PRETTY`. |

## Model And Effort

`--model` and `--effort` are optional knobs. The adapter passes them to
the selected provider only when the caller provides non-empty values and the
provider supports the relevant flag:

- `codex`: `--model` becomes `-m <model>` and `--effort` becomes
  `-c model_reasoning_effort=<effort>`.
- `claude`: `--model` becomes `--model <model>` and `--effort` becomes
  `--effort <effort>`.
- `gemini`: `--model` becomes `-m <model>` and `--effort` is ignored with a
  one-time advisory because the Gemini CLI has no separate effort knob.

When both are absent, loopcoder passes no model or reasoning-effort flags and
the selected provider inherits its own configured defaults.

loopcoder never chooses a model or reasoning effort on its own. The conductor
playbook in [`SKILL.md`](../../SKILL.md) says to inherit the selected provider's
own setting by default, and
[`BACKLOG.md` B1](../BACKLOG.md#b1--worker-model--speed-selection) records the same
principle: configuration and command-line overrides should reflect only what the
user has explicitly requested.

## Verifier Model And Effort Config

The independent verifier uses its own role-scoped `.delivery.yml` settings.
This keeps verifier choice separate from the worker provider and lets projects
pin a stronger review model without changing the worker default:

```yaml
verifier:
  model: "claude-opus-4-8[1m]"
  reasoning_effort: max
```

The `[1m]` suffix must be quoted in YAML. When these fields are present,
`loopcoder loopreview` passes the configured model and effort to the verifier
provider; one-off `--model` and `--effort` flags remain per-run overrides.

## Output

On success, `loopcoder dispatch` prints three newline-terminated stdout records:

1. the stable Worker attestation header;
2. the Worker attestation canonical JSON;
3. the dispatch result JSON.

The final non-empty stdout line is the dispatch result JSON:

```json
{"ok":true,"issue":24,"branch":"loop/issue-24","run_id":"run-24-20260627-120000","pr":"https://github.com/owner/repo/pull/123","summary":"Provider summary text","attempt_path":".loopcoder/runs/run-24-20260627-120000/workers/job-24-1234.attempt.json","status":"succeeded","exit_code":0,"log_bytes":1234,"attestation":{"role":"worker","provider":"codex","model":"gpt-5","model_source":"parsed","effort":"high","permission":"write","action":"implement issue #24","exit_code":0,"started_at":"...","ended_at":"...","duration_ms":120000,"usage":{"total_tokens":12345},"verified":true}}
```

The dispatch result fields are:

- `ok`: `true` on success.
- `issue`: the issue number.
- `branch`: the branch pushed for the PR.
- `pr`: the PR URL returned by `gh pr create`.
- `run_id`: the run id used for durable attempt state.
- `summary`: the selected provider's summary, or a fallback message if no
  summary exists.
- `attempt_path`: path to the durable attempt sidecar.
- `status`: attempt status.
- `exit_code`: provider exit code.
- `log_bytes`: size of the provider log.
- `attestation`: the same validated Worker `AttestationRecord` emitted in the
  first two stdout records.

As of 0.3.5, `dispatch` writes the human-readable pretty attestation block to
stderr by default with the polished display format. The default block uses
emoji on an interactive TTY and plain ASCII on a non-TTY. It shows the provider
vendor plus the CLI `tool`, renders the model source as `(detected)` or
`(self-reported)`, uses host-local timestamps to whole seconds, reports
duration in seconds, and groups token counts with thousands separators. When
input and output tokens are present without a total, the pretty block derives a
display-only total. This never changes or reorders the three stdout records,
the PR body, the stable `Header()` / `[attestation] ...` line, or the canonical
JSON.

Example pretty block:

```text
attestation: verified
  role        worker
  provider    OpenAI
  tool        codex
  model       gpt-5.5 (detected)
  effort      xhigh
  permission  write
  action      "implement issue #293"
  exit        0
  started     2026-06-30 14:25:21 JST
  ended       2026-06-30 14:33:15 JST
  duration    7m53.9s (473.9 s)
  tokens      total=165,268
  verified    true
```

`--pretty` or `LOOPCODER_PRETTY=1` forces emoji pretty output even on non-TTY
stderr. `--no-pretty` or `LOOPCODER_NO_PRETTY=1` suppresses pretty output and
wins over force. When pretty output is shown, `NO_COLOR`, `LOOPCODER_PLAIN=1`,
or `LOOPCODER_NO_EMOJI=1` forces the plain ASCII form.

The same default-on stderr pretty rule applies to `loopcoder loopreview` and
`loopcoder dispatch-wave`: `loopreview` keeps verdict JSON on stdout, and
`dispatch-wave` keeps its stdout text report while emitting one Worker pretty
block per dispatched issue. Pretty output is for human diagnostics and
conductor relay, not for machine parsing.

Design rationale:
[`../specs/0282-default-pretty-attestation.md`](../specs/0282-default-pretty-attestation.md),
[`../specs/0296-attestation-display-polish.md`](../specs/0296-attestation-display-polish.md),
and [`../specs/0300-model-attribution.md`](../specs/0300-model-attribution.md).
