# loopcoder Usage

loopcoder turns one delivery need into a small GitHub-issue batch,
provider-pluggable worker PRs, independent `loopreview` verification, chat
progress, and user-directed merges.

Use it when you want one conductor chat to plan, dispatch, review, and merge a
small batch of repository work without manually relaying every step between
GitHub, git worktrees, worker providers, and PR review.

## Quickstart (new project)

Use this flow to onboard an existing repository from zero to a driven
loopcoder loop. One installed binary can serve many local repositories; each
repository keeps its own `.delivery.yml` and loopcoder run state.

Per-project prerequisites: `git`, authenticated `gh`, at least one
authenticated provider CLI (`codex` and/or `claude`), and a GitHub remote with
push access.

1. Install the binary once per machine, shared across all local projects.

   Unix-like systems:

   ```text
   curl -fsSL https://raw.githubusercontent.com/jasonhnd/loopcoder/main/scripts/install.sh | sh
   ```

   Windows PowerShell:

   ```text
   irm https://raw.githubusercontent.com/jasonhnd/loopcoder/main/scripts/install.ps1 | iex
   ```

   The installer puts `loopcoder` under `~/.loopcoder/bin`. Keep that directory
   on `PATH`, or set `LOOPCODER_BIN` to the full binary path.

2. Verify the install and global environment.

   ```text
   loopcoder --version
   loopcoder doctor
   ```

3. Install the conductor playbook once per agent home and wire project
   conductor hooks into the repository's Claude Code settings.

   ```text
   loopcoder skill install --repo <repo>
   ```

   This writes the bundled `SKILL.md` plus the Codex `AGENTS.md` entrypoint to
   the Claude Code loopcoder skill directory and merges the loopcoder conductor
   hooks into `<repo>/.claude/settings.json`.

4. Initialize each consumer repository.

   ```text
   cd <repo>
   loopcoder init
   loopcoder doctor
   ```

   `loopcoder init` scaffolds `.delivery.yml`, `ROADMAP.md`, and the GitHub
   labels loopcoder uses. The follow-up `loopcoder doctor` confirms `git`, `gh`
   auth, provider CLIs, `origin`, the default branch, and project conductor
   hook settings for that repository.

5. Drive the loop from a conductor session in the repository.

   ```text
   /loopcoder <your need>
   ```

   The conductor plans the work, dispatches workers, runs `loopreview`, and
   reports promotion status. Production promotion is automatic by default when
   the gate passes; set `adapters.gate: human-merge` to opt out.

## Prerequisites

- A conductor host session that follows `SKILL.md`, `AGENTS.md`, or `GEMINI.md`.
- `git` on `PATH`.
- `gh` on `PATH`, authenticated for the target GitHub repository.
- At least one supported provider CLI on `PATH`. `codex` is the default worker,
  `codex` and `claude` are verified worker and verifier providers, and
  `agy` is the Google Antigravity CLI used by provider key `antigravity`.
  The older direct `gemini` worker adapter remains experimental and is not part
  of the static model registry.
- A GitHub repository with a configured remote.
- For the no-Go installer: `curl`, `tar`, and `sha256sum` or `shasum` on
  Unix-like systems, or PowerShell on Windows. Go is optional for developer
  installs and local source builds.

## Install

The supported consumer distribution is GitHub Releases. Tagged releases publish
Windows, macOS, and Linux archives for `amd64` and `arm64`, plus `SHA256SUMS`.
The install scripts select the matching release asset, verify it against
`SHA256SUMS`, install under `~/.loopcoder/bin`, and update or print PATH
instructions. The scripts do not require Go.

On Unix-like systems:

```text
curl -fsSL https://raw.githubusercontent.com/jasonhnd/loopcoder/main/scripts/install.sh | sh
```

To pin a version:

```text
curl -fsSL https://raw.githubusercontent.com/jasonhnd/loopcoder/main/scripts/install.sh | sh -s -- --version 0.3.7
```

On Windows PowerShell:

