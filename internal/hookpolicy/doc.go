// Package hookpolicy implements customer Git-hook policy and bounded
// reconciliation (V090-096 / #1132).
//
// Hooks are respected by default. Bypass requires explicit immutable
// authorization. Hook output is untrusted; timeouts trigger read-back, never
// provider/worker replay. No silent --no-verify from recovery or agent prose.
package hookpolicy
