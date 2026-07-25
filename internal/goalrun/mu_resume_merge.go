package goalrun

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/capacityledger"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

// muDurablePair is one envelope-validated checkpoint MU failed + alternate pair
// after event-log and ledger cross-binding.
type muDurablePair struct {
	Failed workflowrun.ChildOutcome
	Retry  workflowrun.ChildOutcome
	// SeedPriorAtt / SeedAltAtt from validated checkpoint CapacityTransitions.
	SeedPriorAtt string
	SeedAltAtt   string
}

// mergeDurableModelUnavailableOnResume preserves prior MU failed+alternate
// outcomes from the already envelope-validated checkpoint, cross-bound to the
// authoritative JSONL event log with exact project/run/plan/graph/work_item/
// task_class/CCD/graph_id/version/canonical attempt generation.
func mergeDurableModelUnavailableOnResume(
	out *Result,
	cp Checkpoint,
	ledger *capacityledger.Ledger,
	projectID, runID, planDigest, graphDigest, graphID string,
	graphVersion int,
) error {
	if out == nil {
		return fmt.Errorf("goalrun: MU resume merge: nil result")
	}
	if strings.TrimSpace(planDigest) == "" {
		return fmt.Errorf("goalrun: MU resume merge: plan_digest required nonempty")
	}
	if strings.TrimSpace(graphDigest) == "" {
		return fmt.Errorf("goalrun: MU resume merge: graph_digest required nonempty")
	}
	if strings.TrimSpace(graphID) == "" {
		return fmt.Errorf("goalrun: MU resume merge: graph_id required nonempty")
	}
	if graphVersion <= 0 {
		return fmt.Errorf("goalrun: MU resume merge: graph_version required positive")
	}
	if strings.TrimSpace(cp.GraphID) == "" {
		return fmt.Errorf("goalrun: MU resume merge: checkpoint graph_id empty (required nonempty)")
	}
	if cp.GraphID != graphID {
		return fmt.Errorf("goalrun: MU resume merge: checkpoint graph_id %q != current %q", cp.GraphID, graphID)
	}

	pairs, err := extractCheckpointMUPairs(cp, planDigest, runID)
	if err != nil {
		return err
	}
	if len(pairs) == 0 {
		return nil
	}
	if len(pairs) != 1 {
		return fmt.Errorf("goalrun: MU resume merge: want exactly one checkpoint MU pair, got %d", len(pairs))
	}
	pair := pairs[0]

	seed, serr := capacitySeedFromCheckpoint(cp, pair)
	if serr != nil {
		return serr
	}
	if err := requireExactDurableToken("MU seed prior attempt", seed[0].AttemptID); err != nil {
		return err
	}
	if err := requireExactDurableToken("MU seed alt attempt", seed[1].AttemptID); err != nil {
		return err
	}
	pair.SeedPriorAtt = seed[0].AttemptID
	pair.SeedAltAtt = seed[1].AttemptID

	events, err := loadAuthoritativeEvents(out, cp, projectID, runID)
	if err != nil {
		return err
	}
	if err := crossBindMUPairToEvents(pair, events, projectID, runID, planDigest, graphDigest, graphID, graphVersion); err != nil {
		return err
	}

	if err := mergeMUPairIntoChildren(&out.Workflow, pair); err != nil {
		return err
	}

	fin := finalizeCapacityTransitions(ledger, projectID, runID, seed)
	if len(fin) != 2 {
		return fmt.Errorf("goalrun: MU resume merge: ledger capacity finalize failed for prior=%s alt=%s (want 2, got %d)",
			pair.SeedPriorAtt, pair.SeedAltAtt, len(fin))
	}
	if err := validateLedgerMUCapacity(fin); err != nil {
		return err
	}
	out.Workflow.CapacityTransitions = fin

	rAtt := pair.Retry.AttemptID
	if err := requireExactDurableToken("MU retry attempt", rAtt); err != nil {
		return err
	}
	sha := pair.Retry.IntegrateCommitSHA
	if err := requireExactDurableToken("MU IntegrateCommitSHA", sha); err != nil {
		return fmt.Errorf("goalrun: MU resume merge: retry attempt %s missing IntegrateCommitSHA on checkpoint: %w", rAtt, err)
	}
	intEv, ok := findEventKindAttempt(events, "integrate", rAtt)
	if !ok {
		return fmt.Errorf("goalrun: MU resume merge: missing integrate event for retry %s", rAtt)
	}
	if intEv.CommitSHA != sha {
		return fmt.Errorf("goalrun: MU resume merge: integrate event commit %q != checkpoint %q", intEv.CommitSHA, sha)
	}
	if err := requireEventIdentityStamps(intEv, projectID, runID, planDigest, graphDigest, graphID, graphVersion, pair.Retry); err != nil {
		return fmt.Errorf("goalrun: MU resume merge integrate event: %w", err)
	}
	foundIC := false
	for _, ic := range out.Workflow.IntegrateCommits {
		if idExact(ic.AttemptID, rAtt) && ic.CommitSHA == sha {
			foundIC = true
			break
		}
	}
	if !foundIC {
		out.Workflow.IntegrateCommits = append(out.Workflow.IntegrateCommits, workflowrun.IntegrateCommit{
			WorkItemID: pair.Retry.WorkItemID, AttemptID: rAtt, CommitSHA: sha,
		})
	}
	return nil
}

