package goalrun

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/workflowrun"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

// lifecycleEventKinds are attempt-scoped kinds that must bind to PriorOutcomes.
var lifecycleEventKinds = map[string]bool{
	"claim": true, "launch": true, "pid": true, "terminal": true,
	"integrate": true, "model_unavailable": true, "reroute": true,
	"interrupt": true, "reuse": true, "accept": true,
}

// exactCanonicalTerminal accepts only workgraph literals with no trim/alias/case fold.
func exactCanonicalTerminal(term string) bool {
	switch term {
	case string(workgraph.TermSucceeded), string(workgraph.TermFailed),
		string(workgraph.TermCancelled), string(workgraph.TermSkipped):
		return true
	default:
		return false
	}
}

// resolveCanonicalEventLogPath derives the run-local event log path read-only
// and requires any supplied durable stamps (checkpoint/partial) to be byte-exact
// equal to that canonical derivation (no Clean-normalized alias acceptance).
// Rejects symlinks under the durable home boundary. Errors are always authority
// failures (never swallowed).
func resolveCanonicalEventLogPath(homeDir, projectID, runID, cpPath, partPath string) (string, error) {
	canonical, err := eventLogPathRead(homeDir, projectID, runID)
	if err != nil {
		return "", fmt.Errorf("goalrun: canonical event log path: %w", err)
	}
	// Supplied durable stamps must be byte-exact equal to the derived canonical
	// path — no Clean, no TrimSpace, no ./ ../ alias acceptance.
	checkStamp := func(label, p string) error {
		if p == "" {
			return nil
		}
		if p != canonical {
			return fmt.Errorf("goalrun: %s EventLogPath %q != canonical %q (byte-exact required; fail closed before spend)", label, p, canonical)
		}
		return nil
	}
	if err := checkStamp("checkpoint", cpPath); err != nil {
		return "", err
	}
	if err := checkStamp("partial", partPath); err != nil {
		return "", err
	}
	if cpPath != "" && partPath != "" && cpPath != partPath {
		return "", fmt.Errorf("goalrun: checkpoint EventLogPath %q != partial EventLogPath %q (byte-exact)", cpPath, partPath)
	}
	if err := verifySecureEventLogPath(homeDir, projectID, runID, canonical); err != nil {
		return "", err
	}
	return canonical, nil
}

// verifySecureEventLogPath is the single robust event-log path verifier:
//  1. derive canonical absolute run/event path
//  2. resolve the longest existing ancestor (platform prefix only, e.g. /var→/private/var)
//  3. walk every existing component below the durable-home boundary with Lstat and reject symlinks
//  4. reconstruct intended remainder and prove it stays under resolved canonical run root
func verifySecureEventLogPath(homeDir, projectID, runID, eventPath string) error {
	canonical, err := eventLogPathRead(homeDir, projectID, runID)
	if err != nil {
		return fmt.Errorf("goalrun: secure event log path: %w", err)
	}
	if eventPath != canonical {
		return fmt.Errorf("goalrun: secure event log path %q != canonical %q (byte-exact)", eventPath, canonical)
	}
	// Use the same durable-home resolution as eventLogPathRead (empty homeDir → LOOPCODER_HOME).
	resolvedDurableHome, err := workflowrun.ResolveDurableHome(homeDir)
	if err != nil {
		return fmt.Errorf("goalrun: secure event log durable home: %w", err)
	}
	runDir, err := workflowrun.RunDurableDir(homeDir, projectID, runID)
	if err != nil {
		return fmt.Errorf("goalrun: secure event log run dir: %w", err)
	}
	homeAbs, err := filepath.Abs(resolvedDurableHome)
	if err != nil {
		return fmt.Errorf("goalrun: secure event log home Abs: %w", err)
	}
	runAbs, err := filepath.Abs(runDir)
	if err != nil {
		return fmt.Errorf("goalrun: secure event log run Abs: %w", err)
	}
	eventAbs, err := filepath.Abs(eventPath)
	if err != nil {
		return fmt.Errorf("goalrun: secure event log event Abs: %w", err)
	}
	// Platform-prefix resolution only on the longest existing ancestor of home.
	resolvedHome, err := resolveLongestExistingAncestor(homeAbs)
	if err != nil {
		return fmt.Errorf("goalrun: secure event log resolve home prefix: %w", err)
	}
	// Relative path of event under durable home (logical tree).
	eventRel, err := filepath.Rel(homeAbs, eventAbs)
	if err != nil || eventRel == ".." || strings.HasPrefix(eventRel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("goalrun: secure event log event %q escapes home %q (fail closed before spend)", eventAbs, homeAbs)
	}
	runRel, err := filepath.Rel(homeAbs, runAbs)
	if err != nil || runRel == ".." || strings.HasPrefix(runRel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("goalrun: secure event log run %q escapes home %q (fail closed before spend)", runAbs, homeAbs)
	}
	// Walk every existing component under durable-home boundary; reject symlinks.
	// Nonexistent remainder is reconstructed without following any injected link.
	cur := resolvedHome
	parts := splitPathComponents(eventRel)
	for i, part := range parts {
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			return fmt.Errorf("goalrun: secure event log path component .. under durable home (fail closed before spend)")
		}
		next := filepath.Join(cur, part)
		st, lerr := os.Lstat(next)
		if lerr != nil {
			if os.IsNotExist(lerr) {
				// Reconstruct intended remainder from first missing component.
				reconstructed := next
				for _, rest := range parts[i+1:] {
					if rest == "" || rest == "." {
						continue
					}
					if rest == ".." {
						return fmt.Errorf("goalrun: secure event log reconstructed path contains .. (fail closed before spend)")
					}
					reconstructed = filepath.Join(reconstructed, rest)
				}
				resolvedRun := filepath.Join(resolvedHome, runRel)
				rel, rerr := filepath.Rel(resolvedRun, reconstructed)
				if rerr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
					return fmt.Errorf("goalrun: secure event log reconstructed %q escapes run root %q (fail closed before spend)", reconstructed, resolvedRun)
				}
				return nil
			}
			return fmt.Errorf("goalrun: secure event log lstat %s: %w", next, lerr)
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("goalrun: secure event log contains symlink component %q under durable home (fail closed before spend)", next)
		}
		cur = next
	}
	// Full path exists as real components; prove under resolved run root.
	resolvedRun := filepath.Join(resolvedHome, runRel)
	rel, rerr := filepath.Rel(resolvedRun, cur)
	if rerr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("goalrun: secure event log resolved %q escapes run root %q (fail closed before spend)", cur, resolvedRun)
	}
	return nil
}

