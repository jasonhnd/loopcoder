package goalrun

import (
	"fmt"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/capacityledger"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

// terminalCapacityStates cannot be relaunched under the same attempt_id.
// released/cancelled/refused are honest non-success terminals; reconciled is a
// spend terminal that also must not reuse the same attempt on resume when the
// child is not prior-succeeded (should not happen for true success — those are
// seeded in PriorSucceeded and skipped).
func isImmutableCapacityState(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "released", "cancelled", "refused", "reconciled":
		return true
	default:
		return false
	}
}

// applyReleasedReservationResumeGenerations bumps attemptGen for every graph
// item that is NOT prior-succeeded when the canonical attempt at the current
// generation (and any lower) is durably immutable in the capacity ledger
// (released/aborted/cancelled/refused/reconciled).
//
// This fixes the production defect where goalrun pre-reserves all children,
// interrupt releases never-launched later siblings under g0, and Resume tried
// to re-Reserve the same released g0 attempt (ledger correctly refuses).
//
// Rules:
//   - prior-succeeded children are never bumped
//   - immutable ledger entries are never reopened (Actual stays as stored)
//   - next generation is always g+1 of the highest immutable attempt found
//   - still-reserved attempts are left alone (idempotent same-gen resume)
//   - generation is derived only from durable ledger + canonical AttemptID
func applyReleasedReservationResumeGenerations(
	ledger *capacityledger.Ledger,
	projectID, runID, planDigest string,
	items []workgraph.WorkItem,
	priorSucceeded map[string]workflowrun.ChildOutcome,
	attemptGen map[string]int,
	checkpointChildren []ChildReport,
) (map[string]int, error) {
	if ledger == nil {
		return attemptGen, nil
	}
	out := map[string]int{}
	for k, v := range attemptGen {
		if v < 0 {
			return nil, fmt.Errorf("goalrun: attempt_generation[%s]=%d negative", k, v)
		}
		out[k] = v
	}

	// Checkpoint child AttemptIDs are durable hints for non-succeeded items.
	for _, cr := range checkpointChildren {
		id := strings.TrimSpace(cr.ChildID)
		att := strings.TrimSpace(cr.AttemptID)
		if id == "" || att == "" {
			continue
		}
		if priorSucceeded != nil {
			if _, ok := priorSucceeded[id]; ok {
				continue
			}
		}
		g, err := parseAndValidateAbortedAttempt(id, att, planDigest, runID)
		if err != nil {
			// Non-canonical checkpoint attempt: ignore (do not invent).
			continue
		}
		ent, ok := ledger.Get(projectID, runID, att)
		if !ok {
			continue
		}
		if isImmutableCapacityState(ent.State) {
			mergeMonotonicAttemptGen(out, id, g+1)
		}
	}

	const maxGenScan = 64
	for _, it := range items {
		id := strings.TrimSpace(it.ID)
		if id == "" {
			continue
		}
		if priorSucceeded != nil {
			if _, ok := priorSucceeded[id]; ok {
				// Never bump prior success — reuse exact attempt, no re-reserve.
				continue
			}
		}
		// Scan canonical generations 0..max; any immutable entry forces next = g+1.
		for g := 0; g < maxGenScan; g++ {
			att := workflowrun.AttemptID(id, planDigest, runID, g)
			ent, ok := ledger.Get(projectID, runID, att)
			if !ok {
				// No durable entry at this gen — stop scanning upward.
				break
			}
			if isImmutableCapacityState(ent.State) {
				// Leave Actual/ActualSource untouched; only advance next launch gen.
				mergeMonotonicAttemptGen(out, id, g+1)
				continue
			}
			// Still reserved: eligible for same-gen idempotent reserve; do not
			// force a bump past this generation unless a higher immutable exists.
			// Continue scan in case a higher gen was already written.
		}
	}
	return out, nil
}
