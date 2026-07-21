package geminiobs

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/obsplan"
	"github.com/jasonhnd/loopcoder/internal/providerdesc"
)

const (
	AdapterID      = "gemini"
	SchemaCatalog  = "loopcoder.gemini.catalog.v1"
	SchemaSnapshot = "loopcoder.gemini.obs.v1"
)

// ProbeInputs are fixture/injected local probe outputs (no real process).
type ProbeInputs struct {
	// ExecutablePresent is whether a gemini binary would be found.
	ExecutablePresent bool
	Version           string // e.g. "0.1.0"; empty if unknown
	// AuthState is known|unknown|missing
	AuthState string
	// AccountProfile is redacted profile label (never email/token).
	AccountProfile string
	// RawModels are fixture catalog lines "id|alias1,alias2|context|efforts"
	RawModels []string
	// Force outcomes
	Timeout            bool
	Malformed          bool
	UnsupportedVersion bool
	Stale              bool
	// Now injects capture time
	Now func() time.Time
}

// ModelRecord is one normalized catalog entry.
type ModelRecord struct {
	CanonicalID   string    `json:"canonical_id"`
	Aliases       []string  `json:"aliases,omitempty"`
	ContextTokens int       `json:"context_tokens,omitempty"`
	Efforts       []string  `json:"efforts,omitempty"`
	Permissions   []string  `json:"permissions,omitempty"`
	Source        string    `json:"source"`
	CapturedAt    time.Time `json:"captured_at"`
	Confidence    string    `json:"confidence"`
	Freshness     string    `json:"freshness"`
}

// Snapshot is the Gemini CLI observation aggregate (no credentials).
type Snapshot struct {
	Schema             string            `json:"schema"`
	AdapterID          string            `json:"adapter_id"`
	Installed          *bool             `json:"installed,omitempty"` // nil = unknown
	Version            string            `json:"version,omitempty"`
	Auth               string            `json:"auth"` // known|unknown|missing
	AccountProfile     string            `json:"account_profile,omitempty"`
	Models             []ModelRecord     `json:"models,omitempty"`
	Diagnostics        []string          `json:"diagnostics,omitempty"`
	SelectedSources    map[string]string `json:"selected_sources,omitempty"`
	LastKnownPreserved bool              `json:"last_known_preserved"`
	// Cannot launch or route
	LaunchAttempted bool `json:"launch_attempted"`
	RouteChosen     bool `json:"route_chosen"`
	// Descriptor for registry
	Descriptor providerdesc.Descriptor `json:"-"`
}

// Observer runs bounded Gemini CLI observation plans.
type Observer struct {
	// Last is preserved across failed probes.
	Last *Snapshot
}

// Descriptor returns the gemini adapter registration document.
func Descriptor() providerdesc.Descriptor {
	return providerdesc.Descriptor{
		Schema: providerdesc.SchemaDescriptor, AdapterID: AdapterID,
		Version: providerdesc.DescriptorVersion, DisplayName: "Gemini CLI CLI",
		Identity: providerdesc.Identity{InstallID: "gemini-cli", Present: false},
		Operations: []providerdesc.Operation{
			providerdesc.OpDiscover, providerdesc.OpAuthStatus, providerdesc.OpCatalog, providerdesc.OpDiagnose,
		},
		Unsupported: []providerdesc.Operation{providerdesc.OpInvoke, providerdesc.OpQuota},
		ProbePlans: []providerdesc.ProbePlan{
			{Name: "lookpath_gemini", Timeout: 2 * time.Second},
			{Name: "gemini_version", Timeout: 3 * time.Second},
			{Name: "auth_status_local", Timeout: 3 * time.Second, Optional: true},
			{Name: "model_catalog_local", Timeout: 5 * time.Second},
		},
		Notes: "observation only; no invoke/route/credentials",
	}
}

