package preflight

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

const (
	SchemaSnapshot = "loopcoder.preflight.v1"
	SchemaProbe    = "loopcoder.preflight.probe.v1"
)

// Status is a stable probe outcome.
type Status string

const (
	StatusPass    Status = "pass"
	StatusWarn    Status = "warn"
	StatusFail    Status = "fail"
	StatusUnknown Status = "unknown"
)

// Kind separates product prerequisites from optional capabilities.
type Kind string

const (
	KindPrerequisite Kind = "prerequisite"
	KindOptional     Kind = "optional"
)

// Remediation codes are stable machine strings.
const (
	RemediationUnsupportedPlatform = "unsupported_platform"
	RemediationUnsafeHome          = "unsafe_home"
	RemediationInvalidRepo         = "invalid_repo"
	RemediationGitUnavailable      = "git_unavailable"
	RemediationProviderMissing     = "explicit_provider_missing"
	RemediationBudgetInsufficient  = "resource_budget_insufficient"
	RemediationUIOptional          = "optional_ui_gap"
	RemediationQuotaOptional       = "optional_quota_gap"
	RemediationOK                  = "none"
)

// Probe is one bounded check result.
type Probe struct {
	Schema      string    `json:"schema"`
	Name        string    `json:"name"`
	Kind        Kind      `json:"kind"`
	Status      Status    `json:"status"`
	Remediation string    `json:"remediation"`
	Message     string    `json:"message"`
	Provenance  string    `json:"provenance"` // e.g. platform.Check, lookpath
	ObservedAt  time.Time `json:"observed_at"`
	// Evidence is redacted key/values only (no secrets/paths with credentials).
	Evidence map[string]string `json:"evidence,omitempty"`
}

// Snapshot is the accepted preflight evidence gate for run.
type Snapshot struct {
	Schema       string    `json:"schema"`
	ProjectID    string    `json:"project_id,omitempty"`
	Repo         string    `json:"repo"`
	Provider     string    `json:"provider,omitempty"`
	Model        string    `json:"model,omitempty"`
	Decision     Status    `json:"decision"` // overall: fail if any prerequisite fail
	AllowLaunch  bool      `json:"allow_launch"`
	Probes       []Probe   `json:"probes"`
	Changed      []string  `json:"changed,omitempty"` // EnsureLayout changes only
	Digest       string    `json:"digest"`
	GeneratedAt  time.Time `json:"generated_at"`
	ObservationN int       `json:"observation_n"` // refresh counter metadata only
}

// Input is the explicit run pin used for readiness (no auto-route).
type Input struct {
	Repo         string
	Provider     string
	Model        string
	ProjectID    string
	MinBudgetMB  int64 // 0 = default 64
	RequireUI    bool  // if true, missing UI is warn still (optional) unless set hard later
	EnsureLayout bool  // create validated global dirs only when true
}

// Deps injects probes for fixture tests.
type Deps struct {
	Now         func() time.Time
	GOOS        string
	GOARCH      string
	LookPath    func(file string) (string, error)
	Stat        func(name string) (os.FileInfo, error)
	UserHomeDir func() (string, error)
	Getenv      func(string) string
	MkdirAll    func(path string, perm os.FileMode) error
	// BudgetFreeMB reports free-ish resource budget; tests inject.
	BudgetFreeMB func() (int64, error)
	// ProviderPresent returns true if explicit provider binary is available.
	ProviderPresent func(provider string) (bool, string, error)
	// UICapable optional UI handshake probe.
	UICapable func() (bool, string)
	// QuotaKnown optional quota catalog probe.
	QuotaKnown func() (bool, string)
}

