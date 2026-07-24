// Package geminiexec consolidates Gemini CLI invocation behind the minimal provider
// execution contract (V090-105 / #1150).
//
// One immutable request is translated into one bounded Gemini CLI launch evidence
// envelope. Discovery, quota, routing, and process supervision stay outside.
// Credentials remain with Gemini CLI auth; lifecycle/Git/GitHub writes are forbidden.
package geminiexec

// TEST-ONLY / non-production: request-as-actual success is not product evidence.
// Production uses providerexec.AgentAdapter + agent.Runner. Import guard enforced.