func extractCheckpointMUPairs(cp Checkpoint, planDigest, runID string) ([]muDurablePair, error) {
	var failed []workflowrun.ChildOutcome
	retryBySupersedes := map[string]workflowrun.ChildOutcome{}
	for _, c := range cp.WorkflowKids {
		// Exact failure class — no EqualFold/TrimSpace normalize.
		if c.FailureClass == "model_unavailable" {
			failed = append(failed, c)
		}
		sup := c.SupersedesAttemptID
		if sup != "" {
			if sup != strings.TrimSpace(sup) {
				return nil, fmt.Errorf("goalrun: MU resume merge: SupersedesAttemptID has whitespace padding %q", sup)
			}
			if prev, ok := retryBySupersedes[sup]; ok && !childOutcomeExactEqual(prev, c) {
				return nil, fmt.Errorf("goalrun: MU resume merge: conflicting checkpoint retries for supersedes %s", sup)
			}
			retryBySupersedes[sup] = c
		}
	}
	var pairs []muDurablePair
	for _, f := range failed {
		fAtt := f.AttemptID
		if err := requireExactDurableToken("MU failed attempt", fAtt); err != nil {
			return nil, fmt.Errorf("goalrun: MU resume merge: checkpoint MU failed: %w", err)
		}
		if f.FailureClass != "model_unavailable" {
			return nil, fmt.Errorf("goalrun: MU resume merge: failed row failure_class %q != exact model_unavailable", f.FailureClass)
		}
		if f.Terminal != string(workgraph.TermFailed) {
			return nil, fmt.Errorf("goalrun: MU resume merge: MU failed terminal %q != exact failed", f.Terminal)
		}
		if ps, ok := cp.PriorSucceeded[f.WorkItemID]; ok && idExact(ps.AttemptID, fAtt) {
			return nil, fmt.Errorf("goalrun: MU resume merge: failed MU attempt %s listed as PriorSucceeded (fail closed)", fAtt)
		}
		retry, ok := retryBySupersedes[fAtt]
		if !ok {
			return nil, fmt.Errorf("goalrun: MU resume merge: no alternate for failed attempt %s", fAtt)
		}
		rAtt := retry.AttemptID
		if err := requireExactDurableToken("MU retry attempt", rAtt); err != nil {
			return nil, err
		}
		if rAtt == fAtt {
			return nil, fmt.Errorf("goalrun: MU resume merge: invalid retry attempt for %s", fAtt)
		}
		if !idExact(retry.SupersedesAttemptID, fAtt) {
			return nil, fmt.Errorf("goalrun: MU resume merge: retry supersedes %q != failed %q", retry.SupersedesAttemptID, fAtt)
		}
		if err := requireCanonicalOutcomeAttempt(f, planDigest, runID); err != nil {
			return nil, fmt.Errorf("goalrun: MU resume merge failed row: %w", err)
		}
		if err := requireCanonicalOutcomeAttempt(retry, planDigest, runID); err != nil {
			return nil, fmt.Errorf("goalrun: MU resume merge retry row: %w", err)
		}
		if f.WorkItemID != retry.WorkItemID {
			return nil, fmt.Errorf("goalrun: MU resume merge: work_item mismatch failed=%s retry=%s", f.WorkItemID, retry.WorkItemID)
		}
		if strings.TrimSpace(f.TaskClass) == "" || strings.TrimSpace(f.ChildContractDigest) == "" ||
			strings.TrimSpace(f.ExecutionPlanDigest) == "" {
			return nil, fmt.Errorf("goalrun: MU resume merge: failed row missing plan/class/CCD")
		}
		if strings.TrimSpace(retry.TaskClass) == "" || strings.TrimSpace(retry.ChildContractDigest) == "" ||
			strings.TrimSpace(retry.ExecutionPlanDigest) == "" {
			return nil, fmt.Errorf("goalrun: MU resume merge: retry row missing plan/class/CCD")
		}
		if f.ExecutionPlanDigest != planDigest || retry.ExecutionPlanDigest != planDigest {
			return nil, fmt.Errorf("goalrun: MU resume merge: plan_digest mismatch vs current")
		}
		if f.TaskClass != retry.TaskClass || f.ChildContractDigest != retry.ChildContractDigest {
			return nil, fmt.Errorf("goalrun: MU resume merge: failed/retry class or CCD mismatch")
		}
		pairs = append(pairs, muDurablePair{Failed: f, Retry: retry, SeedPriorAtt: fAtt, SeedAltAtt: rAtt})
	}
	return pairs, nil
}

