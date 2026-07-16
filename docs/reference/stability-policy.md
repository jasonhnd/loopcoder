# Stability Policy

This living reference describes loopcoder's 0.x compatibility policy for
project configuration, documented commands, and documented GitHub labels.

Current public release: `0.8.0`.

The v0.8.0 tag, Sigstore-signed checksums, single Darwin arm64 archive, staged
smoke, publication approval, and completed go/no-go record are public. Those
facts establish release identity and integrity, not support for every internal
contract. The binding product-path status is the
[`v0.8.0 capability and support matrix`](v0.8.0-capability-matrix.md): v0.8.0
is for controlled canary and development use, not unattended end-to-end
production orchestration.

v0.8.0 supports native macOS Apple Silicon (`darwin/arm64`) only. Windows,
Linux/Ubuntu, WSL, containers used as a LoopCoder runtime, Intel macOS, and
Rosetta/amd64 macOS are unsupported runtime, install, upgrade, CI, smoke, and
release tuples. v0.7.0 remains the final legacy multi-platform release; users
who require those hosts should stay on v0.7.0 or contribute to a separately
approved future platform roadmap. Frozen v0.7.0 release notes, artifacts, and
go/no-go evidence remain historical truth and are not current v0.8 promises.

## 0.x Stability Promise

Within the 0.x release line, patch releases preserve compatibility for:

- documented `.delivery.yml` schema fields;
- documented CLI flags;
- documented GitHub label names, including dependency labels such as
  `blocked-by:#N`.
- documented provider keys and static `loopcoder models` output semantics.

Patch releases may add optional fields, flags, labels, checks, diagnostics, or
bug fixes. They must not remove, rename, or change the meaning of an existing
documented field, flag, provider key, model/depth default, or label in a way
that breaks working projects.

Breaking compatibility requires a minor release. The minor release must include
migration guidance and `loopcoder doctor` output that explains what changed and
what the project or user must do next. A removed or renamed field, flag, or
label must not fail silently.

## Compatibility Signals

`.delivery.yml` is the project configuration contract. Its required `version`
field identifies the configuration schema version:

```yaml
version: 1
```

The planned optional `min_loopcoder_version` field declares the minimum
loopcoder binary version that can safely operate on the project:

```yaml
version: 1
min_loopcoder_version: 0.3.0
```

`version` answers "which config schema is this project using?"
`min_loopcoder_version` answers "how new must the selected loopcoder binary be?"
Together they let a project reject unsupported combinations before dispatch,
review, recovery, or merge work starts.

Role-scoped model and depth configuration is also part of the documented
project configuration contract:

```yaml
worker:
  model: gpt-5.5
  reasoning_effort: high
verifier:
  model: "claude-opus-4-8[1m]"
  reasoning_effort: max
models:
  strict: true
```

Absent `worker.model`, `worker.reasoning_effort`, `verifier.model`, or
`verifier.reasoning_effort` fields resolve at runtime from the static model
registry: provider default model first, then the resolved model's default depth.
`models.strict: true` changes invalid model/depth diagnostics from warnings to
command-blocking rejections. Patch releases may add new registry rows or
optional depth tokens, but they must not silently change the meaning of an
existing documented provider/model/depth token.

## Doctor Behavior

`loopcoder doctor` is the compatibility reporting surface. It should report the
selected binary path and version, selected track, embedded and installed
playbook versions when applicable, `.delivery.yml` schema version,
`min_loopcoder_version` compatibility, model/depth selection diagnostics,
provider CLI readiness, and the final compatibility result.
In v0.6.1, `doctor --format json` also exposes the ordered check list for host
tools, including local `.loopcoder/` exclude protection, tracked local-state
risk, reportquery readability, installed skill freshness, and project hook
wiring.
Since v0.7.0, `doctor --format json` also exposes the machine-local runtime
health surface used by release smoke: `runtime.database`,
`runtime.project_registry`, `runtime.migration`, `runtime.nested_runs`,
`host_profile`, and the `provider_compatibility[]` matrix. Release checks may
accept `experimental` provider compatibility only for documented host-profile
fallbacks such as `generic-local`; `unsupported` remains a release-blocking
signal for selected Worker and Verifier roles.

For v0.8.0, the startup platform gate precedes storage, provider, credential,
network, repository, and migration initialization. Unsupported hosts return
exit code `78` and the stable `ErrUnsupportedPlatform` human or JSON diagnostic
with `side_effects_performed: false`. On the supported host, doctor also
reports durable process authority, detached-run health, progress/outbox state,
provider inventory, and quota evidence without inventing unavailable provider
facts.

If the selected binary is too old, the schema version is unsupported, or a known
field, flag, provider key, model/depth token, or label has been removed or
renamed, `doctor` should explain the incompatibility and point to the relevant
migration guidance. Incompatible configuration or automation must produce an
explicit diagnostic instead of a silent fallback. When provider `antigravity`
is configured, doctor checks executable `agy` through bounded installation
inventory only; authentication readiness, model authorization, quota, and usable
capacity must remain separate diagnostics.

## CHANGELOG Discipline

Every release updates [`CHANGELOG.md`](../../CHANGELOG.md). The changelog uses
Keep a Changelog sections and SemVer so users can distinguish patch-compatible
fixes from minor releases that require migration work.

The release runbook in spec 0212 treats prepared changelog or release-note
material as part of release readiness. A release that changes documented
configuration, flags, labels, install behavior, upgrade behavior, or migration
requirements must record that change in the changelog.

## Design References

- [`0212-release-distribution-and-upgrade.md`](../specs/0212-release-distribution-and-upgrade.md):
  release, upgrade, `min_loopcoder_version`, `doctor`, and 0.x compatibility
  policy rationale.
- [`0215-per-role-model-override.md`](../specs/0215-per-role-model-override.md):
  role-scoped `.delivery.yml` model and depth fields that must follow this
  stability policy once documented.
- [`0554-model-depth-selection.md`](../specs/0554-model-depth-selection.md):
  static model registry, `loopcoder models`, strict validation, and
  Antigravity provider setup.
- [`0884-macos-arm64-only.md`](../specs/0884-macos-arm64-only.md): binding
  v0.8.0 platform, installer, upgrade, CI, release, and historical-evidence
  contract.
- [`v0.8.0-capability-matrix.md`](v0.8.0-capability-matrix.md): binding
  post-publication capability, reachability, evidence, and support status.
- [`v0.8.0-go-no-go.md`](v0.8.0-go-no-go.md): completed publication gates and
  historical evidence record.
- [`v0.7.0-go-no-go.md`](v0.7.0-go-no-go.md): release-readiness evidence and
  completed historical gate record for v0.7.0.
