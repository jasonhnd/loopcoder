package workflowrun

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// EventSchema is the append-only raw event line schema for forced-interrupt evidence.
const EventSchema = "loopcoder.workflow.event.v1"

// Event is one append-only durable parent/child lifecycle fact.
// Canary evidence must derive interrupt/restart metrics from these lines,
// not from hand-written booleans.
type Event struct {
	Schema     string    `json:"schema"`
	EventID    string    `json:"event_id,omitempty"`
	At         time.Time `json:"at"`
	ProjectID  string    `json:"project_id"`
	RunID      string    `json:"run_id"`
	Kind       string    `json:"kind"` // claim|launch|pid|terminal|integrate|interrupt|reuse|accept|model_unavailable|reroute
	WorkItemID string    `json:"work_item_id,omitempty"`
	AttemptID  string    `json:"attempt_id,omitempty"`
	// GraphID / GraphVersion bind the event to the materialised workgraph (claim fence).
	// Required nonempty/positive on recovery-relevant child lifecycle events.
	GraphID      string `json:"graph_id,omitempty"`
	GraphVersion int    `json:"graph_version,omitempty"`
	// ExecutionPlanDigest is the canonical workflowdef.Normalize digest when known.
	ExecutionPlanDigest string `json:"execution_plan_digest,omitempty"`
	// GraphDigest is the separate workgraph digest when known.
	GraphDigest string `json:"graph_digest,omitempty"`
	// TaskClass is the assignment-time classified floor (luna|tera|soul).
	// Required on child claim/launch/terminal/reuse/model_unavailable/reroute/integrate.
	TaskClass string `json:"task_class,omitempty"`
	// ChildContractDigest is the assignment-time contract digest (never post-exec).
	ChildContractDigest string `json:"child_contract_digest,omitempty"`
	// FailureClass is the canonical structured failure class for terminal/interrupt
	// (also mirrored in payload failure_class). Never derived from Message prose.
	// Success terminals must leave this empty.
	FailureClass string `json:"failure_class,omitempty"`
	Terminal     string `json:"terminal,omitempty"`
	PID          int    `json:"pid,omitempty"`
	CommitSHA    string `json:"commit_sha,omitempty"`
	Message      string `json:"message,omitempty"`
	// Payload is structured JSON (map/object), never semicolon-delimited prose.
	// Do not encode relations into Kind.
	Payload    json.RawMessage `json:"payload,omitempty"`
	Evidence   string          `json:"evidence,omitempty"`
	Generation int             `json:"generation,omitempty"`
}

// WorkflowrunExecutorID is the exact durable claim executor for Service child attempts.
const WorkflowrunExecutorID = "workflowrun"

// EventLog is an append-only JSONL log under the project runs directory.
type EventLog struct {
	mu   sync.Mutex
	path string
	seq  int64
	// FailAppend when non-nil forces Append to fail (tests only).
	FailAppend error
	// FailAppendKind when non-empty forces Append to fail only for that kind (tests).
	FailAppendKind string
}