func requireCanonicalOutcomeAttempt(c workflowrun.ChildOutcome, planDigest, runID string) error {
	// Byte-exact durable identity — no TrimSpace normalize into canonical.
	if err := requireExactDurableToken("work_item_id", c.WorkItemID); err != nil {
		return err
	}
	if err := requireExactDurableToken("attempt_id", c.AttemptID); err != nil {
		return err
	}
	wi, att := c.WorkItemID, c.AttemptID
	g := workflowrun.ParseAttemptGeneration(att)
	if g < 0 {
		return fmt.Errorf("attempt %q malformed generation", att)
	}
	want := workflowrun.AttemptID(wi, planDigest, runID, g)
	if att != want {
		return fmt.Errorf("attempt %q != canonical %q", att, want)
	}
	wantGen, err := workflowrun.ClaimGenerationFromAttemptID(att)
	if err != nil {
		return err
	}
	if c.Generation != wantGen {
		return fmt.Errorf("generation %d != claim gen %d for %s", c.Generation, wantGen, att)
	}
	return nil
}

// loadAuthoritativeEvents requires checkpoint EventLogPath nonempty and
// byte-exact equal to result EventLogPath; reads the existing file only.
// Never TrimSpace EventLogPath stamps.
func loadAuthoritativeEvents(out *Result, cp Checkpoint, projectID, runID string) ([]workflowrun.Event, error) {
	resPath := out.Workflow.EventLogPath
	cpPath := cp.EventLogPath
	if err := requireExactEventLogPathStamp("result", resPath); err != nil {
		return nil, fmt.Errorf("goalrun: MU resume merge: %w", err)
	}
	if err := requireExactEventLogPathStamp("checkpoint", cpPath); err != nil {
		return nil, fmt.Errorf("goalrun: MU resume merge: %w", err)
	}
	if resPath != cpPath {
		return nil, fmt.Errorf("goalrun: MU resume merge: EventLogPath mismatch result=%q checkpoint=%q", resPath, cpPath)
	}
	st, err := os.Stat(resPath)
	if err != nil {
		return nil, fmt.Errorf("goalrun: MU resume merge: event log stat: %w", err)
	}
	if st.IsDir() {
		return nil, fmt.Errorf("goalrun: MU resume merge: event log path is a directory")
	}
	raw, err := os.ReadFile(resPath)
	if err != nil {
		return nil, fmt.Errorf("goalrun: MU resume merge: event log read: %w", err)
	}
	events, err := workflowrun.ParseEventJSONLStrict(string(raw), projectID, runID)
	if err != nil {
		return nil, fmt.Errorf("goalrun: MU resume merge: event log parse: %w", err)
	}
	return events, nil
}

