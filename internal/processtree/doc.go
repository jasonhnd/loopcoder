// Package processtree provides Darwin process-tree identity and liveness
// (V090-013). Liveness is derived from OS evidence (PID + birth identity +
// descendants), not provider output, PTY activity, or heartbeats.
//
// unknown never authorizes takeover or success. Observation failures do not
// kill processes.
package processtree
