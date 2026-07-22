package cliprune

import (
	"sort"
	"strings"
)

// Visibility of a command in help/completions.
type Visibility string

const (
	// VisSupported shown as primary v0.9 command.
	VisSupported Visibility = "supported_v09"
	// VisCompat explicit read-only migration/compat (named).
	VisCompat Visibility = "explicit_compat"
	// VisHidden removed from help/completions; invoke fails closed.
	VisHidden Visibility = "hidden_removed"
)

// Command is one CLI surface after prune.
type Command struct {
	Name        string     `json:"name"`
	Visibility  Visibility `json:"visibility"`
	Help        string     `json:"help,omitempty"`
	Replacement string     `json:"replacement,omitempty"`
	// DeletionIssue must be closed with replacement evidence before VisHidden.
	DeletionIssue int `json:"deletion_issue,omitempty"`
	// ReplacementEvidenceOK gates hiding.
	ReplacementEvidenceOK bool `json:"replacement_evidence_ok,omitempty"`
}

// DefaultCatalog is the pruned command set for v0.9.
func DefaultCatalog() []Command {
	return []Command{
		// Supported
		{Name: "doctor", Visibility: VisSupported, Help: "environment and capability checks"},
		{Name: "status", Visibility: VisSupported, Help: "project/machine status from v0.9 stores"},
		{Name: "version", Visibility: VisSupported, Help: "print version"},
		{Name: "help", Visibility: VisSupported, Help: "show help"},
		// Explicit compat
		{Name: "export-v08", Visibility: VisCompat, Help: "read-only v0.8 export (V090-069)"},
		{Name: "import-v09", Visibility: VisCompat, Help: "import neutral export into v0.9 (V090-070)"},
		// Hidden removed (deletion issues closed in this campaign)
		{Name: "compile", Visibility: VisHidden, Replacement: "ordinary development + GitHub", DeletionIssue: 1190, ReplacementEvidenceOK: true},
		{Name: "dispatch", Visibility: VisHidden, Replacement: "ordinary developer tools", DeletionIssue: 1190, ReplacementEvidenceOK: true},
		{Name: "tick", Visibility: VisHidden, Replacement: "removed autonomous tick", DeletionIssue: 1190, ReplacementEvidenceOK: true},
		{Name: "trigger", Visibility: VisHidden, Replacement: "removed", DeletionIssue: 1190, ReplacementEvidenceOK: true},
		{Name: "promote", Visibility: VisHidden, Replacement: "human/release gate only", DeletionIssue: 1190, ReplacementEvidenceOK: true},
		{Name: "federate", Visibility: VisHidden, Replacement: "not supported in v0.9", DeletionIssue: 1191, ReplacementEvidenceOK: true},
		{Name: "lease-acquire", Visibility: VisHidden, Replacement: "not supported", DeletionIssue: 1191, ReplacementEvidenceOK: true},
	}
}

// HelpLines returns root help entries (supported + explicit compat only).
func HelpLines(catalog []Command) []string {
	var lines []string
	for _, c := range catalog {
		if c.Visibility == VisSupported || c.Visibility == VisCompat {
			tag := ""
			if c.Visibility == VisCompat {
				tag = " [compat]"
			}
			lines = append(lines, c.Name+tag+": "+c.Help)
		}
	}
	sort.Strings(lines)
	return lines
}

// Completions returns names offered for shell completion.
func Completions(catalog []Command) []string {
	var out []string
	for _, c := range catalog {
		if c.Visibility != VisHidden {
			out = append(out, c.Name)
		}
	}
	sort.Strings(out)
	return out
}

// Invoke evaluates whether a command may run and what message to show if not.
type InvokeResult struct {
	Allowed bool
	Message string
}

// Invoke looks up command visibility.
func Invoke(catalog []Command, name string) InvokeResult {
	n := strings.ToLower(strings.TrimSpace(name))
	for _, c := range catalog {
		if strings.ToLower(c.Name) != n {
			continue
		}
		switch c.Visibility {
		case VisSupported, VisCompat:
			return InvokeResult{Allowed: true}
		case VisHidden:
			if !c.ReplacementEvidenceOK {
				return InvokeResult{Allowed: true, Message: "deletion evidence incomplete; still wired"}
			}
			msg := "command removed"
			if c.Replacement != "" {
				msg = "command removed: " + c.Replacement
			}
			return InvokeResult{Allowed: false, Message: msg}
		}
	}
	return InvokeResult{Allowed: false, Message: "unknown command"}
}

// SpecRecord is non-authoritative historical architecture note.
type SpecRecord struct {
	Path           string `json:"path"`
	Authoritative  bool   `json:"authoritative"`
	CompilerActive bool   `json:"compiler_active"`
	Notes          string `json:"notes"`
}

// HistoricalSpecs must not be compiler-active or authoritative.
func HistoricalSpecs() []SpecRecord {
	return []SpecRecord{
		{Path: "docs/archive/v0.8-architecture.md", Authoritative: false, CompilerActive: false, Notes: "historical only; prefer Git history"},
		{Path: "docs/roadmaps/v0.8/", Authoritative: false, CompilerActive: false, Notes: "inert roadmap markers"},
	}
}

// AssertNoCompilerActive fails if any historical spec claims compiler-active.
func AssertNoCompilerActive(specs []SpecRecord) error {
	for _, s := range specs {
		if s.CompilerActive || s.Authoritative {
			return errActive(s.Path)
		}
	}
	return nil
}

type pathErr string

func (e pathErr) Error() string {
	return "compiler-active or authoritative historical spec: " + string(e)
}

func errActive(p string) error { return pathErr(p) }