func crossBindMUPairToEvents(
	pair muDurablePair,
	events []workflowrun.Event,
	projectID, runID, planDigest, graphDigest, graphID string,
	graphVersion int,
) error {
	fAtt := strings.TrimSpace(pair.Failed.AttemptID)
	rAtt := strings.TrimSpace(pair.Retry.AttemptID)
	wi := strings.TrimSpace(pair.Failed.WorkItemID)

	needFailed := map[string]int{"claim": 1, "launch": 1, "model_unavailable": 1, "terminal": 1}
	needRetry := map[string]int{"claim": 1, "launch": 1, "reroute": 1, "terminal": 1, "integrate": 1}
	gotFailed := map[string]int{}
	gotRetry := map[string]int{}

	// Capture exact persisted EventIDs for MU + retry claim (exactly one each).
	var muEventID, retryClaimEventID string
	seenEventIDs := map[string]string{} // eventID → kind@attempt

	for _, e := range events {
		if !idExact(e.ProjectID, projectID) || !idExact(e.RunID, runID) {
			return fmt.Errorf("goalrun: MU resume merge: event project/run drift kind=%s", e.Kind)
		}
		att := strings.TrimSpace(e.AttemptID)
		if att != fAtt && att != rAtt {
			continue
		}
		if !idExact(e.WorkItemID, wi) {
			return fmt.Errorf("goalrun: MU resume merge: event work_item %q != %q kind=%s", e.WorkItemID, wi, e.Kind)
		}
		eid := strings.TrimSpace(e.EventID)
		if eid == "" {
			return fmt.Errorf("goalrun: MU resume merge: empty event_id kind=%s attempt=%s", e.Kind, att)
		}
		key := e.Kind + "@" + att
		if prev, ok := seenEventIDs[eid]; ok {
			return fmt.Errorf("goalrun: MU resume merge: duplicate event_id %q (%s and %s)", eid, prev, key)
		}
		seenEventIDs[eid] = key

		row := pair.Failed
		if att == rAtt {
			row = pair.Retry
		}
		if err := requireEventIdentityStamps(e, projectID, runID, planDigest, graphDigest, graphID, graphVersion, row); err != nil {
			return fmt.Errorf("goalrun: MU resume merge kind=%s: %w", e.Kind, err)
		}
		kind := strings.TrimSpace(e.Kind)
		if att == fAtt {
			gotFailed[kind]++
			if kind == "terminal" {
				if !strings.EqualFold(strings.TrimSpace(e.Terminal), "failed") {
					return fmt.Errorf("goalrun: MU resume merge: failed attempt terminal %q want failed", e.Terminal)
				}
			}
			if kind == "model_unavailable" {
				if !strings.EqualFold(strings.TrimSpace(e.FailureClass), "model_unavailable") {
					return fmt.Errorf("goalrun: MU resume merge: MU event failure_class %q", e.FailureClass)
				}
				if muEventID != "" {
					return fmt.Errorf("goalrun: MU resume merge: multiple model_unavailable events for %s", fAtt)
				}
				muEventID = eid
			}
			if kind == "integrate" {
				return fmt.Errorf("goalrun: MU resume merge: failed attempt must not integrate")
			}
		}
		if att == rAtt {
			gotRetry[kind]++
			if kind == "claim" {
				if retryClaimEventID != "" {
					return fmt.Errorf("goalrun: MU resume merge: multiple claim events for retry %s", rAtt)
				}
				retryClaimEventID = eid
			}
			if kind == "terminal" {
				if !strings.EqualFold(strings.TrimSpace(e.Terminal), "succeeded") {
					return fmt.Errorf("goalrun: MU resume merge: retry terminal %q want succeeded", e.Terminal)
				}
			}
			if kind == "integrate" {
				if strings.TrimSpace(e.CommitSHA) == "" {
					return fmt.Errorf("goalrun: MU resume merge: integrate event missing commit_sha")
				}
			}
		}
	}
	for k, want := range needFailed {
		if gotFailed[k] != want {
			return fmt.Errorf("goalrun: MU resume merge: failed attempt %s kind %s count=%d want %d", fAtt, k, gotFailed[k], want)
		}
	}
	for k, want := range needRetry {
		if gotRetry[k] != want {
			return fmt.Errorf("goalrun: MU resume merge: retry attempt %s kind %s count=%d want %d", rAtt, k, gotRetry[k], want)
		}
	}
	if muEventID == "" {
		return fmt.Errorf("goalrun: MU resume merge: failed model_unavailable event_id not captured")
	}
	if retryClaimEventID == "" {
		return fmt.Errorf("goalrun: MU resume merge: retry claim event_id not captured")
	}
	if muEventID == retryClaimEventID {
		return fmt.Errorf("goalrun: MU resume merge: MU and claim event_id must be distinct")
	}

	// Reroute payload must reference the exact captured EventIDs.
	rrEv, ok := findEventKindAttempt(events, "reroute", rAtt)
	if !ok {
		return fmt.Errorf("goalrun: MU resume merge: reroute event missing for retry %s", rAtt)
	}
	if err := requireReroutePayloadExact(rrEv, wi, fAtt, rAtt, muEventID, retryClaimEventID); err != nil {
		return err
	}
	return nil
}