```text
irm https://raw.githubusercontent.com/jasonhnd/loopcoder/main/scripts/install.ps1 | iex
```

To pin a version on Windows:

```text
$env:LOOPCODER_VERSION = "0.3.7"
irm https://raw.githubusercontent.com/jasonhnd/loopcoder/main/scripts/install.ps1 | iex
```

After installation, confirm the selected binary and environment:

```text
loopcoder --version
loopcoder doctor
```

`go install` remains available for users who already have Go:

```text
go install github.com/jasonhnd/loopcoder/cmd/loopcoder@latest
```

From a source checkout, you can also build a development binary locally:

```text
go build ./cmd/loopcoder
```

Make sure the installed or built binary is on `PATH`, or set `LOOPCODER_BIN` to
its full path. The conductor uses this binary as its only mechanical backend.
Release and installation rationale lives in
[`0212-release-distribution-and-upgrade.md`](../specs/0212-release-distribution-and-upgrade.md).

## Backend Selection

The conductor resolves the `loopcoder` binary before running mechanical work:

1. `LOOPCODER_BIN` when set.
2. `loopcoder` found on `PATH`.
3. Otherwise, `loopcoder` is required on all platforms.

Use the resolved binary for dispatch, ready-set scheduling, status reporting,
resume, recovery, local verification, state, and lease operations.

## Conductor Hooks And Status

Install or refresh the Claude Code playbook and project conductor hooks with:

```text
loopcoder skill install --repo <project>
```

The command writes the bundled `SKILL.md` and `AGENTS.md` files to the Claude
Code loopcoder skill directory and merges two hook commands into
`<project>/.claude/settings.json`: `loopcoder hook conductor-attest` and
`loopcoder hook conductor-relay-guard`. The hooks are embedded in the loopcoder
binary and invoked as subcommands, so they resolve regardless of the working
directory and need no Node dependency; the merge upgrades any stale
`node hooks/*.js` entries idempotently. The merge is project-scoped and
preserves unrelated settings and hooks, and writes a gitignored
`.loopcoder/conductor-workspace` marker that activates auto-enforcement in the
installed repo. `loopcoder doctor --repo <project>` warns when either conductor
hook command is missing or when `loopcoder` does not resolve on `PATH`.

`loopcoder hook conductor-attest` enforces the local Conductor self-attestation
step before a delivery or merge turn can finish.
`loopcoder hook conductor-relay-guard` enforces local visible relay of Worker
and Verifier attestation from `loopcoder dispatch`, `loopcoder dispatch-wave`,
and `loopcoder loopreview`. Do not redirect, hide, or suppress `dispatch` or
`loopreview` stderr, and keep foreground `dispatch-wave` stdout visible because
each Worker pretty block streams there as that Worker completes. The relay guard
covers Bash, PowerShell, and pwsh tool events and treats backgrounded command
output as pending until the block is surfaced.

The Go binary also hard-gates mechanical progress while pending local relay
blocks are unacknowledged. A gated command exits with reserved code `4`, prints
the pending block(s) plus recovery instructions to stdout, and refuses to run.
Use `loopcoder relay flush --repo <project>` in the foreground to print pending
blocks verbatim and clear them, or `loopcoder relay list --repo <project>` to
inspect pending records without clearing.

Report delivery run state with the program-rendered local status command:

```text
loopcoder status --repo .
loopcoder status --repo . --run <run-id>
```

When `--run` is omitted, `status` selects the latest modified local run. The
output is read-only and local-only: it reads gitignored `.loopcoder/` state and
must not be copied into PR bodies, issues, comments, commits, merge artifacts,
docs, examples, fixtures, or tracked files.

## Repository Initialization

Run `loopcoder init` from a repository root to scaffold the local loopcoder
starting point:

```text
loopcoder init
```

`init` creates `.delivery.yml` and `ROADMAP.md` when they are absent and
ensures the default GitHub labels used by loopcoder are present. Existing
`.delivery.yml` and `ROADMAP.md` files are left untouched by default; use
`--force` to overwrite those two files with the current scaffold:

```text
loopcoder init --force
```

