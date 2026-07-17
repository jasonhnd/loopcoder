// Package writeexec owns bounded nested write worktrees and verifies their
// mutation manifests without publishing or remediating provider changes.
package writeexec

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/lockfile"
	"github.com/jasonhnd/loopcoder/internal/pathid"
	"github.com/jasonhnd/loopcoder/internal/readonlyexec"
)

const (
	RecordSchemaVersion    = "loopcoder.bounded_write_enforcement.v1"
	AuthoritySchemaVersion = "loopcoder.bounded_write_authority.v1"
	EnforcementMode        = "isolated-worktree+bounded-manifest-v1"

	VerificationPassed       = "passed"
	VerificationViolation    = "policy-violation"
	VerificationInconclusive = "inconclusive"

	recordStatusBaseline     = "baseline-captured"
	recordStatusVerified     = "verified"
	recordStatusViolation    = "policy-violation"
	recordStatusInconclusive = "inconclusive"
	maxManifestChanges       = 256
	maxManifestViolations    = 256
)

type Options struct {
	RepoPath            string
	WorktreePath        string
	EvidencePath        string
	ContractFingerprint string
	ClaimGeneration     int64
	BaseRevision        string
	AllowedPaths        []string
	ProjectStatePaths   []string
	ExcludedPaths       []string
}

type Audit struct {
	Mode                string      `json:"mode"`
	Verification        string      `json:"verification"`
	WorktreeID          string      `json:"worktree_id"`
	BaseRevision        string      `json:"base_revision"`
	BaselineFingerprint string      `json:"baseline_fingerprint"`
	PostRunFingerprint  string      `json:"post_run_fingerprint,omitempty"`
	ManifestFingerprint string      `json:"manifest_fingerprint,omitempty"`
	Recovered           bool        `json:"recovered_after_interruption"`
	Changes             []Change    `json:"changes"`
	Violations          []Violation `json:"violations"`
}

type Change struct {
	Path       string `json:"path"`
	Kind       string `json:"kind"`
	BeforeHash string `json:"before_hash"`
	AfterHash  string `json:"after_hash"`
}

type Violation struct {
	Code       string `json:"code"`
	Surface    string `json:"surface"`
	TargetID   string `json:"target_id"`
	BeforeHash string `json:"before_hash"`
	AfterHash  string `json:"after_hash"`
}

type PolicyViolationError struct {
	Phase      string
	Violations []Violation
	Reason     string
}

func (e *PolicyViolationError) Error() string {
	if e == nil {
		return "bounded write child policy violation"
	}
	reason := strings.TrimSpace(e.Reason)
	if reason == "" {
		reason = "the bounded write policy could not be proven"
	}
	if len(e.Violations) == 0 {
		return "bounded write child policy violation: " + reason
	}
	return fmt.Sprintf("bounded write child policy violation: %s (%d violation(s))", reason, len(e.Violations))
}

func (*PolicyViolationError) ChildExecutionPolicyViolation() {}

func (*PolicyViolationError) ChildExecutionPolicyOutcome() string {
	return "write_scope_policy_violation"
}

type treeSnapshot struct {
	Fingerprint string            `json:"fingerprint"`
	Entries     map[string]string `json:"entries"`
}

type allowedPath struct {
	Relative  string `json:"relative"`
	Directory bool   `json:"directory"`
}

type evidenceRecord struct {
	SchemaVersion       string                 `json:"schema_version"`
	Mode                string                 `json:"mode"`
	ContractFingerprint string                 `json:"contract_fingerprint"`
	ClaimGeneration     int64                  `json:"claim_generation"`
	BaseRevision        string                 `json:"base_revision"`
	WorktreeID          string                 `json:"worktree_id"`
	WorktreePath        string                 `json:"worktree_path"`
	Status              string                 `json:"status"`
	Outcome             string                 `json:"outcome,omitempty"`
	Recovered           bool                   `json:"recovered_after_interruption"`
	Allowed             []allowedPath          `json:"allowed_paths"`
	BaselineTree        treeSnapshot           `json:"baseline_tree"`
	BaselineProtected   readonlyexec.Snapshot  `json:"baseline_protected"`
	PostRunTree         *treeSnapshot          `json:"post_run_tree,omitempty"`
	PostRunProtected    *readonlyexec.Snapshot `json:"post_run_protected,omitempty"`
	Audit               *Audit                 `json:"audit,omitempty"`
}

type authorityRecord struct {
	SchemaVersion       string `json:"schema_version"`
	ContractFingerprint string `json:"contract_fingerprint"`
	BaseRevision        string `json:"base_revision"`
	Fingerprint         string `json:"fingerprint"`
}

type Session struct {
	opts              Options
	allowed           []allowedPath
	baselineTree      treeSnapshot
	baselineProtected readonlyexec.Snapshot
	worktreeID        string
	metadataIndexKey  string
	recovered         bool
}

