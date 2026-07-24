package goalrun

import (
	"fmt"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/workflowrun"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

// validateCheckpointEnvelope requires exact parent identity before any recovery
// side effect (seed merge, eventlog open, claim, reserve, launch).
func validateCheckpointEnvelope(cp Checkpoint, projectID, runID, graphID, planDigest, graphDigest string) error {
	if strings.TrimSpace(cp.Schema) == "" {
		return fmt.Errorf("goalrun: resume checkpoint schema empty (fail closed)")
	}
	if cp.Schema != CheckpointSchema {
		return fmt.Errorf("goalrun: resume checkpoint schema %q != %q", cp.Schema, CheckpointSchema)
	}
	if cp.ProjectID != projectID {
		return fmt.Errorf("goalrun: resume checkpoint project_id %q != requested %q", cp.ProjectID, projectID)
	}
	if cp.RunID != runID {
		return fmt.Errorf("goalrun: resume checkpoint run_id %q != requested %q", cp.RunID, runID)
	}
	if strings.TrimSpace(cp.GraphID) == "" || cp.GraphID != graphID {
		return fmt.Errorf("goalrun: resume checkpoint graph_id %q != current %q", cp.GraphID, graphID)
	}
	if strings.TrimSpace(cp.PlanDigest) == "" || cp.PlanDigest != planDigest {
		return fmt.Errorf("goalrun: resume checkpoint plan_digest %q != current execution plan %q", cp.PlanDigest, planDigest)
	}
	if strings.TrimSpace(cp.GraphDigest) == "" || cp.GraphDigest != graphDigest {
		return fmt.Errorf("goalrun: resume checkpoint graph_digest %q != current %q", cp.GraphDigest, graphDigest)
	}
	return nil
}

// validatePartialEnvelope requires exact parent identity on mid-run partial.
func validatePartialEnvelope(part workflowrun.PartialCheckpoint, projectID, runID, planDigest, graphDigest string) error {
	if strings.TrimSpace(part.Schema) == "" {
		return fmt.Errorf("goalrun: resume partial schema empty (fail closed)")
	}
	if part.Schema != workflowrun.PartialSchema {
		return fmt.Errorf("goalrun: resume partial schema %q != %q", part.Schema, workflowrun.PartialSchema)
	}
	if part.ProjectID != projectID {
		return fmt.Errorf("goalrun: resume partial project_id %q != requested %q", part.ProjectID, projectID)
	}
	if part.RunID != runID {
		return fmt.Errorf("goalrun: resume partial run_id %q != requested %q", part.RunID, runID)
	}
	if strings.TrimSpace(part.PlanDigest) == "" {
		return fmt.Errorf("goalrun: resume partial plan_digest empty")
	}
	if strings.TrimSpace(part.ExecutionPlanDigest) == "" {
		return fmt.Errorf("goalrun: resume partial execution_plan_digest empty")
	}
	if part.PlanDigest != part.ExecutionPlanDigest {
		return fmt.Errorf("goalrun: resume partial plan_digest %q != execution_plan_digest %q",
			part.PlanDigest, part.ExecutionPlanDigest)
	}
	if part.PlanDigest != planDigest {
		return fmt.Errorf("goalrun: resume partial plan_digest %q != current %q", part.PlanDigest, planDigest)
	}
	if strings.TrimSpace(part.GraphDigest) == "" || part.GraphDigest != graphDigest {
		return fmt.Errorf("goalrun: resume partial graph_digest %q != current %q", part.GraphDigest, graphDigest)
	}
	return nil
}

// childOutcomesExactlyEqual requires full equality on identity fields for seed
// overlap. Contradiction is an error; never first-wins.
func childOutcomesExactlyEqual(a, b workflowrun.ChildOutcome, label string) error {
	check := func(name, av, bv string) error {
		if av != bv {
			return fmt.Errorf("goalrun: seed overlap conflict on %s %s: %q != %q", label, name, av, bv)
		}
		return nil
	}
	if err := check("work_item_id", a.WorkItemID, b.WorkItemID); err != nil {
		return err
	}
	if err := check("terminal", a.Terminal, b.Terminal); err != nil {
		return err
	}
	if err := check("output_evidence", a.OutputEvidence, b.OutputEvidence); err != nil {
		return err
	}
	if err := check("task_class", a.TaskClass, b.TaskClass); err != nil {
		return err
	}
	if err := check("execution_plan_digest", a.ExecutionPlanDigest, b.ExecutionPlanDigest); err != nil {
		return err
	}
	if err := check("child_contract_digest", a.ChildContractDigest, b.ChildContractDigest); err != nil {
		return err
	}
	if err := check("attempt_id", a.AttemptID, b.AttemptID); err != nil {
		return err
	}
	if a.Generation != b.Generation {
		return fmt.Errorf("goalrun: seed overlap conflict on %s generation: %d != %d", label, a.Generation, b.Generation)
	}
	if err := check("provider", a.Provider, b.Provider); err != nil {
		return err
	}
	if err := check("model", a.Model, b.Model); err != nil {
		return err
	}
	if err := check("depth", a.Depth, b.Depth); err != nil {
		return err
	}
	if err := check("permission", a.Permission, b.Permission); err != nil {
		return err
	}
	if err := check("account_ref", a.AccountRef, b.AccountRef); err != nil {
		return err
	}
	if err := check("install_ref", a.InstallRef, b.InstallRef); err != nil {
		return err
	}
	if err := check("window_kind", a.WindowKind, b.WindowKind); err != nil {
		return err
	}
	return nil
}

// mergePriorSucceededMaps merges b into a with exact equality on overlap.
// Returns a new map; never mutates inputs. Conflict → error (no first-wins).
func mergePriorSucceededMaps(a, b map[string]workflowrun.ChildOutcome, label string) (map[string]workflowrun.ChildOutcome, error) {
	if len(a) == 0 && len(b) == 0 {
		return nil, nil
	}
	out := map[string]workflowrun.ChildOutcome{}
	for id, c := range a {
		out[id] = c
	}
	for id, c := range b {
		if prev, ok := out[id]; ok {
			if err := childOutcomesExactlyEqual(prev, c, label+":"+id); err != nil {
				return nil, err
			}
			continue
		}
		out[id] = c
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// requireCanonicalPriorAttempt requires Generation >= 1 and AttemptID equal to
// workflowrun.AttemptID(workItemID, planDigest, runID, Generation-1).
// Claim generation is 1-indexed; attempt suffix generation is 0-indexed.
func requireCanonicalPriorAttempt(prior workflowrun.ChildOutcome, workItemID, planDigest, runID string) error {
	if prior.Generation < 1 {
		return fmt.Errorf("goalrun: resume prior %s generation %d < 1", workItemID, prior.Generation)
	}
	want := workflowrun.AttemptID(workItemID, planDigest, runID, prior.Generation-1)
	if prior.AttemptID != want {
		return fmt.Errorf("goalrun: resume prior %s attempt_id %q != canonical %q (generation=%d)",
			workItemID, prior.AttemptID, want, prior.Generation)
	}
	// Full lowercase CCD (no padding).
	ccd := prior.ChildContractDigest
	if ccd == "" || ccd != strings.ToLower(ccd) || ccd != strings.TrimSpace(ccd) {
		return fmt.Errorf("goalrun: resume prior %s child_contract_digest not full lowercase canonical: %q", workItemID, ccd)
	}
	return nil
}

// parseAndValidateAbortedAttempt requires att == AttemptID(workItem, plan, run, g)
// for some g = ParseAttemptGeneration(att) >= 0. Returns that g or error.
// Cross-plan / malformed / empty IDs fail closed (never invent next gen).
func parseAndValidateAbortedAttempt(workItemID, att, planDigest, runID string) (int, error) {
	att = strings.TrimSpace(att)
	if att == "" {
		return -1, fmt.Errorf("goalrun: aborted %s empty attempt_id (fail closed)", workItemID)
	}
	g := workflowrun.ParseAttemptGeneration(att)
	if g < 0 {
		return -1, fmt.Errorf("goalrun: aborted %s attempt_id %q malformed (no -gN suffix)", workItemID, att)
	}
	want := workflowrun.AttemptID(workItemID, planDigest, runID, g)
	if att != want {
		return -1, fmt.Errorf("goalrun: aborted %s attempt_id %q != canonical %q for plan/run (cross-plan or forged; fail closed)",
			workItemID, att, want)
	}
	return g, nil
}

// mergeAbortedIntoAttemptGen validates each aborted attempt against the current
// plan/run and sets next generation = g+1 (never hardcode 1). On conflicting
// aborted IDs for the same work item, requires exact equality.
// attemptGen values are the 0-indexed next attempt suffix to launch.
func mergeAbortedIntoAttemptGen(
	attemptGen map[string]int,
	aborted map[string]string,
	planDigest, runID, source string,
) error {
	if len(aborted) == 0 {
		return nil
	}
	// Track last validated aborted ID for conflict detection across sources.
	// Callers merge sources sequentially; same key with different IDs fails.
	for id, att := range aborted {
		g, err := parseAndValidateAbortedAttempt(id, att, planDigest, runID)
		if err != nil {
			return fmt.Errorf("%s: %w", source, err)
		}
		next := g + 1
		if prev, ok := attemptGen[id]; ok {
			// Deterministic merge: take max next (higher aborted gen wins).
			// But if we already recorded a next that would re-launch this aborted
			// attempt, fail closed.
			if next < prev {
				// Keep higher next already set.
				continue
			}
			if next == prev {
				continue
			}
			// next > prev: upgrade
			attemptGen[id] = next
			continue
		}
		attemptGen[id] = next
	}
	return nil
}

// validateAttemptGenerationEntries requires nonnegative gens and that aborted
// items will not re-launch the same attempt ID under the current plan/run.
func validateAttemptGenerationEntries(gens map[string]int, aborted map[string]string, planDigest, runID string) error {
	for id, g := range gens {
		if g < 0 {
			return fmt.Errorf("goalrun: attempt_generation[%s]=%d is negative", id, g)
		}
	}
	for id, att := range aborted {
		next := 0
		if gens != nil {
			if g, ok := gens[id]; ok {
				next = g
			}
		}
		// next is the 0-indexed attempt suffix used for the next launch.
		nextID := workflowrun.AttemptID(id, planDigest, runID, next)
		if strings.TrimSpace(att) != "" && att == nextID {
			return fmt.Errorf("goalrun: aborted %s would relaunch same attempt_id %q", id, att)
		}
	}
	return nil
}

// mergeAbortedMaps requires exact equality on overlapping aborted work items.
func mergeAbortedMaps(a, b map[string]string, label string) (map[string]string, error) {
	if len(a) == 0 && len(b) == 0 {
		return nil, nil
	}
	out := map[string]string{}
	for id, att := range a {
		out[id] = att
	}
	for id, att := range b {
		if prev, ok := out[id]; ok {
			if prev != att {
				return nil, fmt.Errorf("goalrun: aborted overlap conflict on %s %s: %q != %q", label, id, prev, att)
			}
			continue
		}
		out[id] = att
	}
	return out, nil
}

// validateParentResumeAgainstGraph is the goalrun parent-boundary preflight:
// after durable seed load and before OpenEventLog / inventory / ledger / route /
// reserve, every PriorSucceeded and AttemptGeneration entry must bind to the
// current materialized graph + frozen route requirements. Ghost keys, key/value
// mismatches, and stale aborted-derived gens for non-graph items fail closed.
//
// currentCCD is computed by the caller from each graph item's output contract +
// parsed route (class/depth/permission). This function never invents or mutates prior.
func validateParentResumeAgainstGraph(
	graphItems []workgraph.WorkItem,
	routeByID map[string]ParsedRouteRequirement,
	prior map[string]workflowrun.ChildOutcome,
	attemptGen map[string]int,
	planDigest, runID string,
	currentCCD map[string]string, // workItemID → full ChildContractDigest
) error {
	itemOK := map[string]bool{}
	for _, it := range graphItems {
		itemOK[it.ID] = true
	}
	for id, p := range prior {
		if !itemOK[id] {
			return fmt.Errorf("goalrun: parent preflight PriorSucceeded ghost key %q not in current graph (fail closed before eventlog/inventory)", id)
		}
		if p.WorkItemID != id {
			return fmt.Errorf("goalrun: parent preflight PriorSucceeded key %q != work_item_id %q (fail closed before eventlog/inventory)", id, p.WorkItemID)
		}
		if !strings.EqualFold(strings.TrimSpace(p.Terminal), "succeeded") {
			return fmt.Errorf("goalrun: parent preflight prior %s terminal %q != succeeded", id, p.Terminal)
		}
		if strings.TrimSpace(p.OutputEvidence) == "" {
			return fmt.Errorf("goalrun: parent preflight prior %s missing output_evidence", id)
		}
		if strings.TrimSpace(p.Provider) == "" || strings.TrimSpace(p.Model) == "" {
			return fmt.Errorf("goalrun: parent preflight prior %s missing provider/model", id)
		}
		pr, ok := routeByID[id]
		if !ok {
			return fmt.Errorf("goalrun: parent preflight prior %s missing route requirement", id)
		}
		wantClass := string(pr.Class)
		if p.TaskClass != wantClass {
			return fmt.Errorf("goalrun: parent preflight prior %s task_class %q != current %q", id, p.TaskClass, wantClass)
		}
		if p.ExecutionPlanDigest != planDigest {
			return fmt.Errorf("goalrun: parent preflight prior %s execution_plan_digest %q != current %q", id, p.ExecutionPlanDigest, planDigest)
		}
		wantCCD := currentCCD[id]
		if wantCCD == "" {
			return fmt.Errorf("goalrun: parent preflight prior %s missing current CCD", id)
		}
		if p.ChildContractDigest != wantCCD {
			return fmt.Errorf("goalrun: parent preflight prior %s child_contract_digest %q != current %q", id, p.ChildContractDigest, wantCCD)
		}
		if err := requireCanonicalPriorAttempt(p, id, planDigest, runID); err != nil {
			return fmt.Errorf("goalrun: parent preflight: %w", err)
		}
	}
	for id, g := range attemptGen {
		if !itemOK[id] {
			return fmt.Errorf("goalrun: parent preflight AttemptGeneration ghost key %q not in current graph (fail closed before eventlog/inventory)", id)
		}
		if g < 0 {
			return fmt.Errorf("goalrun: parent preflight AttemptGeneration[%s]=%d is negative", id, g)
		}
	}
	return nil
}

// loadAndValidateResumeSeeds performs read-only durable loads, envelope
// validation, and seed merge BEFORE any eventlog/claim/reserve/launch side effect.
//
// Resume=true with no valid durable checkpoint/partial fails closed.
// Caller PriorSucceeded must not bypass the parent envelope: only merge when
// exact-equal to durable, or fail as unbound.
//
// Aborted attempt IDs are parsed and validated against AttemptID(workItem,
// currentPlan, runID, g); next generation is always g+1. Final validation runs
// AFTER all durable sources are merged. Never relaunch an aborted attempt ID.
func loadAndValidateResumeSeeds(
	homeDir, projectID, runID, graphID, planDigest, graphDigest string,
	resume bool,
	callerPrior map[string]workflowrun.ChildOutcome,
) (prior map[string]workflowrun.ChildOutcome, attemptGen map[string]int, loadedCP Checkpoint, hasCP bool, err error) {
	attemptGen = map[string]int{}
	var (
		cpOK    bool
		partOK  bool
		cp      Checkpoint
		part    workflowrun.PartialCheckpoint
		durable map[string]workflowrun.ChildOutcome
		aborted map[string]string
	)

	// --- read-only loads (no MkdirAll / OpenEventLog) ---
	cp, _, cpErr := LoadCheckpoint(homeDir, projectID, runID)
	switch {
	case cpErr == nil:
		if vErr := validateCheckpointEnvelope(cp, projectID, runID, graphID, planDigest, graphDigest); vErr != nil {
			return nil, nil, Checkpoint{}, false, vErr
		}
		cpOK = true
		loadedCP = cp
		hasCP = true
	case osIsNotExist(cpErr):
		// absent is ok unless Resume requires a durable envelope
	default:
		return nil, nil, Checkpoint{}, false, fmt.Errorf("goalrun: resume load checkpoint: %w", cpErr)
	}

	part, partErr := workflowrun.LoadPartialPrior(homeDir, projectID, runID)
	switch {
	case partErr == nil:
		if vErr := validatePartialEnvelope(part, projectID, runID, planDigest, graphDigest); vErr != nil {
			return nil, nil, Checkpoint{}, false, vErr
		}
		partOK = true
	case osIsNotExist(partErr):
		// absent ok
	default:
		return nil, nil, Checkpoint{}, false, fmt.Errorf("goalrun: resume load partial: %w", partErr)
	}

	if resume && !cpOK && !partOK {
		return nil, nil, Checkpoint{}, false, fmt.Errorf(
			"goalrun: resume requires valid durable checkpoint or partial (none present or readable); fail closed before eventlog/spend")
	}

	// --- merge PriorSucceeded with exact equality on overlap ---
	if cpOK {
		durable, err = mergePriorSucceededMaps(nil, cp.PriorSucceeded, "checkpoint")
		if err != nil {
			return nil, nil, Checkpoint{}, false, err
		}
		aborted, err = mergeAbortedMaps(nil, cp.AbortedAttempts, "checkpoint")
		if err != nil {
			return nil, nil, Checkpoint{}, false, err
		}
		// Checkpoint AttemptGeneration (if present) seeds nonnegative floors;
		// aborted IDs still override via parse→g+1 after full merge.
		for id, g0 := range cp.AttemptGeneration {
			if g0 < 0 {
				return nil, nil, Checkpoint{}, false, fmt.Errorf("goalrun: attempt_generation[%s]=%d negative", id, g0)
			}
			attemptGen[id] = g0
		}
	}
	if partOK {
		durable, err = mergePriorSucceededMaps(durable, part.PriorSucceeded, "checkpoint-vs-partial")
		if err != nil {
			return nil, nil, Checkpoint{}, false, err
		}
		aborted, err = mergeAbortedMaps(aborted, part.Aborted, "checkpoint-vs-partial")
		if err != nil {
			return nil, nil, Checkpoint{}, false, err
		}
	}

	// --- parse every aborted ID AFTER merge; next = g+1 (never hardcode 1) ---
	// Build attemptGen from aborted as authoritative next launch generation.
	// Start fresh for aborted-driven next so checkpoint AttemptGeneration floors
	// cannot force relaunch of a higher aborted g.
	fromAborted := map[string]int{}
	if err := mergeAbortedIntoAttemptGen(fromAborted, aborted, planDigest, runID, "aborted"); err != nil {
		return nil, nil, Checkpoint{}, false, err
	}
	// Merge: max(next from aborted, checkpoint AttemptGeneration floors).
	for id, next := range fromAborted {
		if prev, ok := attemptGen[id]; ok && prev > next {
			// Higher floor already set — still must not equal aborted ID.
			continue
		}
		attemptGen[id] = next
	}
	// For aborted keys only in fromAborted, ensure present.
	for id, next := range fromAborted {
		if _, ok := attemptGen[id]; !ok {
			attemptGen[id] = next
		}
		// Prefer aborted-derived next when it is higher (safer no-relaunch).
		if attemptGen[id] < next {
			attemptGen[id] = next
		}
	}

	// Caller PriorSucceeded: no unbound product resume seeds.
	if len(callerPrior) > 0 {
		if resume && !cpOK && !partOK {
			return nil, nil, Checkpoint{}, false, fmt.Errorf(
				"goalrun: caller PriorSucceeded unbound without validated durable envelope (fail closed)")
		}
		if len(durable) == 0 {
			return nil, nil, Checkpoint{}, false, fmt.Errorf(
				"goalrun: caller PriorSucceeded unbound without durable checkpoint/partial envelope (fail closed)")
		}
		// Every caller id must exist in durable and exact-equal.
		for id, c := range callerPrior {
			d, ok := durable[id]
			if !ok {
				return nil, nil, Checkpoint{}, false, fmt.Errorf(
					"goalrun: caller PriorSucceeded[%s] unbound (not in durable envelope)", id)
			}
			if err := childOutcomesExactlyEqual(d, c, "caller-vs-durable:"+id); err != nil {
				return nil, nil, Checkpoint{}, false, err
			}
		}
	}

	// --- final validation AFTER all durable sources merged ---
	for id, p := range durable {
		if err := requireCanonicalPriorAttempt(p, id, planDigest, runID); err != nil {
			return nil, nil, Checkpoint{}, false, err
		}
	}
	if err := validateAttemptGenerationEntries(attemptGen, aborted, planDigest, runID); err != nil {
		return nil, nil, Checkpoint{}, false, err
	}

	return durable, attemptGen, loadedCP, hasCP, nil
}
