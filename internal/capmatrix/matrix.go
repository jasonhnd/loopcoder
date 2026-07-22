package capmatrix

import "sort"

// EvidenceTier for a capability claim.
type EvidenceTier string

const (
	// TierProduct tested on product path / dual-green pre-prod evidence.
	TierProduct EvidenceTier = "product"
	// TierFixture pure fixture/unit only — not live product canary.
	TierFixture EvidenceTier = "fixture_only"
	// TierExperimental incomplete or owner-gated.
	TierExperimental EvidenceTier = "experimental"
	// TierUnsupported explicitly not supported in v0.9.
	TierUnsupported EvidenceTier = "unsupported"
	// TierUnknown not yet classified.
	TierUnknown EvidenceTier = "unknown"
)

// Capability is one matrix row.
type Capability struct {
	ID          string       `json:"id"`
	Area        string       `json:"area"`
	Name        string       `json:"name"`
	Supported   bool         `json:"supported"`
	Evidence    EvidenceTier `json:"evidence"`
	Platform    string       `json:"platform,omitempty"`
	Remediation string       `json:"remediation,omitempty"` // doctor-aligned code hint
	Notes       string       `json:"notes"`
}

// DoctorCode is a doctor --json style code aligned with docs.
type DoctorCode struct {
	Code        string `json:"code"`
	Severity    string `json:"severity"` // fail|warn|info|ok
	Remediation string `json:"remediation"`
}

// Matrix returns the closed v0.9.0 capability set.
func Matrix() []Capability {
	rows := []Capability{
		{ID: "plat-darwin-arm64", Area: "platform", Name: "Darwin arm64 host", Supported: true, Evidence: TierProduct, Platform: "darwin/arm64", Notes: "primary supported platform"},
		{ID: "plat-other", Area: "platform", Name: "non-darwin-arm64 hosts", Supported: false, Evidence: TierUnsupported, Notes: "not a v0.9 product platform"},

		{ID: "prov-codex", Area: "provider", Name: "Codex worker adapter", Supported: true, Evidence: TierProduct, Notes: "official adapter facade"},
		{ID: "prov-claude", Area: "provider", Name: "Claude worker adapter", Supported: true, Evidence: TierProduct, Notes: "official adapter facade"},
		{ID: "prov-grok", Area: "provider", Name: "Grok worker adapter", Supported: true, Evidence: TierProduct, Notes: "official adapter facade"},
		{ID: "prov-antigravity", Area: "provider", Name: "Antigravity worker adapter", Supported: true, Evidence: TierProduct, Notes: "official adapter facade"},
		{ID: "prov-gemini", Area: "provider", Name: "Gemini direct adapter", Supported: false, Evidence: TierExperimental, Notes: "experimental; not Full GO"},

		{ID: "ui-protocol", Area: "ui", Name: "loopcoder.ui.v1 projection", Supported: true, Evidence: TierFixture, Notes: "pure event projection"},
		{ID: "ui-final-mile", Area: "ui", Name: "final-mile host bridge", Supported: true, Evidence: TierProduct, Notes: "where wired"},

		{ID: "mode-direct", Area: "workflow", Name: "explicit direct run", Supported: true, Evidence: TierProduct, Notes: "ordinary development path"},
		{ID: "mode-bounded-wave", Area: "workflow", Name: "bounded wave scheduler", Supported: true, Evidence: TierProduct, Notes: "explicit workflow definition required"},
		{ID: "mode-autonomous", Area: "workflow", Name: "autonomous compile/tick/trigger", Supported: false, Evidence: TierUnsupported, Remediation: "use_ordinary_dev", Notes: "removed V090-076"},

		{ID: "priv-repo", Area: "privacy", Name: "private repository redaction", Supported: true, Evidence: TierFixture, Notes: "V090-067 pure canary"},
		{ID: "handoff-github", Area: "handoff", Name: "terminal GitHub rehydration", Supported: true, Evidence: TierFixture, Notes: "V090-068"},
		{ID: "cross-mac-lease", Area: "handoff", Name: "cross-Mac lease/DB merge", Supported: false, Evidence: TierUnsupported, Notes: "removed V090-077"},

		{ID: "store-global", Area: "storage", Name: "global/project store layout", Supported: true, Evidence: TierProduct, Notes: "no production repo-local .loopcoder writes"},
		{ID: "store-repo-local", Area: "storage", Name: "repo-local runtime sidecars", Supported: false, Evidence: TierUnsupported, Remediation: "register_global_project", Notes: "V090-072"},

		{ID: "mig-export", Area: "migration", Name: "v0.8 read-only export", Supported: true, Evidence: TierFixture, Notes: "V090-069"},
		{ID: "mig-import", Area: "migration", Name: "v0.9 import", Supported: true, Evidence: TierFixture, Notes: "V090-070"},
		{ID: "mig-auto-write-v08", Area: "migration", Name: "v0.8 storage mutation from v0.9", Supported: false, Evidence: TierUnsupported, Notes: "V090-073"},

		{ID: "gate-human-merge", Area: "release", Name: "human merge gate", Supported: true, Evidence: TierProduct, Notes: "default scaffold"},
		{ID: "fed-nested", Area: "release", Name: "federation/nested plans", Supported: false, Evidence: TierUnsupported, Notes: "V090-077"},
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Area != rows[j].Area {
			return rows[i].Area < rows[j].Area
		}
		return rows[i].ID < rows[j].ID
	})
	return rows
}

// DoctorCodes aligned with remediation strings in the matrix.
func DoctorCodes() []DoctorCode {
	return []DoctorCode{
		{Code: "platform_unsupported", Severity: "fail", Remediation: "use darwin/arm64 host"},
		{Code: "use_ordinary_dev", Severity: "fail", Remediation: "do not use compile/tick/trigger; use ordinary branch/PR workflow"},
		{Code: "register_global_project", Severity: "fail", Remediation: "register project globally; never write <repo>/.loopcoder runtime"},
		{Code: "provider_not_ready", Severity: "warn", Remediation: "install/auth provider CLI via official adapter; re-run doctor"},
		{Code: "migration_export_only", Severity: "info", Remediation: "use export-v08 then import-v09; no v0.8 write from v0.9"},
		{Code: "human_merge_required", Severity: "info", Remediation: "set adapters.gate human-merge; owner merges production"},
	}
}

// ByArea groups capabilities.
func ByArea(area string) []Capability {
	var out []Capability
	for _, c := range Matrix() {
		if c.Area == area {
			out = append(out, c)
		}
	}
	return out
}

// UnsupportedIDs returns unsupported capability ids.
func UnsupportedIDs() []string {
	var out []string
	for _, c := range Matrix() {
		if !c.Supported || c.Evidence == TierUnsupported {
			out = append(out, c.ID)
		}
	}
	sort.Strings(out)
	return out
}

// Lookup finds by id.
func Lookup(id string) (Capability, bool) {
	for _, c := range Matrix() {
		if c.ID == id {
			return c, true
		}
	}
	return Capability{}, false
}