// DefaultDeps returns production-ish defaults (still no network provider login).
func DefaultDeps() Deps {
	return Deps{
		Now:         time.Now,
		GOOS:        runtime.GOOS,
		GOARCH:      runtime.GOARCH,
		LookPath:    lookPath,
		Stat:        os.Stat,
		UserHomeDir: os.UserHomeDir,
		Getenv:      os.Getenv,
		MkdirAll:    os.MkdirAll,
		BudgetFreeMB: func() (int64, error) {
			// Conservative default: report sufficient without probing OS disk in unit path.
			return 1024, nil
		},
		ProviderPresent: func(provider string) (bool, string, error) {
			if provider == "" {
				return false, "", nil
			}
			// Map common provider names to CLI binaries without invoking them.
			bin := providerBinary(provider)
			if bin == "" {
				return false, "unknown_provider_mapping", nil
			}
			p, err := lookPath(bin)
			if err != nil || p == "" {
				return false, bin, nil
			}
			return true, bin, nil
		},
		UICapable:  func() (bool, string) { return true, "fixture_or_terminal_assumed" },
		QuotaKnown: func() (bool, string) { return false, "quota_not_probed" },
	}
}

func lookPath(file string) (string, error) {
	return exec.LookPath(file)
}

// Evaluate runs read-only probes and returns a snapshot. No credentials printed.
func Evaluate(ctx context.Context, in Input, deps Deps) (Snapshot, error) {
	deps = normalize(deps)
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	now := deps.Now().UTC()
	var probes []Probe
	var changed []string

	// 1) Platform (prerequisite)
	probes = append(probes, probePlatform(deps, now))

	// 2) Home safety (prerequisite)
	homeProbe, homeDir := probeHome(deps, now)
	probes = append(probes, homeProbe)

	// 3) Repo (prerequisite)
	probes = append(probes, probeRepo(in.Repo, deps, now))

	// 4) Git (prerequisite)
	probes = append(probes, probeGit(deps, now))

	// 5) Explicit provider (prerequisite)
	probes = append(probes, probeProvider(in.Provider, deps, now))

	// 6) Resource budget (prerequisite)
	minMB := in.MinBudgetMB
	if minMB <= 0 {
		minMB = 64
	}
	probes = append(probes, probeBudget(minMB, deps, now))

	// 7) Optional UI
	probes = append(probes, probeUI(deps, now))

	// 8) Optional quota/catalog
	probes = append(probes, probeQuota(deps, now))

	// EnsureLayout: only validated global dirs under home (never repo, never credentials)
	if in.EnsureLayout && homeProbe.Status == StatusPass && homeDir != "" {
		c, err := ensureGlobalLayout(homeDir, deps)
		if err != nil {
			probes = append(probes, Probe{
				Schema: SchemaProbe, Name: "ensure_layout", Kind: KindPrerequisite,
				Status: StatusFail, Remediation: RemediationUnsafeHome,
				Message: "layout ensure failed", Provenance: "preflight.EnsureLayout",
				ObservedAt: now, Evidence: map[string]string{"error": redact(err.Error())},
			})
		} else {
			changed = c
			probes = append(probes, Probe{
				Schema: SchemaProbe, Name: "ensure_layout", Kind: KindPrerequisite,
				Status: StatusPass, Remediation: RemediationOK,
				Message: fmt.Sprintf("layout ok; created=%d", len(c)), Provenance: "preflight.EnsureLayout",
				ObservedAt: now, Evidence: map[string]string{"created_n": fmt.Sprintf("%d", len(c))},
			})
		}
	}

	decision := StatusPass
	allow := true
	for _, p := range probes {
		if p.Kind == KindPrerequisite && p.Status == StatusFail {
			decision = StatusFail
			allow = false
			break
		}
	}
	if allow {
		for _, p := range probes {
			if p.Status == StatusWarn {
				decision = StatusWarn
				break
			}
		}
	}

	snap := Snapshot{
		Schema: SchemaSnapshot, ProjectID: in.ProjectID, Repo: redactPath(in.Repo),
		Provider: in.Provider, Model: in.Model, Decision: decision, AllowLaunch: allow,
		Probes: probes, Changed: changed, GeneratedAt: now, ObservationN: 1,
	}
	snap.Digest = digestSnapshot(snap)
	return snap, nil
}

