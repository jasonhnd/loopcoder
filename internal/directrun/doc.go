// Package directrun is the production direct-path application service for
// loopcoder run (V090-RB02 / #1313).
//
// It wires preflight-passed identity through route pin, owned worktree claim,
// start:rendered gate, single provider launch, cleanup-terminal, and durable
// UI reports. Git commit/push/PR are out of scope.
package directrun
