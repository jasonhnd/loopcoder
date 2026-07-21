// Package grokexec consolidates Grok invocation behind the minimal provider
// execution contract (V090-105 / #1150).
//
// One immutable request is translated into one bounded Grok launch evidence
// envelope. Discovery, quota, routing, and process supervision stay outside.
// Credentials remain with Grok auth; lifecycle/Git/GitHub writes are forbidden.
package grokexec