// evalSymlinksFn is the platform EvalSymlinks hook (tests inject deterministic errors).
var evalSymlinksFn = filepath.EvalSymlinks

// resolveLongestExistingAncestor EvalSymlinks the deepest existing prefix of path
// (platform prefix resolution only; does not invent durable components).
// Non-IsNotExist EvalSymlinks errors fail immediately. If resolution never
// succeeds down to root, fail closed (do not return root as success).
func resolveLongestExistingAncestor(path string) (string, error) {
	return resolveLongestExistingAncestorWith(path, evalSymlinksFn)
}

// resolveLongestExistingAncestorWith is the pure injectable implementation.
func resolveLongestExistingAncestorWith(path string, eval func(string) (string, error)) (string, error) {
	orig := filepath.Clean(path)
	p := orig
	var lastErr error
	for {
		resolved, err := eval(p)
		if err == nil {
			if p != orig {
				rel, rerr := filepath.Rel(p, orig)
				if rerr != nil {
					return "", rerr
				}
				return filepath.Join(resolved, rel), nil
			}
			return resolved, nil
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("goalrun: EvalSymlinks %q: %w", p, err)
		}
		lastErr = err
		parent := filepath.Dir(p)
		if parent == p {
			if lastErr != nil {
				return "", fmt.Errorf("goalrun: resolveLongestExistingAncestor failed at root for %q: %w", orig, lastErr)
			}
			return "", fmt.Errorf("goalrun: resolveLongestExistingAncestor failed at root for %q", orig)
		}
		p = parent
	}
}

func splitPathComponents(rel string) []string {
	rel = filepath.ToSlash(rel)
	if rel == "" || rel == "." {
		return nil
	}
	return strings.Split(rel, "/")
}

// rejectSymlinkPath fails if path exists and is itself a symlink (Lstat).
func rejectSymlinkPath(path, label string) error {
	st, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("goalrun: %s lstat: %w", label, err)
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("goalrun: %s must not be a symlink (fail closed before spend): %s", label, path)
	}
	return nil
}

