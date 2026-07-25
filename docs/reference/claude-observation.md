# Claude Code discovery and verified-subset catalog observation

Package: [`internal/claudeobs`](../../internal/claudeobs)  
Issues: [#1146](https://github.com/jasonhnd/loopcoder/issues/1146),
[#1417](https://github.com/jasonhnd/loopcoder/issues/1417)

## Purpose

Consolidate Claude Code observation (install, version, auth, model catalog,
aliases) behind `providerdesc` + `obsplan`. Same guarantees as Codex observation:
no launch, no credentials, no silent default models.

Ordinary `providers refresh` remains observation-only and never launches a paid
Claude model. Static full IDs and aliases are candidate hints, not authoritative
routes.

When a run needs account-scoped Claude model evidence, the operator may invoke
the explicit paid bounded probe:

```bash
loopcoder providers verify-claude-model \
  --repo . \
  --project-id <opaque-project-id> \
  --model sonnet \
  --effort low
```

The probe:

- resolves one exact Claude executable and observes `claude auth status --json`;
- reserves a bounded token budget before the provider call;
- runs official print mode in a disposable directory with safe mode, no tools,
  no slash commands, strict empty MCP configuration, and no session persistence;
- accepts only an adapter-declared candidate and records only the exact model
  identity returned by `modelUsage`;
- re-observes the same account after execution, records exact allowlisted token
  usage, commits actual usage, and releases the unused reservation;
- persists only opaque account/install IDs, timestamps, raw SHA-256 digests,
  bounded usage fields, and the verified model/depth receipt.

Prompt text, result text, session IDs, email addresses, tokens, cookies, raw
provider output, and credential material are never durable output. A failed,
expired, mismatched, malformed, partial, or static-only observation does not
create a production route. When a paid attempt cannot yield exact usage,
LoopCoder commits the reserved-token upper bound as an explicitly estimated
failure usage record instead of reporting zero spend. Exact provider-reported
usage with an unconfirmed post-auth linkage is retained with estimated linkage
confidence and remains unroutable.

## Verification

```bash
go test ./internal/claudeobs/ ./internal/claudecatalog/ \
  ./internal/providerinventory/ ./internal/capacitysnapshot/
```
