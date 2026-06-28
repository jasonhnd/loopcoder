---
id: 146
title: Per-Invocation Attestation for Worker, Verifier, and Conductor
status: draft
date: 2026-06-28
issue: 146
pr: null
supersedes: []
superseded_by: []
---

# Per-Invocation Attestation for Worker, Verifier, and Conductor

This is a design-only spec. This PR must add only this document: no Go code, no
new `loopcoder attest` command, no adapter changes, no structs, and no hooks.
Those implementation changes belong in separate code issues after this spec is
reviewed and merged per [`docs/PROCESS.md`](../PROCESS.md).

## Goal

Every model invocation in loopcoder must declare, in a program-forced record
attached to every returned result, three things:

- **Who it is:** provider, actual model, and intelligence depth or effort.
- **What permission it ran with:** read-only, write, or orchestrate.
- **What it did:** action, exit code, duration, and token usage.

The concept is named **attestation**, not provenance. Provenance describes
lineage or origin. loopcoder needs an emitted, verifiable, per-invocation
statement of who did what under what conditions, similar in spirit to SLSA or
in-toto attestations.

## Decisions

1. **Coverage is all roles.** Worker, Verifier, and Conductor invocations all
   emit attestation records.
2. **Model truthfulness is role-specific.** For Worker and Verifier runs, the
   binary parses the actual model and usage from each provider's real output.
   For Conductor runs, the host session self-attests its own model. loopcoder
   still never auto-selects a model; the inherit-by-default rule stays.
3. **There is one binary-owned format.** All roles use one
   `AttestationRecord`. Worker and Verifier records are stamped by the binary.
   Conductor records are emitted through a future `loopcoder attest`
   subcommand, enforced by a host hook.
4. **Strictness is hard-fail.** If a required field cannot be established, the
   result must not be delivered. A Worker opens no PR, a Verifier returns
   `needs-human`, and Conductor `attest` exits non-zero so the host hook keeps
   blocking. Token usage is required.
5. **The trust marker is explicit.** `verified: true` means the binary parsed
   evidence from real provider output for Worker or Verifier. `verified: false`
   means the Conductor self-attested. The record visibly separates hard evidence
   from self-claim.
6. **Conductor attestation survives context loss.** The Conductor attestation is
   stamped into the artifacts it produces, such as commit, PR, or merge
   artifacts, not only chat. A later Conductor after compaction must be able to
   recognize its own prior work from those artifacts.

## AttestationRecord Schema

`AttestationRecord` is the single schema for every role.

| Field | Required meaning |
|---|---|
| `role` | `worker`, `verifier`, or `conductor`. |
| `provider` | Provider that produced or hosted the invocation. |
| `model` | Actual model used for the invocation. |
| `model_source` | `parsed` for binary-parsed evidence, or `self-reported` for Conductor self-attestation. |
| `effort` | Intelligence depth, reasoning effort, or provider-equivalent effort value. |
| `permission` | `read-only`, `write`, or `orchestrate`. |
| `action` | Human-meaningful action performed by this invocation. |
| `exit_code` | Process exit code, or the equivalent command result for self-attested Conductor records. |
| `started_at` | Invocation start timestamp. |
| `ended_at` | Invocation end timestamp. |
| `duration_ms` | Duration in milliseconds. |
| `usage` | Token usage. Prefer input and output token counts; codex may provide total-only usage. |
| `verified` | Boolean trust marker: `true` for binary-parsed Worker/Verifier records, `false` for self-attested Conductor records. |

The schema has two renderings:

- Canonical JSON for machines and durable artifacts.
- A one-line human header for PR bodies, verifier verdicts, and chat.

The human header replaces bare role markers such as `worker: <provider>` so the
visible artifact states who ran, with what permission, and what was done.

## Per-Provider Facts

These facts were verified on this host and are the evidence basis for future
implementation issues:

- `claude --print --output-format json` reports the model under the
  `modelUsage` key, reports `usage` with input and output tokens, and reports
  duration and cost. It is verified-capable.
- `codex exec` reports `model`, `reasoning effort`, and `tokens used` in a
  stdout header. Token usage is total-only rather than split into input and
  output, and the output is not JSON.
- `gemini` is currently unusable headlessly on this host because auth is
  missing. Under hard-fail strictness, that breakage is surfaced explicitly
  instead of hidden; real gemini attestation waits on the auth fix.

## Mechanisms Per Role

### Worker

Worker invocations are binary-spawned, headless, and write-capable.

The future implementation extends `agent.Result` and `worker.Result` to carry
`AttestationRecord`. The binary stamps the record from real provider output,
sets `permission: write`, sets `verified: true`, and refuses delivery if any
required field, including token usage, cannot be established.

A Worker that cannot produce a valid attestation opens no PR. When it can
produce one, the PR body uses the one-line attestation header instead of the
bare `worker: <provider>` line.

### Verifier

Verifier invocations are binary-spawned, headless, and read-only.

The future implementation extends `loopreview.Verdict` to carry
`AttestationRecord`. The binary stamps the record from real provider output,
sets `permission: read-only`, sets `verified: true`, and refuses to return a
normal pass/fail verdict when any required field, including token usage, cannot
be established.

A Verifier with an incomplete attestation returns `needs-human`. When it can
produce a complete attestation, the verifier verdict includes the one-line
attestation header.

### Conductor

The Conductor is a human-launched host session, not a binary-spawned headless
agent. Its permission is `orchestrate`.

The future implementation adds a `loopcoder attest` command that formats the
same `AttestationRecord` schema for the Conductor, for example:

```text
loopcoder attest --role conductor --provider <provider> --model <model> --permission orchestrate --action "<action>"
```

A per-host hook for Claude Code, Codex, or Gemini blocks the turn until
`loopcoder attest` has been called, following the same gate-guard style as other
host-enforced checks. Because the Conductor self-reports from its host session,
the record uses `model_source: self-reported` and `verified: false`.

The Conductor record must be stamped into artifacts the Conductor produces,
including commit, PR, or merge artifacts where applicable, so the attestation is
available after context compaction or session transfer.

## Process

This issue is only the design-doc unit. The accepted deliverable is the new
spec file at `docs/specs/0146-attestation.md`.

After this document is reviewed and merged, separate code issues implement the
binary schema, Worker stamping, Verifier stamping, `loopcoder attest`, provider
parsers, host hooks, and artifact stamping. Those code issues must reference
this merged spec and follow [`docs/PROCESS.md`](../PROCESS.md).

## Non-Goals

- No secret or auth management inside loopcoder.
- No autonomous Conductor tick. The 0.4.0 autonomous line remains on hold per
  issue #160.
- No Go implementation in this design-doc PR.
- No new `loopcoder attest` command in this design-doc PR.
- No adapter, struct, hook, Worker, Verifier, or Conductor behavior changes in
  this design-doc PR.
