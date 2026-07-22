package nosidecar

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Disposition for a known repo-local path pattern.
type Disposition string

const (
	// DispGlobal map to $LOOPCODER_HOME / project store.
	DispGlobal Disposition = "global_path"
	// DispReadOnlyExport open only via V090-069 exporter.
	DispReadOnlyExport Disposition = "read_only_export"
	// DispPolicyFile user-authored policy; read-only input.
	DispPolicyFile Disposition = "policy_file_readonly"
	// DispRemoved production writer deleted; never write.
	DispRemoved Disposition = "removed_writer"
	// DispRetainDiscover retained for migration discovery only.
	DispRetainDiscover Disposition = "retain_discovery"
)

// PathRule maps a relative repo-local pattern to disposition.
type PathRule struct {
	Pattern     string // e.g. ".loopcoder", ".loopcoder/runs"
	Disposition Disposition
	Notes       string
}

// DefaultManifest is the removal/retention inventory.
func DefaultManifest() []PathRule {
	return []PathRule{
		{Pattern: ".loopcoder", Disposition: DispRemoved, Notes: "no production writes; never auto-create"},
		{Pattern: ".loopcoder/runs", Disposition: DispRemoved, Notes: "use global project store"},
		{Pattern: ".loopcoder/relay", Disposition: DispRemoved, Notes: "relay removed from repo"},
		{Pattern: ".loopcoder/recovery", Disposition: DispRemoved, Notes: "recovery is global/project only"},
		{Pattern: ".loopcoder/logs", Disposition: DispRemoved, Notes: "logs under global payload root"},
		{Pattern: ".loopcoder/tmp", Disposition: DispRemoved, Notes: "temp under global home"},
		{Pattern: ".loopcoder/state", Disposition: DispReadOnlyExport, Notes: "V090-069 exporter only"},
		{Pattern: ".loopcoder/db", Disposition: DispReadOnlyExport, Notes: "legacy db read-only export"},
		{Pattern: ".delivery.yml", Disposition: DispPolicyFile, Notes: "user-authored policy read-only"},
		{Pattern: ".loopcoder.yml", Disposition: DispPolicyFile, Notes: "user-authored policy read-only"},
	}
}

// WriteAttempt is a proposed filesystem write from runtime.
type WriteAttempt struct {
	// RepoRoot is the customer repository root (absolute or logical).
	RepoRoot string
	// TargetPath is the path about to be written.
	TargetPath string
	// IsGitMetadata true for .git/** intentional Git ops.
	IsGitMetadata bool
	// IsWorkerCodeChange true for intentional source edits.
	IsWorkerCodeChange bool
	// ReadOnlyExport true when the open is for V090-069 only.
	ReadOnlyExport bool
	// ProjectRegistered true when project has global identity.
	ProjectRegistered bool
}

// Decision for a write attempt.
type Decision struct {
	Allowed bool
	Reasons []string
	// Guidance for fail/repair when denied.
	Guidance string
}

// ForbiddenRepoLocalReports whether path is under a removed/export-only pattern.
func ForbiddenRepoLocal(repoRoot, target string) (bool, PathRule) {
	rel := relToRepo(repoRoot, target)
	if rel == "" {
		return false, PathRule{}
	}
	rel = filepath.ToSlash(rel)
	for _, rule := range DefaultManifest() {
		pat := strings.TrimSuffix(rule.Pattern, "/")
		if rel == pat || strings.HasPrefix(rel, pat+"/") {
			if rule.Disposition == DispRemoved || rule.Disposition == DispReadOnlyExport || rule.Disposition == DispRetainDiscover {
				return true, rule
			}
			// policy files: not forbidden to exist, but production must not write them as runtime sidecars
			if rule.Disposition == DispPolicyFile {
				// allow if not creating as runtime — still block runtime writer claiming them as state
				return false, rule
			}
		}
	}
	// any path under .loopcoder is forbidden even if not listed
	if rel == ".loopcoder" || strings.HasPrefix(rel, ".loopcoder/") {
		return true, PathRule{Pattern: ".loopcoder", Disposition: DispRemoved, Notes: "catchall"}
	}
	return false, PathRule{}
}

