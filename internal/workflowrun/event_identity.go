package workflowrun

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

// Typed interrupt identity (distinct classes — never conflated).
const (
	// InterruptClassServiceForced is Service context-cancel / forced abort.
	// Must pair with cancelled terminal carrying the same class. Does NOT select gN+1
	// via hard-recovery generation rules (AbortedAttempts handles resume).
	InterruptClassServiceForced = "service_forced_interrupt"
	// InterruptClassHardKillRecovery is authoritative hard-kill recovery.
	InterruptClassHardKillRecovery = "hard_kill_recovery"
)

// FailureClassExecutorCancelled is executor-local cancellation without a Service
// forced_interrupt emission. Distinct from forced_interrupt / service_forced_interrupt;
// never carries interrupt_id and never pairs with a child interrupt event.
const FailureClassExecutorCancelled = "executor_cancelled"

// FailureClassResearchFindingsMaterialization is returned when a research child
// run succeeded at the provider layer but LoopCoder could not safely materialize
// findings.md (directory dest, rename/git-add failure, path safety). Distinct
// from generic missing_evidence so operators see the real cause.
const FailureClassResearchFindingsMaterialization = "research_findings_materialization_failed"

// FailureClassVerifierVerdictMaterialization is returned when a verify child
// succeeded at the provider layer but LoopCoder could not safely materialize
// verdict.md (same safety contract as research findings).
const FailureClassVerifierVerdictMaterialization = "verifier_verdict_materialization_failed"

// ChildLifecycleKinds are event kinds that bind a child work item to an attempt.
// Parent-level interrupt is the only interrupt form that may omit all child identity.
var ChildLifecycleKinds = map[string]bool{
	"claim": true, "launch": true, "pid": true, "terminal": true,
	"reuse": true, "integrate": true, "model_unavailable": true,
	"reroute": true, "interrupt": true,
}

// IsParentInterrupt reports a true parent-level cancel line: interrupt with
// empty WorkItemID, empty AttemptID, and Generation exactly 0 (not negative).
func IsParentInterrupt(ev Event) bool {
	if strings.TrimSpace(ev.Kind) != "interrupt" {
		return false
	}
	return strings.TrimSpace(ev.WorkItemID) == "" &&
		strings.TrimSpace(ev.AttemptID) == "" &&
		ev.Generation == 0
}

// ClaimGenerationFromAttemptID maps 0-indexed attempt suffix -gN to the
// 1-indexed claim/event Generation (N+1). Returns error when suffix absent.
func ClaimGenerationFromAttemptID(attemptID string) (int, error) {
	g := ParseAttemptGeneration(attemptID)
	if g < 0 {
		return 0, fmt.Errorf("workflowrun: attempt_id %q missing -gN suffix", attemptID)
	}
	return g + 1, nil
}

// ValidateChildEventIdentity enforces full child-lifecycle identity without a
// plan/run binding check: nonempty WorkItemID + AttemptID, positive Generation
// equal to ParseAttemptGeneration(AttemptID)+1. Parent interrupts are allowed
// only with Generation exactly 0. Non-interrupt child events with both IDs
// empty fail closed. Negative generation on interrupt with empty IDs is rejected.
func ValidateChildEventIdentity(ev Event) error {
	kind := strings.TrimSpace(ev.Kind)
	if !ChildLifecycleKinds[kind] {
		return nil
	}
	id := strings.TrimSpace(ev.WorkItemID)
	att := strings.TrimSpace(ev.AttemptID)
	if kind == "interrupt" && id == "" && att == "" {
		if ev.Generation < 0 {
			return fmt.Errorf("workflowrun: parent interrupt rejects negative generation %d", ev.Generation)
		}
		if ev.Generation != 0 {
			return fmt.Errorf("workflowrun: parent interrupt requires generation==0, got %d", ev.Generation)
		}
		return nil // IsParentInterrupt
	}
	if IsParentInterrupt(ev) {
		return nil
	}
	// Non-interrupt empty both IDs: fail closed (silent skip is forbidden).
	if id == "" && att == "" {
		return fmt.Errorf("workflowrun: child event kind=%s missing work_item_id and attempt_id (fail closed)", kind)
	}
	if id == "" {
		return fmt.Errorf("workflowrun: child event kind=%s missing work_item_id", kind)
	}
	if att == "" {
		// Child interrupt with only WorkItemID is partial identity — fail closed.
		return fmt.Errorf("workflowrun: child event kind=%s work_item_id=%q missing attempt_id (fail closed)", kind, id)
	}
	wantGen, err := ClaimGenerationFromAttemptID(att)
	if err != nil {
		return fmt.Errorf("workflowrun: child event kind=%s %s: %w", kind, id, err)
	}
	if ev.Generation <= 0 {
		return fmt.Errorf("workflowrun: child event kind=%s %s attempt=%s missing positive generation", kind, id, att)
	}
	if ev.Generation != wantGen {
		return fmt.Errorf("workflowrun: child event kind=%s %s generation=%d != attempt suffix+1=%d (attempt=%s)",
			kind, id, ev.Generation, wantGen, att)
	}
	return nil
}

