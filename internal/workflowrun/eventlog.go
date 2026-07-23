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
	At         time.Time `json:"at"`
	ProjectID  string    `json:"project_id"`
	RunID      string    `json:"run_id"`
	Kind       string    `json:"kind"` // claim|launch|pid|terminal|integrate|interrupt|reuse|accept
	WorkItemID string    `json:"work_item_id,omitempty"`
	AttemptID  string    `json:"attempt_id,omitempty"`
	Terminal   string    `json:"terminal,omitempty"`
	PID        int       `json:"pid,omitempty"`
	CommitSHA  string    `json:"commit_sha,omitempty"`
	Message    string    `json:"message,omitempty"`
	Evidence   string    `json:"evidence,omitempty"`
	Generation int       `json:"generation,omitempty"`
}

// EventLog is an append-only JSONL log under the project runs directory.
type EventLog struct {
	mu   sync.Mutex
	path string
}

// OpenEventLog opens/creates the durable event log for a run under
// $HOME/projects/<project>/runs/<runID>/workflow-events.jsonl.
func OpenEventLog(homeDir, projectID, runID string) (*EventLog, error) {
	projectID = strings.TrimSpace(projectID)
	runID = strings.TrimSpace(runID)
	if projectID == "" || runID == "" {
		return nil, fmt.Errorf("workflowrun: event log requires project_id and run_id")
	}
	root := homeDir
	if root == "" {
		root = filepath.Join(os.TempDir(), "loopcoder-events-home")
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
func (e *EventLog) Append(ev Event) error {
	if e == nil {
		return nil
	}
	if strings.TrimSpace(ev.Schema) == "" {
		ev.Schema = EventSchema
	}
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(e.path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(e.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(raw, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// ReadAll loads all events (tests / evidence builders).
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
	var out []Event
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev Event
		if json.Unmarshal([]byte(line), &ev) == nil {
			out = append(out, ev)
		}
	}
	return out, nil
}

// InterruptedFromEvents reports whether a forced interrupt was recorded and
// which attempts were aborted mid-flight (for resume generation bumps).
func InterruptedFromEvents(events []Event) (interrupted bool, aborted map[string]string) {
	aborted = map[string]string{} // workItemID → attemptID
	launched := map[string]string{}
	terminal := map[string]bool{}
	for _, ev := range events {
		switch ev.Kind {
		case "launch", "pid":
			if ev.WorkItemID != "" && ev.AttemptID != "" {
				launched[ev.WorkItemID] = ev.AttemptID
			}
		case "terminal", "reuse", "integrate":
			if ev.WorkItemID != "" {
				terminal[ev.WorkItemID] = true
				delete(aborted, ev.WorkItemID)
			}
		case "interrupt":
			interrupted = true
			for id, att := range launched {
				if !terminal[id] {
					aborted[id] = att
				}
			}
			if ev.WorkItemID != "" && ev.AttemptID != "" && !terminal[ev.WorkItemID] {
				aborted[ev.WorkItemID] = ev.AttemptID
			}
		}
	}
	return interrupted, aborted
}

// OpenLaunchesWithoutTerminal returns work items with a launch/pid and no
// terminal/reuse/integrate. Used to derive forced-kill recovery interrupts
// from the append-only ledger (never invent booleans without launch facts).
func OpenLaunchesWithoutTerminal(events []Event) map[string]string {
	launched := map[string]string{}
	terminal := map[string]bool{}
	for _, ev := range events {
		switch ev.Kind {
		case "launch", "pid":
			if ev.WorkItemID != "" && ev.AttemptID != "" {
				launched[ev.WorkItemID] = ev.AttemptID
			}
		case "terminal", "reuse", "integrate":
			if ev.WorkItemID != "" {
				terminal[ev.WorkItemID] = true
			}
		}
	}
	open := map[string]string{}
	for id, att := range launched {
		if !terminal[id] {
			open[id] = att
		}
	}
	return open
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

// RecoverOpenLaunchInterrupts appends interrupt events for launches that have
// no terminal and no prior interrupt. This covers true process kill (parent
// SIGKILL / hard exit) where the graceful cancel path never ran. Facts come
// only from the existing ledger (open launch lines); nothing is invented when
// the ledger already has interrupt or no open launches.
// Returns the number of interrupt lines appended.
func RecoverOpenLaunchInterrupts(elog *EventLog, projectID, runID string) (int, error) {
	if elog == nil {
		return 0, nil
	}
	events, err := elog.ReadAll()
	if err != nil {
		return 0, err
	}
	if interrupted, _ := InterruptedFromEvents(events); interrupted {
		return 0, nil
	}
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
		if err := elog.Append(Event{
			ProjectID: projectID, RunID: runID,
			Kind: "interrupt", WorkItemID: id, AttemptID: att,
			Terminal: "cancelled",
			Message:  "forced process kill recovery; open launch without terminal in ledger",
		}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