// OpenEventLog opens/creates the durable event log for a run under
// <durableHome>/projects/<project>/runs/<runID>/workflow-events.jsonl.
// homeDir, when empty, resolves via ResolveDurableHome (LOOPCODER_HOME then
// ~/.loopcoder) — never a PID-scoped temp path.
func OpenEventLog(homeDir, projectID, runID string) (*EventLog, error) {
	projectID = strings.TrimSpace(projectID)
	runID = strings.TrimSpace(runID)
	if projectID == "" || runID == "" {
		return nil, fmt.Errorf("workflowrun: event log requires project_id and run_id")
	}
	root, err := ResolveDurableHome(homeDir)
	if err != nil {
		return nil, fmt.Errorf("workflowrun: durable home: %w", err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	dir := filepath.Join(root, "projects", projectID, "runs", sanitizeBranch(runID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &EventLog{path: filepath.Join(dir, "workflow-events.jsonl")}, nil
}

// Path returns the JSONL path.
func (e *EventLog) Path() string {
	if e == nil {
		return ""
	}
	return e.path
}

// Append writes one event line (fail-closed on I/O error for interrupt evidence).
// Assigns a durable EventID when empty and returns the persisted event.
func (e *EventLog) Append(ev Event) (Event, error) {
	if e == nil {
		return ev, nil
	}
	if e.FailAppend != nil {
		return Event{}, e.FailAppend
	}
	if e.FailAppendKind != "" && strings.TrimSpace(ev.Kind) == e.FailAppendKind {
		return Event{}, fmt.Errorf("workflowrun: forced fail append kind=%s", e.FailAppendKind)
	}
	if strings.TrimSpace(ev.Schema) == "" {
		ev.Schema = EventSchema
	}
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if strings.TrimSpace(ev.EventID) == "" {
		e.seq++
		ev.EventID = fmt.Sprintf("wev_%d_%d", ev.At.UnixNano(), e.seq)
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		return Event{}, err
	}
	if err := os.MkdirAll(filepath.Dir(e.path), 0o700); err != nil {
		return Event{}, err
	}
	f, err := os.OpenFile(e.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return Event{}, err
	}
	defer f.Close()
	if _, err := f.Write(append(raw, '\n')); err != nil {
		return Event{}, err
	}
	if err := f.Sync(); err != nil {
		return Event{}, err
	}
	return ev, nil
}

// ReadAll loads all events fail-closed. Malformed JSON, missing schema/kind/event_id,
// duplicate event_id, or inconsistent project/run (when first event sets them) fail.
// This is the single authoritative parser; canary and recovery share it.
func (e *EventLog) ReadAll() ([]Event, error) {
	if e == nil {
		return nil, nil
	}
	raw, err := os.ReadFile(e.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return ParseEventJSONLStrict(string(raw), "", "")
}

// ReadAllForRun is ReadAll with required exact project_id/run_id on every event.
func (e *EventLog) ReadAllForRun(projectID, runID string) ([]Event, error) {
	if e == nil {
		return nil, nil
	}
	raw, err := os.ReadFile(e.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return ParseEventJSONLStrict(string(raw), projectID, runID)
}

// ParseEventJSONLStrict is the authoritative EventLog JSONL parser.
// Fail closed on: malformed JSON, missing schema/kind/event_id, duplicate event_id,
// empty/mismatched project/run when expectProject/expectRun are non-empty.
// When expectProject/expectRun empty, the first nonempty project/run becomes the
// expected value for subsequent lines (internal consistency).
func ParseEventJSONLStrict(raw, expectProject, expectRun string) ([]Event, error) {
	expectProject = strings.TrimSpace(expectProject)
	expectRun = strings.TrimSpace(expectRun)
	var out []Event
	seenID := map[string]bool{}
	for i, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			return nil, fmt.Errorf("workflowrun: event log line %d: malformed JSON: %w", i+1, err)
		}
		if strings.TrimSpace(ev.Schema) != "" && strings.TrimSpace(ev.Schema) != EventSchema {
			return nil, fmt.Errorf("workflowrun: event log line %d: schema want %q got %q", i+1, EventSchema, ev.Schema)
		}
		if strings.TrimSpace(ev.Schema) == "" {
			return nil, fmt.Errorf("workflowrun: event log line %d: missing schema", i+1)
		}
		if strings.TrimSpace(ev.Kind) == "" {
			return nil, fmt.Errorf("workflowrun: event log line %d: missing kind", i+1)
		}
		eid := strings.TrimSpace(ev.EventID)
		if eid == "" {
			return nil, fmt.Errorf("workflowrun: event log line %d: missing event_id", i+1)
		}
		if seenID[eid] {
			return nil, fmt.Errorf("workflowrun: event log line %d: duplicate event_id %q", i+1, eid)
		}
		seenID[eid] = true
		// Every event must have nonempty exact project_id/run_id (including first).
		if strings.TrimSpace(ev.ProjectID) == "" || strings.TrimSpace(ev.RunID) == "" {
			return nil, fmt.Errorf("workflowrun: event log line %d: project_id and run_id required nonempty", i+1)
		}
		if expectProject == "" {
			expectProject = strings.TrimSpace(ev.ProjectID)
		}
		if expectRun == "" {
			expectRun = strings.TrimSpace(ev.RunID)
		}
		if strings.TrimSpace(ev.ProjectID) != expectProject {
			return nil, fmt.Errorf("workflowrun: event log line %d: project_id mismatch", i+1)
		}
		if strings.TrimSpace(ev.RunID) != expectRun {
			return nil, fmt.Errorf("workflowrun: event log line %d: run_id mismatch", i+1)
		}
		out = append(out, ev)
	}
	return out, nil
}

// InterruptedFromEvents reports whether any interrupt was recorded and, for each
// work item, the latest aborted attempt ID (exact attempt-scoped projection).
// A terminal/integrate/reuse closes only its matching attempt. A parent interrupt
// marks only attempts open at that point; a child interrupt marks only that exact
// attempt. Historical g0 interrupt does not contaminate a later g1 open attempt.
func InterruptedFromEvents(events []Event) (interrupted bool, aborted map[string]string) {
	aborted = map[string]string{} // workItemID → latest aborted attemptID
	// Per exact work-item+attempt state.
	launched := map[string]bool{}      // attemptKey
	terminal := map[string]bool{}      // attemptKey
	interruptedAt := map[string]bool{} // attemptKey
	// Track last launch order per work item for latest-open reduction.
	type launchRec struct {
		att string
		gen int
	}
	lastLaunch := map[string]launchRec{}

	for _, ev := range events {
		id := strings.TrimSpace(ev.WorkItemID)
		att := strings.TrimSpace(ev.AttemptID)
		switch ev.Kind {
		case "launch", "pid":
			if id == "" || att == "" {
				continue
			}
			k := attemptKey(id, att)
			launched[k] = true
			g := ParseAttemptGeneration(att)
			if prev, ok := lastLaunch[id]; !ok || g >= prev.gen {
				lastLaunch[id] = launchRec{att: att, gen: g}
			}
		case "terminal", "reuse", "integrate":
			// Close only the matching attempt (require attempt_id).
			if id == "" || att == "" {
				continue
			}
			k := attemptKey(id, att)
			terminal[k] = true
			delete(interruptedAt, k)
		case "interrupt":
			interrupted = true
			if IsParentInterrupt(ev) {
				// Parent interrupt: mark every currently open attempt as aborted.
				for k := range launched {
					if terminal[k] || interruptedAt[k] {
						continue
					}
					interruptedAt[k] = true
					wid, aatt := splitAttemptKey(k)
					// Keep latest aborted generation per work item.
					g := ParseAttemptGeneration(aatt)
					if prev, ok := aborted[wid]; ok {
						if ParseAttemptGeneration(prev) > g {
							continue
						}
					}
					aborted[wid] = aatt
				}
				continue
			}
			// Child interrupt: only this exact attempt.
			if id == "" || att == "" {
				continue
			}
			k := attemptKey(id, att)
			if terminal[k] {
				continue
			}
			interruptedAt[k] = true
			g := ParseAttemptGeneration(att)
			if prev, ok := aborted[id]; ok {
				if ParseAttemptGeneration(prev) > g {
					continue
				}
			}
			aborted[id] = att
		}
	}
	// Reduce aborted to latest open-at-interrupt generation only (already done).
	// Drop aborted entries that later terminalized the same attempt.
	for id, att := range aborted {
		if terminal[attemptKey(id, att)] {
			delete(aborted, id)
		}
	}
	return interrupted, aborted
}

// OpenLaunchesWithoutTerminal returns workItemID → attemptID for each open
// attempt (launch/pid with no terminal/reuse/integrate and no interrupt for
// that exact attempt). Older g0 closed/interrupted attempts do not hide a later
// open g1. Callers that require a single open attempt per work item must call
// ValidateEventStreamInvariants first (overlapping open attempts fail closed).
// When multiple open attempts exist for one work item this map keeps the highest
// generation only for resume convenience after invariants have passed.
func OpenLaunchesWithoutTerminal(events []Event) map[string]string {
	type rec struct {
		att string
		gen int
	}
	openKeys := map[string]rec{} // attemptKey → rec
	terminal := map[string]bool{}
	interruptedAt := map[string]bool{}

	for _, ev := range events {
		id := strings.TrimSpace(ev.WorkItemID)
		att := strings.TrimSpace(ev.AttemptID)
		switch ev.Kind {
		case "launch", "pid":
			if id == "" || att == "" {
				continue
			}
			k := attemptKey(id, att)
			openKeys[k] = rec{att: att, gen: ParseAttemptGeneration(att)}
		case "terminal", "reuse", "integrate":
			if id == "" || att == "" {
				continue
			}
			k := attemptKey(id, att)
			terminal[k] = true
			delete(openKeys, k)
		case "interrupt":
			if IsParentInterrupt(ev) {
				for k := range openKeys {
					interruptedAt[k] = true
					delete(openKeys, k)
				}
				continue
			}
			if id == "" || att == "" {
				continue
			}
			k := attemptKey(id, att)
			interruptedAt[k] = true
			delete(openKeys, k)
		}
	}
	// After invariants pass, at most one open attempt per work item remains.
	best := map[string]rec{}
	for k, r := range openKeys {
		if terminal[k] || interruptedAt[k] {
			continue
		}
		wid, _ := splitAttemptKey(k)
		if prev, ok := best[wid]; ok && prev.gen > r.gen {
			continue
		}
		best[wid] = r
	}
	out := map[string]string{}
	for wid, r := range best {
		out[wid] = r.att
	}
	return out
}

// FailedRetryGenerations returns work-item → next attempt generation for
// children whose latest terminal is non-succeeded (failed/cancelled/etc).
// Resume retries must not re-launch the same attempt_id (exactly_once /
// dupLaunch). Succeeded / reused children are omitted; callers that already
// seed PriorSucceeded will skip re-exec entirely.
// Generation is max seen -gN for that work item + 1.
func FailedRetryGenerations(events []Event) map[string]int {
	maxGen := map[string]int{}
	// last non-empty terminal per work item (event order).
	lastTerm := map[string]string{}
	for _, ev := range events {
		id := strings.TrimSpace(ev.WorkItemID)
		if id == "" {
			continue
		}
		if g := parseAttemptGeneration(ev.AttemptID); g >= 0 {
			if g > maxGen[id] {
				maxGen[id] = g
			}
		}
		switch ev.Kind {
		case "terminal":
			if t := strings.TrimSpace(ev.Terminal); t != "" {
				lastTerm[id] = t
			}
		case "reuse", "integrate":
			// Durable success path — do not treat as failed retry.
			lastTerm[id] = "succeeded"
		}
	}
	out := map[string]int{}
	for id, term := range lastTerm {
		if strings.EqualFold(term, "succeeded") {
			continue
		}
		out[id] = maxGen[id] + 1
	}
	return out
}

// parseAttemptGeneration extracts N from att-…-gN. Returns -1 when absent.
func parseAttemptGeneration(attemptID string) int {
	return ParseAttemptGeneration(attemptID)
}

// ParseAttemptGeneration extracts the 0-indexed attempt suffix N from
// att-…-gN. Returns -1 when absent or malformed. Exported for goalrun resume
// aborted-ID validation (never invent next generation without a proven g).
func ParseAttemptGeneration(attemptID string) int {
	attemptID = strings.TrimSpace(attemptID)
	idx := strings.LastIndex(attemptID, "-g")
	if idx < 0 || idx+2 >= len(attemptID) {
		return -1
	}
	n := 0
	for _, r := range attemptID[idx+2:] {
		if r < '0' || r > '9' {
			return -1
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// RecoverOpenLaunchInterrupts appends child interrupt events for exact open
// attempts that have no terminal/reuse/integrate and no interrupt for that
// same attempt. Historical g0 interrupts do not suppress recovery of a later
// open g1. Repeated recovery of the same exact open attempt is idempotent
// (second call appends zero). Generation is always suffix+1, never hardcoded.
// Returns the number of interrupt lines appended.
func RecoverOpenLaunchInterrupts(elog *EventLog, projectID, runID string) (int, error) {
	if elog == nil {
		return 0, nil
	}
	events, err := elog.ReadAllForRun(projectID, runID)
	if err != nil {
		return 0, err
	}
	// Plan-independent identity on every child lifecycle event before append.
	// Direct callers must not recover over generation-mismatched/partial logs.
	for i, ev := range events {
		if err := ValidateChildEventIdentity(ev); err != nil {
			return 0, fmt.Errorf("workflowrun: recover pre-append identity line %d: %w", i, err)
		}
	}
	// Fail closed before any append on overlapping open attempts / reopens.
	if err := ValidateEventStreamInvariants(events); err != nil {
		return 0, err
	}
	// No global early-return on historical interrupt — open set is attempt-scoped.
	open := OpenLaunchesWithoutTerminal(events)
	if len(open) == 0 {
		return 0, nil
	}
	n := 0
	// Stable order for tests/determinism.
	ids := make([]string, 0, len(open))
	for id := range open {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		att := open[id]
		gen, gerr := ClaimGenerationFromAttemptID(att)
		if gerr != nil {
			return n, fmt.Errorf("workflowrun: recover interrupt %s: %w", id, gerr)
		}
		// Soft ledger recovery: complete service_forced_interrupt structured pair
		// (distinct from authoritative hard recovery; does not select gN+1).
		intID := newInterruptID(att, gen)
		payload, _ := json.Marshal(map[string]string{
			"failure_class":   "forced_interrupt",
			"interrupt_class": InterruptClassServiceForced,
			"interrupt_id":    intID,
			"terminal":        "cancelled",
			"work_item_id":    id,
			"attempt_id":      att,
		})
		if _, err := elog.Append(Event{
			ProjectID: projectID, RunID: runID,
			Kind: "interrupt", WorkItemID: id, AttemptID: att, Generation: gen,
			Terminal: "cancelled",
			Message:  "forced process kill recovery; open launch without terminal in ledger",
			Payload:  payload,
		}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