// ValidateEventStreamInvariants fails closed on impossible exactly-once states:
// concurrent open attempts for one work item (detected at the moment a new
// launch arrives), duplicate launch of the same exact attempt, launch/pid
// reopening a terminal or interrupted attempt, second launch while another
// attempt is still open, or a second pid for the same exact work_item/attempt
// (identical or divergent payload). pid may follow its one matching launch once.
func ValidateEventStreamInvariants(events []Event) error {
	// closed: attempt finalized (terminal|reuse|integrate) — blocks relaunch/pid/interrupt.
	closed := map[string]bool{}
	// terminalKind: exact kind=terminal lines (exactly-once, even byte-identical).
	terminalKind := map[string]bool{}
	// interruptedAt: typed child interrupt only — requires matching typed terminal pair.
	interruptedAt := map[string]bool{}
	// parentInterruptedAt: wave-level parent interrupt closed the open set without a
	// typed child class. Allows a subsequent normal/cancelled terminal; still blocks
	// relaunch/pid (attempt is no longer open).
	parentInterruptedAt := map[string]bool{}
	// interruptClass: typed class of the open child interrupt for matching terminal pairs.
	interruptClass := map[string]string{}
	// interruptEvents: prior typed child interrupt for interrupt_id pair matching.
	interruptEvents := map[string]Event{}
	// openKeys: currently open attempts; launchedKeys: launch seen (pid allowed after).
	openKeys := map[string]bool{}
	launchedKeys := map[string]bool{}
	// pidKeys: exact work_item/attempt already has a durable pid event.
	pidKeys := map[string]bool{}
	// openByItem: workItemID → set of open attempt IDs (for concurrent detection).
	openByItem := map[string]map[string]bool{}

	for i, ev := range events {
		kind := strings.TrimSpace(ev.Kind)
		id := strings.TrimSpace(ev.WorkItemID)
		att := strings.TrimSpace(ev.AttemptID)
		switch kind {
		case "launch":
			if id == "" || att == "" {
				continue
			}
			k := attemptKey(id, att)
			if closed[k] {
				return fmt.Errorf("workflowrun: event stream line %d reopens terminal attempt %s/%s (fail closed)", i, id, att)
			}
			if interruptedAt[k] || parentInterruptedAt[k] {
				return fmt.Errorf("workflowrun: event stream line %d reopens interrupted attempt %s/%s (fail closed)", i, id, att)
			}
			if launchedKeys[k] {
				return fmt.Errorf("workflowrun: event stream line %d duplicate launch of exact attempt %s/%s (fail closed)", i, id, att)
			}
			// Concurrent open: another attempt for this work item still open.
			if others, ok := openByItem[id]; ok {
				for oatt := range others {
					if oatt != att {
						return fmt.Errorf("workflowrun: event stream line %d launch %s/%s while attempt %s still open (exactly-once violation; fail closed)",
							i, id, att, oatt)
					}
				}
			}
			launchedKeys[k] = true
			openKeys[k] = true
			if openByItem[id] == nil {
				openByItem[id] = map[string]bool{}
			}
			openByItem[id][att] = true
		case "pid":
			// Exactly one pid per exact work_item/attempt after its launch.
			if id == "" || att == "" {
				continue
			}
			k := attemptKey(id, att)
			if !launchedKeys[k] {
				return fmt.Errorf("workflowrun: event stream line %d pid without prior launch for %s/%s (fail closed)", i, id, att)
			}
			if closed[k] {
				return fmt.Errorf("workflowrun: event stream line %d pid after terminal for %s/%s (fail closed)", i, id, att)
			}
			if interruptedAt[k] || parentInterruptedAt[k] {
				return fmt.Errorf("workflowrun: event stream line %d pid after interrupt for %s/%s (fail closed)", i, id, att)
			}
			if pidKeys[k] {
				return fmt.Errorf("workflowrun: event stream line %d duplicate pid for exact attempt %s/%s (fail closed)", i, id, att)
			}
			pidKeys[k] = true
			// pid does not open a new attempt; leave openKeys as-is.
		case "terminal":
			if id == "" || att == "" {
				continue
			}
			k := attemptKey(id, att)
			// Exactly-once kind=terminal (even byte-identical). integrate/reuse do not count.
			if terminalKind[k] {
				return fmt.Errorf("workflowrun: event stream line %d duplicate child terminal for %s/%s (fail closed)", i, id, att)
			}
			// After typed child interrupt: only exact matching typed pair is legal.
			//   service_forced_interrupt → cancelled terminal with same class
			//   hard_kill_recovery → authoritative hard-recovery terminal
			// Untyped/mismatched pairs are corruption.
			// Parent-wave interrupt alone does not require a typed pair.
			if interruptedAt[k] {
				if err := validateTypedTerminalAfterInterrupt(ev, interruptEvents[k], interruptClass[k]); err != nil {
					return fmt.Errorf("workflowrun: event stream line %d %s for %s/%s (fail closed)", i, err.Error(), id, att)
				}
			}
			terminalKind[k] = true
			closed[k] = true
			delete(openKeys, k)
			delete(parentInterruptedAt, k)
			if openByItem[id] != nil {
				delete(openByItem[id], att)
			}
		case "reuse", "integrate":
			if id == "" || att == "" {
				continue
			}
			k := attemptKey(id, att)
			// Reject reuse/integrate after a typed child interrupt (must pair terminal first).
			if interruptedAt[k] {
				return fmt.Errorf("workflowrun: event stream line %d %s after typed child interrupt for %s/%s (fail closed)", i, kind, id, att)
			}
			closed[k] = true
			delete(openKeys, k)
			delete(parentInterruptedAt, k)
			if openByItem[id] != nil {
				delete(openByItem[id], att)
			}
		case "interrupt":
			if IsParentInterrupt(ev) {
				for k := range openKeys {
					// Wave-level only — not a typed child interrupt class.
					parentInterruptedAt[k] = true
					wid, aatt := splitAttemptKey(k)
					if openByItem[wid] != nil {
						delete(openByItem[wid], aatt)
					}
					delete(openKeys, k)
				}
				continue
			}
			if id == "" || att == "" {
				continue
			}
			k := attemptKey(id, att)
			// Reject child interrupt after terminal/reuse/integrate; reject duplicate interrupt.
			if closed[k] || terminalKind[k] {
				return fmt.Errorf("workflowrun: event stream line %d child interrupt after terminal for %s/%s (fail closed)", i, id, att)
			}
			if interruptedAt[k] {
				return fmt.Errorf("workflowrun: event stream line %d duplicate child interrupt for %s/%s (fail closed)", i, id, att)
			}
			cls := childInterruptClass(ev)
			if cls == "" {
				return fmt.Errorf("workflowrun: event stream line %d untyped child interrupt for %s/%s (fail closed)", i, id, att)
			}
			if strings.TrimSpace(eventPayloadString(ev, "interrupt_id")) == "" {
				return fmt.Errorf("workflowrun: event stream line %d child interrupt missing interrupt_id for %s/%s (fail closed)", i, id, att)
			}
			interruptedAt[k] = true
			interruptClass[k] = cls
			interruptEvents[k] = ev
			delete(parentInterruptedAt, k) // child typed class supersedes parent wave mark
			delete(openKeys, k)
			if openByItem[id] != nil {
				delete(openByItem[id], att)
			}
		}
	}
	return nil
}