// validateAndMergePriorOutcomes merges durable WorkflowKids by AttemptID with
// full-struct equality, cross-binds every kid to typed lifecycle evidence,
// requires every non-parent attempt-scoped event to map to exactly one
// PriorOutcome, and requires every AbortedAttempts entry to be backed by an
// exact PriorOutcome plus typed interrupt/cancelled evidence. Never invents
// rows from events. Unbound gen-only AbortedAttempts fail closed before spend.
func validateAndMergePriorOutcomes(
	rawKids []workflowrun.ChildOutcome,
	aborted map[string]string,
	elogPath string,
	id lifecycleBindIdentity,
) ([]workflowrun.ChildOutcome, error) {
	if len(aborted) > 0 && len(rawKids) == 0 {
		return nil, fmt.Errorf("goalrun: durable AbortedAttempts present but WorkflowKids empty (unbound gen-only seed; fail closed before spend; never invent from events)")
	}
	if len(rawKids) == 0 {
		p := elogPath
		if p == "" {
			return nil, nil
		}
		// Propagate Stat/Lstat/read errors (fail-open if ignored).
		if err := rejectSymlinkPath(p, "event log"); err != nil {
			return nil, err
		}
		st, err := os.Stat(p)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, nil
			}
			return nil, fmt.Errorf("goalrun: PriorOutcomes empty-kids event log stat: %w", err)
		}
		if st.IsDir() {
			return nil, fmt.Errorf("goalrun: PriorOutcomes event log path is directory")
		}
		if st.Size() == 0 {
			return nil, nil
		}
		raw, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil, fmt.Errorf("goalrun: PriorOutcomes event log read: %w", rerr)
		}
		events, perr := workflowrun.ParseEventJSONLStrict(string(raw), id.ProjectID, id.RunID)
		if perr != nil {
			return nil, fmt.Errorf("goalrun: PriorOutcomes event log parse: %w", perr)
		}
		for _, e := range events {
			if workflowrun.IsParentOnlyEvent(e) {
				continue
			}
			// ANY non-parent event with AttemptID is event-only fail closed
			// (recognized or unknown kinds).
			if e.AttemptID != "" {
				return nil, fmt.Errorf("goalrun: event-only attempt %s kind=%s with empty WorkflowKids (fail closed before spend)", e.AttemptID, e.Kind)
			}
		}
		return nil, nil
	}
	if strings.TrimSpace(elogPath) == "" {
		return nil, fmt.Errorf("goalrun: PriorOutcomes requires canonical event log path when WorkflowKids present (fail closed before spend)")
	}
	if err := rejectSymlinkPath(elogPath, "event log"); err != nil {
		return nil, err
	}
	st, err := os.Stat(elogPath)
	if err != nil {
		return nil, fmt.Errorf("goalrun: PriorOutcomes event log stat: %w", err)
	}
	if st.IsDir() || st.Size() == 0 {
		return nil, fmt.Errorf("goalrun: PriorOutcomes event log invalid or empty")
	}
	raw, err := os.ReadFile(elogPath)
	if err != nil {
		return nil, fmt.Errorf("goalrun: PriorOutcomes event log read: %w", err)
	}
	events, err := workflowrun.ParseEventJSONLStrict(string(raw), id.ProjectID, id.RunID)
	if err != nil {
		return nil, fmt.Errorf("goalrun: PriorOutcomes event log parse: %w", err)
	}
	// Full stream invariant validator before any per-row binding.
	if err := workflowrun.ValidateEventStreamInvariants(events); err != nil {
		return nil, fmt.Errorf("goalrun: PriorOutcomes event stream invariants: %w", err)
	}

	byAttEv := map[string][]workflowrun.Event{}
	for _, e := range events {
		if workflowrun.IsParentOnlyEvent(e) {
			continue
		}
		// Exact kind identity at resume authority — no ToLower/TrimSpace normalize.
		kind := e.Kind
		if kind != strings.TrimSpace(kind) || kind != strings.ToLower(kind) {
			return nil, fmt.Errorf("goalrun: noncanonical event kind %q (byte-exact lowercase required; fail closed before spend)", e.Kind)
		}
		if !lifecycleEventKinds[kind] {
			if e.AttemptID != "" {
				return nil, fmt.Errorf("goalrun: unknown attempt-scoped event kind %q attempt %s (fail closed before spend)", e.Kind, e.AttemptID)
			}
			continue
		}
		if err := requireStrictLifecycleEventStamps(e, id); err != nil {
			return nil, fmt.Errorf("goalrun: event %s: %w", e.EventID, err)
		}
		att := e.AttemptID // exact; stamps already require nonempty
		if att != strings.TrimSpace(att) {
			return nil, fmt.Errorf("goalrun: event %s attempt_id has whitespace padding (fail closed before spend)", e.EventID)
		}
		byAttEv[att] = append(byAttEv[att], e)
	}

	byAtt := map[string]workflowrun.ChildOutcome{}
	for _, kid := range rawKids {
		att := kid.AttemptID
		wi := kid.WorkItemID
		if att == "" || wi == "" {
			return nil, fmt.Errorf("goalrun: PriorOutcomes kid missing work_item/attempt")
		}
		// Reject surrounding whitespace on terminal (exact canonical only).
		if !exactCanonicalTerminal(kid.Terminal) {
			return nil, fmt.Errorf("goalrun: PriorOutcomes attempt %s invalid terminal %q (want exact succeeded|failed|cancelled|skipped)", att, kid.Terminal)
		}
		if err := validateCheckpointKidEnvelope(kid, id); err != nil {
			return nil, fmt.Errorf("goalrun: PriorOutcomes attempt %s: %w", att, err)
		}
		if prev, ok := byAtt[att]; ok {
			if !reflect.DeepEqual(prev, kid) {
				return nil, fmt.Errorf("goalrun: PriorOutcomes conflicting durable rows for AttemptID %s (full equality required; fail closed before spend)", att)
			}
			continue
		}
		byAtt[att] = kid
	}

	for att := range byAttEv {
		if _, ok := byAtt[att]; !ok {
			return nil, fmt.Errorf("goalrun: event-only AttemptID %s has no PriorOutcome (fail closed before spend)", att)
		}
	}
	// Cross-bind all kids first (cardinality), then MU/winner cross-links.
	for att, kid := range byAtt {
		evs := byAttEv[att]
		if len(evs) == 0 {
			return nil, fmt.Errorf("goalrun: PriorOutcomes attempt %s has no lifecycle events (fail closed before spend)", att)
		}
		if err := strictCrossBindKidToEvents(kid, evs, byAtt, byAttEv, id); err != nil {
			return nil, fmt.Errorf("goalrun: PriorOutcomes attempt %s: %w", att, err)
		}
	}

	for wi, att := range aborted {
		if att == "" || wi == "" {
			return nil, fmt.Errorf("goalrun: empty AbortedAttempts entry (fail closed before spend)")
		}
		// Reject whitespace-padded keys as unbound.
		if strings.TrimSpace(att) != att || strings.TrimSpace(wi) != wi {
			return nil, fmt.Errorf("goalrun: AbortedAttempts entry has surrounding whitespace (fail closed before spend)")
		}
		kid, ok := byAtt[att]
		if !ok {
			return nil, fmt.Errorf("goalrun: AbortedAttempts[%s]=%s unbound (no PriorOutcome; fail closed before spend)", wi, att)
		}
		if kid.WorkItemID != wi {
			return nil, fmt.Errorf("goalrun: AbortedAttempts[%s] work_item != PriorOutcome %q", wi, kid.WorkItemID)
		}
		if err := requireTypedAbortEvidence(kid, byAttEv[att]); err != nil {
			return nil, fmt.Errorf("goalrun: AbortedAttempts[%s]=%s: %w", wi, att, err)
		}
	}

	out := make([]workflowrun.ChildOutcome, 0, len(byAtt))
	for _, k := range byAtt {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].AttemptID < out[j].AttemptID
	})
	return out, nil
}

func requireStrictLifecycleEventStamps(e workflowrun.Event, id lifecycleBindIdentity) error {
	if e.EventID == "" {
		return fmt.Errorf("missing event_id")
	}
	if e.WorkItemID == "" {
		return fmt.Errorf("missing work_item_id")
	}
	if e.AttemptID == "" {
		return fmt.Errorf("missing attempt_id")
	}
	if e.Generation < 1 {
		return fmt.Errorf("generation must be positive, got %d", e.Generation)
	}
	if e.TaskClass == "" {
		return fmt.Errorf("missing task_class")
	}
	if e.ChildContractDigest == "" {
		return fmt.Errorf("missing child_contract_digest")
	}
	if e.ProjectID != id.ProjectID || e.RunID != id.RunID {
		return fmt.Errorf("project/run stamp mismatch")
	}
	if e.ExecutionPlanDigest != id.PlanDigest {
		return fmt.Errorf("plan_digest mismatch")
	}
	if e.GraphDigest != id.GraphDigest {
		return fmt.Errorf("graph_digest mismatch")
	}
	if e.GraphID != id.GraphID {
		return fmt.Errorf("graph_id mismatch")
	}
	if e.GraphVersion != id.GraphVersion {
		return fmt.Errorf("graph_version mismatch got %d want %d", e.GraphVersion, id.GraphVersion)
	}
	return nil
}