Label setup is best-effort through `gh label list` and `gh label create`. If
`gh` is unavailable or label creation fails, `init` reports a warning on stderr
instead of treating file scaffolding as failed.

The model and reasoning-effort flags are first-run persistence helpers. Use
them only when you want the generated `.delivery.yml` to pin project defaults:

```text
loopcoder init \
  --worker-model gpt-5.5 \
  --worker-effort high \
  --verifier-model claude-haiku-4-5-20251001 \
  --verifier-effort medium
```

Omitting these flags leaves model and depth absent in `.delivery.yml`. At run
time, each role resolves the selected provider's static registry default model
and then that model's default depth; those resolved defaults are not written
back to config unless you explicitly persist a preference.

## Per-Repo Setup

Use `loopcoder init` or manually add a `.delivery.yml` file at the repository
root. If it is absent, loopcoder uses the current defaults: GitHub issues, git
worktrees, the Codex worker adapter, GitHub PRs/checks/merges, independent
`loopreview` verification, pre-prod-only auto-integration for clean `tick` PRs,
default `auto` production promotion, and chat reporting. Use `gate:
human-merge` when a project needs humans to choose production merges explicitly.

The current example is:

```yaml
version: 1
adapters:
  work_items: github      # Work items are GitHub issues.
  workspace: git-worktree # Work happens in git worktrees.
  conductor: codex-cli    # Transparency only: the human session that conducts.
  worker: codex           # Default worker provider.
  vcs: github             # GitHub hosts PRs and checks.
  verifier: claude        # Should differ from worker; provider registry key.
  gate: auto              # Default-on production promotion; set human-merge to opt out so humans choose what merges.
worker:
  # Optional. Absent = static registry default model for the resolved worker provider.
  # model:
  # Optional. Absent = default depth for the resolved worker model.
  # reasoning_effort:
  base_branch: main
  command_hint: "implement the issue, run relevant checks, commit"
environment:
  pre_prod_branch: pre-prod # Tick auto-merges clean PRs here only; promote is the separate production step.
# evidence:
#   # Optional. Tick copies configured evidence onto dispatched, pending, and pre-prod report items.
#   website:
#     preview_url: https://preview.example.com
#   cli:
#     example_output: |
#       $ loopcoder --version
#       version=dev commit=unknown date=unknown
#   library:
#     test_results: go test ./...
#   app:
#     preview_build: dist/app-preview.zip
# verifier:
#   # Optional. Absent = static registry default model for the resolved verifier provider.
#   # model:
#   # Optional. Absent = default depth for the resolved verifier model.
#   # reasoning_effort:
# models:
#   # Optional. false warns and continues; true rejects invalid model/depth selections before launch.
#   # strict: true
ci:
  checks: []
report:
  channel: chat
```

`environment.pre_prod_branch` defaults to `pre-prod`. If that branch is absent,
empty, reserved as `main`/production, or cannot accept the merge, `tick` skips
auto-merge, records `needs-human`, and leaves production untouched. Promotion to
production remains a separate `promote` step: with the default `gate: auto`, it
auto-promotes only when required CI is green, `loopreview` passed, configured
evidence is present, and the red-line floor is clean. A failed post-promote
check triggers deterministic rollback to the recorded `prior_stable_commit`.
`gate: human-merge` keeps the old explicit human promotion behavior.

`evidence` is optional. When present, it is keyed by project type (`website`,
`cli`, `library`, or `app`) and supports simple proof fields such as
`preview_url`, `example_output`, `test_results`, and `preview_build`. `tick`
copies those configured artifacts into the JSON report and the human-readable
summary for dispatched, pending, and pre-prod items.

The verifier role has its own optional model and `reasoning_effort` depth
settings. Quote model IDs that contain YAML-special characters such as `[1m]`:

```yaml
verifier:
  model: "claude-opus-4-8[1m]"
  reasoning_effort: max
```

For compatibility signals such as `min_loopcoder_version`, see
[`stability-policy.md`](stability-policy.md).

## End-To-End Use

