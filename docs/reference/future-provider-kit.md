# Future-provider registration kit (V090-049)

Packages: [`internal/providerkit`](../../internal/providerkit),  
[`internal/exampleprovider`](../../internal/exampleprovider)  
Issue: [#1159](https://github.com/jasonhnd/loopcoder/issues/1159)

## Purpose

Prove a fourth/fifth company can add an adapter through the provider contract
**without** editing scheduler, store schema, direct-run lifecycle, or route engine.

## Rules

1. Explicit **allowlist** registration only — no arbitrary executable discovery  
2. **No auto-load** from customer repositories  
3. Contract version must be in supported range (v1 only in v0.9)  
4. Synthetic `example` provider is **test-only**  
5. User-installable provider packs are **deferred** (need signing/trust/update design)

## Checklist (required)

auth_ownership, source_authority, bounds, redaction, model_identity,
quota_semantics, actual_route_proof, cancellation, child_cleanup, contract_version

## Verification

```bash
go test ./internal/providerkit/ ./internal/exampleprovider/
```