// lifecycleKindCard is the exact allowed-kind cardinality table per terminal class.
// Extra kinds outside the table are rejected. PID is 0|1 after launch for executed
// rows (stream invariants already reject duplicate pid); accept only on succeeded.
type lifecycleKindCard struct {
	claim, launch, pid, terminal, interrupt, mu, reroute, integrate, reuse, accept struct {
		min, max int
	}
}

func lifecycleCardForKid(kid workflowrun.ChildOutcome) (lifecycleKindCard, error) {
	var c lifecycleKindCard
	set := func(field *struct{ min, max int }, min, max int) { field.min, field.max = min, max }
	switch {
	case kid.Terminal == string(workgraph.TermSucceeded):
		// Exactly one original claim/launch/terminal; reuse additive; integrate/reroute/accept 0|1; pid 0|1.
		set(&c.claim, 1, 1)
		set(&c.launch, 1, 1)
		set(&c.pid, 0, 1)
		set(&c.terminal, 1, 1)
		set(&c.interrupt, 0, 0)
		set(&c.mu, 0, 0)
		set(&c.reroute, 0, 1)
		set(&c.integrate, 0, 1)
		set(&c.reuse, 0, 1<<30) // additive multi-restart confirmations
		set(&c.accept, 0, 1)
		return c, nil
	case kid.FailureClass == "model_unavailable" && kid.Terminal == string(workgraph.TermFailed):
		set(&c.claim, 1, 1)
		set(&c.launch, 1, 1)
		set(&c.pid, 0, 1)
		set(&c.terminal, 1, 1)
		set(&c.interrupt, 0, 0)
		set(&c.mu, 1, 1)
		set(&c.reroute, 0, 0)
		set(&c.integrate, 0, 0)
		set(&c.reuse, 0, 0)
		set(&c.accept, 0, 0)
		return c, nil
	case kid.Terminal == string(workgraph.TermCancelled):
		// Typed cancelled persistence: exact interrupt+terminal pair.
		set(&c.claim, 1, 1)
		set(&c.launch, 1, 1)
		set(&c.pid, 0, 1)
		set(&c.terminal, 1, 1)
		set(&c.interrupt, 1, 1)
		set(&c.mu, 0, 0)
		set(&c.reroute, 0, 0)
		set(&c.integrate, 0, 0)
		set(&c.reuse, 0, 0)
		set(&c.accept, 0, 0)
		return c, nil
	case kid.Terminal == string(workgraph.TermFailed):
		set(&c.claim, 1, 1)
		set(&c.launch, 1, 1)
		set(&c.pid, 0, 1)
		set(&c.terminal, 1, 1)
		set(&c.interrupt, 0, 0)
		set(&c.mu, 0, 0)
		set(&c.reroute, 0, 0)
		set(&c.integrate, 0, 0)
		set(&c.reuse, 0, 0)
		set(&c.accept, 0, 0)
		return c, nil
	case kid.Terminal == string(workgraph.TermSkipped):
		// Exactly one explicit canonical no-exec closure (terminal=skipped); no exec kinds.
		set(&c.claim, 0, 0)
		set(&c.launch, 0, 0)
		set(&c.pid, 0, 0)
		set(&c.terminal, 1, 1)
		set(&c.interrupt, 0, 0)
		set(&c.mu, 0, 0)
		set(&c.reroute, 0, 0)
		set(&c.integrate, 0, 0)
		set(&c.reuse, 0, 0)
		set(&c.accept, 0, 0)
		return c, nil
	default:
		return c, fmt.Errorf("unhandled terminal class %q failure_class=%q", kid.Terminal, kid.FailureClass)
	}
}

func checkCard(name string, n int, bound struct{ min, max int }) error {
	if n < bound.min || n > bound.max {
		return fmt.Errorf("%s count %d outside [%d,%d]", name, n, bound.min, bound.max)
	}
	return nil
}

