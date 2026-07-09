# Worker Adapter

This document describes the worker adapter as built in `loopcoder dispatch`,
the native `loopcoder` binary subcommand. The adapter is provider-pluggable
and uses the shared provider registry plus the static model/depth registry.

## Purpose

The worker adapter turns one approved GitHub issue into a pull request. The
subcommand owns the mechanical delivery steps: repository resolution, worktree
creation, provider invocation, change detection, commit, push, PR creation,
attempt state, recovery briefs, and cleanup.

The selected provider only edits files in the fresh worktree. It does not
commit, push, or open the PR; loopcoder owns those VCS steps. The worker prompt
requires the provider's final summary to be in English.

## Supported worker providers

The worker is provider-pluggable. The adapter step is delegated to a provider
registry. The registered worker providers are:

- `codex` (default; verified)
- `claude` (verified)
- `antigravity` (Google Antigravity CLI path through executable `agy`)
- `gemini` (experimental/unverified)

`loopcoder models` exposes the static model registry for `codex`, `claude`,
and `antigravity`. The older direct `gemini` worker adapter code is still
present and registered, but it is experimental and is not part of the static
model registry because the Antigravity provider is the current Gemini-family
target path.

The provider is selected per dispatch with the `--provider` flag and defaults to
`codex`. The registry rejects any unknown provider with an actionable error.

`--model` and `--effort` are provider-specific overrides. When either value is
absent, loopcoder resolves it from `.delivery.yml` and then from the static
registry: absent model becomes the resolved provider's default model, and absent
effort becomes the resolved model's default depth. Values are exact and
case-sensitive. Default mode warns and preserves invalid pass-through values;
`models.strict: true` or `--strict` rejects invalid provider/model/depth
selections before launching the provider.

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
5. Build and validate the Worker `Report` from parsed provider
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

## Reporter

A successful dispatch creates the Worker `Report` after the provider
exits and before commit, push, or PR creation. For providers with parseable
usage, the record is derived from provider output and carries `role: worker`,
the selected provider, the real parsed model and effort,
`model_source: parsed`, `permission: write`, the issue action, exit code,
timing, duration, token usage, and `verified: true`.

For Claude invocations with an explicit configured model, the reported model is
the pinned/configured model when that exact model appears in Claude's reported
model usage. Auxiliary models can still appear in provider usage, but a larger
auxiliary token count does not relabel the Worker or Verifier report away
from the configured model that the provider reported.

Worker reports are surfaced locally only: the `dispatch` stdout records, the
dispatch result JSON `report` object, stderr pretty output, and gitignored
`.loopcoder/` run records. The PR body does not carry reports; it should
contain delivery text such as the issue closing line and provider summary.

This replaces the older bare `worker: <provider>` line. If report
validation fails, including missing model identity or token usage, dispatch
hard-fails before delivery and opens no PR. Antigravity is a provider-scoped
exception: because the `agy` CLI does not expose stable parseable model usage
or token usage in this path, Worker reports use the selected Antigravity
model string as `model_source: self-reported` and accepts absent token usage.
This exception does not relax validation for `codex`, `claude`, or `gemini`.

Design rationale: [`../specs/0567-reporter.md`](../specs/0567-reporter.md) and
the historical foundation in [`../specs/0146-attestation.md`](../specs/0146-attestation.md).

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

The `antigravity` adapter runs:

```text
agy -p <prompt> --add-dir <worktree> --model "<model> (<Depth>)"
```

The command uses executable `agy`, closes stdin, sets the process working
directory to the worktree, and always includes `--add-dir <worktree>` as the
workspace pin. The `--model` value is the selected Antigravity model string
after registry/default resolution, for example `Gemini 3.1 Pro (High)`;
future depthless models would be passed as just `<model>`. The adapter captures
plain stdout as the summary and writes stdout/stderr to the normal provider log.
Antigravity read-only mode is not available or verified, so read-only
Verifier/audit invocations fail closed before launching `agy`.

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
| `--provider` | No | `codex` | Worker provider registered in the provider registry: `codex`, `claude`, `antigravity`, or experimental/unverified `gemini`. |
| `--model` | No | resolved registry default | Optional provider-specific model override. When absent, role config and then the provider registry default are used. |
| `--effort` | No | resolved model default | Optional provider-specific reasoning effort/depth override. When absent, role config and then the resolved model's default depth are used. |
| `--strict` | No | false | Reject invalid model/depth selections instead of warning and preserving the pass-through value. |
| `--keep-worktree` | No | false | Keeps the worktree and scratch directory for inspection instead of cleaning them up. |
| `--pretty` | No | false | Forces the human-readable pretty report block to stderr in emoji form, even on non-TTY output; absent still uses the default pretty behavior. |
| `--no-pretty` | No | false | Suppresses the human-readable pretty report block. This wins over `--pretty` and `LOOPCODER_PRETTY`. |

## Failure Semantics

Worker attempt state uses distinct terminal statuses so nested runs can be
recovered safely:

- `failed`: the worker or adapter returned a non-context error.
- `cancelled`: the parent context was cancelled and the child stopped through
  normal supervision.
- `timed_out`: the parent context deadline elapsed before or during child work.
- `abandoned`: a queued child was selected for a parent wave but never launched
  because the parent stopped first.