// Observe runs discovery/auth/catalog through ordered sources using inputs.
func (o *Observer) Observe(in ProbeInputs) (Snapshot, error) {
	now := time.Now().UTC()
	if in.Now != nil {
		now = in.Now().UTC()
	}
	snap := Snapshot{
		Schema: SchemaSnapshot, AdapterID: AdapterID,
		Auth: "unknown", SelectedSources: map[string]string{},
		LaunchAttempted: false, RouteChosen: false,
		Descriptor: Descriptor(),
	}

	// Build plan runner from inputs (fixture)
	runner := func(st obsplan.SourceStep) (obsplan.StepOutcome, map[string]string, string, string) {
		if in.Timeout {
			return obsplan.OutcomeTimeout, nil, "E_TIMEOUT", "probe timeout"
		}
		if in.UnsupportedVersion && st.Name == "cli_primary" {
			return obsplan.OutcomeUnsupported, nil, "E_VERSION", "unsupported gemini version"
		}
		if in.Malformed && (st.Capability == providerdesc.OpCatalog || st.Name == "cli_primary") {
			return obsplan.OutcomeMalformed, nil, "E_MALFORMED", "malformed catalog"
		}
		switch st.Capability {
		case providerdesc.OpDiscover:
			if !in.ExecutablePresent {
				// not installed is a fact, not timeout
				return obsplan.OutcomeOK, map[string]string{"present": "false"}, "", "not installed"
			}
			fact := map[string]string{"present": "true"}
			if in.Version != "" {
				fact["version"] = in.Version
			}
			return obsplan.OutcomeOK, fact, "", "discovered"
		case providerdesc.OpAuthStatus:
			auth := in.AuthState
			if auth == "" {
				auth = "unknown"
			}
			if auth != "known" && auth != "unknown" && auth != "missing" {
				return obsplan.OutcomeMalformed, nil, "E_AUTH", "bad auth state"
			}
			f := map[string]string{"auth": auth}
			if in.AccountProfile != "" && !looksSecret(in.AccountProfile) {
				f["account_profile"] = in.AccountProfile
			}
			return obsplan.OutcomeOK, f, "", "auth"
		case providerdesc.OpCatalog:
			if !in.ExecutablePresent {
				return obsplan.OutcomeSkipped, nil, "no_install", "skip catalog"
			}
			if in.Stale {
				return obsplan.OutcomeStale, nil, "E_STALE", "catalog stale"
			}
			return obsplan.OutcomeOK, map[string]string{"catalog_lines": fmt.Sprintf("%d", len(in.RawModels))}, "", "catalog"
		default:
			return obsplan.OutcomeUnsupported, nil, "E_UNSUP", "unsupported"
		}
	}

	ex := &obsplan.Executor{Now: func() time.Time { return now }, ScrubEnv: true, Runner: runner}

	// Discover
	dPlan := obsplan.DefaultPlan(AdapterID, providerdesc.OpDiscover)
	dSnap, err := ex.Run(dPlan)
	if err != nil {
		return Snapshot{}, err
	}
	snap.SelectedSources["discover"] = dSnap.SelectedSource
	snap.Diagnostics = append(snap.Diagnostics, dSnap.Diagnostics...)
	if dSnap.Facts["present"] == "true" {
		v := true
		snap.Installed = &v
		snap.Version = dSnap.Facts["version"]
	} else if dSnap.Facts["present"] == "false" {
		v := false
		snap.Installed = &v
	}
	// Preserve last known on hard failure
	if len(dSnap.Facts) == 0 && o.Last != nil {
		snap.Installed = o.Last.Installed
		snap.Version = o.Last.Version
		snap.LastKnownPreserved = true
		snap.Diagnostics = append(snap.Diagnostics, "preserved_last_known_install")
	}

	// Auth
	aPlan := obsplan.DefaultPlan(AdapterID, providerdesc.OpAuthStatus)
	aSnap, err := ex.Run(aPlan)
	if err != nil {
		return Snapshot{}, err
	}
	snap.SelectedSources["auth"] = aSnap.SelectedSource
	snap.Diagnostics = append(snap.Diagnostics, aSnap.Diagnostics...)
	if a := aSnap.Facts["auth"]; a != "" {
		snap.Auth = a
	} else if o.Last != nil {
		snap.Auth = o.Last.Auth
		snap.LastKnownPreserved = true
	}
	snap.AccountProfile = aSnap.Facts["account_profile"]
	if looksSecret(snap.AccountProfile) {
		snap.AccountProfile = ""
	}

	// Catalog
	cPlan := obsplan.DefaultPlan(AdapterID, providerdesc.OpCatalog)
	cSnap, err := ex.Run(cPlan)
	if err != nil {
		return Snapshot{}, err
	}
	snap.SelectedSources["catalog"] = cSnap.SelectedSource
	snap.Diagnostics = append(snap.Diagnostics, cSnap.Diagnostics...)
	if cSnap.Facts["catalog_lines"] != "" && !in.Malformed && !in.Timeout {
		models, mdiag := parseModels(in.RawModels, now)
		snap.Models = models
		snap.Diagnostics = append(snap.Diagnostics, mdiag...)
	} else if o.Last != nil && len(o.Last.Models) > 0 {
		snap.Models = o.Last.Models
		snap.LastKnownPreserved = true
		snap.Diagnostics = append(snap.Diagnostics, "preserved_last_known_catalog")
	}

	// Never silently invent default model
	o.Last = &snap
	return snap, nil
}

