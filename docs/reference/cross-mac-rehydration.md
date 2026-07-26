# Cross-Mac GitHub rehydration after terminal handoff (V090-068)

Package: [`internal/rehydrate`](../../internal/rehydrate)  
Issue: [#1180](https://github.com/jasonhnd/loopcoder/issues/1180)

## Purpose

Let the owner finish issue/PR work on Mac A and continue later on Mac B by
reading GitHub and creating **fresh local state**. Mac B must not copy or merge
Mac A SQLite files, local leases, process identity, or in-flight scheduler state.

## Inputs

- **Remote evidence** (normalized): repo (owner/name/visibility), issue, PR,
  commits, checks, reviews, route evidence references.
- **Explicit local checkout** on Mac B (path, optional head SHA/branch).
- **Machine id** of the receiving Mac.

No foreign machine database or state branch is read.

## Delivery states

| State | Terminal? | Rehydrate? |
| --- | --- | --- |
| `merged` | yes | yes |
| `closed` | yes | yes |
| `delivered` | yes | delivery-only continuation |
| `gated` | yes | yes (human gate) |
| `in_flight` | no | **rejected** — not a live local attempt |
| `ambiguous` | no | **rejected** — explicit reconciliation |

Conflicting signals (e.g. `merged=true` with `state=open`) classify as
`ambiguous`.

## Behavior

1. Validate repo owner/name and fail closed on unknown visibility.
2. Require an explicit local checkout path.
3. Classify remote delivery state; reject in-flight and ambiguous.
4. Reject any attempt to adopt a foreign machine's in-flight process/attempt id.
5. Report remote/local divergences (head SHA, branch) **before** mutation.
6. Create or reuse a stable project identity keyed by `owner/name` (not short
   name alone); isolate visibility changes.
7. Append a local rehydration event referencing remote identities and route
   evidence refs; issue a **new** local execution identity for Mac B.
8. Repeating the same evidence fingerprint is **idempotent** (no second project).

## Isolation

- Same short name, different owner → distinct projects.
- Private/public visibility flip on an existing project → fail closed until
  explicit reconciliation.
- Separate `Store` instances (Mac B vs Mac C) share nothing.

## Boundaries (honest)

- No database sync, Dolt, or distributed lease.
- No simultaneous same-attempt work across machines.
- PR CI uses isolated home fixtures and fake GitHub histories; no physical
  second Mac required. Release consumer smoke on two Macs is optional later.

## Verification

```bash
go test ./internal/rehydrate/
go test ./internal/rehydrate/ -run TestHandoffCanary -count=1
```
