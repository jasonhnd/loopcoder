package noproviderdup

import (
	"fmt"
	"strings"
)

// Surface is a dual-written provider/router surface.
type Surface string

const (
	SurfaceInventory     Surface = "providerinventory"
	SurfaceAgentLegacy   Surface = "agent_legacy"
	SurfaceRoutingWrite  Surface = "routing_write"
	SurfaceQuotaSnapshot Surface = "quota_snapshot_write"
	SurfaceReconcile     Surface = "reconciliation_write"
	SurfaceFallbackRoute Surface = "fallback_decision"
	SurfaceRawSQLRepo    Surface = "raw_sql_repository"
	// Preserved
	SurfaceOfficialAdapter Surface = "official_adapter"
	SurfacePinReader       Surface = "explicit_pin_reader"
	SurfaceHistoryImport   Surface = "historical_route_import"
	SurfaceProcessInvoke   Surface = "process_invocation_behind_adapter"
)

// Action on a surface.
type Action string

const (
	ActionRegister SurfaceAction = "register"
	ActionRefresh  SurfaceAction = "refresh"
	ActionDecide   SurfaceAction = "decide"
	ActionFallback SurfaceAction = "fallback"
	ActionWrite    SurfaceAction = "write"
	ActionRead     SurfaceAction = "read"
	ActionInvoke   SurfaceAction = "invoke"
)

// SurfaceAction alias for clarity in Evaluate.
type SurfaceAction = Action

// Entry inventory row.
type Entry struct {
	Surface     Surface `json:"surface"`
	Disposition string  `json:"disposition"` // removed|facade_only|preserved_reader
	Notes       string  `json:"notes"`
}

// Inventory lists dual-write surfaces and disposition.
func Inventory() []Entry {
	return []Entry{
		{Surface: SurfaceInventory, Disposition: "removed", Notes: "superseded by official adapter registration"},
		{Surface: SurfaceAgentLegacy, Disposition: "removed", Notes: "legacy agent entry points removed from new-path reachability"},
		{Surface: SurfaceRoutingWrite, Disposition: "removed", Notes: "route writers superseded by V090-037..055 router"},
		{Surface: SurfaceQuotaSnapshot, Disposition: "removed", Notes: "stale quota snapshot writers removed"},
		{Surface: SurfaceReconcile, Disposition: "removed", Notes: "duplicate reconciliation writers removed"},
		{Surface: SurfaceFallbackRoute, Disposition: "removed", Notes: "fallback decision paths removed"},
		{Surface: SurfaceRawSQLRepo, Disposition: "removed", Notes: "raw SQL repos superseded by machine/project stores"},
		{Surface: SurfaceOfficialAdapter, Disposition: "facade_only", Notes: "only accepted adapter facade"},
		{Surface: SurfacePinReader, Disposition: "preserved_reader", Notes: "explicit pin readers retained"},
		{Surface: SurfaceHistoryImport, Disposition: "preserved_reader", Notes: "historical route import readers retained"},
		{Surface: SurfaceProcessInvoke, Disposition: "facade_only", Notes: "invocation only behind official adapters"},
	}
}

// Decision for an operation.
type Decision struct {
	Allowed bool
	Reasons []string
}

// Evaluate enforces retirement: removed surfaces cannot write/register/decide;
// readers and facade-only paths allowed per disposition.
func Evaluate(surface Surface, action Action, viaOfficialAdapter bool) Decision {
	disp := disposition(surface)
	switch disp {
	case "removed":
		if action == ActionRead {
			return Decision{Allowed: false, Reasons: []string{fmt.Sprintf("%s removed; use official facade/readers", surface)}}
		}
		return Decision{Allowed: false, Reasons: []string{fmt.Sprintf("%s %s retired after router conformance", surface, action)}}
	case "preserved_reader":
		if action == ActionRead {
			return Decision{Allowed: true, Reasons: []string{"preserved pin/history reader"}}
		}
		return Decision{Allowed: false, Reasons: []string{fmt.Sprintf("%s is read-only preserved", surface)}}
	case "facade_only":
		if surface == SurfaceProcessInvoke || surface == SurfaceOfficialAdapter {
			if !viaOfficialAdapter {
				return Decision{Allowed: false, Reasons: []string{"must go through official provider adapter facade"}}
			}
			if action == ActionInvoke || action == ActionRead || action == ActionRegister {
				// register only on official adapter surface
				if action == ActionRegister && surface != SurfaceOfficialAdapter {
					return Decision{Allowed: false, Reasons: []string{"registration only on official adapter"}}
				}
				return Decision{Allowed: true, Reasons: []string{"official adapter facade"}}
			}
		}
		return Decision{Allowed: false, Reasons: []string{fmt.Sprintf("action %s not allowed on facade_only %s", action, surface)}}
	default:
		return Decision{Allowed: false, Reasons: []string{"unknown surface"}}
	}
}

// CLICallerDenied reports new-path CLI that would hit duplicate writers.
func CLICallerDenied(command string) bool {
	switch strings.ToLower(strings.TrimSpace(command)) {
	case "provider-inventory-refresh", "route-write", "quota-snapshot", "agent-legacy-run", "fallback-route":
		return true
	default:
		return false
	}
}

func disposition(s Surface) string {
	for _, e := range Inventory() {
		if e.Surface == s {
			return e.Disposition
		}
	}
	return ""
}