func parseModels(raw []string, now time.Time) ([]ModelRecord, []string) {
	var out []ModelRecord
	var diags []string
	seen := map[string]bool{}
	for _, line := range raw {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) < 1 || parts[0] == "" {
			diags = append(diags, "malformed_model_line")
			continue
		}
		id := normalizeID(parts[0])
		if id == "" || id == "unknown" {
			diags = append(diags, "unknown_model_not_defaulted:"+parts[0])
			continue
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		rec := ModelRecord{
			CanonicalID: id, Source: "fixture_catalog", CapturedAt: now,
			Confidence: "high", Freshness: "fresh",
			Permissions: []string{"default"},
		}
		if len(parts) > 1 && parts[1] != "" {
			for _, a := range strings.Split(parts[1], ",") {
				a = strings.TrimSpace(a)
				if a == "" {
					continue
				}
				// reversible alias: store original form
				rec.Aliases = append(rec.Aliases, a)
			}
			sort.Strings(rec.Aliases)
		}
		if len(parts) > 2 {
			fmt.Sscanf(parts[2], "%d", &rec.ContextTokens)
		}
		if len(parts) > 3 && parts[3] != "" {
			rec.Efforts = strings.Split(parts[3], ",")
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CanonicalID < out[j].CanonicalID })
	return out, diags
}

// NormalizeAlias maps an alias to a canonical ID if known; never invents.
func NormalizeAlias(models []ModelRecord, alias string) (canonical string, ok bool, explanation string) {
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return "", false, "empty_alias"
	}
	for _, m := range models {
		if m.CanonicalID == alias || m.CanonicalID == normalizeID(alias) {
			return m.CanonicalID, true, "exact_canonical"
		}
		for _, a := range m.Aliases {
			if a == alias {
				return m.CanonicalID, true, "alias_of:" + m.CanonicalID
			}
		}
	}
	return "", false, "unknown_model_not_eligible"
}

func normalizeID(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	s = strings.ReplaceAll(s, " ", "-")
	if s == "" || s == "default" || s == "auto" {
		return ""
	}
	return s
}

func looksSecret(s string) bool {
	ls := strings.ToLower(s)
	return strings.HasPrefix(ls, "ghp_") || strings.HasPrefix(ls, "sk-") || strings.Contains(ls, "token=")
}

// AsAdapter wraps Observer as providerdesc.Adapter for registry tests.
type AsAdapter struct {
	Obs *Observer
	In  ProbeInputs
}

func (a *AsAdapter) Descriptor() providerdesc.Descriptor {
	d := Descriptor()
	d.Identity.Present = a.In.ExecutablePresent
	if a.In.ExecutablePresent {
		d.Models = []providerdesc.ModelEntry{{ID: "gemini-2.5-flash", DisplayName: "gemini-2.5-flash"}}
	} else {
		d.Models = nil
		// still claim catalog but empty
	}
	return d
}

func (a *AsAdapter) Observe(op providerdesc.Operation, _ map[string]string) (providerdesc.Observation, error) {
	if a.Obs == nil {
		a.Obs = &Observer{}
	}
	snap, err := a.Obs.Observe(a.In)
	if err != nil {
		return providerdesc.Observation{}, err
	}
	obs := providerdesc.Observation{
		Schema: providerdesc.SchemaObservation, AdapterID: AdapterID, Operation: op,
		Provenance: providerdesc.Provenance{Source: "geminiobs_fixture", ObservedAt: time.Now().UTC(), Freshness: "fresh"},
		Confidence: providerdesc.ConfidenceHigh, OK: true,
		Payload: map[string]string{},
	}
	switch op {
	case providerdesc.OpDiscover:
		if snap.Installed != nil {
			obs.Payload["present"] = fmt.Sprintf("%v", *snap.Installed)
		} else {
			obs.Payload["present"] = "unknown"
			obs.Confidence = providerdesc.ConfidenceLow
		}
		obs.Payload["version"] = snap.Version
	case providerdesc.OpAuthStatus:
		obs.Payload["auth"] = snap.Auth
	case providerdesc.OpCatalog:
		obs.Payload["count"] = fmt.Sprintf("%d", len(snap.Models))
	default:
		obs.OK = false
		obs.Diagnostic = &providerdesc.Diagnostic{Schema: providerdesc.SchemaDiagnostic, Class: providerdesc.DiagUnsupported, Message: "unsupported"}
		return obs, providerdesc.ErrUnsupported
	}
	return obs, nil
}
