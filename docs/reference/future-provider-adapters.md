# Future Provider Adapter Declarations

LoopCoder v0.8 provider adapters are trusted, versioned declarations plus
LoopCoder-owned parser and runner code. They are not binary plugins, and they
do not authorize untrusted third-party code execution. A new provider must be
registered through the runtime capability contract and pass the provider
inventory conformance suite before planner, router, or scheduler code may use
its inventory facts.

## Declaration Fields

Every adapter declaration must define:

| Field | Requirement |
| --- | --- |
| `adapter_id` | Stable provider key. It must be a safe command-style identifier with no path separators. |
| `adapter_version` | Version of this declaration. Any breaking contract change increments it. |
| `declaration_schema_version` | Currently `loopcoder.adapter_declaration.v1`. Unknown schemas fail closed. |
| `display_name` / `vendor` | Human-readable labels for local output. |
| `executable_names` | Command names only, never absolute paths. User configured executable paths are separate local configuration. |
| `version_argv` | Fixed argv suffix for install/version probes, usually `--version`. |
| `auth_readiness_contract` | One credential-blind mechanism: bounded status command, auth artifact existence, environment-name existence, or explicit unsupported reason. |
| `catalog_contract` | Static model entries and optional bounded catalog command with source provenance. Network-capable catalog commands must declare that fact. |
| `invocation_profile` | Provider-neutral capabilities: read-only, JSON output, MCP config, cancellation, usage reporting, and nested sub-agent support. |
| `unsupported_operations` | Missing capabilities with typed reasons and actionable suggestions. |
| `conformance_version` | The conformance suite version the declaration passed. |

Example minimal partial adapter:

```go
runtimecap.ProviderRuntime{
    Name:                  "example",
    AdapterVersion:        "v1",
    DisplayName:           "Example",
    Vendor:                "Example Vendor",
    Executable:            "example",
    VersionArgv:           []string{"--version"},
    AuthUnsupportedReason: "example has no credential-blind status command",
    Cancellation:          true,
    StaticModelCatalog: []runtimecap.ProviderModelCapability{{
        ModelID:            "example-model",
        Cancellation:       true,
        AvailabilityState:  "available",
        LifecycleState:     "available",
    }},
}
```

## Security Obligations

Adapters receive typed, least-privilege inputs such as `ProbeExecution`; they
do not receive arbitrary project or global state. Probes must use fixed argv
arrays, no shell interpolation, bounded environment allowlists, timeouts, byte
limits, and local-read side effects unless a later policy explicitly grants
more.

Auth readiness must be credential-blind. An adapter may record file existence,
environment variable name existence, or provider status fields declared as
non-secret. It must never read credential bytes, serialize environment values,
parse token caches, run login or refresh commands, scrape private provider UI,
or persist raw provider output.

Provider output is untrusted. Parser code must reject malformed schemas,
preserve `unknown` for ambiguous evidence, redact before persistence, and map
timeouts, cancellation, malformed output, unsupported operations, and schema
mismatch to typed gap reasons.

## Compatibility Lifecycle

Non-breaking declaration changes may keep the same `adapter_version` only when
old records remain valid and conservative. Breaking changes require a new
`adapter_version`, tests proving the new adapter declaration ID changes, and
migration or fail-closed behavior for old records.

Adding a fourth provider must require declaration/registration and any parser
code behind the adapter boundary only. It must not require provider-name
branches in planner, router, scheduler, task lifecycle, DeliveryRun approval,
or routing-policy core.

Before a provider is considered conformant, run the provider inventory
conformance tests against it, including partial implementation, malformed
output, timeout, cancellation, redaction, unsupported operations, schema
mismatch, and declaration version upgrade coverage.