// strictCrossBindKidToEvents enforces exact event→kid identity and the full
// allowed-kind cardinality table for the kid's terminal class.
func strictCrossBindKidToEvents(
	kid workflowrun.ChildOutcome,
	evs []workflowrun.Event,
	byAtt map[string]workflowrun.ChildOutcome,
	byAttEv map[string][]workflowrun.Event,
	id lifecycleBindIdentity,
) error {
	_ = id
	att := kid.AttemptID
	wi := kid.WorkItemID
	card, err := lifecycleCardForKid(kid)
	if err != nil {
		return err
	}
	var nLaunch, nTerminal, nInterrupt, nReuse, nMU, nReroute, nIntegrate, nClaim, nAccept, nPID int
	var term string
	var rerouteEv workflowrun.Event
	var supersedesFromReroute, retryFromReroute string
	for _, e := range evs {
		if e.AttemptID != att {
			return fmt.Errorf("event attempt mismatch")
		}
		if e.WorkItemID != wi {
			return fmt.Errorf("event work_item %q != kid %q", e.WorkItemID, wi)
		}
		if e.TaskClass != kid.TaskClass {
			return fmt.Errorf("event task_class %q != kid %q", e.TaskClass, kid.TaskClass)
		}
		if e.ChildContractDigest != kid.ChildContractDigest {
			return fmt.Errorf("event CCD mismatch vs kid")
		}
		if e.Generation != kid.Generation {
			return fmt.Errorf("event generation %d != kid %d", e.Generation, kid.Generation)
		}
		// Exact kind — no ToLower/TrimSpace.
		kind := e.Kind
		if !lifecycleEventKinds[kind] {
			return fmt.Errorf("extra/unknown kind %q rejected for terminal class %q", kind, kid.Terminal)
		}
		switch kind {
		case "claim":
			nClaim++
		case "launch":
			nLaunch++
		case "terminal":
			nTerminal++
			if e.Terminal == "" {
				return fmt.Errorf("terminal event missing terminal")
			}
			if !exactCanonicalTerminal(e.Terminal) {
				return fmt.Errorf("terminal event invalid terminal %q", e.Terminal)
			}
			if e.Terminal != kid.Terminal {
				return fmt.Errorf("terminal event %q != kid terminal %q", e.Terminal, kid.Terminal)
			}
			term = e.Terminal
		case "interrupt":
			nInterrupt++
		case "reuse":
			nReuse++
		case "model_unavailable":
			nMU++
		case "reroute":
			nReroute++
			rerouteEv = e
			if len(e.Payload) > 0 {
				var m map[string]string
				if err := json.Unmarshal(e.Payload, &m); err != nil {
					return fmt.Errorf("reroute payload JSON: %w", err)
				}
				supersedesFromReroute = m["supersedes_attempt_id"]
				retryFromReroute = m["retry_attempt_id"]
			}
		case "integrate":
			nIntegrate++
		case "accept":
			nAccept++
		case "pid":
			nPID++
		default:
			return fmt.Errorf("extra kind %q rejected", kind)
		}
	}
	if err := checkCard("claim", nClaim, card.claim); err != nil {
		return err
	}
	if err := checkCard("launch", nLaunch, card.launch); err != nil {
		return err
	}
	if err := checkCard("pid", nPID, card.pid); err != nil {
		return err
	}
	if err := checkCard("terminal", nTerminal, card.terminal); err != nil {
		return err
	}
	if err := checkCard("interrupt", nInterrupt, card.interrupt); err != nil {
		return err
	}
	if err := checkCard("model_unavailable", nMU, card.mu); err != nil {
		return err
	}
	if err := checkCard("reroute", nReroute, card.reroute); err != nil {
		return err
	}
	if err := checkCard("integrate", nIntegrate, card.integrate); err != nil {
		return err
	}
	if err := checkCard("reuse", nReuse, card.reuse); err != nil {
		return err
	}
	if err := checkCard("accept", nAccept, card.accept); err != nil {
		return err
	}

	// Succeeded: no failure_class; integrate presence binds SHA + Integrated membership.
	if kid.Terminal == string(workgraph.TermSucceeded) {
		if kid.FailureClass != "" {
			return fmt.Errorf("succeeded row must not carry failure_class %q", kid.FailureClass)
		}
		if nIntegrate == 0 {
			if kid.IntegrateCommitSHA != "" {
				return fmt.Errorf("succeeded without integrate event must not carry IntegrateCommitSHA")
			}
		} else {
			// nIntegrate == 1: exact commit SHA on integrate event and kid.
			if kid.IntegrateCommitSHA == "" {
				return fmt.Errorf("succeeded with integrate event missing IntegrateCommitSHA")
			}
			var intSHA string
			for _, e := range evs {
				if e.Kind == "integrate" {
					intSHA = e.CommitSHA
					break
				}
			}
			if intSHA == "" || intSHA != kid.IntegrateCommitSHA {
				return fmt.Errorf("integrate event commit %q != kid IntegrateCommitSHA %q", intSHA, kid.IntegrateCommitSHA)
			}
		}
		if nReroute == 0 {
			if kid.SupersedesAttemptID != "" || kid.RerouteEventRef != "" {
				return fmt.Errorf("succeeded without reroute must not carry SupersedesAttemptID/RerouteEventRef")
			}
			return nil
		}
		// nReroute == 1
		if kid.SupersedesAttemptID == "" {
			return fmt.Errorf("retry winner with reroute missing SupersedesAttemptID")
		}
		if kid.RerouteEventRef == "" {
			return fmt.Errorf("retry winner with reroute missing RerouteEventRef")
		}
		if supersedesFromReroute == "" {
			return fmt.Errorf("reroute payload missing supersedes_attempt_id")
		}
		if supersedesFromReroute != kid.SupersedesAttemptID {
			return fmt.Errorf("reroute supersedes %q != kid SupersedesAttemptID %q", supersedesFromReroute, kid.SupersedesAttemptID)
		}
		if retryFromReroute != kid.AttemptID {
			return fmt.Errorf("reroute retry_attempt_id %q != kid AttemptID %q", retryFromReroute, kid.AttemptID)
		}
		failed, ok := byAtt[kid.SupersedesAttemptID]
		if !ok {
			return fmt.Errorf("SupersedesAttemptID %s missing from PriorOutcomes", kid.SupersedesAttemptID)
		}
		if failed.WorkItemID != kid.WorkItemID {
			return fmt.Errorf("MU winner WorkItemID %q != failed %q", kid.WorkItemID, failed.WorkItemID)
		}
		if failed.Terminal != string(workgraph.TermFailed) {
			return fmt.Errorf("SupersedesAttemptID terminal must be exact failed, got %q", failed.Terminal)
		}
		if failed.FailureClass != "model_unavailable" {
			return fmt.Errorf("SupersedesAttemptID %s is not model_unavailable failed PriorOutcome", kid.SupersedesAttemptID)
		}
		if kid.Generation != failed.Generation+1 {
			return fmt.Errorf("MU winner generation %d != failed %d + 1", kid.Generation, failed.Generation)
		}
		failedEvs := byAttEv[kid.SupersedesAttemptID]
		if len(failedEvs) == 0 {
			return fmt.Errorf("SupersedesAttemptID %s has no events in byAttEv", kid.SupersedesAttemptID)
		}
		if err := requireExactRerouteEventRef(kid, failed, failedEvs, evs, rerouteEv); err != nil {
			return err
		}
		return nil
	}

	// Cancelled: every persisted cancelled row requires typed interrupt+terminal
	// pair with exact kid/top-level/payload class/terminal/interrupt_id binding
	// (not only when listed in AbortedAttempts).
	if kid.Terminal == string(workgraph.TermCancelled) {
		if term != string(workgraph.TermCancelled) {
			return fmt.Errorf("cancelled row terminal event %q", term)
		}
		if err := requireTypedAbortEvidence(kid, evs); err != nil {
			return fmt.Errorf("cancelled persisted row: %w", err)
		}
		return nil
	}

	// Skipped: exact terminal only; no failure_class; no integrate/MU/reroute fields.
	if kid.Terminal == string(workgraph.TermSkipped) {
		if term != string(workgraph.TermSkipped) {
			return fmt.Errorf("skipped row terminal event %q", term)
		}
		if kid.FailureClass != "" {
			return fmt.Errorf("skipped row must not carry failure_class %q", kid.FailureClass)
		}
		if kid.IntegrateCommitSHA != "" || kid.SupersedesAttemptID != "" || kid.RerouteEventRef != "" {
			return fmt.Errorf("skipped row must not carry integrate/supersedes/reroute identity")
		}
		return nil
	}

	// model_unavailable failed: exact top-level + payload failure_class + terminal failed.
	if kid.FailureClass == "model_unavailable" {
		if kid.Terminal != string(workgraph.TermFailed) {
			return fmt.Errorf("MU kid terminal must be exact failed, got %q", kid.Terminal)
		}
		if kid.IntegrateCommitSHA != "" {
			return fmt.Errorf("MU failed must not carry IntegrateCommitSHA")
		}
		if kid.SupersedesAttemptID != "" || kid.RerouteEventRef != "" {
			return fmt.Errorf("MU failed must not carry SupersedesAttemptID/RerouteEventRef")
		}
		// Bind MU event top-level + payload failure_class exactly.
		var sawMU bool
		for _, e := range evs {
			if e.Kind != "model_unavailable" {
				continue
			}
			sawMU = true
			if e.FailureClass != "model_unavailable" {
				return fmt.Errorf("MU event top-level FailureClass %q != exact model_unavailable", e.FailureClass)
			}
			if e.Terminal != "" && e.Terminal != string(workgraph.TermFailed) {
				return fmt.Errorf("MU event terminal %q != exact failed", e.Terminal)
			}
			if len(e.Payload) > 0 {
				var m map[string]string
				if err := json.Unmarshal(e.Payload, &m); err != nil {
					return fmt.Errorf("MU payload JSON: %w", err)
				}
				if fc := m["failure_class"]; fc != "" && fc != "model_unavailable" {
					return fmt.Errorf("MU payload failure_class %q != exact model_unavailable", fc)
				}
			}
		}
		if !sawMU {
			return fmt.Errorf("MU kid missing model_unavailable event")
		}
		return nil
	}

	// Generic failed: exact failed terminal; no typed-abort/MU/reroute contradictions.
	if kid.Terminal == string(workgraph.TermFailed) {
		if term != string(workgraph.TermFailed) {
			return fmt.Errorf("failed row terminal event %q", term)
		}
		if exactTypedAbortClass(kid.FailureClass) {
			return fmt.Errorf("failed non-cancelled row must not carry typed abort class %q without cancelled terminal", kid.FailureClass)
		}
		if kid.IntegrateCommitSHA != "" || kid.SupersedesAttemptID != "" || kid.RerouteEventRef != "" {
			return fmt.Errorf("failed non-MU row must not carry integrate/supersedes/reroute identity")
		}
		return nil
	}
	return fmt.Errorf("unhandled terminal class %q failure_class=%q", kid.Terminal, kid.FailureClass)
}

