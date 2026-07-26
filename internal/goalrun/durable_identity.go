package goalrun

import (
	"fmt"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/workflowrun"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

// Durable identity helpers for resume / checkpoint / partial / PriorOutcomes /
// event / execution-authority / ledger / recovery boundaries. Never normalize an
// existing durable identity into authority (no TrimSpace/EqualFold rewrite).
// Fail closed before inventory / ledger / claim / reserve / launch.

// idExact is byte-exact identity equality (no TrimSpace). Empty equals empty.
func idExact(a, b string) bool {
	return a == b
}

// requireExactDurableToken rejects empty or whitespace-padded durable tokens.
func requireExactDurableToken(label, s string) error {
	if s == "" {
		return fmt.Errorf("goalrun: durable %s empty (fail closed)", label)
	}
	if s != strings.TrimSpace(s) {
		return fmt.Errorf("goalrun: durable %s has surrounding whitespace %q (byte-nonexact; fail closed)", label, s)
	}
	return nil
}

// requireExactSucceededTerminal requires exact workgraph.TermSucceeded.
func requireExactSucceededTerminal(label, term string) error {
	if term != string(workgraph.TermSucceeded) {
		return fmt.Errorf("goalrun: %s terminal %q != exact succeeded (fail closed)", label, term)
	}
	return nil
}

// requireExactCanonicalTerminalToken requires exact succeeded|failed|cancelled|skipped.
func requireExactCanonicalTerminalToken(label, term string) error {
	if !exactCanonicalTerminal(term) {
		return fmt.Errorf("goalrun: %s terminal %q not exact succeeded|failed|cancelled|skipped", label, term)
	}
	return nil
}

// isResumeEligibleExact is the audit/product eligibility gate: exact succeeded
// terminal, exact nonempty attempt_id and evidence (no padding, no EqualFold).
func isResumeEligibleExact(terminal, attemptID, evidence string) bool {
	if terminal != string(workgraph.TermSucceeded) {
		return false
	}
	if attemptID == "" || attemptID != strings.TrimSpace(attemptID) {
		return false
	}
	if evidence == "" || evidence != strings.TrimSpace(evidence) {
		return false
	}
	return true
}

// isExactMUFailureClass is the only model_unavailable durable class token.
func isExactMUFailureClass(fc string) bool {
	return fc == "model_unavailable"
}

// isExactFinalTerminal is exact succeeded|failed|cancelled|skipped (no canceled/aborted aliases).
func isExactFinalTerminal(term string) bool {
	return exactCanonicalTerminal(term)
}

// exactGenericFailedClasses enumerates allowed failure_class values on terminal=failed
// that are not model_unavailable and not typed abort. Empty class is rejected.
// Production workflowrun/child_executor classes only — no case/space aliases.
var exactGenericFailedClasses = map[string]bool{
	"executor_error": true, "missing_terminal": true, "route_identity_mismatch": true,
	"interrupt_pid_mismatch": true, "interrupt_event_failed": true,
	"executor_cancelled": true, "missing_evidence": true,
	"acceptance_failed": true, "integrate_failed": true, "integrate_event_failed": true,
	"terminal_event_failed": true, "worktree": true, "worktree_lease": true,
	"evidence": true, "invocation_binding": true, "injected_fail": true,
	"route_incomplete": true, "route_unresolved": true, "fixture_forbidden": true,
	"unsupported_provider": true, "process_failure": true, "nonzero_exit": true,
	"isolation_violation": true, "capacity_refused": true, "event_log": true,
	"hard_kill_recovery": true, // hard recovery may close as failed
}

// requireTerminalFailureSemantics enforces the exact terminal/failure_class table.
func requireTerminalFailureSemantics(term, fc string) error {
	if err := requireExactCanonicalTerminalToken("terminal", term); err != nil {
		return err
	}
	if fc != "" && fc != strings.TrimSpace(fc) {
		return fmt.Errorf("goalrun: failure_class has whitespace padding %q", fc)
	}
	switch term {
	case string(workgraph.TermSucceeded):
		if fc != "" {
			return fmt.Errorf("goalrun: succeeded must have empty failure_class, got %q", fc)
		}
	case string(workgraph.TermCancelled):
		if !exactTypedAbortClass(fc) {
			return fmt.Errorf("goalrun: cancelled requires exact typed abort class, got %q", fc)
		}
	case string(workgraph.TermSkipped):
		if fc != "" {
			return fmt.Errorf("goalrun: skipped must have empty failure_class, got %q", fc)
		}
	case string(workgraph.TermFailed):
		if isExactMUFailureClass(fc) {
			return nil
		}
		if exactTypedAbortClass(fc) {
			return fmt.Errorf("goalrun: typed abort class %q requires cancelled terminal, got failed", fc)
		}
		if fc == "" || !exactGenericFailedClasses[fc] {
			return fmt.Errorf("goalrun: failed failure_class %q not in exact allowed generic set", fc)
		}
	}
	return nil
}

// requirePersistedOutcomeRuntimeIdentity requires a complete durable ChildOutcome
// bind to current plan/graph identity before report projection or reuse.
func requirePersistedOutcomeRuntimeIdentity(co workflowrun.ChildOutcome, id lifecycleBindIdentity, wantWI, wantAtt string) error {
	if err := requireExactDurableToken("work_item_id", co.WorkItemID); err != nil {
		return err
	}
	if err := requireExactDurableToken("attempt_id", co.AttemptID); err != nil {
		return err
	}
	if wantWI != "" && co.WorkItemID != wantWI {
		return fmt.Errorf("goalrun: outcome work_item %q != want %q", co.WorkItemID, wantWI)
	}
	if wantAtt != "" && co.AttemptID != wantAtt {
		return fmt.Errorf("goalrun: outcome attempt %q != want %q", co.AttemptID, wantAtt)
	}
	if err := requireTerminalFailureSemantics(co.Terminal, co.FailureClass); err != nil {
		return err
	}
	if err := requireExactDurableToken("task_class", co.TaskClass); err != nil {
		return err
	}
	if err := requireExactDurableToken("child_contract_digest", co.ChildContractDigest); err != nil {
		return err
	}
	if co.ChildContractDigest != strings.ToLower(co.ChildContractDigest) {
		return fmt.Errorf("goalrun: child_contract_digest not lowercase canonical %q", co.ChildContractDigest)
	}
	if co.Generation < 1 {
		return fmt.Errorf("goalrun: outcome generation %d < 1", co.Generation)
	}
	if id.PlanDigest == "" || co.ExecutionPlanDigest != id.PlanDigest {
		return fmt.Errorf("goalrun: outcome plan_digest %q != current %q", co.ExecutionPlanDigest, id.PlanDigest)
	}
	if id.ProjectID == "" || id.RunID == "" {
		return fmt.Errorf("goalrun: lifecycle identity missing project/run")
	}
	if err := requireExactDurableToken("project_id", id.ProjectID); err != nil {
		return err
	}
	if err := requireExactDurableToken("run_id", id.RunID); err != nil {
		return err
	}
	if err := requireExactDurableToken("plan_digest", id.PlanDigest); err != nil {
		return err
	}
	if err := requireExactDurableToken("graph_digest", id.GraphDigest); err != nil {
		return err
	}
	if err := requireExactDurableToken("graph_id", id.GraphID); err != nil {
		return err
	}
	if id.GraphVersion <= 0 {
		return fmt.Errorf("goalrun: graph_version %d <= 0", id.GraphVersion)
	}
	wantCanon := workflowrun.AttemptID(co.WorkItemID, id.PlanDigest, id.RunID, co.Generation-1)
	if co.AttemptID != wantCanon {
		return fmt.Errorf("goalrun: attempt %q != canonical %q", co.AttemptID, wantCanon)
	}
	return nil
}

// requireExactEventLogPathStamp rejects empty/padded EventLogPath.
func requireExactEventLogPathStamp(label, path string) error {
	if path == "" {
		return fmt.Errorf("goalrun: %s EventLogPath empty (fail closed)", label)
	}
	if path != strings.TrimSpace(path) {
		return fmt.Errorf("goalrun: %s EventLogPath has surrounding whitespace (fail closed)", label)
	}
	return nil
}

// exactDurableFieldMatch requires nonempty want and byte-exact equality when have set.
// Empty have is allowed (populate path); conflict when both set and unequal.
func exactDurableFieldMatch(field, have, want, att string) error {
	if want == "" {
		return fmt.Errorf("goalrun: attempt %s empty durable want %s", att, field)
	}
	if want != strings.TrimSpace(want) {
		return fmt.Errorf("goalrun: attempt %s want %s has whitespace padding", att, field)
	}
	if have != "" {
		if have != strings.TrimSpace(have) {
			return fmt.Errorf("goalrun: attempt %s have %s has whitespace padding", att, field)
		}
		if have != want {
			return fmt.Errorf("goalrun: attempt %s %s conflict report=%q durable=%q", att, field, have, want)
		}
	}
	return nil
}
