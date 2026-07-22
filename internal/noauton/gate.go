package noauton

import (
	"fmt"
	"strings"
)

// EntryPoint is an autonomous control-loop surface.
type EntryPoint string

const (
	EPCompile      EntryPoint = "compile"
	EPTick         EntryPoint = "tick"
	EPTrigger      EntryPoint = "trigger"
	EPPromote      EntryPoint = "promote"
	EPConductor    EntryPoint = "conductor_auto"
	EPOrchestrate  EntryPoint = "orchestration_loop"
	EPRiskGateAuto EntryPoint = "risk_gate_auto"
	EPIssueSynth   EntryPoint = "issue_synthesis"
	EPScheduleAuto EntryPoint = "autonomous_schedule"
	// Preserved
	EPBoundedWave   EntryPoint = "bounded_wave_scheduler"
	EPHumanGate     EntryPoint = "human_release_gate"
	EPWatcherFacade EntryPoint = "zero_model_watcher_facade"
)

// Decision for invoking an entry point.
type Decision struct {
	Allowed bool
	Reasons []string
}

// Evaluate denies autonomous loops; allows explicit human/bounded facades.
func Evaluate(ep EntryPoint, explicitHuman bool, boundedWorkflow bool) Decision {
	switch ep {
	case EPCompile, EPTick, EPTrigger, EPPromote, EPConductor, EPOrchestrate, EPRiskGateAuto, EPIssueSynth, EPScheduleAuto:
		return Decision{
			Allowed: false,
			Reasons: []string{fmt.Sprintf("%s removed from v0.9: no autonomous compile/tick/trigger/promote/issue-synthesis", ep)},
		}
	case EPBoundedWave:
		if !boundedWorkflow {
			return Decision{Allowed: false, Reasons: []string{"bounded wave requires explicit workflow definition"}}
		}
		return Decision{Allowed: true, Reasons: []string{"explicit bounded workflow scheduler"}}
	case EPHumanGate:
		if !explicitHuman {
			return Decision{Allowed: false, Reasons: []string{"human/release gate requires explicit human action"}}
		}
		return Decision{Allowed: true, Reasons: []string{"explicit human/release gate"}}
	case EPWatcherFacade:
		return Decision{Allowed: true, Reasons: []string{"deterministic zero-model watcher via accepted facade only"}}
	default:
		return Decision{Allowed: false, Reasons: []string{"unknown entry point"}}
	}
}

// CLIDenied reports autonomous CLI verbs.
func CLIDenied(command string) bool {
	switch strings.ToLower(strings.TrimSpace(command)) {
	case "compile", "tick", "trigger", "promote", "dispatch", "orchestrate-auto", "synth-issues":
		return true
	default:
		return false
	}
}

// RoadmapMarkerInert documents that markers are documentation only.
func RoadmapMarkerInert(marker string) bool {
	// Any V090-/ROADMAP marker text is never an executable trigger.
	m := strings.ToUpper(marker)
	return strings.Contains(m, "V090-") || strings.Contains(m, "ROADMAP") || strings.Contains(m, "<!--")
}

// Inventory of deleted vs preserved.
type Entry struct {
	Name        EntryPoint `json:"name"`
	Disposition string     `json:"disposition"`
}

// Inventory returns disposition list.
func Inventory() []Entry {
	return []Entry{
		{EPCompile, "deleted"}, {EPTick, "deleted"}, {EPTrigger, "deleted"},
		{EPPromote, "deleted"}, {EPConductor, "deleted"}, {EPOrchestrate, "deleted"},
		{EPRiskGateAuto, "deleted"}, {EPIssueSynth, "deleted"}, {EPScheduleAuto, "deleted"},
		{EPBoundedWave, "preserved_explicit"}, {EPHumanGate, "preserved_explicit"},
		{EPWatcherFacade, "preserved_facade"},
	}
}
