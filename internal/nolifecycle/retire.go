package nolifecycle

import (
	"fmt"
	"sort"
	"strings"
)

// WriterKind is a superseded lifecycle writer surface.
type WriterKind string

const (
	WriterProgress     WriterKind = "progress"
	WriterReport       WriterKind = "report"
	WriterReportQuery  WriterKind = "reportquery"
	WriterReporter     WriterKind = "reporter_lifecycle"
	WriterRelay        WriterKind = "relay"
	WriterRelayGate    WriterKind = "relaygate"
	WriterProgressHost WriterKind = "progresshost"
	WriterOutbox       WriterKind = "outbox"
	WriterClaims       WriterKind = "claims_ack"
)

// Action attempted against a lifecycle writer.
type Action string

const (
	ActionCreate Action = "create"
	ActionFlush  Action = "flush"
	ActionClose  Action = "close"
	ActionAck    Action = "ack"
	ActionWrite  Action = "write"
	// ActionProject is pure projection from events — allowed for UI helpers.
	ActionProject Action = "project"
)

// InventoryEntry one package/symbol disposition.
type InventoryEntry struct {
	Package     string     `json:"package"`
	Kind        WriterKind `json:"kind"`
	Disposition string     `json:"disposition"` // removed_writer|pure_projection
	Notes       string     `json:"notes"`
}

// DefaultInventory maps progress/report/relay packages.
func DefaultInventory() []InventoryEntry {
	return []InventoryEntry{
		{Package: "internal/progress", Kind: WriterProgress, Disposition: "removed_writer", Notes: "superseded by project events"},
		{Package: "internal/report", Kind: WriterReport, Disposition: "removed_writer", Notes: "lifecycle writes removed"},
		{Package: "internal/reportquery", Kind: WriterReportQuery, Disposition: "pure_projection", Notes: "query may project events only"},
		{Package: "internal/reporter", Kind: WriterReporter, Disposition: "pure_projection", Notes: "rendering from events for loopcoder.ui.v1"},
		{Package: "internal/relay", Kind: WriterRelay, Disposition: "removed_writer", Notes: "relay gates removed"},
		{Package: "internal/relaygate", Kind: WriterRelayGate, Disposition: "removed_writer", Notes: "ack/relay superseded by UI-client acknowledgement"},
		{Package: "internal/progresshost", Kind: WriterProgressHost, Disposition: "removed_writer", Notes: "host progress writer removed"},
		{Package: "internal/outbox", Kind: WriterOutbox, Disposition: "removed_writer", Notes: "outbox flush/close removed"},
	}
}

// Decision for a lifecycle write attempt.
type Decision struct {
	Allowed bool
	Reasons []string
}

// Evaluate denies create/flush/close/ack/write on superseded writers.
// Pure projection from event input is allowed only for pure_projection packages.
func Evaluate(kind WriterKind, action Action, fromEvents bool) Decision {
	// All lifecycle mutation denied.
	switch action {
	case ActionCreate, ActionFlush, ActionClose, ActionAck, ActionWrite:
		return Decision{
			Allowed: false,
			Reasons: []string{fmt.Sprintf("%s %s retired; project events are sole lifecycle truth", kind, action)},
		}
	case ActionProject:
		if !fromEvents {
			return Decision{Allowed: false, Reasons: []string{"projection requires pure event/projection input"}}
		}
		// Only reportquery/reporter projection classes
		if kind == WriterReportQuery || kind == WriterReporter {
			return Decision{Allowed: true, Reasons: []string{"pure projection for loopcoder.ui.v1"}}
		}
		return Decision{Allowed: false, Reasons: []string{fmt.Sprintf("%s is not a pure projection surface", kind)}}
	default:
		return Decision{Allowed: false, Reasons: []string{"unknown action"}}
	}
}

// CompatCommandDenied reports whether a CLI path would create lifecycle state.
func CompatCommandDenied(command string) bool {
	c := strings.ToLower(strings.TrimSpace(command))
	switch c {
	case "progress", "report-write", "relay-flush", "outbox-flush", "outbox-close", "ack-progress":
		return true
	default:
		return false
	}
}

// UISchema is the only allowed report path schema for projections.
const UISchema = "loopcoder.ui.v1"

// ProjectReport is a pure projection from events (no parallel lifecycle write).
type ProjectReport struct {
	Schema string   `json:"schema"`
	Events []string `json:"event_ids"` // references only
	Body   string   `json:"body"`      // redacted/rendered
}

// ProjectFromEvents builds a UI projection; never writes progress/outbox.
func ProjectFromEvents(eventIDs []string, rendered string) (ProjectReport, error) {
	if len(eventIDs) == 0 {
		return ProjectReport{}, fmt.Errorf("event input required")
	}
	ids := append([]string(nil), eventIDs...)
	sort.Strings(ids)
	return ProjectReport{Schema: UISchema, Events: ids, Body: rendered}, nil
}