// ResolveAuthority persists the first accepted base revision for a logical
// child. Later claim generations reuse it even if the remote-tracking branch
// moves, and a changed execution contract fails closed.
func ResolveAuthority(path, contractFingerprint, requestedBase string) (string, error) {
	path = normalizePhysicalPath(path)
	contractFingerprint = strings.TrimSpace(contractFingerprint)
	requestedBase = strings.TrimSpace(requestedBase)
	if path == "" || contractFingerprint == "" {
		return "", &PolicyViolationError{Phase: "authority", Reason: "bounded write authority requires a private path and immutable contract"}
	}
	if existing, ok, err := loadAuthority(path); err != nil {
		return "", &PolicyViolationError{Phase: "authority", Reason: "bounded write authority is unreadable"}
	} else if ok {
		if !validAuthority(existing) || existing.ContractFingerprint != contractFingerprint {
			return "", &PolicyViolationError{Phase: "authority", Reason: "bounded write authority failed integrity or contract validation"}
		}
		return existing.BaseRevision, nil
	}
	if !validObjectID(requestedBase) {
		return "", &PolicyViolationError{Phase: "authority", Reason: "initial bounded write authority requires an exact base revision"}
	}
	record := authorityRecord{SchemaVersion: AuthoritySchemaVersion, ContractFingerprint: contractFingerprint, BaseRevision: requestedBase}
	record.Fingerprint = authorityFingerprint(record)
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", &PolicyViolationError{Phase: "authority", Reason: "bounded write authority directory could not be prepared"}
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		existing, ok, loadErr := loadAuthority(path)
		if loadErr != nil || !ok || !validAuthority(existing) || existing.ContractFingerprint != contractFingerprint {
			return "", &PolicyViolationError{Phase: "authority", Reason: "concurrent bounded write authority could not be authenticated"}
		}
		return existing.BaseRevision, nil
	}
	if err != nil {
		return "", &PolicyViolationError{Phase: "authority", Reason: "bounded write authority could not be created"}
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return "", &PolicyViolationError{Phase: "authority", Reason: "bounded write authority could not be persisted"}
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return "", &PolicyViolationError{Phase: "authority", Reason: "bounded write authority could not be synchronized"}
	}
	if err := file.Close(); err != nil {
		return "", &PolicyViolationError{Phase: "authority", Reason: "bounded write authority could not be closed"}
	}
	return requestedBase, nil
}

func Begin(ctx context.Context, opts Options) (*Session, Audit, error) {
	opts = normalizeOptions(opts)
	if err := validateOptions(opts); err != nil {
		audit := inconclusiveAudit("pre-launch")
		return nil, audit, policyError("pre-launch", audit, "bounded write enforcement could not be prepared")
	}
	if err := ensureIsolatedWorktree(ctx, opts); err != nil {
		audit := inconclusiveAudit("worktree")
		return nil, audit, policyError("pre-launch", audit, "the isolated bounded write worktree could not be created or adopted")
	}
	worktreeID, err := worktreeID(opts.WorktreePath)
	if err != nil {
		audit := inconclusiveAudit("worktree-identity")
		return nil, audit, policyError("pre-launch", audit, "the isolated worktree identity could not be verified")
	}
	metadataIndexKey, err := worktreeMetadataIndexKey(ctx, opts.WorktreePath)
	if err != nil {
		audit := inconclusiveAudit("worktree-metadata")
		return nil, audit, policyError("pre-launch", audit, "the isolated worktree metadata could not be verified")
	}
	allowed, err := resolveAllowedPaths(opts)
	if err != nil {
		audit := inconclusiveAudit("mutation-scope")
		return nil, audit, policyError("pre-launch", audit, "the explicit allowed mutation scope is empty, ambiguous, or unsafe")
	}
	if err := validateWorktreeSymlinks(opts.WorktreePath); err != nil {
		audit := Audit{Mode: EnforcementMode, Verification: VerificationViolation, WorktreeID: worktreeID, BaseRevision: opts.BaseRevision, Violations: []Violation{simpleViolation("preexisting_symlink_escape", "worktree", "symlink-escape", "expected", "outside")}, Changes: []Change{}}
		return nil, audit, policyError("pre-launch", audit, "the isolated worktree contains a symlink or junction escape")
	}

	session := &Session{opts: opts, allowed: allowed, worktreeID: worktreeID, metadataIndexKey: metadataIndexKey}
	if prior, ok, err := loadRecord(opts.EvidencePath); err != nil {
		audit := inconclusiveAudit("crash-recovery")
		return nil, audit, policyError("crash-recovery", audit, "prior bounded write evidence is unreadable")
	} else if ok {
		if !validEvidenceRecord(opts, allowed, worktreeID, prior) {
			audit := inconclusiveAudit("evidence-integrity")
			audit.WorktreeID, audit.BaseRevision, audit.Recovered = worktreeID, opts.BaseRevision, true
			return nil, audit, policyError("crash-recovery", audit, "prior bounded write evidence failed integrity validation")
		}
		switch prior.Status {
		case recordStatusVerified:
			audit := *prior.Audit
			audit.Verification = VerificationInconclusive
			audit.Violations = append(audit.Violations, simpleViolation("verified_execution_requires_reconciliation", "audit-state", "verified-execution", "verified", "relaunch-refused"))
			return nil, audit, policyError("crash-recovery", audit, "a verified bounded write execution must be reconciled instead of relaunched")
		case recordStatusViolation, recordStatusInconclusive:
			audit := auditFromRecord(prior)
			return nil, audit, policyError("crash-recovery", audit, "a prior bounded write violation or inconclusive verification requires human review")
		case recordStatusBaseline:
			currentTree, currentProtected, captureErr := session.capture(ctx)
			if captureErr != nil {
				audit := inconclusiveAudit("crash-recovery-capture")
				audit.WorktreeID, audit.BaseRevision, audit.Recovered = worktreeID, opts.BaseRevision, true
				return nil, audit, policyError("crash-recovery", audit, "the interrupted bounded write baseline could not be recaptured")
			}
			if currentTree.Fingerprint != prior.BaselineTree.Fingerprint || currentProtected.Fingerprint != prior.BaselineProtected.Fingerprint {
				audit := session.evaluate(prior.BaselineTree, currentTree, prior.BaselineProtected, currentProtected, true)
				audit.Verification = VerificationViolation
				audit.Violations = appendBoundedViolation(audit.Violations, simpleViolation("interrupted_execution_changed_state", "crash-recovery", "interrupted-state", prior.BaselineTree.Fingerprint, currentTree.Fingerprint))
				audit.ManifestFingerprint = auditFingerprint(audit)
				prior.Status, prior.Recovered, prior.PostRunTree, prior.PostRunProtected, prior.Audit = recordStatusViolation, true, &currentTree, &currentProtected, &audit
				_ = writeRecord(opts.EvidencePath, prior)
				return nil, audit, policyError("crash-recovery", audit, "state changed after an interrupted bounded write provider launch")
			}
			session.baselineTree, session.baselineProtected, session.recovered = currentTree, currentProtected, true
		default:
			audit := inconclusiveAudit("evidence-state")
			return nil, audit, policyError("crash-recovery", audit, "prior bounded write evidence has an unsupported lifecycle state")
		}
	}

	if session.baselineTree.Entries == nil {
		session.baselineTree, session.baselineProtected, err = session.capture(ctx)
		if err != nil {
			audit := inconclusiveAudit("baseline-capture")
			audit.WorktreeID, audit.BaseRevision = worktreeID, opts.BaseRevision
			return nil, audit, policyError("pre-launch", audit, "the bounded write baseline could not be captured")
		}
	}
	record := evidenceRecord{
		SchemaVersion: RecordSchemaVersion, Mode: EnforcementMode,
		ContractFingerprint: opts.ContractFingerprint, ClaimGeneration: opts.ClaimGeneration,
		BaseRevision: opts.BaseRevision, WorktreeID: worktreeID, WorktreePath: opts.WorktreePath,
		Status: recordStatusBaseline, Recovered: session.recovered, Allowed: allowed,
		BaselineTree: session.baselineTree, BaselineProtected: session.baselineProtected,
	}
	if err := writeRecord(opts.EvidencePath, record); err != nil {
		audit := inconclusiveAudit("baseline-write")
		audit.WorktreeID, audit.BaseRevision = worktreeID, opts.BaseRevision
		return nil, audit, policyError("pre-launch", audit, "the bounded write baseline could not be persisted")
	}
	audit := Audit{Mode: EnforcementMode, Verification: recordStatusBaseline, WorktreeID: worktreeID, BaseRevision: opts.BaseRevision, BaselineFingerprint: session.combinedFingerprint(session.baselineTree, session.baselineProtected), Recovered: session.recovered, Changes: []Change{}, Violations: []Violation{}}
	return session, audit, nil
}