func requireEventIdentityStamps(
	e workflowrun.Event,
	projectID, runID, planDigest, graphDigest, graphID string,
	graphVersion int,
	row workflowrun.ChildOutcome,
) error {
	if !idExact(e.ProjectID, projectID) || !idExact(e.RunID, runID) {
		return fmt.Errorf("project/run stamp mismatch")
	}
	if strings.TrimSpace(e.ExecutionPlanDigest) == "" || e.ExecutionPlanDigest != planDigest {
		return fmt.Errorf("plan_digest required exact got %q want %q", e.ExecutionPlanDigest, planDigest)
	}
	if strings.TrimSpace(graphDigest) == "" {
		return fmt.Errorf("graph_digest required nonempty")
	}
	if strings.TrimSpace(e.GraphDigest) == "" || e.GraphDigest != graphDigest {
		return fmt.Errorf("graph_digest required exact got %q want %q", e.GraphDigest, graphDigest)
	}
	if strings.TrimSpace(graphID) == "" {
		return fmt.Errorf("graph_id required nonempty")
	}
	if strings.TrimSpace(e.GraphID) == "" || e.GraphID != graphID {
		return fmt.Errorf("graph_id required exact got %q want %q", e.GraphID, graphID)
	}
	if graphVersion <= 0 {
		return fmt.Errorf("graph_version required positive")
	}
	if e.GraphVersion != graphVersion {
		return fmt.Errorf("graph_version required exact got %d want %d", e.GraphVersion, graphVersion)
	}
	wantClass := strings.TrimSpace(row.TaskClass)
	if wantClass == "" || strings.TrimSpace(e.TaskClass) != wantClass {
		return fmt.Errorf("task_class required exact got %q want %q", e.TaskClass, wantClass)
	}
	wantCCD := strings.TrimSpace(row.ChildContractDigest)
	if wantCCD == "" || strings.TrimSpace(e.ChildContractDigest) != wantCCD {
		return fmt.Errorf("child_contract_digest required exact mismatch")
	}
	if strings.TrimSpace(e.AttemptID) == "" {
		return fmt.Errorf("attempt_id required")
	}
	wantGen, err := workflowrun.ClaimGenerationFromAttemptID(e.AttemptID)
	if err != nil {
		return err
	}
	if e.Generation != wantGen {
		return fmt.Errorf("generation %d != claim gen %d", e.Generation, wantGen)
	}
	if strings.TrimSpace(e.EventID) == "" {
		return fmt.Errorf("event_id required nonempty")
	}
	return nil
}