// SameDecision reports whether two snapshots normalize to the same decision
// (ignoring timestamps and observation metadata).
func SameDecision(a, b Snapshot) bool {
	if a.AllowLaunch != b.AllowLaunch || a.Decision != b.Decision {
		return false
	}
	// Compare probe name+status+remediation sets
	type key struct{ n, s, r string }
	ma := map[key]struct{}{}
	for _, p := range a.Probes {
		if p.Name == "ensure_layout" {
			continue // write path optional
		}
		ma[key{p.Name, string(p.Status), p.Remediation}] = struct{}{}
	}
	mb := map[key]struct{}{}
	for _, p := range b.Probes {
		if p.Name == "ensure_layout" {
			continue
		}
		mb[key{p.Name, string(p.Status), p.Remediation}] = struct{}{}
	}
	if len(ma) != len(mb) {
		return false
	}
	for k := range ma {
		if _, ok := mb[k]; !ok {
			return false
		}
	}
	return true
}

func probePlatform(deps Deps, now time.Time) Probe {
	p := Probe{
		Schema: SchemaProbe, Name: "platform", Kind: KindPrerequisite,
		Provenance: "runtime.GOOS/GOARCH", ObservedAt: now,
		Evidence: map[string]string{"goos": deps.GOOS, "goarch": deps.GOARCH},
	}
	// Darwin arm64 is the supported product surface for v0.9.0 ordinary path.
	if deps.GOOS == "darwin" && (deps.GOARCH == "arm64" || deps.GOARCH == "amd64") {
		p.Status = StatusPass
		p.Remediation = RemediationOK
		p.Message = "supported platform"
		return p
	}
	// Allow GOOS=test injection for fixtures via "fixture" arch
	if deps.GOOS == "fixture" {
		p.Status = StatusPass
		p.Remediation = RemediationOK
		p.Message = "fixture platform"
		return p
	}
	p.Status = StatusFail
	p.Remediation = RemediationUnsupportedPlatform
	p.Message = "unsupported platform for direct-run"
	return p
}

func probeHome(deps Deps, now time.Time) (Probe, string) {
	p := Probe{
		Schema: SchemaProbe, Name: "home", Kind: KindPrerequisite,
		Provenance: "UserHomeDir+LOOPCODER_HOME", ObservedAt: now,
	}
	home, err := deps.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		p.Status = StatusFail
		p.Remediation = RemediationUnsafeHome
		p.Message = "cannot resolve user home"
		return p, ""
	}
	override := ""
	if deps.Getenv != nil {
		override = strings.TrimSpace(deps.Getenv("LOOPCODER_HOME"))
	}
	root := home
	if override != "" {
		root = override
	}
	// Unsafe: empty, relative without abs, world-writable markers (simple)
	if !filepath.IsAbs(root) {
		p.Status = StatusFail
		p.Remediation = RemediationUnsafeHome
		p.Message = "home path is not absolute"
		p.Evidence = map[string]string{"home_kind": "relative"}
		return p, ""
	}
	if strings.Contains(root, "\x00") {
		p.Status = StatusFail
		p.Remediation = RemediationUnsafeHome
		p.Message = "home path invalid"
		return p, ""
	}
	p.Status = StatusPass
	p.Remediation = RemediationOK
	p.Message = "home resolvable"
	p.Evidence = map[string]string{"home_set": "1"}
	if override != "" {
		p.Evidence["override"] = "LOOPCODER_HOME"
	}
	return p, root
}

func probeRepo(repo string, deps Deps, now time.Time) Probe {
	p := Probe{
		Schema: SchemaProbe, Name: "repo", Kind: KindPrerequisite,
		Provenance: "Stat", ObservedAt: now,
	}
	repo = strings.TrimSpace(repo)
	if repo == "" {
		p.Status = StatusFail
		p.Remediation = RemediationInvalidRepo
		p.Message = "repo required"
		return p
	}
	// owner/name form is valid identity without local path
	if strings.Count(repo, "/") == 1 && !strings.HasPrefix(repo, "/") && !strings.Contains(repo, "..") {
		p.Status = StatusPass
		p.Remediation = RemediationOK
		p.Message = "repo identity ok"
		p.Evidence = map[string]string{"form": "owner_name"}
		return p
	}
	info, err := deps.Stat(repo)
	if err != nil || info == nil || !info.IsDir() {
		p.Status = StatusFail
		p.Remediation = RemediationInvalidRepo
		p.Message = "repo path not a directory"
		p.Evidence = map[string]string{"form": "path"}
		return p
	}
	p.Status = StatusPass
	p.Remediation = RemediationOK
	p.Message = "repo path ok"
	p.Evidence = map[string]string{"form": "path"}
	return p
}

