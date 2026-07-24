// Package codexexec consolidates Codex invocation behind the minimal provider
// execution contract (V090-103 / #1144).
//
// One immutable request is translated into one bounded Codex launch evidence
// envelope. Discovery, quota, routing, and process supervision stay outside.
// Credentials remain with Codex auth; lifecycle/Git/GitHub writes are forbidden.
package codexexec

// TEST-ONLY / non-production: request-as-actual success is not product evidence.
// Production uses providerexec.AgentAdapter + agent.Runner. Import guard enforced.
