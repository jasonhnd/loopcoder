# Provider descriptor SPI and registry (V090-037)

Package: [`internal/providerdesc`](../../internal/providerdesc)  
Issue: [#1140](https://github.com/jasonhnd/loopcoder/issues/1140)

## Purpose

One versioned provider **descriptor + registry + conformance** surface so official
and future adapters share discovery, auth status, catalog, quota, invoke, and
diagnostics without each package inventing a probe format.

## Descriptor

Schema: `loopcoder.provider.descriptor.v1` (version **1**)

| Field | Role |
| --- | --- |
| `adapter_id` | Stable lower-case ID |
| `operations` | Claimed ops only |
| `unsupported` | Explicit non-support |
| `identity` | Install/account markers (no paths/tokens) |
| `probe_plans` | Bounded named probes |
| `models` | Catalog entries (requires `catalog` op) |

## Observation envelope

Schema: `loopcoder.provider.observation.v1`

Shared by discover / auth_status / catalog / quota / invoke / diagnose:

- `provenance` (source, observed_at, freshness)
- `confidence`
- typed `diagnostic` (`missing_install`, `auth_unknown`, `malformed_output`, `timeout`, `rate_limit`, `unsupported_operation`)
- redacted `payload` map

## Registry rules

- Duplicate `adapter_id` → reject; no second entry  
- Incompatible descriptor version → reject; **empty** registry side effect  
- Capability mismatch (e.g. models without `catalog`) → reject  
- Credential / route / lifecycle / GitHub keys in observe input → boundary error  
- Unclaimed operation → `unsupported_operation`  

## Adapter boundary

Adapters **must not**:

- read/write route decisions (`routepin` policy stays outside)
- mutate project lifecycle
- perform GitHub delivery
- accept or emit raw credentials

## Conformance

`RunConformance` covers: success ops, missing install, auth unknown, malformed,
timeout, rate limit, unsupported op, duplicate ID, bad version, credential
boundary, capability mismatch. Bounded call count; no network.

## Existing package disposition

See `ExistingProviderInventory()` — e.g. `providerexec` **wrap** for invoke,
`routepin` **keep_aux**, smoke `provider` **retire** into conformance.

## Verification

```bash
go test ./internal/providerdesc/
```
