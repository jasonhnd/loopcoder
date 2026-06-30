# Stability Policy

This living reference describes loopcoder's 0.x compatibility policy for
project configuration, documented commands, and documented GitHub labels.

Current stable release: `0.3.5`.

## 0.x Stability Promise

Within the 0.x release line, patch releases preserve compatibility for:

- documented `.delivery.yml` schema fields;
- documented CLI flags;
- documented GitHub label names, including dependency labels such as
  `blocked-by:#N`.

Patch releases may add optional fields, flags, labels, checks, diagnostics, or
bug fixes. They must not remove, rename, or change the meaning of an existing
documented field, flag, or label in a way that breaks working projects.

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

## Doctor Behavior

`loopcoder doctor` is the compatibility reporting surface. It should report the
selected binary path and version, selected track, embedded and installed
playbook versions when applicable, `.delivery.yml` schema version,
`min_loopcoder_version` compatibility, and the final compatibility result.

If the selected binary is too old, the schema version is unsupported, or a known
field, flag, or label has been removed or renamed, `doctor` should explain the
incompatibility and point to the relevant migration guidance. Incompatible
configuration or automation must produce an explicit diagnostic instead of a
silent fallback.

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
  role-scoped `.delivery.yml` model and effort fields that must follow this
  stability policy once documented.