// childInterruptClass returns the typed interrupt identity for a child interrupt event.
// Empty means untyped (not allowed for durable child interrupts).
//
// Classes are distinct and require complete structured payloads:
//   - hard_kill_recovery: only complete authoritative hard recovery
//   - service_forced_interrupt: only complete Service forced-cancel payload
func childInterruptClass(ev Event) string {
	if isAuthoritativeHardRecoveryEvent(ev) {
		return InterruptClassHardKillRecovery
	}
	if isServiceForcedInterruptEvent(ev) {
		return InterruptClassServiceForced
	}
	return ""
}

// isServiceForcedInterruptEvent reports Service forced-cancel interrupt/terminal
// with complete structured payload (failure_class + interrupt_class + interrupt_id).
// Distinct from hard recovery; never selects hard-recovery gN+1.
func isServiceForcedInterruptEvent(ev Event) bool {
	kind := strings.TrimSpace(ev.Kind)
	if kind != "interrupt" && kind != "terminal" {
		return false
	}
	if isAuthoritativeHardRecoveryEvent(ev) {
		return false
	}
	if len(ev.Payload) == 0 {
		return false
	}
	fc := eventFailureClass(ev)
	if fc != "forced_interrupt" {
		return false
	}
	if eventPayloadString(ev, "interrupt_class") != InterruptClassServiceForced {
		return false
	}
	if strings.TrimSpace(eventPayloadString(ev, "interrupt_id")) == "" {
		return false
	}
	if kind == "terminal" {
		term := strings.TrimSpace(firstNonEmpty(ev.Terminal, eventPayloadString(ev, "terminal")))
		if !strings.EqualFold(term, string(workgraph.TermCancelled)) {
			return false
		}
	}
	return true
}