func (s *Session) Finish(ctx context.Context, outcome string) (Audit, error) {
	if s == nil {
		audit := inconclusiveAudit("post-run")
		return audit, policyError("post-run", audit, "bounded write enforcement session is missing")
	}
	if !s.evidenceFenceValid() {
		audit := Audit{Mode: EnforcementMode, Verification: VerificationViolation, WorktreeID: s.worktreeID, BaseRevision: s.opts.BaseRevision, BaselineFingerprint: s.combinedFingerprint(s.baselineTree, s.baselineProtected), Recovered: s.recovered, Changes: []Change{}, Violations: []Violation{simpleViolation("enforcement_evidence_modified", "audit-state", "evidence-fence", "expected", "mismatch")}}
		audit.ManifestFingerprint = auditFingerprint(audit)
		return audit, policyError("post-run", audit, "bounded write enforcement evidence changed during provider execution")
	}
	postTree, postProtected, err := s.capture(ctx)
	if err != nil {
		audit := inconclusiveAudit("post-run-capture")
		audit.WorktreeID, audit.BaseRevision, audit.BaselineFingerprint, audit.Recovered = s.worktreeID, s.opts.BaseRevision, s.combinedFingerprint(s.baselineTree, s.baselineProtected), s.recovered
		audit.ManifestFingerprint = auditFingerprint(audit)
		record := s.record(recordStatusInconclusive, outcome, nil, nil, audit)
		_ = writeRecord(s.opts.EvidencePath, record)
		return audit, policyError("post-run", audit, "post-run bounded write state could not be verified")
	}
	audit := s.evaluate(s.baselineTree, postTree, s.baselineProtected, postProtected, s.recovered)
	status := recordStatusVerified
	if len(audit.Violations) > 0 {
		audit.Verification, status = VerificationViolation, recordStatusViolation
	}
	audit.ManifestFingerprint = auditFingerprint(audit)
	record := s.record(status, outcome, &postTree, &postProtected, audit)
	if err := writeRecord(s.opts.EvidencePath, record); err != nil {
		audit.Verification = VerificationInconclusive
		audit.Violations = appendBoundedViolation(audit.Violations, simpleViolation("manifest_persistence_failed", "audit-state", "manifest-write", "expected", "unknown"))
		audit.ManifestFingerprint = auditFingerprint(audit)
		return audit, policyError("post-run", audit, "the bounded mutation manifest could not be persisted")
	}
	if len(audit.Violations) > 0 {
		return audit, policyError("post-run", audit, "the bounded mutation manifest contains forbidden or inconclusive changes")
	}
	return audit, nil
}

