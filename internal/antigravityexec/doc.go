// Package antigravityexec consolidates Antigravity invocation behind the minimal provider
// execution contract (V090-107 / #1153).
//
// One immutable request is translated into one bounded Antigravity launch evidence
// envelope. Discovery, quota, routing, and process supervision stay outside.
// Credentials remain with Antigravity auth; lifecycle/Git/GitHub writes are forbidden.
package antigravityexec

// TEST-ONLY / non-production: request-as-actual success is not product evidence.
// Production uses providerexec.AgentAdapter + agent.Runner. Import guard enforced.