// requireExactRerouteEventRef parses the canonical RerouteEventRef grammar:
//
//	event_id=<mu>;event_id=<winner_claim>;event_id=<reroute>;event_id=<winner_launch>;supersedes_attempt_id=<failed>;retry_attempt_id=<winner>
//
// Production (workflowrun service) stamps MU on the failed attempt and claim/
// reroute/launch on the winner — bind via byAttEv, not substring. Exact
// supersedes/retry attempt IDs; no unknown/duplicate/trailing tokens.
func requireExactRerouteEventRef(
	winner, failed workflowrun.ChildOutcome,
	failedEvs, winnerEvs []workflowrun.Event,
	rerouteEv workflowrun.Event,
) error {
	muID := ""
	for _, e := range failedEvs {
		if e.Kind == "model_unavailable" {
			if muID != "" {
				return fmt.Errorf("duplicate model_unavailable on failed attempt")
			}
			muID = e.EventID
		}
	}
	claimWinnerID, launchWinnerID := "", ""
	for _, e := range winnerEvs {
		switch e.Kind {
		case "claim":
			if claimWinnerID != "" {
				return fmt.Errorf("duplicate claim on winner attempt")
			}
			claimWinnerID = e.EventID
		case "launch":
			if launchWinnerID != "" {
				return fmt.Errorf("duplicate launch on winner attempt")
			}
			launchWinnerID = e.EventID
		}
	}
	if muID == "" || claimWinnerID == "" || launchWinnerID == "" || rerouteEv.EventID == "" {
		return fmt.Errorf("reroute binding missing required event roles mu=%q claim=%q launch=%q reroute=%q",
			muID, claimWinnerID, launchWinnerID, rerouteEv.EventID)
	}
	// Order matches production stamp: mu, winner claim, reroute, winner launch.
	requiredIDs := []string{muID, claimWinnerID, rerouteEv.EventID, launchWinnerID}
	// Parse grammar without TrimSpace (whitespace-padded tokens fail).
	ref := winner.RerouteEventRef
	if ref == "" {
		return fmt.Errorf("empty RerouteEventRef")
	}
	parts := strings.Split(ref, ";")
	refIDs := map[string]int{}
	keyCount := map[string]int{}
	var refSupersedes, refRetry string
	for _, part := range parts {
		if part == "" {
			return fmt.Errorf("RerouteEventRef empty token (trailing/duplicate separators)")
		}
		if part != strings.TrimSpace(part) {
			return fmt.Errorf("RerouteEventRef token has whitespace padding %q", part)
		}
		key, val, ok := strings.Cut(part, "=")
		if !ok || key == "" || val == "" {
			return fmt.Errorf("RerouteEventRef malformed token %q", part)
		}
		if key != strings.TrimSpace(key) || val != strings.TrimSpace(val) {
			return fmt.Errorf("RerouteEventRef key/val whitespace padding in %q", part)
		}
		keyCount[key]++
		switch key {
		case "event_id":
			refIDs[val]++
		case "supersedes_attempt_id":
			refSupersedes = val
		case "retry_attempt_id":
			refRetry = val
		default:
			return fmt.Errorf("RerouteEventRef unknown key %q", key)
		}
	}
	if keyCount["supersedes_attempt_id"] != 1 || keyCount["retry_attempt_id"] != 1 {
		return fmt.Errorf("RerouteEventRef wants exactly one supersedes_attempt_id and retry_attempt_id")
	}
	if refSupersedes != failed.AttemptID {
		return fmt.Errorf("RerouteEventRef supersedes_attempt_id %q != failed %q", refSupersedes, failed.AttemptID)
	}
	if refRetry != winner.AttemptID {
		return fmt.Errorf("RerouteEventRef retry_attempt_id %q != winner %q", refRetry, winner.AttemptID)
	}
	if keyCount["event_id"] != len(requiredIDs) {
		return fmt.Errorf("RerouteEventRef event_id count %d != required %d", keyCount["event_id"], len(requiredIDs))
	}
	if len(refIDs) != len(requiredIDs) {
		return fmt.Errorf("RerouteEventRef unique event_id count %d != required %d (duplicate or missing)", len(refIDs), len(requiredIDs))
	}
	for _, id := range requiredIDs {
		if refIDs[id] != 1 {
			return fmt.Errorf("RerouteEventRef missing or non-exact event_id %q (no substring spoof)", id)
		}
	}
	// Reject any extra event_id not in required set (already covered by len equality + exact match).
	for id, n := range refIDs {
		if n != 1 {
			return fmt.Errorf("RerouteEventRef duplicate event_id %q", id)
		}
		found := false
		for _, want := range requiredIDs {
			if id == want {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("RerouteEventRef extra event_id %q", id)
		}
	}
	return nil
}

// allowedAbortPayloadKeys is the exact key set for typed abort interrupt/terminal
// payloads: required self-bind + stampChildIdentityPayload + route fields.
// Unknown keys fail closed (no spoof).
var allowedAbortPayloadKeys = map[string]bool{
	"failure_class": true, "interrupt_class": true, "interrupt_id": true,
	"terminal": true, "work_item_id": true, "attempt_id": true, "generation": true,
	"pid": true, "output_evidence": true,
	// stampChildIdentityPayload
	"project_id": true, "run_id": true, "graph_id": true, "graph_version": true,
	"execution_plan_digest": true, "graph_digest": true, "task_class": true,
	"child_contract_digest": true,
	// childRoutePayloadFields
	"provider": true, "model": true, "depth": true, "permission": true,
	"account_ref": true, "install_ref": true, "window_kind": true,
	"reservation_id": true, "route_reason": true,
}

// requireTypedAbortEvidence requires BOTH interrupt and terminal for a persisted
// aborted ChildOutcome, with strict JSON (DisallowUnknownFields + allowed-key set)
// and exact top-level/payload class/terminal consistency.
func requireTypedAbortEvidence(kid workflowrun.ChildOutcome, evs []workflowrun.Event) error {
	if kid.Terminal == string(workgraph.TermSucceeded) {
		return fmt.Errorf("AbortedAttempts cannot point at succeeded PriorOutcome")
	}
	if kid.Terminal != string(workgraph.TermCancelled) && kid.Terminal != string(workgraph.TermFailed) {
		return fmt.Errorf("AbortedAttempts PriorOutcome terminal %q not cancelled|failed", kid.Terminal)
	}
	if !exactTypedAbortClass(kid.FailureClass) {
		return fmt.Errorf("kid FailureClass %q is not exact typed abort class", kid.FailureClass)
	}
	var sawInterrupt, sawTerminal bool
	var interruptID string
	for _, e := range evs {
		kind := e.Kind
		if kind != "interrupt" && kind != "terminal" {
			continue
		}
		if kind == "interrupt" {
			sawInterrupt = true
		}
		if kind == "terminal" {
			sawTerminal = true
			if e.Terminal != kid.Terminal {
				return fmt.Errorf("terminal event %q != kid %q", e.Terminal, kid.Terminal)
			}
		}
		if e.FailureClass == "" || !exactTypedAbortClass(e.FailureClass) {
			return fmt.Errorf("%s top-level FailureClass %q not exact typed abort", kind, e.FailureClass)
		}
		if e.FailureClass != kid.FailureClass {
			return fmt.Errorf("%s FailureClass %q != kid %q", kind, e.FailureClass, kid.FailureClass)
		}
		if len(e.Payload) == 0 {
			return fmt.Errorf("%s missing payload (required self-binding work_item/attempt/generation/interrupt_id)", kind)
		}
		dec := json.NewDecoder(strings.NewReader(string(e.Payload)))
		dec.DisallowUnknownFields()
		var m map[string]string
		if err := dec.Decode(&m); err != nil {
			return fmt.Errorf("%s payload JSON: %w", kind, err)
		}
		for k := range m {
			if !allowedAbortPayloadKeys[k] {
				return fmt.Errorf("%s payload unknown key %q (exact allowed-key set)", kind, k)
			}
		}
		if m["work_item_id"] != kid.WorkItemID {
			return fmt.Errorf("%s payload work_item_id %q != kid %q", kind, m["work_item_id"], kid.WorkItemID)
		}
		if m["attempt_id"] != kid.AttemptID {
			return fmt.Errorf("%s payload attempt_id %q != kid %q", kind, m["attempt_id"], kid.AttemptID)
		}
		wantGen := fmt.Sprintf("%d", kid.Generation)
		if m["generation"] != wantGen {
			return fmt.Errorf("%s payload generation %q != kid %s", kind, m["generation"], wantGen)
		}
		fc := m["failure_class"]
		if fc == "" || !exactTypedAbortClass(fc) {
			return fmt.Errorf("%s payload failure_class %q not exact typed abort", kind, fc)
		}
		if fc != kid.FailureClass {
			return fmt.Errorf("%s payload failure_class %q != kid %q", kind, fc, kid.FailureClass)
		}
		ic := m["interrupt_class"]
		if ic == "" || !exactTypedInterruptClass(ic) {
			return fmt.Errorf("%s payload interrupt_class %q not exact typed", kind, ic)
		}
		iid := m["interrupt_id"]
		if iid == "" {
			return fmt.Errorf("%s payload missing interrupt_id", kind)
		}
		if interruptID == "" {
			interruptID = iid
		} else if interruptID != iid {
			return fmt.Errorf("interrupt_id conflict %q vs %q", interruptID, iid)
		}
		if m["terminal"] != kid.Terminal {
			return fmt.Errorf("%s payload terminal %q != kid %q", kind, m["terminal"], kid.Terminal)
		}
	}
	// Production writes both interrupt and cancelled terminal for forced_interrupt.
	if !sawInterrupt || !sawTerminal {
		return fmt.Errorf("aborted PriorOutcome requires both interrupt and terminal evidence (got interrupt=%v terminal=%v)", sawInterrupt, sawTerminal)
	}
	if interruptID == "" {
		return fmt.Errorf("missing interrupt_id evidence")
	}
	return nil
}

func exactTypedAbortClass(fc string) bool {
	switch fc {
	case "forced_interrupt", "service_forced_interrupt", "hard_kill_recovery":
		return true
	default:
		return false
	}
}

func exactTypedInterruptClass(ic string) bool {
	switch ic {
	case "service_forced_interrupt", "hard_kill_recovery":
		return true
	default:
		return false
	}
}

// priorRouteSnap holds class/depth/permission from routeByID for graph check.
type priorRouteSnap struct {
	Class      string
	Depth      string
	Permission string
}

// validatePriorOutcomesAgainstGraphMaps requires exact nonempty TaskClass, Depth,
// Permission equal to current route, exact CCD/plan/terminal, and coherent
// capacity identity when any capacity field is present.
func validatePriorOutcomesAgainstGraphMaps(
	outcomes []workflowrun.ChildOutcome,
	items []workgraph.WorkItem,
	planDigest, runID string,
	currentCCD map[string]string,
	routes map[string]priorRouteSnap,
) error {
	itemOK := map[string]bool{}
	for _, it := range items {
		itemOK[it.ID] = true
	}
	for _, o := range outcomes {
		wi := o.WorkItemID
		att := o.AttemptID
		if !itemOK[wi] {
			return fmt.Errorf("goalrun: PriorOutcomes ghost work_item %q not in current graph (fail closed before spend)", wi)
		}
		if !exactCanonicalTerminal(o.Terminal) {
			return fmt.Errorf("goalrun: PriorOutcomes %s invalid terminal %q (exact succeeded|failed|cancelled|skipped)", att, o.Terminal)
		}
		if err := requireTerminalOutcomeIdentity(wi, o); err != nil {
			return fmt.Errorf("goalrun: PriorOutcomes %s: %w", att, err)
		}
		if err := requireCanonicalOutcomeAttempt(o, planDigest, runID); err != nil {
			return fmt.Errorf("goalrun: PriorOutcomes %s: %w", att, err)
		}
		if o.ExecutionPlanDigest != planDigest {
			return fmt.Errorf("goalrun: PriorOutcomes %s plan_digest mismatch", att)
		}
		wantCCD := currentCCD[wi]
		if wantCCD == "" || o.ChildContractDigest != wantCCD {
			return fmt.Errorf("goalrun: PriorOutcomes %s CCD %q != current %q (fail closed before spend)", att, o.ChildContractDigest, wantCCD)
		}
		rs, ok := routes[wi]
		if !ok {
			return fmt.Errorf("goalrun: PriorOutcomes %s missing current route for %s", att, wi)
		}
		if o.TaskClass == "" || o.TaskClass != rs.Class {
			return fmt.Errorf("goalrun: PriorOutcomes %s task_class %q != current %q (nonempty required)", att, o.TaskClass, rs.Class)
		}
		if o.Depth == "" || o.Depth != rs.Depth {
			return fmt.Errorf("goalrun: PriorOutcomes %s depth %q != current %q (nonempty required)", att, o.Depth, rs.Depth)
		}
		if o.Permission == "" || o.Permission != rs.Permission {
			return fmt.Errorf("goalrun: PriorOutcomes %s permission %q != current %q (nonempty required)", att, o.Permission, rs.Permission)
		}
		// Capacity/route-bearing coherence: any capacity field ⇒ full set + provider/model.
		acc := o.AccountRef
		inst := o.InstallRef
		win := o.WindowKind
		res := o.ReservationID
		if acc != "" || inst != "" || win != "" || res != "" || o.Provider != "" || o.Model != "" {
			if o.Provider == "" || o.Model == "" || o.Depth == "" || o.Permission == "" ||
				acc == "" || inst == "" || win == "" {
				return fmt.Errorf("goalrun: PriorOutcomes %s capacity/route-bearing incomplete provider/model/depth/permission/account/install/window (fail closed before spend)", att)
			}
			// Reservation must not be partially present without the rest (already covered);
			// if reservation set alone without others, caught above.
		}
	}
	return nil
}

// requirePriorSucceededExactSubset requires every PriorSucceeded row to be
// full-struct equal to some PriorOutcomes row (when outcomes nonempty).
func requirePriorSucceededExactSubset(priors map[string]workflowrun.ChildOutcome, outcomes []workflowrun.ChildOutcome) error {
	if len(priors) == 0 {
		return nil
	}
	if len(outcomes) == 0 {
		return nil
	}
	byAtt := map[string]workflowrun.ChildOutcome{}
	for _, o := range outcomes {
		byAtt[o.AttemptID] = o
	}
	for id, p := range priors {
		o, ok := byAtt[p.AttemptID]
		if !ok {
			return fmt.Errorf("goalrun: PriorSucceeded[%s] attempt %s not in PriorOutcomes (fail closed before spend)", id, p.AttemptID)
		}
		if !reflect.DeepEqual(p, o) {
			return fmt.Errorf("goalrun: PriorSucceeded[%s] full-row mismatch vs PriorOutcomes attempt %s (fail closed before spend)", id, p.AttemptID)
		}
		if p.Terminal != string(workgraph.TermSucceeded) {
			return fmt.Errorf("goalrun: PriorSucceeded[%s] terminal must be exact succeeded", id)
		}
	}
	return nil
}