func ReconcileVerified(ctx context.Context, opts Options) (Audit, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	opts = normalizeOptions(opts)
	if err := validateOptions(opts); err != nil {
		audit := inconclusiveAudit("reconciliation")
		return audit, policyError("reconciliation", audit, "bounded write evidence could not be prepared for reconciliation")
	}
	if err := validateAdoptedWorktree(ctx, opts); err != nil {
		audit := inconclusiveAudit("reconciliation-worktree")
		return audit, policyError("reconciliation", audit, "bounded write worktree is missing or no longer matches its pinned authority")
	}
	worktreeID, err := worktreeID(opts.WorktreePath)
	if err != nil {
		audit := inconclusiveAudit("reconciliation-worktree")
		return audit, policyError("reconciliation", audit, "bounded write worktree identity could not be reconciled")
	}
	allowed, err := resolveAllowedPaths(opts)
	if err != nil {
		audit := inconclusiveAudit("reconciliation-scope")
		return audit, policyError("reconciliation", audit, "bounded write scope could not be reconciled")
	}
	record, ok, err := loadRecord(opts.EvidencePath)
	if err != nil || !ok || !validEvidenceRecord(opts, allowed, worktreeID, record) || record.Status != recordStatusVerified || record.Audit == nil || record.Audit.Verification != VerificationPassed {
		audit := inconclusiveAudit("reconciliation-evidence")
		return audit, policyError("reconciliation", audit, "verified bounded write evidence is missing, invalid, or non-terminal")
	}
	if err := validateWorktreeSymlinks(opts.WorktreePath); err != nil {
		audit := *record.Audit
		audit.Verification = VerificationViolation
		audit.Violations = appendBoundedViolation(audit.Violations, simpleViolation("post_manifest_symlink_escape", "worktree", "reconciliation-symlink", "verified", "changed"))
		audit.ManifestFingerprint = auditFingerprint(audit)
		return audit, policyError("reconciliation", audit, "the preserved bounded write worktree contains a post-manifest symlink escape")
	}
	currentTree, err := captureTree(opts.WorktreePath)
	if err != nil || record.PostRunTree == nil || currentTree.Fingerprint != record.PostRunTree.Fingerprint {
		audit := *record.Audit
		audit.Verification = VerificationViolation
		audit.Violations = appendBoundedViolation(audit.Violations, simpleViolation("post_manifest_worktree_changed", "worktree", "reconciliation-tree", record.Audit.PostRunFingerprint, "changed"))
		audit.ManifestFingerprint = auditFingerprint(audit)
		return audit, policyError("reconciliation", audit, "the preserved bounded write worktree changed after its verified manifest")
	}
	index, err := runGit(ctx, opts.WorktreePath, "ls-files", "--stage", "-z")
	expectedIndex := ""
	if record.PostRunProtected != nil {
		expectedIndex = record.PostRunProtected.Entries["worktree:external-"+worktreeID+":index-entries"]
	}
	if err != nil || expectedIndex == "" || digestBytes(index) != expectedIndex {
		audit := *record.Audit
		audit.Verification = VerificationViolation
		audit.Violations = appendBoundedViolation(audit.Violations, simpleViolation("post_manifest_git_index_changed", "index", "reconciliation-index", expectedIndex, "changed"))
		audit.ManifestFingerprint = auditFingerprint(audit)
		return audit, policyError("reconciliation", audit, "the preserved bounded write index changed after its verified manifest")
	}
	return *record.Audit, nil
}

func (s *Session) capture(ctx context.Context) (treeSnapshot, readonlyexec.Snapshot, error) {
	tree, err := captureTree(s.opts.WorktreePath)
	if err != nil {
		return treeSnapshot{}, readonlyexec.Snapshot{}, err
	}
	protected, err := readonlyexec.Capture(ctx, readonlyexec.Options{RepoPath: s.opts.RepoPath, ProjectStatePaths: s.opts.ProjectStatePaths, ExcludedPaths: s.opts.ExcludedPaths})
	if err != nil {
		return treeSnapshot{}, readonlyexec.Snapshot{}, err
	}
	protected = filterProtectedSnapshot(protected, s.worktreeID, s.metadataIndexKey)
	globalConfig, err := captureGlobalGitConfig(ctx, s.opts.RepoPath)
	if err != nil {
		return treeSnapshot{}, readonlyexec.Snapshot{}, err
	}
	protected.Entries["external:git-global-config"] = digestBytes(globalConfig)
	if homeDir, homeErr := os.UserHomeDir(); homeErr == nil {
		credentialPaths := []string{
			filepath.Join(homeDir, ".gitconfig"),
			filepath.Join(homeDir, ".git-credentials"),
			filepath.Join(homeDir, ".config", "git", "config"),
			filepath.Join(homeDir, ".config", "git", "credentials"),
		}
		for index, path := range credentialPaths {
			digest, digestErr := digestPath(path)
			if digestErr != nil {
				return treeSnapshot{}, readonlyexec.Snapshot{}, digestErr
			}
			protected.Entries[fmt.Sprintf("external:git-credential-state:%d", index)] = digest
		}
	}
	protected.Fingerprint = fingerprintEntries(protected.Entries)
	return tree, protected, nil
}

func captureGlobalGitConfig(ctx context.Context, repoPath string) ([]byte, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	candidates := []string{
		filepath.Join(homeDir, ".gitconfig"),
		filepath.Join(homeDir, ".config", "git", "config"),
	}
	found := false
	for _, path := range candidates {
		if _, statErr := os.Lstat(path); statErr == nil {
			found = true
			break
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return nil, statErr
		}
	}
	if !found {
		return []byte{}, nil
	}
	return runGit(ctx, repoPath, "config", "--global", "--null", "--list", "--show-origin")
}

