// Package reportsched implements the five-minute status receipt scheduler and
// no-progress policy (V090-020 / #1112).
//
// Uses an injected clock, persists next-due times, deduplicates wakes across
// restart, and after two consecutive no-progress intervals emits a single
// documented no-progress action. Waiting performs zero provider calls and no
// busy loops.
package reportsched