1. In a new repository, run `loopcoder init` once, or add `.delivery.yml`
   manually.

2. State a delivery need, for example:

   ```text
   /loopcoder add usage docs for the project
   ```

3. loopcoder inspects the repo context and drafts GitHub issues plus a
   dependency DAG. It shows the proposed issues, blocked-by relationships, and
   worker setting in chat.

4. You approve the plan before anything is published.

5. loopcoder creates the approved GitHub issues and dispatches ready issues to
   workers through `loopcoder dispatch` / `loopcoder dispatch-wave`. The binary
   creates a fresh git worktree, runs the selected provider, commits the
   resulting changes, pushes the branch, opens a PR, and cleans up.

6. loopcoder runs `loopcoder loopreview` for each PR, checks the diff and
   `gh pr checks`, relays Worker and Verifier attestation blocks verbatim, and
   reports progress, failures, risks, and final run state through
   `loopcoder status`. `codex` and `claude` have real verifier smoke proof;
   ambiguous, malformed, timed-out, or incomplete verifier output is still
   reported as `needs-human`.

7. After `loopreview = pass`, `tick` runs a deterministic risk gate. Clean PRs
   auto-merge into `environment.pre_prod_branch`; red-line PRs, red/missing CI,
   loopcoder-core changes, and verifier non-pass results are reported as
   `needs-human` and do not merge. After a pre-prod merge, `tick` reads the
   configured CI checks on the pre-prod branch head; if that head is the
   just-created merge commit and a required check is red, `tick` reverts that
   commit on pre-prod and records the PR as `needs-human`.

8. `loopcoder promote` advances the pre-prod batch to production/main. With the
   default `gate: auto`, promotion happens only after the deterministic gate has
   CI-green, verifier-pass, evidence-present, and red-line-clean signals, and it
   records rollback SHAs before merging. With `gate: human-merge`, you name the
   pre-prod batch or PRs to promote and loopcoder uses the explicit human merge
   path. `tick` never auto-merges to `main`.

## Version And Doctor

Use any of these forms to print one line of build information:

```text
loopcoder version
loopcoder --version
loopcoder -v
```

The output includes the loopcoder version, commit, build date, Go runtime, and
platform. The exact version, commit, and date values depend on the build
metadata available to the binary; a local source build without injected metadata
prints `version=dev commit=unknown date=unknown`.

```text
loopcoder version=<version> commit=<commit> date=<build-date> go=<go-version> platform=<os>/<arch>
```

`loopcoder doctor` is a read-only preflight for the current repository:

```text
loopcoder doctor --repo .
```

It reports `[ok]`, `[warn]`, or `[fail]` checks for:

- `git` on `PATH`;
- `gh` on `PATH` and `gh auth status`;
- `.delivery.yml` presence and parse validity;
- configured worker and verifier provider CLI availability;
- repository `origin` and detectable default branch;
- selected loopcoder binary path, version, commit, date, and release or
  development track;
- `.delivery.yml` schema version and `min_loopcoder_version` compatibility when
  declared;
- audit readiness: config parse result, effective threshold, configured SAST
  commands, required tool availability, parser recognition, rubric and
  baseline files, required `audit` CI check wiring, and Layer 2 verifier
  provider resolution;
- project Claude Code conductor hook settings, warning when the
  `loopcoder hook conductor-attest` or `loopcoder hook conductor-relay-guard`
  command is missing or when `loopcoder` does not resolve on `PATH`;
- conductor runtime responsibility, which remains user-provided by the active
  host.

Provider authentication is reported only where loopcoder has a stable cheap
probe. Today `doctor` checks `gh` authentication and provider CLI presence; it
does not invent provider-authentication status when the provider has no stable
probe.

## Security Audit

`loopcoder audit` runs a read-only security audit. The deterministic layer
(`--layer sast`) runs configured SAST tools plus native secret and sensitive
file-mode scans and is suitable for required CI. The LLM layer (`--layer llm`
or `--layer all`) uses the configured verifier provider in read-only mode with
the built-in threat model plus any configured rubric.

