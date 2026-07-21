package securitypolicy

// Capability is a deny-by-default permission required before a side effect.
type Capability string

const (
	CapReadRepo        Capability = "cap.read_repo"
	CapBoundedWrite    Capability = "cap.bounded_write"
	CapNetworkProvider Capability = "cap.network_provider"
	CapNetworkGitHub   Capability = "cap.network_github"
	CapGitMutate       Capability = "cap.git_mutate"
	CapProcessControl  Capability = "cap.process_control"
	CapNativeDelegate  Capability = "cap.native_delegate"
	CapUIAction        Capability = "cap.ui_action"
	CapConfigFreeze    Capability = "cap.config_freeze"
	CapMachineState    Capability = "cap.machine_state"
	CapProjectState    Capability = "cap.project_state"
)

// EnforcementOwner names the package or planned issue that must enforce a capability.
type EnforcementOwner struct {
	Capability Capability
	Owner      string
	Status     string // existing | planned | gap
}

// CapabilityOwners is the fail-closed inventory: unlisted capabilities are denied.
func CapabilityOwners() []EnforcementOwner {
	return []EnforcementOwner{
		{CapReadRepo, "internal/readonlyexec", "existing"},
		{CapBoundedWrite, "internal/writeexec", "existing"},
		{CapNetworkProvider, "run admission + internal/supervisedexec", "planned"},
		{CapNetworkGitHub, "direct-run GitHub stages", "planned"},
		{CapGitMutate, "internal/gitutil + writeexec worktree isolation", "existing"},
		{CapProcessControl, "internal/supervisedexec", "existing"},
		{CapNativeDelegate, "denied unless attempt pin allows", "gap"},
		{CapUIAction, "UI protocol ack verification (P2)", "gap"},
		{CapConfigFreeze, "V090-085 effective-policy snapshot", "planned"},
		{CapMachineState, "internal/store + machine schema issues", "planned"},
		{CapProjectState, "internal/store + project schema issues", "planned"},
	}
}

// KnownCapability reports whether cap is part of the v0.9 vocabulary.
func KnownCapability(cap Capability) bool {
	for _, owner := range CapabilityOwners() {
		if owner.Capability == cap {
			return true
		}
	}
	return false
}

// Allowed reports whether a capability may proceed. Unknown capabilities are
// denied. Capabilities marked gap/planned are still "known" but callers must
// not treat planned gaps as enforced.
func Allowed(granted map[Capability]bool, required Capability) bool {
	if !KnownCapability(required) {
		return false
	}
	if granted == nil {
		return false
	}
	return granted[required]
}