func validateTypedTerminalAfterInterrupt(term, priorInterrupt Event, intClass string) error {
	// Full pair identity: interrupt_id, class, work/attempt/gen, terminal semantics.
	if err := validateInterruptTerminalPair(priorInterrupt, term); err != nil {
		return err
	}
	switch intClass {
	case InterruptClassHardKillRecovery:
		if !isAuthoritativeHardRecoveryEvent(term) {
			return fmt.Errorf("terminal after hard_kill_recovery interrupt must be authoritative hard-recovery terminal")
		}
		return nil
	case InterruptClassServiceForced:
		if !isServiceForcedInterruptEvent(term) {
			return fmt.Errorf("terminal after service_forced_interrupt must be matching cancelled forced_interrupt terminal")
		}
		if !strings.EqualFold(strings.TrimSpace(term.Terminal), string(workgraph.TermCancelled)) {
			return fmt.Errorf("service forced_interrupt terminal must be cancelled")
		}
		return nil
	default:
		return fmt.Errorf("terminal after untyped/mismatched interrupt (class=%q)", intClass)
	}
}

// eventFailureClass returns the canonical structured failure class.
// Preference: top-level FailureClass, then payload failure_class. Never Message.
func eventFailureClass(ev Event) string {
	if fc := strings.TrimSpace(ev.FailureClass); fc != "" {
		return fc
	}
	return eventPayloadString(ev, "failure_class")
}

func eventPayloadString(ev Event, key string) string {
	if len(ev.Payload) == 0 {
		return ""
	}
	var m map[string]string
	if err := json.Unmarshal(ev.Payload, &m); err != nil {
		return ""
	}
	return strings.TrimSpace(m[key])
}

// ValidateExistingEventLogForPlan reads raw events and enforces child identity
// + stream invariants against the current graph/plan/run. Does not mutate.
func ValidateExistingEventLogForPlan(events []Event, planDigest, runID string, itemOK map[string]bool) error {
	for i, ev := range events {
		if err := ValidateChildEventIdentityForPlan(ev, planDigest, runID, itemOK); err != nil {
			return fmt.Errorf("event log line %d: %w", i, err)
		}
	}
	if err := ValidateEventStreamInvariants(events); err != nil {
		return err
	}
	return nil
}

// ValidateChildEventIdentityForPlan extends ValidateChildEventIdentity with
// current-graph membership and canonical AttemptID(workItem, plan, run, g).
func ValidateChildEventIdentityForPlan(ev Event, planDigest, runID string, itemOK map[string]bool) error {
	if err := ValidateChildEventIdentity(ev); err != nil {
		return err
	}
	if IsParentInterrupt(ev) {
		return nil
	}
	kind := strings.TrimSpace(ev.Kind)
	if !ChildLifecycleKinds[kind] {
		return nil
	}
	id := strings.TrimSpace(ev.WorkItemID)
	att := strings.TrimSpace(ev.AttemptID)
	if itemOK != nil && !itemOK[id] {
		return fmt.Errorf("workflowrun: child event ghost work_item_id %q kind=%s not in current graph", id, kind)
	}
	g := ParseAttemptGeneration(att)
	want := AttemptID(id, planDigest, runID, g)
	if att != want {
		return fmt.Errorf("workflowrun: child event %s attempt_id %q != canonical %q", id, att, want)
	}
	return nil
}

// attemptKey is the exact work-item+attempt durable identity for recovery.
func attemptKey(workItemID, attemptID string) string {
	return strings.TrimSpace(workItemID) + "\x00" + strings.TrimSpace(attemptID)
}

func splitAttemptKey(k string) (workItemID, attemptID string) {
	i := strings.IndexByte(k, 0)
	if i < 0 {
		return k, ""
	}
	return k[:i], k[i+1:]
}