```text
loopcoder audit --repo . --layer sast
loopcoder audit --repo . --layer all --provider claude --pretty
```

The exit codes are `0` clean, `1` threshold findings, `2` needs-human, `3`
command/runtime failure, and `4` relay gate. Configure thresholds, SAST argv
arrays, rubrics, and baselines under `.delivery.yml audit`. Baseline waivers
must record a justification, date, review/expiry date, and either a
`fingerprint` or `normalized_evidence`; stale waivers are reported without
gating, while expired or malformed waivers require human judgment.

Layer 2 attestation is local-only, like `loopreview`: pretty blocks, canonical
JSON, relay records, and logs must not be copied into repository-visible
artifacts. See [`audit.md`](audit.md) for the full command reference.

## Model And Depth

`loopcoder models` prints the static model registry. It does not read
`.delivery.yml`, call provider CLIs, call `agy models`, read provider config,
mutate files, or require provider authentication:

```text
loopcoder models
loopcoder models --provider codex
loopcoder models --provider claude
loopcoder models --provider antigravity
```

The registry provider key for Google Antigravity is `antigravity`; `agy` is the
CLI executable name. `loopcoder models --provider agy` exits non-zero and hints
to use `--provider antigravity`.

Worker and Verifier model selection is role-scoped. For each role, provider
comes from the command flag, then `.delivery.yml` (`adapters.worker` or
`adapters.verifier`), then the built-in role fallback (`codex` for Worker,
`claude` for Verifier). Model comes from the role command flag, then
`worker.model` or `verifier.model`, then the static registry default model for
the resolved provider. Depth comes from the role command `--effort` flag, then
`worker.reasoning_effort` or `verifier.reasoning_effort`, then the resolved
model's default depth.

The initial registry defaults are:

| Provider | CLI | Default model | Default depth |
| --- | --- | --- | --- |
| `codex` | `codex` | `gpt-5.5` | `high` |
| `claude` | `claude` | `claude-opus-4-8[1m]` | `max` |
| `antigravity` | `agy` | `Gemini 3.1 Pro` | `High` |

Configured model and depth values are exact and case-sensitive. By default,
invalid provider, model, or depth selections warn and keep the selected
pass-through value. Strict mode rejects invalid selections before launching a
provider. Enable strict mode durably with:

```yaml
models:
  strict: true
```

For one run, use `--strict` on the commands that resolve Worker or Verifier
model/depth selections: `dispatch`, `dispatch-wave`, `loopreview`, `audit`,
`tick`, `trigger`, and `recover`.

For a single run, say what you want in chat, such as:

```text
run faster
#B use max
```

The conductor translates that request into exact provider/model/depth flags for
that run only. Natural-language depth mapping is still a conductor concern; the
binary validates exact registry tokens such as `high`, `max`, or Antigravity's
capitalized `High`.

For a permanent project preference, say so explicitly, for example:

```text
from now on default to high
```

Only then should loopcoder write `worker.reasoning_effort`, `worker.model`,
`verifier.reasoning_effort`, or `verifier.model` into `.delivery.yml`. Absent
fields mean "resolve from the static registry at runtime," not "inherit a
provider global config."

Antigravity setup uses the Google Antigravity CLI:

```text
agy login
agy models
loopcoder doctor --repo .
```

When the configured Worker or Verifier provider is `antigravity`, `doctor`
looks for executable `agy` and runs `agy models` as a bounded OAuth readiness
probe. The Antigravity Worker invocation uses this argv shape after
registry/default resolution:

```text
agy -p <prompt> --add-dir <worktree> --model "<model> (<Depth>)"
```

The mandatory `--add-dir` is the workspace pin. Antigravity read-only mode is
not available or verified, so `loopreview` and audit-review invocations fail
closed when selected with provider `antigravity`.

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
loopcoder version
loopcoder --version
loopcoder -v

loopcoder doctor --repo .

loopcoder models
loopcoder models --provider antigravity

