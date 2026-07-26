// Package mergegate implements the independent verifier and human merge gate
// (V090-034 / #1136).
//
// A separately selected read-only verifier runs only after worker cleanup-terminal
// and required CI readiness. Verdicts are head-scoped; pass still stops at an
// explicit human merge decision. No automatic merge in v0.9 default.
package mergegate
