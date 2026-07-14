# Release Regression Fixtures

These fixtures are the reviewed v0.8.0 routing, handoff, and agent-federation
evaluation evidence for issue #866.

- `matrix.json` maps issue #714 release invariants to named deterministic
  scenarios.
- `*.canonical.json` files are exact canonical JSON bytes from
  `CanonicalResultJSON`.
- `*.human.txt` files are the stable redacted human decision evidence from
  `HumanDecisionEvidence`. They are reviewed as exact bytes and must remain LF
  only on checkout.

Ordinary `go test` runs must never rewrite these files. For an intentional
fixture or policy update, change `fixture_schema_version` or
`policy_schema_version` as appropriate. Update mode refuses to rewrite existing
evidence when those versions are unchanged. Then run:

```sh
LOOPCODER_UPDATE_RELEASE_REGRESSION_GOLDENS=1 go test ./internal/evaluation/simulation
```