- `needs-human`: partial or ambiguous work was preserved and requires human
  review before retrying or dispatching dependents.

Every failed, cancelled, timed-out, hung, or abandoned attempt must remain
visible in local state and must point at a recovery brief when loopcoder can
write one. `dispatch-wave` passes the parent context to all launched children;
when the parent stops before a queued child starts, it writes a synthetic child
attempt and recovery brief instead of treating that child as success.

## Model And Depth

`--model` and `--effort` are optional knobs, but the runtime selection passed to
the provider is always resolved before launch. Resolution order is command flag,
role-scoped `.delivery.yml` value, then static registry default:

- `codex`: `--model` becomes `-m <model>` and `--effort` becomes
  `-c model_reasoning_effort=<effort>`.
- `claude`: `--model` becomes `--model <model>` and `--effort` becomes
  `--effort <effort>`.
- `antigravity`: selected model and depth become one `agy --model
  "<model> (<Depth>)"` value, such as `Gemini 3.1 Pro (High)`.
- `gemini`: `--model` becomes `-m <model>` and `--effort` is ignored with a
  one-time advisory because the Gemini CLI has no separate effort knob.

When both are absent, loopcoder uses the selected provider's static registry
default model and that model's default depth. The initial registry defaults are
`codex` `gpt-5.5` / `high`, `claude` `claude-opus-4-8[1m]` / `max`, and
`antigravity` `Gemini 3.1 Pro` / `High`.

Registry defaults are runtime fallbacks. They are not written back to
`.delivery.yml` unless the user explicitly asks to persist a preference.
Configured values are validated exactly and case-sensitively. Invalid values
warn by default and are passed through unchanged; `models.strict: true` or
`dispatch --strict` rejects them before provider launch.

The one-run `--strict` flag is available on the commands that resolve Worker or
Verifier model/depth selections: `dispatch`, `dispatch-wave`, `loopreview`,
`audit`, `tick`, `trigger`, and `recover`.

## Verifier Model And Depth Config

The independent verifier uses its own role-scoped `.delivery.yml` settings.
This keeps verifier choice separate from the worker provider and lets projects
pin a stronger review model without changing the worker default:

```yaml
verifier:
  model: "claude-opus-4-8[1m]"
  reasoning_effort: max
```

The `[1m]` suffix must be quoted in YAML. When these fields are present,
`loopcoder loopreview` uses them after any one-off `--model` or `--effort`
flags. When they are absent, the verifier resolves the selected verifier
provider's static registry default model and then that model's default depth.
`loopreview --strict` applies the same strict validation as `dispatch
--strict`.

## Output

On success, `loopcoder dispatch` prints three newline-terminated stdout records:

1. the stable Worker report header;
2. the Worker report canonical JSON;
3. the dispatch result JSON.

The final non-empty stdout line is the dispatch result JSON:

```json
{"ok":true,"issue":24,"branch":"loop/issue-24","run_id":"run-24-20260627-120000","pr":"https://github.com/owner/repo/pull/123","summary":"Provider summary text","attempt_path":".loopcoder/runs/run-24-20260627-120000/workers/job-24-1234.attempt.json","status":"succeeded","exit_code":0,"log_bytes":1234,"report":{"role":"worker","provider":"codex","model":"gpt-5","model_source":"parsed","effort":"high","permission":"write","action":"implement issue #24","exit_code":0,"started_at":"...","ended_at":"...","duration_ms":120000,"usage":{"total_tokens":12345},"verified":true}}
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
- `report`: the same validated Worker `Report` emitted in the
  first two stdout records.

During the 0.6.x transition window, readers accept legacy dispatch result JSON with an
`attestation` object and legacy `[attestation]` headers, but new output uses
the `report` object and `[reporter]` header per
[`../specs/0567-reporter.md`](../specs/0567-reporter.md).

The default pretty behavior writes the human-readable pretty report block to
stderr by default with the polished display format. The default block uses
emoji on an interactive TTY and plain ASCII on a non-TTY. It shows provider
vendor and provider key on one combined line, such as `OpenAI Codex / codex`,
renders the model and depth plus source as `gpt-5.5 (xhigh) (parsed)` or
`Gemini 3.1 Pro (High) (self-reported)`, uses host-local timestamps to whole
seconds, reports duration in seconds, and groups token counts with thousands
separators. When input and output tokens are present without a total, the
pretty block derives a display-only total. This never changes or reorders the
three stdout records, the stable `Header()` / `[reporter] ...` line, or the
canonical JSON, and it never adds reports to PR bodies.

Example pretty block:

```text
report: verified
who
  role        worker
  provider    OpenAI Codex / codex
  model       gpt-5.5 (xhigh) (parsed)
  permission  write
what
  issue       #293
  action      "implement issue #293"
result
  exit        0
  duration    7m53.9s (473.9 s)
  started     2026-06-30 14:25:21 JST
  ended       2026-06-30 14:33:15 JST
  verified    true
cost
  tokens      total=165,268
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
[`../specs/0567-reporter.md`](../specs/0567-reporter.md),
[`../specs/0282-default-pretty-attestation.md`](../specs/0282-default-pretty-attestation.md),
[`../specs/0296-attestation-display-polish.md`](../specs/0296-attestation-display-polish.md),
and [`../specs/0300-model-attribution.md`](../specs/0300-model-attribution.md).