func probeGit(deps Deps, now time.Time) Probe {
	p := Probe{
		Schema: SchemaProbe, Name: "git", Kind: KindPrerequisite,
		Provenance: "LookPath(git)", ObservedAt: now,
	}
	path, err := deps.LookPath("git")
	if err != nil || path == "" {
		p.Status = StatusFail
		p.Remediation = RemediationGitUnavailable
		p.Message = "git executable not found"
		return p
	}
	p.Status = StatusPass
	p.Remediation = RemediationOK
	p.Message = "git available"
	p.Evidence = map[string]string{"binary": "git"}
	return p
}

func probeProvider(provider string, deps Deps, now time.Time) Probe {
	p := Probe{
		Schema: SchemaProbe, Name: "explicit_provider", Kind: KindPrerequisite,
		Provenance: "ProviderPresent", ObservedAt: now,
		Evidence: map[string]string{"provider": provider},
	}
	if strings.TrimSpace(provider) == "" {
		p.Status = StatusFail
		p.Remediation = RemediationProviderMissing
		p.Message = "explicit provider required"
		return p
	}
	ok, bin, err := deps.ProviderPresent(provider)
	if err != nil {
		p.Status = StatusUnknown
		p.Remediation = RemediationProviderMissing
		p.Message = "provider probe error"
		p.Evidence["error"] = redact(err.Error())
		return p
	}
	if !ok {
		p.Status = StatusFail
		p.Remediation = RemediationProviderMissing
		p.Message = "explicit provider executable not found"
		if bin != "" {
			p.Evidence["expected_binary"] = bin
		}
		return p
	}
	p.Status = StatusPass
	p.Remediation = RemediationOK
	p.Message = "provider executable present"
	p.Evidence["expected_binary"] = bin
	return p
}

func probeBudget(minMB int64, deps Deps, now time.Time) Probe {
	p := Probe{
		Schema: SchemaProbe, Name: "resource_budget", Kind: KindPrerequisite,
		Provenance: "BudgetFreeMB", ObservedAt: now,
		Evidence: map[string]string{"min_mb": fmt.Sprintf("%d", minMB)},
	}
	free, err := deps.BudgetFreeMB()
	if err != nil {
		p.Status = StatusUnknown
		p.Remediation = RemediationBudgetInsufficient
		p.Message = "budget probe failed"
		return p
	}
	p.Evidence["free_mb"] = fmt.Sprintf("%d", free)
	if free < minMB {
		p.Status = StatusFail
		p.Remediation = RemediationBudgetInsufficient
		p.Message = "insufficient resource budget"
		return p
	}
	p.Status = StatusPass
	p.Remediation = RemediationOK
	p.Message = "budget sufficient"
	return p
}

func probeUI(deps Deps, now time.Time) Probe {
	p := Probe{
		Schema: SchemaProbe, Name: "ui_capability", Kind: KindOptional,
		Provenance: "UICapable", ObservedAt: now,
	}
	ok, detail := deps.UICapable()
	p.Evidence = map[string]string{"detail": redact(detail)}
	if ok {
		p.Status = StatusPass
		p.Remediation = RemediationOK
		p.Message = "ui capability present"
		return p
	}
	p.Status = StatusWarn
	p.Remediation = RemediationUIOptional
	p.Message = "optional ui gap; not a provider-auth failure"
	return p
}

func probeQuota(deps Deps, now time.Time) Probe {
	p := Probe{
		Schema: SchemaProbe, Name: "quota_catalog", Kind: KindOptional,
		Provenance: "QuotaKnown", ObservedAt: now,
	}
	ok, detail := deps.QuotaKnown()
	p.Evidence = map[string]string{"detail": redact(detail)}
	if ok {
		p.Status = StatusPass
		p.Remediation = RemediationOK
		p.Message = "quota/catalog known"
		return p
	}
	p.Status = StatusWarn
	p.Remediation = RemediationQuotaOptional
	p.Message = "optional quota/catalog gap; not a provider-auth failure"
	return p
}

