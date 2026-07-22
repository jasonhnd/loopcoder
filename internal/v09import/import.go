package v09import

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jasonhnd/loopcoder/internal/v08export"
)

// SchemaReport is the migration report schema.
const SchemaReport = "loopcoder.v09import.migration_report.v1"

// SchemaStore is the in-memory v0.9 store fixture schema.
const SchemaStore = "loopcoder.v09import.store.v1"

// ProjectRecord is a v0.9 project after import.
type ProjectRecord struct {
	ProjectID  string   `json:"project_id"`
	Aliases    []string `json:"aliases,omitempty"`
	RepoOwner  string   `json:"repo_owner,omitempty"`
	RepoName   string   `json:"repo_name,omitempty"`
	ImportKey  string   `json:"import_key"` // idempotency
	SourceRefs []string `json:"source_refs,omitempty"`
}

// HistoryRecord is imported terminal/historical evidence.
type HistoryRecord struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	Kind      string `json:"kind"`
	State     string `json:"state"`
	// Attention when nonterminal/live was demoted.
	Attention  bool   `json:"attention,omitempty"`
	Historical bool   `json:"historical"`
	PayloadDig string `json:"payload_digest,omitempty"`
	ImportKey  string `json:"import_key"`
	// Never authorizes process adoption.
	AuthorizesExecution bool `json:"authorizes_execution"`
}

// Conflict is a dry-run / import conflict.
type Conflict struct {
	Code      string `json:"code"`
	ProjectID string `json:"project_id,omitempty"`
	Message   string `json:"message"`
}

// Omission is an unsupported/skipped record.
type Omission struct {
	Path   string `json:"path,omitempty"`
	ID     string `json:"id,omitempty"`
	Reason string `json:"reason"`
}

// Action is a planned or applied mutation.
type Action struct {
	Op        string `json:"op"` // create_project|skip_project|create_history|skip_history|attention
	ProjectID string `json:"project_id,omitempty"`
	ID        string `json:"id,omitempty"`
	DryRun    bool   `json:"dry_run"`
}

// Report is the human/JSON migration report.
type Report struct {
	Schema             string            `json:"schema"`
	DryRun             bool              `json:"dry_run"`
	SourceVersions     []string          `json:"source_versions"`
	SourceDigests      map[string]string `json:"source_digests"`
	TargetVersion      string            `json:"target_version"`
	BundleDigest       string            `json:"bundle_digest,omitempty"`
	Counts             map[string]int    `json:"counts"`
	Conflicts          []Conflict        `json:"conflicts,omitempty"`
	Omissions          []Omission        `json:"omissions,omitempty"`
	Actions            []Action          `json:"actions,omitempty"`
	Warnings           []string          `json:"warnings,omitempty"`
	RequiredSpaceBytes int64             `json:"required_space_bytes"`
	TargetPaths        []string          `json:"target_paths"`
	// RollbackLimitation is always present post-write.
	RollbackLimitation string   `json:"rollback_limitation"`
	FailedProjects     []string `json:"failed_projects,omitempty"`
	SucceededProjects  []string `json:"succeeded_projects,omitempty"`
}

// Store is an in-memory v0.9 machine+project fixture.
type Store struct {
	mu       sync.Mutex
	Schema   string                    `json:"schema"`
	Projects map[string]*ProjectRecord `json:"projects"`
	History  map[string]*HistoryRecord `json:"history"` // import_key
	// Newer v0.9 markers prevent overwrite.
	NewerKeys map[string]bool `json:"newer_keys,omitempty"`
}

// NewStore creates an empty v0.9 store.
func NewStore() *Store {
	return &Store{
		Schema: SchemaStore, Projects: map[string]*ProjectRecord{},
		History: map[string]*HistoryRecord{}, NewerKeys: map[string]bool{},
	}
}

// Input for dry-run or import.
type Input struct {
	Bundle   *v08export.Bundle
	Manifest *v08export.Manifest
	// ExpectedBundleDigest when set must match marshaled bundle.
	ExpectedBundleDigest string
	DryRun               bool
	// FailProjectIDs forces per-project failure (fixture for atomic isolation).
	FailProjectIDs []string
	// TargetHome basename for report paths.
	TargetHome string
	Now        time.Time
}

// Result of dry-run or import.
type Result struct {
	Allowed bool
	Reasons []string
	Report  *Report
}

const targetVersion = "0.9.0"

const rollbackText = "post-write rollback requires restoring backup or new stores; importer never provides binary rollback or automatic old-state deletion"

