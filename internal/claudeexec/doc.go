// Package claudeexec consolidates Claude Code invocation behind the minimal provider
// execution contract (V090-104 / #1147).
//
// One immutable request is translated into one bounded Claude Code launch evidence
// envelope. Discovery, quota, routing, and process supervision stay outside.
// Credentials remain with Claude Code auth; lifecycle/Git/GitHub writes are forbidden.
package claudeexec
