// Package directcanary implements documentation and Go-code visible direct-path
// canaries (V090-036 / #1138).
//
// It proves the complete explicit-route product path in two disposable consumer
// repositories (docs-only and small-go) using the real P3 stage packages with
// deterministic fake provider/GitHub. No network, no real model calls, and no
// LoopCoder self-bootstrap.
//
// Scenarios cover success, worker failure, push timeout resume, UI reconnect,
// cancellation, changed PR head, and delivery-only resume. Evidence manifests
// are redacted and tied to the tested SHA; customer repos must leave zero
// LoopCoder runtime residue.
package directcanary
