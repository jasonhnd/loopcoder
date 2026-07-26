package goalrun

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/jasonhnd/loopcoder/internal/workflowrun"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

// eventLogPathRead derives the workflow-events.jsonl path without creating dirs/files.
func eventLogPathRead(homeDir, projectID, runID string) (string, error) {
	dir, err := workflowrun.RunDurableDir(homeDir, projectID, runID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "workflow-events.jsonl"), nil
}

// nextAttemptGenerationFromAttemptID validates att is the canonical AttemptID for
// some g and returns next launch generation index g+1. Shared helper — never
// hardcode next=1.
func nextAttemptGenerationFromAttemptID(workItemID, att, planDigest, runID string) (int, error) {
	g, err := parseAndValidateAbortedAttempt(workItemID, att, planDigest, runID)
	if err != nil {
		return -1, err
	}
	return g + 1, nil
}

// validateEventsAgainstGraph requires every child-lifecycle fact to carry full
// identity (WorkItemID, canonical AttemptID, positive Generation == suffix+1)
// against the current graph/plan. Parent interrupt may omit all child identity.
// Does not mutate the log.
func validateEventsAgainstGraph(
	events []workflowrun.Event,
	graphItems []workgraph.WorkItem,
	projectID, planDigest, graphDigest, graphID, runID string,
	graphVersion int,
) error {
	itemOK := map[string]bool{}
	for _, it := range graphItems {
		itemOK[it.ID] = true
	}
	id := workflowrun.EventWriteIdentity{
		ProjectID: projectID, RunID: runID,
		PlanDigest: planDigest, GraphDigest: graphDigest,
		GraphID: graphID, GraphVersion: graphVersion,
	}
	for i, ev := range events {
		if err := workflowrun.ValidateChildEventIdentityForPlan(ev, id, itemOK); err != nil {
			return fmt.Errorf("goalrun: event log line %d: %w (fail closed; log unchanged)", i, err)
		}
	}
	return nil
}

// mergeMonotonicAttemptGen sets attemptGen[id] = max(existing, next).
func mergeMonotonicAttemptGen(attemptGen map[string]int, id string, next int) {
	if next < 0 {
		return
	}
	if cur, ok := attemptGen[id]; !ok || next > cur {
		attemptGen[id] = next
	}
}

// applyEventLogResumeSource treats an existing event log as a durable resume
// source. On missing log returns (attemptGen, false, nil). On present log:
//  1. strict read+parse (propagate errors)
//  2. validate all child lifecycle facts against current graph/plan (no append)
//  3. recover open launches (propagate errors)
//  4. strict reread + re-validate
//  5. merge next generations as parsed g+1 (never hardcode 1)
//
// Invalid logs are left byte-for-byte unchanged (no recovery interrupt appended).
func applyEventLogResumeSource(
	homeDir, projectID, runID, planDigest, graphDigest, graphID string,
	graphVersion int,
	graphItems []workgraph.WorkItem,
	attemptGen map[string]int,
	priorSucceeded map[string]workflowrun.ChildOutcome,
) (map[string]int, bool, error) {
	if attemptGen == nil {
		attemptGen = map[string]int{}
	}
	path, err := eventLogPathRead(homeDir, projectID, runID)
	if err != nil {
		return nil, false, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return attemptGen, false, nil
		}
		return nil, false, fmt.Errorf("goalrun: event log read: %w", err)
	}
	// Pre-recovery validation — any failure leaves raw bytes untouched.
	events, err := workflowrun.ParseEventJSONLStrict(string(raw), projectID, runID)
	if err != nil {
		return nil, false, fmt.Errorf("goalrun: event log parse: %w", err)
	}
	if err := validateEventsAgainstGraph(events, graphItems, projectID, planDigest, graphDigest, graphID, runID, graphVersion); err != nil {
		return nil, false, err
	}
	if err := workflowrun.ValidateEventStreamInvariants(events); err != nil {
		return nil, false, fmt.Errorf("goalrun: event log stream: %w", err)
	}

	// Open only after pre-recovery validation succeeds.
	elog, err := workflowrun.OpenEventLog(homeDir, projectID, runID)
	if err != nil {
		return nil, false, fmt.Errorf("goalrun: event log open: %w", err)
	}
	if _, err := workflowrun.RecoverOpenLaunchInterrupts(elog, projectID, runID); err != nil {
		return nil, false, fmt.Errorf("goalrun: event log recover: %w", err)
	}
	events, err = elog.ReadAllForRun(projectID, runID)
	if err != nil {
		return nil, false, fmt.Errorf("goalrun: event log reread: %w", err)
	}
	if err := validateEventsAgainstGraph(events, graphItems, projectID, planDigest, graphDigest, graphID, runID, graphVersion); err != nil {
		return nil, false, err
	}
	if err := workflowrun.ValidateEventStreamInvariants(events); err != nil {
		return nil, false, fmt.Errorf("goalrun: event log post-recovery stream: %w", err)
	}

	// Copy attemptGen for merge.
	out := map[string]int{}
	for k, v := range attemptGen {
		out[k] = v
	}

	interrupted, aborted := workflowrun.InterruptedFromEvents(events)
	open := workflowrun.OpenLaunchesWithoutTerminal(events)

	// Open + interrupted aborted: next = g+1 from durable attempt ID.
	for id, att := range aborted {
		if priorSucceeded != nil {
			if _, ok := priorSucceeded[id]; ok {
				continue
			}
		}
		next, nerr := nextAttemptGenerationFromAttemptID(id, att, planDigest, runID)
		if nerr != nil {
			return nil, false, fmt.Errorf("goalrun: event aborted: %w", nerr)
		}
		mergeMonotonicAttemptGen(out, id, next)
	}
	for id, att := range open {
		if priorSucceeded != nil {
			if _, ok := priorSucceeded[id]; ok {
				continue
			}
		}
		next, nerr := nextAttemptGenerationFromAttemptID(id, att, planDigest, runID)
		if nerr != nil {
			return nil, false, fmt.Errorf("goalrun: event open launch: %w", nerr)
		}
		mergeMonotonicAttemptGen(out, id, next)
	}
	// Failed terminals: FailedRetryGenerations already returns max g+1; still
	// require graph membership (validateEvents already proved attempts canonical).
	itemOK := map[string]bool{}
	for _, it := range graphItems {
		itemOK[it.ID] = true
	}
	for id, next := range workflowrun.FailedRetryGenerations(events) {
		if priorSucceeded != nil {
			if _, ok := priorSucceeded[id]; ok {
				continue
			}
		}
		if !itemOK[id] {
			return nil, false, fmt.Errorf("goalrun: event FailedRetryGenerations ghost key %q", id)
		}
		if next < 0 {
			return nil, false, fmt.Errorf("goalrun: event FailedRetryGenerations[%s]=%d negative", id, next)
		}
		mergeMonotonicAttemptGen(out, id, next)
	}

	// Final: no relaunch of aborted/open attempt IDs.
	allOpen := map[string]string{}
	for id, att := range aborted {
		allOpen[id] = att
	}
	for id, att := range open {
		allOpen[id] = att
	}
	if err := validateAttemptGenerationEntries(out, allOpen, planDigest, runID); err != nil {
		return nil, false, err
	}

	eventResumed := interrupted || len(aborted) > 0 || len(open) > 0
	return out, eventResumed, nil
}