func (s *Session) evaluate(beforeTree, afterTree treeSnapshot, beforeProtected, afterProtected readonlyexec.Snapshot, recovered bool) Audit {
	audit := Audit{
		Mode: EnforcementMode, Verification: VerificationPassed,
		WorktreeID: s.worktreeID, BaseRevision: s.opts.BaseRevision,
		BaselineFingerprint: s.combinedFingerprint(beforeTree, beforeProtected),
		PostRunFingerprint:  s.combinedFingerprint(afterTree, afterProtected),
		Recovered:           recovered, Changes: []Change{}, Violations: []Violation{},
	}
	for _, violation := range readonlyexec.Compare(beforeProtected, afterProtected) {
		audit.Violations = appendBoundedViolation(audit.Violations, Violation{Code: violation.Code, Surface: violation.Surface, TargetID: violation.TargetID, BeforeHash: violation.BeforeHash, AfterHash: violation.AfterHash})
	}
	keys := map[string]bool{}
	for key := range beforeTree.Entries {
		keys[key] = true
	}
	for key := range afterTree.Entries {
		keys[key] = true
	}
	ordered := make([]string, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	for _, rel := range ordered {
		before, after := valueOrAbsent(beforeTree.Entries, rel), valueOrAbsent(afterTree.Entries, rel)
		if before == after {
			continue
		}
		kind := "modified"
		if before == "absent" {
			kind = "created"
		} else if after == "absent" {
			kind = "deleted"
		}
		if reservedGitStatePath(rel) {
			audit.Violations = appendBoundedViolation(audit.Violations, pathViolation("git_worktree_metadata_modified", "worktree-metadata", rel, before, after))
			continue
		}
		if reservedProjectStatePath(rel) {
			audit.Violations = appendBoundedViolation(audit.Violations, pathViolation("loopcoder_project_state_modified", "project-state", rel, before, after))
			continue
		}
		if !pathAllowed(rel, s.allowed) {
			audit.Violations = appendBoundedViolation(audit.Violations, pathViolation("out_of_scope_mutation", "worktree", rel, before, after))
			continue
		}
		if after != "absent" && !physicalPathWithin(s.opts.WorktreePath, filepath.Join(s.opts.WorktreePath, filepath.FromSlash(rel))) {
			audit.Violations = appendBoundedViolation(audit.Violations, pathViolation("symlink_or_junction_escape", "worktree", rel, before, after))
			continue
		}
		if len(audit.Changes) >= maxManifestChanges {
			audit.Violations = appendBoundedViolation(audit.Violations, simpleViolation("mutation_manifest_limit_exceeded", "manifest", "manifest-limit", digestBytes([]byte(fmt.Sprint(maxManifestChanges))), digestBytes([]byte(fmt.Sprint(len(ordered))))))
			continue
		}
		audit.Changes = append(audit.Changes, Change{Path: filepath.ToSlash(rel), Kind: kind, BeforeHash: before, AfterHash: after})
	}
	if len(audit.Violations) > 0 {
		audit.Verification = VerificationViolation
	}
	return audit
}

func (s *Session) combinedFingerprint(tree treeSnapshot, protected readonlyexec.Snapshot) string {
	return digestBytes([]byte(tree.Fingerprint + "\x00" + protected.Fingerprint))
}

func (s *Session) evidenceFenceValid() bool {
	record, ok, err := loadRecord(s.opts.EvidencePath)
	return err == nil && ok && validEvidenceRecord(s.opts, s.allowed, s.worktreeID, record) && record.Status == recordStatusBaseline && record.BaselineTree.Fingerprint == s.baselineTree.Fingerprint && record.BaselineProtected.Fingerprint == s.baselineProtected.Fingerprint
}

func (s *Session) record(status, outcome string, postTree *treeSnapshot, postProtected *readonlyexec.Snapshot, audit Audit) evidenceRecord {
	return evidenceRecord{
		SchemaVersion: RecordSchemaVersion, Mode: EnforcementMode,
		ContractFingerprint: s.opts.ContractFingerprint, ClaimGeneration: s.opts.ClaimGeneration,
		BaseRevision: s.opts.BaseRevision, WorktreeID: s.worktreeID, WorktreePath: s.opts.WorktreePath,
		Status: status, Outcome: strings.TrimSpace(outcome), Recovered: s.recovered, Allowed: append([]allowedPath(nil), s.allowed...),
		BaselineTree: s.baselineTree, BaselineProtected: s.baselineProtected,
		PostRunTree: postTree, PostRunProtected: postProtected, Audit: &audit,
	}
}

func ensureIsolatedWorktree(ctx context.Context, opts Options) error {
	if count, err := registeredWorktreeCount(ctx, opts.RepoPath, opts.WorktreePath); err != nil {
		return err
	} else if count == 1 {
		return validateAdoptedWorktree(ctx, opts)
	} else if count > 1 {
		return errors.New("isolated worktree is registered more than once")
	}
	if _, err := os.Lstat(opts.WorktreePath); err == nil {
		return errors.New("unregistered isolated worktree path already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(opts.WorktreePath), 0o700); err != nil {
		return err
	}
	lock, err := lockfile.Acquire(opts.RepoPath, 30*time.Second)
	if err != nil {
		return err
	}
	defer lock.Release()
	if count, err := registeredWorktreeCount(ctx, opts.RepoPath, opts.WorktreePath); err != nil {
		return err
	} else if count == 1 {
		return validateAdoptedWorktree(ctx, opts)
	} else if count > 1 {
		return errors.New("isolated worktree is registered more than once")
	}
	if _, err := runGit(ctx, opts.RepoPath, "worktree", "add", "--detach", opts.WorktreePath, opts.BaseRevision); err != nil {
		return err
	}
	return validateAdoptedWorktree(ctx, opts)
}

func validateAdoptedWorktree(ctx context.Context, opts Options) error {
	head, err := runGit(ctx, opts.WorktreePath, "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(string(head)) != opts.BaseRevision {
		return errors.New("isolated worktree base revision mismatch")
	}
	branch, err := runGit(ctx, opts.WorktreePath, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || strings.TrimSpace(string(branch)) != "HEAD" {
		return errors.New("isolated worktree must remain detached")
	}
	count, err := registeredWorktreeCount(ctx, opts.RepoPath, opts.WorktreePath)
	if err != nil || count != 1 {
		return errors.New("isolated worktree registration is not unique")
	}
	return nil
}

func registeredWorktreeCount(ctx context.Context, repoPath, target string) (int, error) {
	output, err := runGit(ctx, repoPath, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return 0, err
	}
	targetID, err := pathid.Identity(target)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, field := range bytes.Split(output, []byte{0}) {
		line := string(field)
		if !strings.HasPrefix(line, "worktree ") {
			continue
		}
		identity, identityErr := pathid.Identity(strings.TrimPrefix(line, "worktree "))
		if identityErr != nil {
			return 0, identityErr
		}
		if identity == targetID {
			count++
		}
	}
	return count, nil
}

func resolveAllowedPaths(opts Options) ([]allowedPath, error) {
	repoID, err := pathid.Identity(opts.RepoPath)
	if err != nil {
		return nil, err
	}
	worktreeID, err := pathid.Identity(opts.WorktreePath)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	allowed := make([]allowedPath, 0, len(opts.AllowedPaths))
	for _, raw := range opts.AllowedPaths {
		pathID, err := pathid.Identity(raw)
		if err != nil || !pathWithin(repoID, pathID) {
			return nil, errors.New("allowed mutation path escapes registered checkout")
		}
		rel, err := filepath.Rel(repoID, pathID)
		if err != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, errors.New("allowed mutation path is empty or ambiguous")
		}
		rel = filepath.ToSlash(filepath.Clean(rel))
		if rel == ".git" || strings.HasPrefix(rel, ".git/") || reservedProjectStatePath(rel) || seen[rel] {
			return nil, errors.New("allowed mutation path targets reserved state or is duplicated")
		}
		candidate := filepath.Join(worktreeID, filepath.FromSlash(rel))
		candidateID, err := pathid.Identity(candidate)
		if err != nil || !pathWithin(worktreeID, candidateID) {
			return nil, errors.New("allowed mutation path resolves outside isolated worktree")
		}
		directory := false
		if info, statErr := os.Stat(candidateID); statErr == nil {
			directory = info.IsDir()
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return nil, statErr
		}
		seen[rel] = true
		allowed = append(allowed, allowedPath{Relative: rel, Directory: directory})
	}
	if len(allowed) == 0 {
		return nil, errors.New("at least one concrete allowed mutation path is required")
	}
	sort.Slice(allowed, func(i, j int) bool { return allowed[i].Relative < allowed[j].Relative })
	return allowed, nil
}

func validateWorktreeSymlinks(worktree string) error {
	worktreeID, err := pathid.Identity(worktree)
	if err != nil {
		return err
	}
	return filepath.WalkDir(worktree, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if current == worktree {
			return nil
		}
		rel, err := filepath.Rel(worktree, current)
		if err != nil {
			return err
		}
		if rel == ".git" && entry.IsDir() {
			return filepath.SkipDir
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			return nil
		}
		identity, err := pathid.Identity(current)
		if err != nil || !pathWithin(worktreeID, identity) {
			return errors.New("worktree symlink escapes isolated checkout")
		}
		return nil
	})
}

func captureTree(root string) (treeSnapshot, error) {
	entries := map[string]string{}
	err := filepath.WalkDir(root, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if current == root {
			return nil
		}
		rel, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		if rel == ".git" && entry.IsDir() {
			return filepath.SkipDir
		}
		digest, err := digestPath(current)
		if err != nil {
			return err
		}
		entries[filepath.ToSlash(rel)] = digest
		return nil
	})
	if err != nil {
		return treeSnapshot{}, err
	}
	return treeSnapshot{Fingerprint: fingerprintEntries(entries), Entries: entries}, nil
}

