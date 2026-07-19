# Release provider canaries

Protected Codex and Claude canaries produce sanitized evidence against a
specific macOS arm64 candidate. They are release blockers for v0.8.1; they do
not run on pull requests or forked repositories.

## Harness

| Item | Path |
| --- | --- |
| Runner | [`scripts/release-provider-canary.sh`](../../scripts/release-provider-canary.sh) |
| Local tests | [`scripts/release-provider-canary_test.sh`](../../scripts/release-provider-canary_test.sh) |
| Workflow | [`.github/workflows/release-provider-canary.yml`](../../.github/workflows/release-provider-canary.yml) |

## Modes

### Fixture

Deterministic scenarios with no paid provider calls:

- `success`, `auth_failure`, `quota_failure`, `timeout`, `malformed_output`,
  `cancel`, `missing_cli`

Run:

```bash
bash scripts/release-provider-canary_test.sh
bash scripts/release-provider-canary.sh --mode fixture --provider codex --scenario success --artifact-dir /tmp/canary
```

### Live

Requires:

1. Explicit operator approval: `LOOPCODER_REAL_PROVIDER_CANARY=1`
2. Trusted runner with authenticated `codex` and `claude` CLIs
3. Packaged `darwin/arm64` candidate binary
4. GitHub environment `release-canary` (workflow live jobs)

Each provider runs independently with `max-calls=1`, concurrency 1, and no
cross-provider fallback. Evidence never includes credentials, prompts, raw
provider output, account IDs, or personal filesystem paths.

```bash
LOOPCODER_REAL_PROVIDER_CANARY=1 bash scripts/release-provider-canary.sh \
  --mode live \
  --provider codex \
  --binary ./loopcoder \
  --expected-digest <sha256> \
  --candidate-sha <git-sha> \
  --artifact-dir ./canary-evidence \
  --timeout-seconds 180 \
  --max-calls 1
```

## Workflow dispatch

- `mode=fixture` — always safe; runs the harness self-test and scenario matrix.
- `mode=live` — builds one candidate, then runs Codex and Claude canaries under
  environment `release-canary`. Both must pass; a failure of one does not count
  as success for the other.

## Evidence schema

`loopcoder.release_provider_canary.v1` JSON per provider, including status,
result class, detail code, resolved model (when known), adapter mode, receipt
delivery flag, digests, timestamps, hard limits, and redaction flags.