// requireReroutePayloadExact requires structured JSON with exact supersedes,
// retry, work_item, and the exact captured MU + claim event IDs.
func requireReroutePayloadExact(e workflowrun.Event, wi, failedAtt, retryAtt, muEventID, claimEventID string) error {
	if len(e.Payload) == 0 {
		return fmt.Errorf("goalrun: MU resume merge: reroute payload empty")
	}
	var m map[string]string
	if err := json.Unmarshal(e.Payload, &m); err != nil {
		return fmt.Errorf("goalrun: MU resume merge: reroute payload JSON: %w", err)
	}
	if m["work_item_id"] != wi {
		return fmt.Errorf("goalrun: MU resume merge: reroute work_item_id %q != %q", m["work_item_id"], wi)
	}
	if m["supersedes_attempt_id"] != failedAtt {
		return fmt.Errorf("goalrun: MU resume merge: reroute supersedes_attempt_id %q != %q", m["supersedes_attempt_id"], failedAtt)
	}
	if m["retry_attempt_id"] != retryAtt {
		return fmt.Errorf("goalrun: MU resume merge: reroute retry_attempt_id %q != %q", m["retry_attempt_id"], retryAtt)
	}
	if m["model_unavailable_event_id"] != muEventID {
		return fmt.Errorf("goalrun: MU resume merge: reroute model_unavailable_event_id %q != persisted %q",
			m["model_unavailable_event_id"], muEventID)
	}
	if m["claim_event_id"] != claimEventID {
		return fmt.Errorf("goalrun: MU resume merge: reroute claim_event_id %q != persisted %q",
			m["claim_event_id"], claimEventID)
	}
	return nil
}

func findEventKindAttempt(events []workflowrun.Event, kind, attempt string) (workflowrun.Event, bool) {
	for _, e := range events {
		if strings.EqualFold(strings.TrimSpace(e.Kind), kind) && idExact(e.AttemptID, attempt) {
			return e, true
		}
	}
	return workflowrun.Event{}, false
}

// mergeMUPairIntoChildren merges by exact AttemptID using whole-struct equality.
func mergeMUPairIntoChildren(wf *workflowrun.Result, pair muDurablePair) error {
	if wf == nil {
		return fmt.Errorf("goalrun: MU resume merge: nil workflow")
	}
	upsert := func(row workflowrun.ChildOutcome) error {
		att := strings.TrimSpace(row.AttemptID)
		for i := range wf.Children {
			if !idExact(wf.Children[i].AttemptID, att) {
				continue
			}
			if childOutcomeExactEqual(wf.Children[i], row) {
				return nil
			}
			return fmt.Errorf("goalrun: MU resume merge: conflicting outcome for attempt %s (whole-struct equality required)", att)
		}
		wf.Children = append(wf.Children, row)
		return nil
	}
	if err := upsert(pair.Failed); err != nil {
		return err
	}
	return upsert(pair.Retry)
}

// childOutcomeExactEqual is true whole-struct equality (all fields, slices,
// pointers). PriorSucceeded is copied as-is — any difference fails closed.
func childOutcomeExactEqual(a, b workflowrun.ChildOutcome) bool {
	return reflect.DeepEqual(a, b)
}

