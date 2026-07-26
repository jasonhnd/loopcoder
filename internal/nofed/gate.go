package nofed

import (
	"fmt"
	"strings"
)

// System is a removed distributed/nested surface.
type System string

const (
	SysNestedPlan      System = "nested_plan"
	SysNestedScope     System = "nested_scope"
	SysFederation      System = "agent_federation"
	SysFedLock         System = "federation_lock"
	SysStateBranch     System = "state_branch"
	SysStatePublish    System = "state_publication"
	SysConductorLease  System = "conductor_lease"
	SysCrossMacLease   System = "cross_machine_lease"
	SysStateDBPushPull System = "state_db_push_pull_merge"
	// Preserved
	SysWorkGraph       System = "work_graph_p5"
	SysNativeChild     System = "native_child_containment"
	SysGitHubRehydrate System = "terminal_github_rehydrate"
	SysV08ExportRead   System = "v08_export_reader"
)

// Action on a system.
type Action string

const (
	ActionExecute Action = "execute"
	ActionWrite   Action = "write"
	ActionLock    Action = "lock"
	ActionPush    Action = "push"
	ActionPull    Action = "pull"
	ActionMerge   Action = "merge"
	ActionRead    Action = "read"
)

// Decision result.
type Decision struct {
	Allowed bool
	Reasons []string
}

// Evaluate denies federation/nested/state-branch/lease writes; allows preserved.
func Evaluate(sys System, action Action) Decision {
	switch sys {
	case SysNestedPlan, SysNestedScope, SysFederation, SysFedLock,
		SysStateBranch, SysStatePublish, SysConductorLease, SysCrossMacLease, SysStateDBPushPull:
		return Decision{
			Allowed: false,
			Reasons: []string{fmt.Sprintf("%s %s removed: no distributed DB peers or multi-Mac ownership in v0.9", sys, action)},
		}
	case SysWorkGraph, SysNativeChild, SysGitHubRehydrate:
		return Decision{Allowed: true, Reasons: []string{"preserved P5/handoff capability: " + string(sys)}}
	case SysV08ExportRead:
		if action == ActionRead {
			return Decision{Allowed: true, Reasons: []string{"explicit v0.8 export reader only"}}
		}
		return Decision{Allowed: false, Reasons: []string{"v08 export reader is read-only"}}
	default:
		return Decision{Allowed: false, Reasons: []string{"unknown system"}}
	}
}

// CLIDenied for removed federation/lease CLIs.
func CLIDenied(command string) bool {
	switch strings.ToLower(strings.TrimSpace(command)) {
	case "federate", "lease-acquire", "lease-release", "state-branch-push", "state-branch-pull", "nested-plan":
		return true
	default:
		return false
	}
}

// Capability reports unsupported vs supported.
type Capability struct {
	Name      System `json:"name"`
	Supported bool   `json:"supported"`
	Notes     string `json:"notes"`
}

// CapabilityMatrix for migration reports.
func CapabilityMatrix() []Capability {
	return []Capability{
		{SysNestedPlan, false, "removed"},
		{SysFederation, false, "removed"},
		{SysStateBranch, false, "removed"},
		{SysCrossMacLease, false, "removed"},
		{SysStateDBPushPull, false, "removed"},
		{SysWorkGraph, true, "P5 Work Graph"},
		{SysNativeChild, true, "P5 native child containment"},
		{SysGitHubRehydrate, true, "terminal GitHub rehydration"},
		{SysV08ExportRead, true, "read-only export"},
	}
}
