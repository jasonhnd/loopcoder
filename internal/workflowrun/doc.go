// Package workflowrun is the production bounded-workflow entry on the
// direct-run lifecycle (V090-RB05 / #1316, transparent children #1342).
//
// It freezes a finite workflowdef, materializes a workgraph, schedules
// dependency-ready items under hard budgets, claims each WorkItem at most
// once, and runs each child through a LoopCoder-owned ChildExecutor:
//
//   - Production default (ProductionChildExecutor) allocates an exclusive
//     worktree and invokes the routed provider via agent.Runner with
//     DisableDelegation — never provider-native opaque subagents.
//   - Fixture routes write durable local evidence without a live process.
//   - Focused tests inject FakeChildExecutor; remote acceptance must use
//     the production executor (no Claim→Close stub without evidence).
//
// Success close requires non-empty output evidence. Failure/cancel/timeout
// close the claim so capacity can be released and recovery stays idempotent.
// Integration is declared order; parent stops at the human merge gate.
// Invalid/cyclic graphs create no claims or launches.
package workflowrun
