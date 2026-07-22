// Package workflowdef accepts explicit user-authored workflow definitions,
// normalizes them to a stable plan digest, requires approval, and materializes
// one immutable Work Graph version (V090-060).
//
// LoopCoder does not call a planner model. ROADMAP markers, GitHub epics, model
// output, and hidden auto-split cannot create WorkItems.
package workflowdef
