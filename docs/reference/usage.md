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

3. Install the conductor playbook once per agent home so Claude Code and Codex
   know how to act as conductor.

   ```text
   loopcoder skill install
   ```

   This writes the bundled `SKILL.md` plus the Codex `AGENTS.md` entrypoint to
   the Claude Code loopcoder skill directory.

4. Initialize each consumer repository.

   ```text
   cd <repo>
   loopcoder init
   loopcoder doctor
   ```

   `loopcoder init` scaffolds `.delivery.yml`, `ROADMAP.md`, and the GitHub
   labels loopcoder uses. The follow-up `loopcoder doctor` confirms `git`, `gh`
   auth, provider CLIs, `origin`, and the default branch for that repository.

5. Drive the loop from a conductor session in the repository.

   ```text
   /loopcoder <your need>
   ```

   The conductor plans the work, dispatches workers, runs `loopreview`, and
   reports merge-ready pull requests. You remain the merge gate.

## Prerequisites

- A conductor host session that follows `SKILL.md`, `AGENTS.md`, or `GEMINI.md`.
- `git` on `PATH`.
- `gh` on `PATH`, authenticated for the target GitHub repository.
- At least one supported provider CLI on `PATH`. `codex` is the default worker,
  `codex` and `claude` are verified worker and verifier providers, and
  `gemini` is experimental and unverified end-to-end.
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
curl -fsSL https://raw.githubusercontent.com/jasonhnd/loopcoder/main/scripts/install.sh | sh -s -- --version 0.3.3
```

On Windows PowerShell:

```text
irm https://raw.githubusercontent.com/jasonhnd/loopcoder/main/scripts/install.ps1 | iex
```

To pin a version on Windows:

```text
$env:LOOPCODER_VERSION = "0.3.3"
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

Use the resolved binary for dispatch, ready-set scheduling, resume, recovery,
local verification, state, and lease operations.

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

Omitting these flags leaves model and effort absent so each provider inherits
its own global configuration.

## Per-Repo Setup

Use `loopcoder init` or manually add a `.delivery.yml` file at the repository
root. If it is absent, loopcoder uses the v1 defaults from the current design:
GitHub issues, git worktrees, the Codex worker adapter, GitHub
PRs/checks/merges, independent `loopreview` verification, human merge gating,
and chat reporting.

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
  gate: human-merge       # Humans choose what merges.
worker:
  # Optional. Absent = inherit the worker provider's global config. loopcoder never sets this on its own.
  # model:
  # Optional. Absent = inherit the worker provider's global config. loopcoder never sets this on its own.
  # reasoning_effort:
  base_branch: main
  command_hint: "implement the issue, run relevant checks, commit"
# verifier:
#   # Optional. Absent = inherit the verifier provider's global config. loopcoder never sets this on its own.
#   # model:
#   # Optional. Absent = inherit the verifier provider's global config. loopcoder never sets this on its own.
#   # reasoning_effort:
ci:
  checks: []
report:
  channel: chat
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
   `gh pr checks`, and reports progress, failures, risks, and final status in
   chat. `codex` and `claude` have real verifier smoke proof; ambiguous,
   malformed, timed-out, or incomplete verifier output is still reported as
   `needs-human`.

7. You name which PRs to merge. loopcoder merges only those named PRs by running
   `gh pr merge`, following `.delivery.yml` merge settings when present.

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
- conductor runtime responsibility, which remains user-provided by the active
  host.

Provider authentication is reported only where loopcoder has a stable cheap
probe. Today `doctor` checks `gh` authentication and provider CLI presence; it
does not invent provider-authentication status when the provider has no stable
probe.

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
loopcoder version
loopcoder --version
loopcoder -v

loopcoder doctor --repo .

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
  --pretty

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

## Attestation

Worker and verifier invocations carry binary-stamped attestation records with
`verified: true`, `model_source: parsed`, provider, real parsed model, effort,
permission, action, exit code, timing, and token usage. Missing required
identity or usage fails closed: `dispatch` opens no PR, and `loopreview`
returns `needs-human` with the incomplete-attestation finding.

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
Worker attestation can read either the header or the nested `attestation`
object. The canonical JSON line is the exact machine rendering of that same
record and is not wrapped in Markdown on stdout.

When `dispatch` runs in an interactive terminal, it also renders the same
validated Worker attestation in a human-readable form on stderr. This display
never appears between the three stdout records. Redirected or piped dispatch
runs emit no pretty output unless the caller opts in with `--pretty` or
`LOOPCODER_PRETTY=1`; opt-in pretty output still goes to stderr, and stdout
keeps the same three records.

`loopcoder loopreview` follows the same interactive display rule for the
Verifier attestation: the verdict JSON remains stdout, and any pretty
attestation display is stderr-only. Use `--pretty` or `LOOPCODER_PRETTY=1` to
request the display in non-interactive runs.

`loopcoder attest` is for Conductor self-attestation. It emits canonical JSON
followed by the one-line `[attestation] ...` header, forces `model_source` to
`self-reported`, and forces `verified` to `false` even if flags try to set other
values. It exits non-zero when required fields are missing or invalid,
including provider, model, action, timing, and usage. Provide either
`--total-tokens` or both `--input-tokens` and `--output-tokens`.

Keep the default `loopcoder attest` output for durable artifacts such as merge
comments, merge commit bodies, and PR notes. Use `loopcoder attest --pretty`
only for direct human reading; it prints the pretty rendering to stdout instead
of the canonical JSON plus header.

Interactive pretty output uses emoji when the target is an interactive terminal
and emoji is not disabled:

```text
✅ attestation verified
   role        worker
   provider    codex
   model       gpt-5.5 (source=parsed)
   effort      xhigh
   permission  write
   action      "implement issue #218"
   exit        0
   duration    42s (42000 ms)
   started     2026-06-29T00:00:00Z
   ended       2026-06-29T00:00:42Z
   tokens      input=2447 output=4461 total=6908
   verified    true
```

Non-interactive output, `NO_COLOR`, `LOOPCODER_NO_EMOJI=1`, or
`LOOPCODER_PLAIN=1` forces the plain ASCII form with the same fields:

```text
attestation: verified
  role        worker
  provider    codex
  model       gpt-5.5 (source=parsed)
  effort      xhigh
  permission  write
  action      "implement issue #218"
  exit        0
  duration    42s (42000 ms)
  started     2026-06-29T00:00:00Z
  ended       2026-06-29T00:00:42Z
  tokens      input=2447 output=4461 total=6908
  verified    true
```

Design rationale: [`../specs/0146-attestation.md`](../specs/0146-attestation.md),
[`../specs/0214-human-readable-attestation.md`](../specs/0214-human-readable-attestation.md),
and [`../specs/0218-surface-worker-attestation.md`](../specs/0218-surface-worker-attestation.md).

## Limits

loopcoder v1 is intentionally small-batch and single-session. It is meant for a
handful of issues in one open conductor session, not large unattended roadmaps.

State lives in GitHub plus the conductor's in-chat state table and dependency
DAG. If the session ends, a later session can re-read GitHub state, but v1 does
not provide a fully stateless background conductor that automatically adopts
orphaned workers. See [`architecture.md`](architecture.md) for the current
architecture and limits.