loopcoder audit --repo . --layer sast
loopcoder audit --repo . --layer all --provider claude --strict

loopcoder skill install --repo .

loopcoder init
loopcoder init --force
loopcoder init --worker-model <model> --worker-effort <effort>
loopcoder init --verifier-model <model> --verifier-effort <effort>

loopcoder ready-set --repo . --base-branch main --format text

loopcoder dispatch \
  --repo . \
  --issue-number <number> \
  --issue-title "<title>" \
  --issue-body "<body>" \
  --base-branch main \
  --provider codex

loopcoder dispatch \
  --repo . \
  --issue-number <number> \
  --issue-title "<title>" \
  --provider codex \
  --strict \
  --pretty

loopcoder dispatch-wave --repo . --base-branch main --issue-numbers <n1>,<n2> --strict

loopcoder tick --repo . --strict
loopcoder trigger goal-loop --repo . --max-iterations <n> --strict

loopcoder status --repo .
loopcoder status --repo . --run <run-id>

loopcoder relay list --repo .
loopcoder relay flush --repo .

loopcoder resume --repo . --run-id <run-id>

loopcoder recover \
  --repo . \
  --issue-number <number> \
  --issue-title "<title>" \
  --issue-body "<body>" \
  --run-id <run-id> \
  --strict

loopcoder verify-local --repo . --pr-number <pr>

loopcoder loopreview --repo . --pr-number <pr> --provider claude --strict
loopcoder loopreview --repo . --pr-number <pr> --provider claude --pretty

loopcoder attest \
  --role conductor \
  --provider <host-provider> \
  --model <host-model> \
  --permission orchestrate \
  --action "<delivery action>" \
  --duration-ms <milliseconds> \
  --total-tokens <tokens>

loopcoder attest --pretty \
  --role conductor \
  --provider <host-provider> \
  --model <host-model> \
  --permission orchestrate \
  --action "<delivery action>" \
  --duration-ms <milliseconds> \
  --total-tokens <tokens>

loopcoder state push --repo . --run-id <run-id>
loopcoder state pull --repo .
loopcoder lease acquire --repo . --run-id <run-id>
loopcoder lease release --repo . --run-id <run-id>
```

## Exit Codes

- `loopcoder loopreview` reserves `0`, `1`, and `2` for clean verifier verdicts:
  `pass`, `fail`, and `needs-human`.
- `loopcoder loopreview` exits `3` when the command itself fails before or after
  a clean verdict, such as invalid flags, repository/config/provider setup
  failure, or output/relay write failure.
- `loopcoder audit` reserves `0`, `1`, and `2` for clean audit verdicts:
  `clean`, `findings`, and `needs-human`; it exits `3` for command/runtime
  failures.
- Mechanical progress commands exit `4` when the relay hard gate finds pending
  local-only Worker/Verifier blocks. Run `loopcoder relay flush --repo <path>`
  to print and acknowledge them, or `loopcoder relay list --repo <path>` to
  inspect without clearing.

## Verifier Provider Status

`loopcoder loopreview` has 0.3.3 smoke proof for the `claude` and `codex`
verifier mechanism: both providers returned a valid structured
verdict plus attestation within the timeout. This proof does not make the
LLM verdict itself deterministic; `pass` and `fail` remain model judgments that
can vary across otherwise valid runs.

One representative point-in-time run used merged PR #202 (`0.3.3: loopreview
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
`antigravity` is registered as a provider but read-only mode is not available
or verified, so selecting it for `loopreview` fails closed rather than running
a mutating review.

## Attestation

