# Release provider canaries

Protected provider canaries produce sanitized evidence against a specific
macOS arm64 candidate. They do not run on pull requests or forked repositories.

## Provider classes

| Provider | CLI | Blocking for v0.8.1? | Notes |
| --- | --- | --- | --- |
| Codex | `codex` | **Yes** (#1020) | Required live canary for release GO |
| Claude | `claude` | **Yes** (#1020) | Required live canary for release GO |
| Grok | `grok` | **No** (#1021) | Opt-in capacity evidence; missing CLI/auth → `not_available` |
| Antigravity | `agy` | **No** (#1021) | Separate adapter; read-only nested canary may be `not_available` |

Providers are never aliased to each other. A failure or absence of Grok/Antigravity
cannot delay the blocking Codex/Claude release gate.

## Harness

| Item | Path |
| --- | --- |
| Runner | [`scripts/release-provider-canary.sh`](../../scripts/release-provider-canary.sh) |
| Local tests | [`scripts/release-provider-canary_test.sh`](../../scripts/release-provider-canary_test.sh) |
| Workflow | [`.github/workflows/release-provider-canary.yml`](../../.github/workflows/release-provider-canary.yml) |

## Modes

### Fixture

Deterministic scenarios with no paid provider calls:

- Blocking: `success`, `auth_failure`, `quota_failure`, `timeout`,
  `malformed_output`, `cancel`, `missing_cli` (hard exit non-zero on failures)
- Non-blocking extras: `not_available`, `model_unavailable`; missing CLI/auth
  and unknown quota exit **0** with `status: not_available` (never fabricated
  zero quota)

```bash
bash scripts/release-provider-canary_test.sh
bash scripts/release-provider-canary.sh --mode fixture --provider grok --scenario missing_cli --artifact-dir /tmp/canary
```

### Live

Requires:

1. Explicit operator approval: `LOOPCODER_REAL_PROVIDER_CANARY=1`
2. Trusted runner with the relevant authenticated CLIs
3. Packaged `darwin/arm64` candidate binary
4. GitHub environment `release-canary`

Each provider runs independently with `max-calls=1`, concurrency 1, and no
cross-provider fallback. Evidence never includes credentials, prompts, raw
provider output, account IDs, or personal filesystem paths.

```bash
LOOPCODER_REAL_PROVIDER_CANARY=1 bash scripts/release-provider-canary.sh \
  --mode live \
  --provider grok \
  --binary ./loopcoder \
  --expected-digest <sha256> \
  --candidate-sha <git-sha> \
  --artifact-dir ./canary-evidence \
  --timeout-seconds 180 \
  --max-calls 1
```

## Workflow dispatch

- `mode=fixture` — safe harness self-test + scenario matrix (blocking + non-blocking).
- `mode=live` — builds one candidate; **requires** Codex and Claude; optionally
  runs Grok/Antigravity (`include_non_blocking`, default true). Summary
  **only** fails on blocking providers.

## Evidence schema

`loopcoder.release_provider_canary.v1` JSON per provider:

- `blocking` — `true` for codex/claude, `false` for grok/antigravity
- `status` — `passed` | `failed` | `not_available`
- result class, detail code, resolved model (when known), adapter mode,
  receipt delivery, digests, timestamps, hard limits, redaction flags
- `fallback_provider` is always `null`