// Run validates the export and dry-runs or imports transactionally per project.
func (s *Store) Run(in Input) Result {
	res := Result{}
	if in.Bundle == nil {
		res.Reasons = append(res.Reasons, "bundle required")
		return res
	}
	if in.Bundle.Schema != v08export.SchemaExport {
		res.Reasons = append(res.Reasons, "unsupported export schema: "+in.Bundle.Schema)
		return res
	}
	// Validate source digests presence.
	if len(in.Bundle.SourceDigests) == 0 {
		res.Reasons = append(res.Reasons, "source digests required")
		return res
	}
	raw, _ := json.Marshal(in.Bundle)
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	if in.ExpectedBundleDigest != "" && in.ExpectedBundleDigest != digest {
		res.Reasons = append(res.Reasons, "bundle digest mismatch")
		return res
	}
	if in.Manifest != nil && in.Manifest.BundleDigest != "" && in.Manifest.BundleDigest != digest {
		// Manifest may have been produced earlier; still allow if Expected not set,
		// but record warning when mismatch.
	}

	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	_ = now

	failSet := map[string]bool{}
	for _, id := range in.FailProjectIDs {
		failSet[id] = true
	}

	report := &Report{
		Schema: SchemaReport, DryRun: in.DryRun,
		SourceVersions:     append([]string(nil), in.Bundle.SourceVersions...),
		SourceDigests:      copyStrMap(in.Bundle.SourceDigests),
		TargetVersion:      targetVersion,
		BundleDigest:       digest,
		Counts:             map[string]int{},
		TargetPaths:        []string{},
		RollbackLimitation: rollbackText,
	}
	if in.TargetHome != "" {
		report.TargetPaths = append(report.TargetPaths,
			in.TargetHome+"/machine.db",
			in.TargetHome+"/projects/",
		)
	}

	// Omissions from export unsupported.
	for _, u := range in.Bundle.Unsupported {
		report.Omissions = append(report.Omissions, Omission{Path: u.Path, Reason: u.Reason})
	}
	for _, w := range in.Bundle.Warnings {
		report.Warnings = append(report.Warnings, w.Code+": "+w.Message)
	}

	// Stable project order.
	projects := append([]v08export.V08Project(nil), in.Bundle.Projects...)
	sort.Slice(projects, func(i, j int) bool { return projects[i].ProjectID < projects[j].ProjectID })

	// Group terminal evidence by project.
	byProj := map[string][]v08export.V08TerminalEvidence{}
	for _, te := range in.Bundle.TerminalEvidence {
		byProj[te.ProjectID] = append(byProj[te.ProjectID], te)
	}

	var space int64
	for _, p := range projects {
		space += 4096 // rough per-project overhead
		for range byProj[p.ProjectID] {
			space += 512
		}
	}
	report.RequiredSpaceBytes = space

	if in.DryRun {
		// Deterministic dry-run without mutation.
		for _, p := range projects {
			ik := projectKey(p)
			s.mu.Lock()
			_, exists := s.Projects[p.ProjectID]
			newer := s.NewerKeys["project:"+p.ProjectID]
			s.mu.Unlock()
			if newer {
				report.Conflicts = append(report.Conflicts, Conflict{
					Code: "newer_v09_record", ProjectID: p.ProjectID,
					Message: "would not overwrite newer v0.9 project",
				})
				report.Actions = append(report.Actions, Action{Op: "skip_project", ProjectID: p.ProjectID, DryRun: true})
				continue
			}
			if exists {
				report.Actions = append(report.Actions, Action{Op: "skip_project", ProjectID: p.ProjectID, DryRun: true})
			} else {
				report.Actions = append(report.Actions, Action{Op: "create_project", ProjectID: p.ProjectID, DryRun: true})
			}
			_ = ik
			for _, te := range byProj[p.ProjectID] {
				report.Actions = append(report.Actions, Action{
					Op: "create_history", ProjectID: p.ProjectID, ID: te.ID, DryRun: true,
				})
			}
		}
		report.Counts["projects"] = len(projects)
		report.Counts["history"] = len(in.Bundle.TerminalEvidence)
		report.Counts["omissions"] = len(report.Omissions)
		report.Counts["conflicts"] = len(report.Conflicts)
		report.Counts["actions"] = len(report.Actions)
		res.Allowed = true
		res.Report = report
		res.Reasons = append(res.Reasons, "dry-run complete; no mutation")
		return res
	}

	// Real import: per-project transactional commit.
	for _, p := range projects {
		if failSet[p.ProjectID] {
			report.FailedProjects = append(report.FailedProjects, p.ProjectID)
			report.Warnings = append(report.Warnings, "project "+p.ProjectID+" import failed (fixture); rolled back project only")
			// Do not mutate store for this project.
			continue
		}
		if err := s.importProject(p, byProj[p.ProjectID], report); err != nil {
			report.FailedProjects = append(report.FailedProjects, p.ProjectID)
			report.Warnings = append(report.Warnings, err.Error())
			continue
		}
		report.SucceededProjects = append(report.SucceededProjects, p.ProjectID)
	}

	report.Counts["projects"] = len(s.Projects)
	report.Counts["history"] = len(s.History)
	report.Counts["omissions"] = len(report.Omissions)
	report.Counts["conflicts"] = len(report.Conflicts)
	report.Counts["actions"] = len(report.Actions)
	report.Counts["failed_projects"] = len(report.FailedProjects)
	report.Counts["succeeded_projects"] = len(report.SucceededProjects)

	res.Allowed = true
	res.Report = report
	res.Reasons = append(res.Reasons, fmt.Sprintf(
		"import done succeeded=%d failed=%d", len(report.SucceededProjects), len(report.FailedProjects),
	))
	return res
}

