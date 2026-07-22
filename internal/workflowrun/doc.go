// Package workflowrun is the production bounded-workflow entry on the
// direct-run lifecycle (V090-RB05 / #1316).
//
// It freezes a finite workflowdef, materializes a workgraph, schedules
// dependency-ready items under hard budgets, claims each WorkItem at most
// once, runs a single fake child attempt per claim (no live provider in the
// default path), integrates in declared order, and stops at the human merge
// gate. Invalid/cyclic graphs create no claims or launches.
package workflowrun