func filterProtectedSnapshot(snapshot readonlyexec.Snapshot, worktreeID, metadataIndexKey string) readonlyexec.Snapshot {
	entries := make(map[string]string, len(snapshot.Entries))
	childPrefix := "worktree:external-" + worktreeID + ":"
	for key, value := range snapshot.Entries {
		if strings.HasPrefix(key, childPrefix) && key != childPrefix+"index-entries" {
			continue
		}
		if key == metadataIndexKey {
			continue
		}
		entries[key] = value
	}
	return readonlyexec.Snapshot{Fingerprint: fingerprintEntries(entries), Entries: entries}
}

func worktreeID(path string) (string, error) {
	identity, err := pathid.Identity(path)
	if err != nil {
		return "", err
	}
	return digestHex([]byte(identity))[:16], nil
}

func worktreeMetadataIndexKey(ctx context.Context, worktree string) (string, error) {
	gitDir, err := gitAbsolutePath(ctx, worktree, "--git-dir")
	if err != nil {
		return "", err
	}
	commonDir, err := gitAbsolutePath(ctx, worktree, "--git-common-dir")
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(filepath.Join(commonDir, "worktrees"), gitDir)
	if err != nil || rel == "." || filepath.IsAbs(rel) || strings.HasPrefix(rel, "..") {
		return "", errors.New("worktree metadata directory is outside common Git state")
	}
	return "git:worktree-metadata:" + filepath.ToSlash(filepath.Join(rel, "index")), nil
}

