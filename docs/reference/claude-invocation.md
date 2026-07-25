# Claude Code invocation consolidation (V090-104)

Package: [`internal/claudeexec`](../../internal/claudeexec)  
Issue: [#1147](https://github.com/jasonhnd/loopcoder/issues/1147)

## Purpose

Translate one immutable `providerexec.Request` into one bounded Claude Code
command plan and normalized outcome. Discovery stays in `claudeobs`.

For account-bound production routing, the runner resolves the same Claude
executable represented by `InstallRef`, executes `auth status --json` in a
bounded credential-blind preflight, and requires the resulting opaque
`AccountProfileID` to equal the selected `AccountRef`. It repeats the observation
after execution and fails closed on drift. Model identity comes from the Claude
stream; effort may be affirmed only from an exact accepted `--effort` option
after full success.

The separate paid verified-subset catalog probe is documented in
[`claude-observation.md`](claude-observation.md). It is never an implicit part of
ordinary discovery.

## Verification

```bash
go test ./internal/claudeexec/
```
