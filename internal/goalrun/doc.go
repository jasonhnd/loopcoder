// Package goalrun is the product entry for goal → WorkGraph → transparent
// LoopCoder-owned child execution with capacity/route reporting (V090-CRO-009 / #1342).
//
// Children are never provider-native opaque subagents. Each child has its own
// attempt identity, route requirement, exclusive worktree, and capacity ledger
// line (reserve → real execute → reconcile actual when known, else honest
// unknown / release). Focused tests may inject a FakeChildExecutor; production
// and remote acceptance use the real LoopCoder-owned executor path.
package goalrun