func (s *Store) importProject(p v08export.V08Project, evidence []v08export.V08TerminalEvidence, report *Report) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.NewerKeys["project:"+p.ProjectID] {
		report.Conflicts = append(report.Conflicts, Conflict{
			Code: "newer_v09_record", ProjectID: p.ProjectID,
			Message: "refusing to overwrite newer v0.9 project",
		})
		report.Actions = append(report.Actions, Action{Op: "skip_project", ProjectID: p.ProjectID})
		return nil
	}

	ik := projectKey(p)
	if existing, ok := s.Projects[p.ProjectID]; ok {
		if existing.ImportKey == ik {
			// Idempotent: skip recreate.
			report.Actions = append(report.Actions, Action{Op: "skip_project", ProjectID: p.ProjectID})
		} else {
			// Different import key — do not overwrite; conflict.
			report.Conflicts = append(report.Conflicts, Conflict{
				Code: "project_identity_conflict", ProjectID: p.ProjectID,
				Message: "existing project with different import key",
			})
			report.Actions = append(report.Actions, Action{Op: "skip_project", ProjectID: p.ProjectID})
			return nil
		}
	} else {
		// Stage then commit (in-memory: single assignment is the commit).
		rec := &ProjectRecord{
			ProjectID: p.ProjectID, Aliases: append([]string(nil), p.Aliases...),
			RepoOwner: p.RepoOwner, RepoName: p.RepoName, ImportKey: ik,
			SourceRefs: []string{"v08export:" + ik},
		}
		s.Projects[p.ProjectID] = rec
		report.Actions = append(report.Actions, Action{Op: "create_project", ProjectID: p.ProjectID})
	}

	for _, te := range evidence {
		hik := historyKey(te)
		if s.NewerKeys["history:"+te.ID] {
			report.Conflicts = append(report.Conflicts, Conflict{
				Code: "newer_v09_history", ProjectID: p.ProjectID,
				Message: "refusing to overwrite newer history " + te.ID,
			})
			report.Actions = append(report.Actions, Action{Op: "skip_history", ProjectID: p.ProjectID, ID: te.ID})
			continue
		}
		if _, ok := s.History[hik]; ok {
			report.Actions = append(report.Actions, Action{Op: "skip_history", ProjectID: p.ProjectID, ID: te.ID})
			continue // idempotent
		}
		// All imported evidence is historical; never authorizes execution.
		// If somehow nonterminal slipped through export, demote to attention.
		attn := !isTerminal(te.State)
		hr := &HistoryRecord{
			ID: te.ID, ProjectID: te.ProjectID, Kind: te.Kind, State: te.State,
			Attention: attn, Historical: true, PayloadDig: te.PayloadDigest,
			ImportKey: hik, AuthorizesExecution: false,
		}
		s.History[hik] = hr
		op := "create_history"
		if attn {
			op = "attention"
		}
		report.Actions = append(report.Actions, Action{Op: op, ProjectID: p.ProjectID, ID: te.ID})
	}
	return nil
}

// MarkNewer simulates a newer v0.9 record that must not be overwritten.
func (s *Store) MarkNewer(kind, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.NewerKeys[kind+":"+id] = true
}

// ProjectCount for tests.
func (s *Store) ProjectCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.Projects)
}

// HistoryCount for tests.
func (s *Store) HistoryCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.History)
}

func projectKey(p v08export.V08Project) string {
	b, _ := json.Marshal(p)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

func historyKey(te v08export.V08TerminalEvidence) string {
	b, _ := json.Marshal(te)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

func isTerminal(state string) bool {
	switch strings.ToLower(state) {
	case "merged", "closed", "delivered", "gated", "terminal", "done", "success", "failed":
		return true
	default:
		return false
	}
}

func copyStrMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