func normalizeOptions(opts Options) Options {
	opts.RepoPath = normalizePhysicalPath(opts.RepoPath)
	opts.WorktreePath = normalizePhysicalPath(opts.WorktreePath)
	opts.EvidencePath = normalizePhysicalPath(opts.EvidencePath)
	opts.ContractFingerprint = strings.TrimSpace(opts.ContractFingerprint)
	opts.BaseRevision = strings.TrimSpace(opts.BaseRevision)
	opts.AllowedPaths = normalizePaths(opts.AllowedPaths)
	opts.ProjectStatePaths = normalizePaths(opts.ProjectStatePaths)
	opts.ExcludedPaths = normalizePaths(opts.ExcludedPaths)
	return opts
}

func normalizePaths(paths []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		path = normalizePhysicalPath(path)
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

func normalizePhysicalPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	canonical, err := pathid.Canonicalize(path)
	if err == nil && canonical.Identity != "" {
		return filepath.Clean(canonical.Identity)
	}
	return filepath.Clean(path)
}

func validateOptions(opts Options) error {
	if opts.RepoPath == "" || opts.WorktreePath == "" || opts.EvidencePath == "" || opts.ContractFingerprint == "" || opts.ClaimGeneration <= 0 || !validObjectID(opts.BaseRevision) || len(opts.AllowedPaths) == 0 {
		return errors.New("repo, isolated worktree, evidence, immutable contract, claim generation, base revision, and mutation scope are required")
	}
	if pathWithin(opts.RepoPath, opts.WorktreePath) || pathWithin(opts.WorktreePath, opts.RepoPath) {
		return errors.New("isolated worktree must be outside the registered checkout")
	}
	for _, guarded := range append([]string{opts.RepoPath, opts.WorktreePath}, opts.ProjectStatePaths...) {
		if pathWithin(guarded, opts.EvidencePath) {
			return errors.New("private evidence must be outside guarded state")
		}
	}
	return nil
}

func validEvidenceRecord(opts Options, allowed []allowedPath, worktreeID string, record evidenceRecord) bool {
	if record.SchemaVersion != RecordSchemaVersion || record.Mode != EnforcementMode || record.ContractFingerprint != opts.ContractFingerprint || record.ClaimGeneration != opts.ClaimGeneration || record.BaseRevision != opts.BaseRevision || record.WorktreeID != worktreeID || normalizePhysicalPath(record.WorktreePath) != opts.WorktreePath || !equalAllowed(record.Allowed, allowed) {
		return false
	}
	switch record.Status {
	case recordStatusBaseline, recordStatusVerified, recordStatusViolation, recordStatusInconclusive:
	default:
		return false
	}
	if !validTreeSnapshot(record.BaselineTree) || !validProtectedSnapshot(record.BaselineProtected) {
		return false
	}
	if record.PostRunTree != nil && !validTreeSnapshot(*record.PostRunTree) {
		return false
	}
	if record.PostRunProtected != nil && !validProtectedSnapshot(*record.PostRunProtected) {
		return false
	}
	if record.Status == recordStatusBaseline {
		return record.PostRunTree == nil && record.PostRunProtected == nil && record.Audit == nil
	}
	if record.Audit == nil || record.Audit.ManifestFingerprint != auditFingerprint(*record.Audit) {
		return false
	}
	if record.Status == recordStatusInconclusive {
		return record.PostRunTree == nil && record.PostRunProtected == nil && record.Audit.Verification == VerificationInconclusive && len(record.Audit.Violations) > 0
	}
	if record.PostRunTree == nil || record.PostRunProtected == nil {
		return false
	}
	session := &Session{opts: opts, allowed: allowed, worktreeID: worktreeID, recovered: record.Recovered}
	expectedAudit := session.evaluate(record.BaselineTree, *record.PostRunTree, record.BaselineProtected, *record.PostRunProtected, record.Recovered)
	if record.Status == recordStatusViolation && record.Recovered && strings.TrimSpace(record.Outcome) == "" {
		expectedAudit.Verification = VerificationViolation
		expectedAudit.Violations = appendBoundedViolation(expectedAudit.Violations, simpleViolation("interrupted_execution_changed_state", "crash-recovery", "interrupted-state", record.BaselineTree.Fingerprint, record.PostRunTree.Fingerprint))
	}
	expectedAudit.ManifestFingerprint = auditFingerprint(expectedAudit)
	if record.Audit.ManifestFingerprint != expectedAudit.ManifestFingerprint {
		return false
	}
	if record.Status == recordStatusVerified && (record.Audit.Verification != VerificationPassed || len(record.Audit.Violations) != 0) {
		return false
	}
	if (record.Status == recordStatusViolation || record.Status == recordStatusInconclusive) && len(record.Audit.Violations) == 0 {
		return false
	}
	return true
}

func validTreeSnapshot(snapshot treeSnapshot) bool {
	return validDigest(snapshot.Fingerprint) && fingerprintEntries(snapshot.Entries) == snapshot.Fingerprint
}

func validProtectedSnapshot(snapshot readonlyexec.Snapshot) bool {
	return validDigest(snapshot.Fingerprint) && fingerprintEntries(snapshot.Entries) == snapshot.Fingerprint
}

func equalAllowed(left, right []allowedPath) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func auditFromRecord(record evidenceRecord) Audit {
	if record.Audit != nil {
		return *record.Audit
	}
	return inconclusiveAudit("missing-audit")
}

func auditFingerprint(audit Audit) string {
	copy := audit
	copy.ManifestFingerprint = ""
	data, _ := json.Marshal(copy)
	return digestBytes(data)
}