// capacitySeedFromCheckpoint requires exactly two checkpoint transitions:
// one prior and one alternate matching the pair attempt IDs.
func capacitySeedFromCheckpoint(cp Checkpoint, pair muDurablePair) ([]workflowrun.CapacityTransition, error) {
	if len(cp.CapacityTransitions) != 2 {
		return nil, fmt.Errorf("goalrun: MU resume merge: checkpoint CapacityTransitions len=%d want exactly 2", len(cp.CapacityTransitions))
	}
	var prior, alt workflowrun.CapacityTransition
	for _, tr := range cp.CapacityTransitions {
		role := strings.TrimSpace(tr.Role)
		att := strings.TrimSpace(tr.AttemptID)
		if role == "" || att == "" {
			return nil, fmt.Errorf("goalrun: MU resume merge: checkpoint transition missing role/attempt")
		}
		switch role {
		case "prior":
			if prior.AttemptID != "" {
				return nil, fmt.Errorf("goalrun: MU resume merge: duplicate prior transition")
			}
			// Keep Permission for finalizer (ledger Entry has no permission field).
			prior = workflowrun.CapacityTransition{Role: "prior", AttemptID: att, Permission: strings.TrimSpace(tr.Permission)}
		case "alternate":
			if alt.AttemptID != "" {
				return nil, fmt.Errorf("goalrun: MU resume merge: duplicate alternate transition")
			}
			alt = workflowrun.CapacityTransition{Role: "alternate", AttemptID: att, Permission: strings.TrimSpace(tr.Permission)}
		default:
			return nil, fmt.Errorf("goalrun: MU resume merge: unknown checkpoint transition role %q", role)
		}
	}
	if prior.AttemptID == "" || alt.AttemptID == "" {
		return nil, fmt.Errorf("goalrun: MU resume merge: checkpoint missing prior or alternate transition")
	}
	if !idExact(prior.AttemptID, pair.Failed.AttemptID) {
		return nil, fmt.Errorf("goalrun: MU resume merge: checkpoint prior %q != failed %q", prior.AttemptID, pair.Failed.AttemptID)
	}
	if !idExact(alt.AttemptID, pair.Retry.AttemptID) {
		return nil, fmt.Errorf("goalrun: MU resume merge: checkpoint alternate %q != retry %q", alt.AttemptID, pair.Retry.AttemptID)
	}
	// Fallback: pair outcomes carry permission when checkpoint mid-seed omitted it.
	if prior.Permission == "" {
		prior.Permission = strings.TrimSpace(pair.Failed.Permission)
	}
	if alt.Permission == "" {
		alt.Permission = strings.TrimSpace(pair.Retry.Permission)
	}
	if prior.Permission == "" || alt.Permission == "" {
		return nil, fmt.Errorf("goalrun: MU resume merge: permission required on prior/alternate seed (non-qualifying without it)")
	}
	return []workflowrun.CapacityTransition{prior, alt}, nil
}

func validateLedgerMUCapacity(trs []workflowrun.CapacityTransition) error {
	if len(trs) != 2 {
		return fmt.Errorf("goalrun: MU capacity want 2 transitions, got %d", len(trs))
	}
	var prior, alt workflowrun.CapacityTransition
	for _, tr := range trs {
		switch strings.TrimSpace(tr.Role) {
		case "prior":
			prior = tr
		case "alternate":
			alt = tr
		default:
			return fmt.Errorf("goalrun: MU capacity unknown role %q", tr.Role)
		}
	}
	for _, tr := range []workflowrun.CapacityTransition{prior, alt} {
		st := strings.ToLower(strings.TrimSpace(tr.State))
		if st != "released" && st != "reconciled" {
			return fmt.Errorf("goalrun: MU capacity attempt %s state %q want released|reconciled", tr.AttemptID, tr.State)
		}
		if strings.TrimSpace(tr.ReservationID) == "" {
			return fmt.Errorf("goalrun: MU capacity attempt %s missing reservation_id", tr.AttemptID)
		}
		if strings.TrimSpace(tr.Provider) == "" || strings.TrimSpace(tr.Model) == "" ||
			strings.TrimSpace(tr.Depth) == "" || strings.TrimSpace(tr.AccountRef) == "" ||
			strings.TrimSpace(tr.WindowKind) == "" || strings.TrimSpace(tr.InstallRef) == "" ||
			strings.TrimSpace(tr.Permission) == "" {
			return fmt.Errorf("goalrun: MU capacity attempt %s incomplete identity (need provider/model/depth/permission/account/install/window)", tr.AttemptID)
		}
	}
	if strings.TrimSpace(prior.ReservationID) == strings.TrimSpace(alt.ReservationID) {
		return fmt.Errorf("goalrun: MU capacity duplicate reservation_id %s", prior.ReservationID)
	}
	if idExact(prior.AttemptID, alt.AttemptID) {
		return fmt.Errorf("goalrun: MU capacity same attempt on prior and alternate")
	}
	return nil
}