Worker and verifier invocations produce validated local-only attestation records
with provider, model, effort, permission, action, exit code, timing, and
verification fields. For providers with parseable usage, including `codex` and
`claude`, records use `model_source: parsed`, real parsed model identity, and
token usage. For Claude runs with an explicit pinned model, the attested model
is the pinned/configured model when that exact model appears in
provider-reported usage; a token-dominant auxiliary model does not override it.
Missing required identity or usage fails closed for providers that expose those
signals: `dispatch` opens no PR, and `loopreview` returns `needs-human` with
the incomplete-attestation finding. Antigravity is a provider-scoped exception:
Worker records use the selected `agy --model` string, such as `Gemini 3.1 Pro
(High)`, as `model_source: self-reported` and accept absent token usage.
Attestation surfaces are local-only:
`dispatch` / `loopreview` stderr pretty blocks, foreground `dispatch-wave`
stdout Worker pretty blocks, `dispatch` / `loopreview` result JSON, and
gitignored `.loopcoder/` run records. PR bodies, merge commits, and merge
comments are not attestation surfaces and must not contain attestation headers
or canonical JSON.

For every successful `loopcoder dispatch`, stdout contains three
newline-terminated records in this order:

1. The stable Worker attestation header from `record.Header()`.
2. The Worker attestation canonical JSON from `record.CanonicalJSON()`.
3. The dispatch result JSON, whose `attestation` object is the same validated
   Worker attestation record.

Example:

```text
[attestation] role=worker provider=codex model=gpt-5.5(parsed) effort=xhigh perm=write action="implement issue #218" exit=0 dur=42s tokens=2447/4461|6908 verified=true
{"role":"worker","provider":"codex","model":"gpt-5.5","model_source":"parsed","effort":"xhigh","permission":"write","action":"implement issue #218","exit_code":0,"started_at":"2026-06-29T00:00:00Z","ended_at":"2026-06-29T00:00:42Z","duration_ms":42000,"usage":{"input_tokens":2447,"output_tokens":4461,"total_tokens":6908},"verified":true}
{"ok":true,"issue":218,"branch":"loop/issue-218","run_id":"run-218","pr":"https://github.com/owner/repo/pull/999","summary":"Worker summary","attempt_path":".loopcoder/runs/run-218/workers/job-218-1.attempt.json","status":"succeeded","exit_code":0,"log_bytes":12345,"attestation":{"role":"worker","provider":"codex","model":"gpt-5.5","model_source":"parsed","effort":"xhigh","permission":"write","action":"implement issue #218","exit_code":0,"started_at":"2026-06-29T00:00:00Z","ended_at":"2026-06-29T00:00:42Z","duration_ms":42000,"usage":{"input_tokens":2447,"output_tokens":4461,"total_tokens":6908},"verified":true}}
```

The final non-empty stdout line remains the dispatch result JSON. Consumers
that need only the summary should parse the last line; conductors that need
Worker attestation can read either the local header or the nested
`attestation` object. The canonical JSON line is the exact machine rendering of
that same record and is not wrapped in Markdown on stdout.

`loopcoder dispatch` and `loopcoder loopreview` emit the human-readable pretty
attestation block to stderr by default. Foreground `loopcoder dispatch-wave`
streams one Worker pretty block per dispatched issue to stdout as that Worker
completes, before the final aggregate wave report. The default block uses emoji
on an interactive TTY and plain ASCII on a non-TTY.

The pretty block displays the provider vendor (`OpenAI`, `Anthropic`,
`Google`, or `Google Antigravity`) plus a separate `tool` line with the
provider key (`codex`, `claude`, `gemini`, or `antigravity`). It renders parsed
model sources as `(detected)` and self-reported sources as `(self-reported)`,
displays `started` and `ended` in the host local timezone to whole seconds,
reports duration as human seconds plus total seconds, and groups token counts
with thousands separators. When input and output tokens are present without a
total, the pretty display derives `total=<input+output>` for display only;
canonical JSON and the stable `[attestation]` header are unchanged.

`--pretty` or `LOOPCODER_PRETTY=1` forces the emoji form even on non-TTY
output. `--no-pretty` or `LOOPCODER_NO_PRETTY=1` suppresses the pretty block
and wins over any force or default setting. When pretty output is shown,
`NO_COLOR`, `LOOPCODER_PLAIN=1`, or `LOOPCODER_NO_EMOJI=1` forces the plain
ASCII form.