func authorityFingerprint(record authorityRecord) string {
	return digestBytes([]byte(record.SchemaVersion + "\x00" + record.ContractFingerprint + "\x00" + record.BaseRevision))
}

func validAuthority(record authorityRecord) bool {
	return record.SchemaVersion == AuthoritySchemaVersion && record.ContractFingerprint != "" && validObjectID(record.BaseRevision) && record.Fingerprint == authorityFingerprint(record)
}

func loadAuthority(path string) (authorityRecord, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return authorityRecord{}, false, nil
	}
	if err != nil {
		return authorityRecord{}, false, err
	}
	var record authorityRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return authorityRecord{}, false, err
	}
	return record, true, nil
}

func loadRecord(path string) (evidenceRecord, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return evidenceRecord{}, false, nil
	}
	if err != nil {
		return evidenceRecord{}, false, err
	}
	var record evidenceRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return evidenceRecord{}, false, err
	}
	return record, true, nil
}

func writeRecord(path string, record evidenceRecord) error {
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".bounded-write-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	remove := true
	defer func() {
		_ = tmp.Close()
		if remove {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	remove = false
	return nil
}

func runGit(ctx context.Context, repoPath string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repoPath}, args...)...)
	cmd.Env = append(cleanGitEnv(os.Environ()), "GIT_OPTIONAL_LOCKS=0")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("bounded write Git inspection failed: %w", err)
	}
	return stdout.Bytes(), nil
}

func cleanGitEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, entry := range environ {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || strings.HasPrefix(key, "GIT_CONFIG") {
			continue
		}
		switch key {
		case "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_COMMON_DIR", "GIT_DIR", "GIT_INDEX_FILE", "GIT_NAMESPACE", "GIT_OBJECT_DIRECTORY", "GIT_OPTIONAL_LOCKS", "GIT_WORK_TREE":
			continue
		}
		out = append(out, entry)
	}
	return out
}

func gitAbsolutePath(ctx context.Context, repoPath string, args ...string) (string, error) {
	output, err := runGit(ctx, repoPath, append([]string{"rev-parse", "--path-format=absolute"}, args...)...)
	if err != nil {
		return "", err
	}
	path := strings.TrimSpace(string(output))
	if path == "" {
		return "", errors.New("Git returned an empty path")
	}
	return filepath.Clean(path), nil
}

func digestPath(path string) (string, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "absent", nil
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		return digestBytes([]byte("symlink\x00" + target + "\x00" + info.Mode().String())), nil
	}
	if info.IsDir() {
		return digestBytes([]byte("directory\x00" + info.Mode().String())), nil
	}
	if !info.Mode().IsRegular() {
		return digestBytes([]byte("special\x00" + info.Mode().String() + "\x00" + fmt.Sprint(info.Size()))), nil
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	_, _ = io.WriteString(hash, info.Mode().String())
	_, _ = io.WriteString(hash, "\x00")
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func fingerprintEntries(entries map[string]string) string {
	keys := make([]string, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	hash := sha256.New()
	for _, key := range keys {
		_, _ = io.WriteString(hash, fmt.Sprintf("%d:%s%d:%s", len(key), key, len(entries[key]), entries[key]))
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func pathAllowed(rel string, allowed []allowedPath) bool {
	rel = filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
	for _, scope := range allowed {
		if rel == scope.Relative || (scope.Directory && strings.HasPrefix(rel, strings.TrimSuffix(scope.Relative, "/")+"/")) {
			return true
		}
	}
	return false
}

func reservedProjectStatePath(rel string) bool {
	rel = filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
	return rel == ".loopcoder" || strings.HasPrefix(rel, ".loopcoder/")
}

func reservedGitStatePath(rel string) bool {
	rel = filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
	return rel == ".git" || strings.HasPrefix(rel, ".git/")
}

func physicalPathWithin(parent, child string) bool {
	parentID, err := pathid.Identity(parent)
	if err != nil {
		return false
	}
	childID, err := pathid.Identity(child)
	return err == nil && pathWithin(parentID, childID)
}

func pathWithin(parent, child string) bool {
	rel, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(child))
	return err == nil && !filepath.IsAbs(rel) && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
}

func valueOrAbsent(values map[string]string, key string) string {
	if value, ok := values[key]; ok {
		return value
	}
	return "absent"
}

func pathViolation(code, surface, rel, before, after string) Violation {
	return Violation{Code: code, Surface: surface, TargetID: digestBytes([]byte(rel)), BeforeHash: before, AfterHash: after}
}

func simpleViolation(code, surface, target, before, after string) Violation {
	return Violation{Code: code, Surface: surface, TargetID: digestBytes([]byte(target)), BeforeHash: before, AfterHash: after}
}

func appendBoundedViolation(violations []Violation, violation Violation) []Violation {
	if len(violations) < maxManifestViolations {
		return append(violations, violation)
	}
	if len(violations) == maxManifestViolations {
		return append(violations, simpleViolation("additional_policy_violations_omitted", "summary", "violation-limit", "expected", "omitted"))
	}
	return violations
}

func inconclusiveAudit(phase string) Audit {
	return Audit{Mode: EnforcementMode, Verification: VerificationInconclusive, Changes: []Change{}, Violations: []Violation{simpleViolation("verification_inconclusive", "verification", phase, "unknown", "unknown")}}
}

func policyError(phase string, audit Audit, reason string) error {
	return &PolicyViolationError{Phase: phase, Violations: append([]Violation(nil), audit.Violations...), Reason: reason}
}

func digestBytes(data []byte) string { return "sha256:" + digestHex(data) }

func digestHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func validDigest(value string) bool {
	if len(value) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

func validObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