// EvaluateWrite enforces no production write into customer repo sidecars.
func EvaluateWrite(a WriteAttempt) Decision {
	// Git metadata and intentional code changes always allowed.
	if a.IsGitMetadata || a.IsWorkerCodeChange {
		return Decision{Allowed: true, Reasons: []string{"intended git/code change"}}
	}

	// Unregistered project: never fall back to <repo>/.loopcoder.
	if !a.ProjectRegistered {
		if forbidden, rule := ForbiddenRepoLocal(a.RepoRoot, a.TargetPath); forbidden {
			return Decision{
				Allowed:  false,
				Reasons:  []string{"unregistered project must not write " + rule.Pattern},
				Guidance: "register project globally or fail; never choose <repo>/.loopcoder",
			}
		}
		// any write that would create .loopcoder
		if isLoopcoderPath(a.RepoRoot, a.TargetPath) {
			return Decision{
				Allowed:  false,
				Reasons:  []string{"unregistered/invalid identity never chooses <repo>/.loopcoder"},
				Guidance: "auto-register globally or return typed fail/repair guidance",
			}
		}
	}

	if forbidden, rule := ForbiddenRepoLocal(a.RepoRoot, a.TargetPath); forbidden {
		if rule.Disposition == DispReadOnlyExport && a.ReadOnlyExport {
			return Decision{Allowed: true, Reasons: []string{"read-only exporter open of " + rule.Pattern}}
		}
		return Decision{
			Allowed:  false,
			Reasons:  []string{fmt.Sprintf("production write to %s disposition=%s", rule.Pattern, rule.Disposition)},
			Guidance: "use global/project store; legacy path is export-only or removed",
		}
	}

	// Runtime write outside repo is fine (not checked here).
	return Decision{Allowed: true, Reasons: []string{"path not a forbidden repo-local sidecar"}}
}

// RegistrationFallback is denied — never fall back to repo-local on error.
func RegistrationFallback(err error) Decision {
	_ = err
	return Decision{
		Allowed:  false,
		Reasons:  []string{"registration error must not fall back to <repo>/.loopcoder"},
		Guidance: "typed fail or global auto-register/repair; no production fallback",
	}
}

// ScanCanaryPaths returns paths that must remain free of new production writes
// after direct/provider/workflow/cancel/resume/failure scenarios.
func ScanCanaryPaths() []string {
	var out []string
	for _, r := range DefaultManifest() {
		if r.Disposition == DispRemoved || r.Disposition == DispReadOnlyExport {
			out = append(out, r.Pattern)
		}
	}
	return out
}

// ManifestReport is a redacted inventory for acceptance criterion 5.
type ManifestReport struct {
	Rules []PathRule `json:"rules"`
}

// ReportManifest returns the full removal/retention inventory.
func ReportManifest() ManifestReport {
	return ManifestReport{Rules: DefaultManifest()}
}

func relToRepo(repoRoot, target string) string {
	repoRoot = filepath.Clean(repoRoot)
	target = filepath.Clean(target)
	if repoRoot == "" || target == "" {
		return ""
	}
	// logical: if target is absolute under repo
	if strings.HasPrefix(target, repoRoot+string(filepath.Separator)) || target == repoRoot {
		rel, err := filepath.Rel(repoRoot, target)
		if err != nil {
			return ""
		}
		return rel
	}
	// already relative
	if !filepath.IsAbs(target) {
		return target
	}
	return ""
}

func isLoopcoderPath(repoRoot, target string) bool {
	rel := relToRepo(repoRoot, target)
	if rel == "" {
		return false
	}
	rel = filepath.ToSlash(rel)
	return rel == ".loopcoder" || strings.HasPrefix(rel, ".loopcoder/")
}