Pretty output is diagnostic local output only. It never appears between the
three `dispatch` stdout records, and it does not change `loopreview` verdict
JSON, canonical JSON, or the stable `Header()` / `[attestation] ...` contracts.
Together with result JSON and gitignored `.loopcoder/` run records, it is a
local attestation surface only. It is not copied into PR bodies, comments,
commits, merge commit bodies, merge comments, or other repository-visible
artifacts. The conductor must keep `dispatch` and `loopreview` stderr visible,
keep foreground `dispatch-wave` stdout visible, and relay each Worker or
Verifier pretty block verbatim for human reporting. The `relay` command group
is the explicit recovery surface: `relay flush` prints pending blocks verbatim
to stdout and clears them, while `relay list` inspects pending records without
clearing. `conductor-relay-guard` locally backstops hidden or suppressed
`dispatch`, `dispatch-wave`, and `loopreview` blocks where hooks are active.
Machine consumers should continue to parse local canonical JSON or stable
headers.

`loopcoder attest` is for Conductor self-attestation. It emits canonical JSON
followed by the one-line `[attestation] ...` header, forces `model_source` to
`self-reported`, and forces `verified` to `false` even if flags try to set other
values. It exits non-zero when required fields are missing or invalid,
including provider, model, action, timing, and usage. Provide either
`--total-tokens` or both `--input-tokens` and `--output-tokens`.

Keep the default `loopcoder attest` output for local machine-readable
Conductor attestation. Use `loopcoder attest --pretty` only for direct human
reading; it prints the pretty rendering to stdout instead of the canonical JSON
plus header. Conductor recovery after compaction or same-host session transfer
reads gitignored `.loopcoder/` run records and local command results, never
GitHub artifacts.

Pretty output uses emoji when the target is an interactive terminal, or when
emoji is forced, and emoji is not disabled:

```text
✅ attestation verified
   role        worker
   provider    OpenAI
   tool        codex
   model       gpt-5.5 (detected)
   effort      xhigh
   permission  write
   action      "implement issue #218"
   exit        0
   started     2026-06-30 14:25:21 JST
   ended       2026-06-30 14:33:15 JST
   duration    7m53.9s (473.9 s)
   tokens      total=165,268
   verified    true
```

Non-interactive default output, `NO_COLOR`, `LOOPCODER_NO_EMOJI=1`, or
`LOOPCODER_PLAIN=1` uses the plain ASCII form with the same fields:

```text
attestation: verified
  role        verifier
  provider    Anthropic
  tool        claude
  model       claude-opus-4-8[1m] (detected)
  effort      max
  permission  read-only
  action      "review PR #295"
  exit        0
  started     2026-06-30 14:33:51 JST
  ended       2026-06-30 14:35:58 JST
  duration    2m7.0s (127.0 s)
  tokens      input=2,447  output=9,844  total=12,291
  verified    true
```

Design rationale: [`../specs/0146-attestation.md`](../specs/0146-attestation.md),
[`../specs/0214-human-readable-attestation.md`](../specs/0214-human-readable-attestation.md),
[`../specs/0218-surface-worker-attestation.md`](../specs/0218-surface-worker-attestation.md),
[`../specs/0282-default-pretty-attestation.md`](../specs/0282-default-pretty-attestation.md),
[`../specs/0296-attestation-display-polish.md`](../specs/0296-attestation-display-polish.md),
[`../specs/0300-model-attribution.md`](../specs/0300-model-attribution.md),
[`../specs/0306-local-only-attestation.md`](../specs/0306-local-only-attestation.md),
and [`../specs/0316-conductor-local-enforcement.md`](../specs/0316-conductor-local-enforcement.md).

## Limits

loopcoder v1 is intentionally small-batch and single-session. It is meant for a
handful of issues in one open conductor session, not large unattended roadmaps.

State lives in GitHub plus local `.loopcoder/` run records rendered by
`loopcoder status` and the conductor's dependency DAG. If the session ends, a
later session can re-read GitHub state, but v1 does not provide a fully
stateless background conductor that automatically adopts orphaned workers. See
[`architecture.md`](architecture.md) for the current architecture and limits.
