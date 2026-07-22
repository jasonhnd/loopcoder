// Package workgraph defines the LoopCoder-owned Work Graph public contract and
// materialization boundary (V090-056).
//
// A one-node graph is behaviorally equivalent to a direct run (no extra provider
// call). Multi-node workflows require explicit definition, approval, stable plan
// digest, limits, and visible integration order. Graph mutation after execution
// starts is a versioned replan only — completed history is never rewritten.
//
// Forbidden workflow sources: automatic ROADMAP compilation, synthetic epic
// expansion, and implicit self-bootstrap. Provider-native child sessions are not
// WorkItems.
package workgraph
