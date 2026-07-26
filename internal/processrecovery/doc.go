// Package processrecovery reconciles persisted attempt/process evidence with
// the OS on LoopCoder restart (V090-018 / #1110).
//
// Only an exactly proven owned process may be adopted. PID reuse, uncertain
// descendants, or incomplete launch evidence become attention-required.
// Recovery never silently launches provider work or frees ambiguous ownership.
// Decisions are idempotent across crash windows.
package processrecovery