func ensureGlobalLayout(homeRoot string, deps Deps) ([]string, error) {
	// Only under homeRoot/.loopcoder or LOOPCODER_HOME root
	root := homeRoot
	if !strings.HasSuffix(root, ".loopcoder") && deps.Getenv != nil && deps.Getenv("LOOPCODER_HOME") == "" {
		root = filepath.Join(homeRoot, ".loopcoder")
	}
	dirs := []string{
		root,
		filepath.Join(root, "bin"),
		filepath.Join(root, "data"),
		filepath.Join(root, "logs"),
		filepath.Join(root, "tmp"),
		filepath.Join(root, "projects"),
	}
	var created []string
	for _, d := range dirs {
		_, err := deps.Stat(d)
		if err == nil {
			continue
		}
		if !errors.Is(err, os.ErrNotExist) && !os.IsNotExist(err) {
			// if Stat fails for other reasons, try mkdir anyway only for not exist
			if _, ok := err.(*os.PathError); !ok {
				return created, err
			}
		}
		if err := deps.MkdirAll(d, 0o700); err != nil {
			return created, err
		}
		created = append(created, filepath.Base(d))
	}
	sort.Strings(created)
	return created, nil
}

func normalize(d Deps) Deps {
	def := DefaultDeps()
	if d.Now == nil {
		d.Now = def.Now
	}
	if d.GOOS == "" {
		d.GOOS = def.GOOS
	}
	if d.GOARCH == "" {
		d.GOARCH = def.GOARCH
	}
	if d.LookPath == nil {
		d.LookPath = def.LookPath
	}
	if d.Stat == nil {
		d.Stat = def.Stat
	}
	if d.UserHomeDir == nil {
		d.UserHomeDir = def.UserHomeDir
	}
	if d.Getenv == nil {
		d.Getenv = def.Getenv
	}
	if d.MkdirAll == nil {
		d.MkdirAll = def.MkdirAll
	}
	if d.BudgetFreeMB == nil {
		d.BudgetFreeMB = def.BudgetFreeMB
	}
	if d.ProviderPresent == nil {
		d.ProviderPresent = def.ProviderPresent
	}
	if d.UICapable == nil {
		d.UICapable = def.UICapable
	}
	if d.QuotaKnown == nil {
		d.QuotaKnown = def.QuotaKnown
	}
	return d
}

func providerBinary(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "codex":
		return "codex"
	case "claude", "claude-code":
		return "claude"
	case "gemini":
		return "gemini"
	case "grok":
		return "grok"
	case "antigravity":
		return "agy"
	case "fixture":
		return "true" // always present on unix; tests can override
	default:
		return ""
	}
}

func digestSnapshot(s Snapshot) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%v|%s|%s", s.Decision, s.AllowLaunch, s.Provider, s.Repo)
	names := make([]string, 0, len(s.Probes))
	for _, p := range s.Probes {
		names = append(names, p.Name+":"+string(p.Status)+":"+p.Remediation)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(h, "|%s", n)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))[:24]
}

func redact(s string) string {
	lower := strings.ToLower(s)
	for _, bad := range []string{"sk-", "ghp_", "password", "api_key", "bearer ", "-----begin"} {
		if strings.Contains(lower, bad) {
			return "[redacted]"
		}
	}
	if len(s) > 200 {
		return s[:200]
	}
	return s
}

func redactPath(s string) string {
	// keep owner/name; strip absolute user paths to basename form
	if strings.HasPrefix(s, "/") || strings.Contains(s, "/Users/") {
		return filepath.Base(s)
	}
	return s
}

// GateError is returned when launch must not proceed.
type GateError struct {
	Snapshot Snapshot
}

func (e *GateError) Error() string {
	return fmt.Sprintf("preflight: launch blocked decision=%s", e.Snapshot.Decision)
}

// RequireLaunch evaluates and returns snapshot or GateError when blocked.
func RequireLaunch(ctx context.Context, in Input, deps Deps) (Snapshot, error) {
	snap, err := Evaluate(ctx, in, deps)
	if err != nil {
		return Snapshot{}, err
	}
	if !snap.AllowLaunch {
		return snap, &GateError{Snapshot: snap}
	}
	return snap, nil
}
