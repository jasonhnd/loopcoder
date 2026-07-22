package providerkit

import (
	"fmt"
	"strings"
	"sync"

	"github.com/jasonhnd/loopcoder/internal/providerdesc"
)

const (
	SchemaChecklist = "loopcoder.provider.kit.checklist.v1"
	SchemaAllowlist = "loopcoder.provider.kit.allowlist.v1"
	// MinContractVersion is the minimum SPI version accepted.
	MinContractVersion = 1
	MaxContractVersion = 1
)

// Checklist is the developer registration checklist.
type Checklist struct {
	Schema   string   `json:"schema"`
	Items    []string `json:"items"`
	Required []string `json:"required"`
}

// DefaultChecklist returns the full extension checklist.
func DefaultChecklist() Checklist {
	return Checklist{
		Schema: SchemaChecklist,
		Required: []string{
			"auth_ownership", "source_authority", "bounds", "redaction",
			"model_identity", "quota_semantics", "actual_route_proof",
			"cancellation", "child_cleanup", "contract_version",
		},
		Items: []string{
			"auth_ownership", "source_authority", "bounds", "redaction",
			"model_identity", "quota_semantics", "actual_route_proof",
			"cancellation", "child_cleanup", "contract_version",
			"no_scheduler_edit", "no_store_schema_edit", "no_route_engine_edit",
			"explicit_allowlist", "no_repo_autoload",
		},
	}
}

// ValidateChecklist reports missing required items.
func ValidateChecklist(claimed []string) (ok bool, missing []string) {
	need := map[string]bool{}
	for _, r := range DefaultChecklist().Required {
		need[r] = true
	}
	for _, c := range claimed {
		delete(need, c)
	}
	for m := range need {
		missing = append(missing, m)
	}
	return len(missing) == 0, missing
}

// VersionPolicy checks descriptor SPI compatibility.
func VersionPolicy(version int) error {
	if version < MinContractVersion || version > MaxContractVersion {
		return fmt.Errorf("providerkit: incompatible contract version %d (supported %d-%d)",
			version, MinContractVersion, MaxContractVersion)
	}
	return nil
}

// Allowlist is the explicit registration gate (no arbitrary discovery).
type Allowlist struct {
	mu  sync.Mutex
	ids map[string]bool
}

// NewAllowlist creates an empty allowlist.
func NewAllowlist(ids ...string) *Allowlist {
	a := &Allowlist{ids: map[string]bool{}}
	for _, id := range ids {
		a.ids[strings.ToLower(strings.TrimSpace(id))] = true
	}
	return a
}

// Allow adds an adapter ID (owner/admin only surface).
func (a *Allowlist) Allow(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ids[strings.ToLower(strings.TrimSpace(id))] = true
}

// IsAllowed reports whether id may register.
func (a *Allowlist) IsAllowed(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ids[strings.ToLower(strings.TrimSpace(id))]
}

// RegisterSafe validates version, allowlist, descriptor, then registers.
// Untrusted adapters from customer repos cannot auto-load.
func RegisterSafe(reg *providerdesc.Registry, allow *Allowlist, ad providerdesc.Adapter, fromCustomerRepo bool) (providerdesc.Registered, error) {
	if fromCustomerRepo {
		return providerdesc.Registered{}, fmt.Errorf("providerkit: auto-load from customer repo forbidden")
	}
	if ad == nil {
		return providerdesc.Registered{}, fmt.Errorf("providerkit: nil adapter")
	}
	d := ad.Descriptor()
	if err := VersionPolicy(d.Version); err != nil {
		return providerdesc.Registered{}, err
	}
	if allow != nil && !allow.IsAllowed(d.AdapterID) {
		return providerdesc.Registered{}, fmt.Errorf("providerkit: adapter %q not allowlisted", d.AdapterID)
	}
	return reg.Register(ad)
}

// SupportModel documents built-in vs future external adapters.
type SupportModel struct {
	OfficialBuiltIn []string `json:"official_builtin"`
	FutureExternal  string   `json:"future_external"`
	DeferredPacks   string   `json:"deferred_user_packs"`
}

// DefaultSupportModel returns the v0.9 support statement.
func DefaultSupportModel() SupportModel {
	return SupportModel{
		OfficialBuiltIn: []string{"codex", "claude", "gemini", "antigravity", "grok", "example"},
		FutureExternal:  "explicit allowlisted registration via providerkit; security review required",
		DeferredPacks:   "user-installable provider packs deferred; need signing/trust/update design",
	}
}
