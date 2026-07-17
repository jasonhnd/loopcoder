// Package readonlyexec verifies that a read-only child leaves repository and
// registered project state unchanged across a provider invocation.
package readonlyexec

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

	"github.com/jasonhnd/loopcoder/internal/pathid"
)

const (
	RecordSchemaVersion = "loopcoder.read_only_enforcement.v1"
	EnforcementMode     = "provider-read-only+repository-state-v1"

	VerificationPassed       = "passed"
	VerificationViolation    = "policy-violation"
	VerificationInconclusive = "inconclusive"

	recordStatusBaseline     = "baseline-captured"
	recordStatusVerified     = "verified"
	recordStatusViolation    = "policy-violation"
	recordStatusInconclusive = "inconclusive"
	maxPublicViolations      = 256
)

// Options defines the durable evidence and state roots guarded around one
// provider invocation. EvidencePath must live outside every guarded path.
type Options struct {
	RepoPath            string
	EvidencePath        string
	ContractFingerprint string
	ClaimGeneration     int64
	ProjectStatePaths   []string
	ExcludedPaths       []string
}

// Audit is safe for public CLI output: it contains only stable fingerprints,
// bounded codes, and hashed target identities. Raw paths remain local in the
// durable evidence record.
type Audit struct {
	Mode                string      `json:"mode"`
	Verification        string      `json:"verification"`
	BaselineFingerprint string      `json:"baseline_fingerprint"`
	PostRunFingerprint  string      `json:"post_run_fingerprint,omitempty"`
	Recovered           bool        `json:"recovered_after_interruption"`
	Violations          []Violation `json:"violations"`
}

// Violation identifies a changed authority surface without exposing a local
// path or file contents.
type Violation struct {
	Code       string `json:"code"`
	Surface    string `json:"surface"`
	TargetID   string `json:"target_id"`
	BeforeHash string `json:"before_hash"`
	AfterHash  string `json:"after_hash"`
}

// PolicyViolationError is a typed fail-closed result consumed by the nested
// scheduler. It intentionally exposes no raw local path.
type PolicyViolationError struct {
	Phase      string
	Violations []Violation
	Reason     string
}

func (e *PolicyViolationError) Error() string {
	if e == nil {
		return "read-only child policy violation"
	}
	reason := strings.TrimSpace(e.Reason)
	if reason == "" {
		reason = "repository or project state changed"
	}
	if len(e.Violations) == 0 {
		return "read-only child policy violation: " + reason
	}
	return fmt.Sprintf("read-only child policy violation: %s (%d changed surface(s))", reason, len(e.Violations))
}

// ChildExecutionPolicyViolation marks this error for scheduler-level
// needs-human handling without importing this package into orchestration.
func (*PolicyViolationError) ChildExecutionPolicyViolation() {}

// Snapshot is persisted locally for crash recovery. Entries contain only
// relative repository names and digests; no file contents are stored.
type Snapshot struct {
	Fingerprint string            `json:"fingerprint"`
	Entries     map[string]string `json:"entries"`
}

type evidenceRecord struct {
	SchemaVersion       string      `json:"schema_version"`
	Mode                string      `json:"mode"`
	ContractFingerprint string      `json:"contract_fingerprint"`
	ClaimGeneration     int64       `json:"claim_generation"`
	Status              string      `json:"status"`
	Outcome             string      `json:"outcome,omitempty"`
	Recovered           bool        `json:"recovered_after_interruption"`
	Baseline            Snapshot    `json:"baseline"`
	PostRun             *Snapshot   `json:"post_run,omitempty"`
	Violations          []Violation `json:"violations"`
}

// Session owns one persisted pre-run baseline.
type Session struct {
	opts      Options
	baseline  Snapshot
	recovered bool
}

// Begin captures and persists the pre-run baseline. An interrupted prior
// baseline is verified before a new provider may launch.
func Begin(ctx context.Context, opts Options) (*Session, Audit, error) {
	opts = normalizeOptions(opts)
	if err := validateOptions(opts); err != nil {
		audit := inconclusiveAudit()
		return nil, audit, &PolicyViolationError{Phase: "pre-launch", Violations: audit.Violations, Reason: "read-only enforcement could not be prepared"}
	}

	recovered := false
	var baseline Snapshot
	baselineCaptured := false
	if prior, ok, err := loadRecord(opts.EvidencePath); err != nil {
		audit := inconclusiveAudit()
		return nil, audit, &PolicyViolationError{Phase: "crash-recovery", Violations: audit.Violations, Reason: "prior enforcement evidence could not be verified"}
	} else if ok && !validEvidenceRecord(opts, prior) {
		audit := Audit{Mode: EnforcementMode, Verification: VerificationInconclusive, Recovered: true, Violations: []Violation{contractEvidenceViolation()}}
		return nil, audit, &PolicyViolationError{Phase: "crash-recovery", Violations: audit.Violations, Reason: "prior enforcement evidence failed integrity validation"}
	} else if ok && prior.Status == recordStatusVerified {
		audit := auditFromRecord(prior)
		audit.Verification = VerificationInconclusive
		audit.Violations = append(audit.Violations, completedEvidenceViolation())
		return nil, audit, &PolicyViolationError{Phase: "crash-recovery", Violations: audit.Violations, Reason: "a verified provider execution already exists and must be reconciled instead of relaunched"}
	} else if ok && (prior.Status == recordStatusViolation || prior.Status == recordStatusInconclusive) {
		audit := auditFromRecord(prior)
		return nil, audit, &PolicyViolationError{Phase: "crash-recovery", Violations: audit.Violations, Reason: "a prior policy violation or inconclusive verification requires human review"}
	} else if ok && prior.Status == recordStatusBaseline {
		recovered = true
		current, captureErr := Capture(ctx, opts)
		if captureErr != nil {
			audit := Audit{Mode: EnforcementMode, Verification: VerificationInconclusive, BaselineFingerprint: prior.Baseline.Fingerprint, Recovered: true, Violations: []Violation{inconclusiveViolation("crash-recovery")}}
			return nil, audit, &PolicyViolationError{Phase: "crash-recovery", Violations: audit.Violations, Reason: "interrupted baseline could not be verified"}
		}
		violations := Compare(prior.Baseline, current)
		if len(violations) > 0 {
			audit := Audit{Mode: EnforcementMode, Verification: VerificationViolation, BaselineFingerprint: prior.Baseline.Fingerprint, PostRunFingerprint: current.Fingerprint, Recovered: true, Violations: violations}
			prior.Status = recordStatusViolation
			prior.Recovered = true
			prior.PostRun = &current
			prior.Violations = violations
			_ = writeRecord(opts.EvidencePath, prior)
			return nil, audit, &PolicyViolationError{Phase: "crash-recovery", Violations: violations, Reason: "state changed after an interrupted provider launch"}
		}
		baseline = current
		baselineCaptured = true
	} else if ok {
		audit := Audit{
			Mode:         EnforcementMode,
			Verification: VerificationInconclusive,
			Recovered:    true,
			Violations:   []Violation{contractEvidenceViolation()},
		}
		return nil, audit, &PolicyViolationError{Phase: "crash-recovery", Violations: audit.Violations, Reason: "prior enforcement evidence has an unsupported lifecycle state"}
	}

	var captureErr error
	if !baselineCaptured {
		baseline, captureErr = Capture(ctx, opts)
	}
	if captureErr != nil {
		audit := inconclusiveAudit()
		record := evidenceRecord{
			SchemaVersion: RecordSchemaVersion, Mode: EnforcementMode,
			ContractFingerprint: opts.ContractFingerprint, ClaimGeneration: opts.ClaimGeneration,
			Status: recordStatusInconclusive, Recovered: recovered,
			Baseline: Snapshot{Entries: map[string]string{}}, Violations: audit.Violations,
		}
		_ = writeRecord(opts.EvidencePath, record)
		return nil, audit, &PolicyViolationError{Phase: "pre-launch", Violations: audit.Violations, Reason: "the pre-run baseline could not be captured"}
	}
	record := evidenceRecord{
		SchemaVersion:       RecordSchemaVersion,
		Mode:                EnforcementMode,
		ContractFingerprint: opts.ContractFingerprint,
		ClaimGeneration:     opts.ClaimGeneration,
		Status:              recordStatusBaseline,
		Recovered:           recovered,
		Baseline:            baseline,
		Violations:          []Violation{},
	}
	if err := writeRecord(opts.EvidencePath, record); err != nil {
		audit := inconclusiveAudit()
		return nil, audit, &PolicyViolationError{Phase: "pre-launch", Violations: audit.Violations, Reason: "the pre-run baseline could not be persisted"}
	}
	audit := Audit{Mode: EnforcementMode, Verification: "baseline-captured", BaselineFingerprint: baseline.Fingerprint, Recovered: recovered, Violations: []Violation{}}
	return &Session{opts: opts, baseline: baseline, recovered: recovered}, audit, nil
}

// Finish captures the post-run state and closes durable evidence. It must be
// called after every provider result, including cancellation and failure.
func (s *Session) Finish(ctx context.Context, outcome string) (Audit, error) {
	if s == nil {
		audit := inconclusiveAudit()
		return audit, &PolicyViolationError{Phase: "post-run", Violations: audit.Violations, Reason: "read-only enforcement session is missing"}
	}
	if err := s.verifyEvidenceFence(); err != nil {
		audit := Audit{Mode: EnforcementMode, Verification: VerificationViolation, BaselineFingerprint: s.baseline.Fingerprint, Recovered: s.recovered, Violations: []Violation{evidenceFenceViolation()}}
		return audit, &PolicyViolationError{Phase: "post-run", Violations: audit.Violations, Reason: "durable enforcement evidence was changed during provider execution"}
	}
	post, err := Capture(ctx, s.opts)
	if err != nil {
		audit := Audit{Mode: EnforcementMode, Verification: VerificationInconclusive, BaselineFingerprint: s.baseline.Fingerprint, Recovered: s.recovered, Violations: []Violation{inconclusiveViolation("post-run")}}
		record := s.record(recordStatusInconclusive, outcome, nil, audit.Violations)
		_ = writeRecord(s.opts.EvidencePath, record)
		return audit, &PolicyViolationError{Phase: "post-run", Violations: audit.Violations, Reason: "post-run state could not be verified"}
	}
	violations := Compare(s.baseline, post)
	verification := VerificationPassed
	status := recordStatusVerified
	if len(violations) > 0 {
		verification = VerificationViolation
		status = recordStatusViolation
	}
	audit := Audit{
		Mode:                EnforcementMode,
		Verification:        verification,
		BaselineFingerprint: s.baseline.Fingerprint,
		PostRunFingerprint:  post.Fingerprint,
		Recovered:           s.recovered,
		Violations:          violations,
	}
	record := s.record(status, outcome, &post, violations)
	if err := writeRecord(s.opts.EvidencePath, record); err != nil {
		audit.Verification = VerificationInconclusive
		audit.Violations = append(audit.Violations, inconclusiveViolation("evidence-write"))
		return audit, &PolicyViolationError{Phase: "post-run", Violations: audit.Violations, Reason: "final enforcement evidence could not be persisted"}
	}
	if len(violations) > 0 {
		return audit, &PolicyViolationError{Phase: "post-run", Violations: violations, Reason: "repository or project state changed during provider execution"}
	}
	return audit, nil
}

func (s *Session) record(status, outcome string, post *Snapshot, violations []Violation) evidenceRecord {
	return evidenceRecord{
		SchemaVersion:       RecordSchemaVersion,
		Mode:                EnforcementMode,
		ContractFingerprint: s.opts.ContractFingerprint,
		ClaimGeneration:     s.opts.ClaimGeneration,
		Status:              status,
		Outcome:             strings.TrimSpace(outcome),
		Recovered:           s.recovered,
		Baseline:            s.baseline,
		PostRun:             post,
		Violations:          append([]Violation{}, violations...),
	}
}

func (s *Session) verifyEvidenceFence() error {
	record, ok, err := loadRecord(s.opts.EvidencePath)
	if err != nil || !ok {
		return errors.New("enforcement evidence missing")
	}
	if record.SchemaVersion != RecordSchemaVersion || record.Mode != EnforcementMode || record.Status != recordStatusBaseline || record.ContractFingerprint != s.opts.ContractFingerprint || record.ClaimGeneration != s.opts.ClaimGeneration || record.Baseline.Fingerprint != s.baseline.Fingerprint || fingerprintEntries(record.Baseline.Entries) != s.baseline.Fingerprint {
		return errors.New("enforcement evidence fence mismatch")
	}
	return nil
}

// Capture records repository, Git authority, other-worktree, and project-state
// fingerprints without invoking any mutating Git command.
func Capture(ctx context.Context, opts Options) (Snapshot, error) {
	opts = normalizeOptions(opts)
	entries := map[string]string{}
	worktreeOutput, err := runGit(ctx, opts.RepoPath, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return Snapshot{}, err
	}
	entries["git:worktree-list"] = digestBytes(worktreeOutput)
	worktrees, err := parseWorktrees(worktreeOutput)
	if err != nil {
		return Snapshot{}, err
	}
	primaryIdentity, err := canonicalIdentity(opts.RepoPath)
	if err != nil {
		return Snapshot{}, err
	}
	for _, worktree := range worktrees {
		identity, err := canonicalIdentity(worktree)
		if err != nil {
			return Snapshot{}, err
		}
		id := "external-" + shortDigest(identity)
		if identity == primaryIdentity {
			id = "primary"
		}
		if err := captureWorktree(ctx, entries, id, worktree, opts.ExcludedPaths); err != nil {
			return Snapshot{}, err
		}
	}
	if len(worktrees) == 0 {
		return Snapshot{}, errors.New("git worktree inventory is empty")
	}

	refs, err := runGit(ctx, opts.RepoPath, "for-each-ref", "--format=%(refname)%00%(objectname)%00%(symref)")
	if err != nil {
		return Snapshot{}, err
	}
	entries["git:refs"] = digestBytes(refs)
	config, err := runGit(ctx, opts.RepoPath, "config", "--local", "--null", "--list", "--show-origin")
	if err != nil {
		return Snapshot{}, err
	}
	entries["git:config"] = digestBytes(config)
	commonDir, err := gitAbsolutePath(ctx, opts.RepoPath, "--git-common-dir")
	if err != nil {
		return Snapshot{}, err
	}
	if err := capturePath(entries, "git:hooks", filepath.Join(commonDir, "hooks")); err != nil {
		return Snapshot{}, err
	}
	if err := capturePath(entries, "git:worktree-metadata", filepath.Join(commonDir, "worktrees")); err != nil {
		return Snapshot{}, err
	}
	for _, name := range []string{"HEAD", "config", "config.worktree", "packed-refs"} {
		if err := capturePath(entries, "git:authority:"+name, filepath.Join(commonDir, name)); err != nil {
			return Snapshot{}, err
		}
	}
	for index, path := range opts.ProjectStatePaths {
		if err := capturePath(entries, fmt.Sprintf("project-state:%d", index), path, opts.ExcludedPaths...); err != nil {
			return Snapshot{}, err
		}
	}
	entries["policy:excluded-paths"] = digestBytes([]byte(strings.Join(opts.ExcludedPaths, "\x00")))
	snapshot := Snapshot{Entries: entries}
	snapshot.Fingerprint = fingerprintEntries(entries)
	return snapshot, nil
}

func captureWorktree(ctx context.Context, entries map[string]string, id, worktree string, excludedPaths []string) error {
	prefix := "worktree:" + id
	status, err := runGit(ctx, worktree, "status", "--porcelain=v2", "-z", "--untracked-files=no")
	if err != nil {
		return err
	}
	entries[prefix+":status"] = digestBytes(status)
	index, err := runGit(ctx, worktree, "ls-files", "--stage", "-z")
	if err != nil {
		return err
	}
	entries[prefix+":index-entries"] = digestBytes(index)
	indexPath, err := gitAbsolutePath(ctx, worktree, "--git-path", "index")
	if err != nil {
		return err
	}
	if err := capturePath(entries, prefix+":index-file", indexPath); err != nil {
		return err
	}

	trackedOutput, err := runGit(ctx, worktree, "ls-files", "-z")
	if err != nil {
		return err
	}
	tracked := map[string]bool{}
	for _, name := range splitNUL(trackedOutput) {
		tracked[name] = true
	}
	untrackedOutput, err := runGit(ctx, worktree, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return err
	}
	untracked := map[string]bool{}
	for _, name := range splitNUL(untrackedOutput) {
		untracked[name] = true
	}
	ignoredOutput, err := runGit(ctx, worktree, "ls-files", "--others", "--ignored", "--exclude-standard", "-z")
	if err != nil {
		return err
	}
	ignored := map[string]bool{}
	for _, name := range splitNUL(ignoredOutput) {
		ignored[name] = true
	}
	for name := range tracked {
		if pathExcluded(filepath.Join(worktree, filepath.FromSlash(name)), excludedPaths) {
			continue
		}
		if err := captureRelativeFile(entries, prefix+":tracked", worktree, name); err != nil {
			return err
		}
	}
	for name := range untracked {
		if pathExcluded(filepath.Join(worktree, filepath.FromSlash(name)), excludedPaths) {
			continue
		}
		if err := captureRelativeFile(entries, prefix+":untracked", worktree, name); err != nil {
			return err
		}
	}
	for name := range ignored {
		if pathExcluded(filepath.Join(worktree, filepath.FromSlash(name)), excludedPaths) {
			continue
		}
		if err := captureRelativeFile(entries, prefix+":ignored", worktree, name); err != nil {
			return err
		}
	}
	return nil
}

func captureRelativeFile(entries map[string]string, prefix, root, name string) error {
	name = filepath.Clean(filepath.FromSlash(name))
	if name == "." || filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
		return errors.New("git returned an unsafe worktree path")
	}
	digest, err := digestPath(filepath.Join(root, name))
	if err != nil {
		return err
	}
	entries[prefix+":"+filepath.ToSlash(name)] = digest
	return nil
}

func capturePath(entries map[string]string, prefix, path string, excludedPaths ...string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	if pathExcluded(path, excludedPaths) {
		entries[prefix] = "excluded"
		return nil
	}
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		entries[prefix] = "absent"
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		digest, err := digestPath(path)
		if err != nil {
			return err
		}
		entries[prefix] = digest
		return nil
	}
	entries[prefix] = digestMetadata(info)
	return filepath.WalkDir(path, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if current == path {
			return nil
		}
		if pathExcluded(current, excludedPaths) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(path, current)
		if err != nil {
			return err
		}
		digest, err := digestPath(current)
		if err != nil {
			return err
		}
		entries[prefix+":"+filepath.ToSlash(rel)] = digest
		return nil
	})
}

// Compare returns stable, path-free violations for every changed entry.
func Compare(before, after Snapshot) []Violation {
	keys := map[string]bool{}
	for key := range before.Entries {
		keys[key] = true
	}
	for key := range after.Entries {
		keys[key] = true
	}
	ordered := make([]string, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	violations := make([]Violation, 0)
	omitted := 0
	for _, key := range ordered {
		left := valueOrAbsent(before.Entries, key)
		right := valueOrAbsent(after.Entries, key)
		if left == right {
			continue
		}
		code, surface := classifyEntry(key, left, right)
		if len(violations) >= maxPublicViolations {
			omitted++
			continue
		}
		violations = append(violations, Violation{
			Code:       code,
			Surface:    surface,
			TargetID:   "sha256:" + digestHex([]byte(key)),
			BeforeHash: left,
			AfterHash:  right,
		})
	}
	if omitted > 0 {
		violations = append(violations, Violation{
			Code: "additional_guarded_changes_omitted", Surface: "summary",
			TargetID:   "sha256:" + digestHex([]byte("additional-guarded-changes")),
			BeforeHash: digestBytes([]byte("count:0")), AfterHash: digestBytes([]byte(fmt.Sprintf("count:%d", omitted))),
		})
	}
	return violations
}

func classifyEntry(key, before, after string) (string, string) {
	switch {
	case strings.HasPrefix(key, "project-state:"):
		return "loopcoder_project_state_modified", "project-state"
	case key == "git:refs" || strings.HasPrefix(key, "git:authority:packed-refs") || strings.HasPrefix(key, "git:authority:HEAD"):
		return "git_refs_modified", "refs"
	case key == "git:config" || strings.Contains(key, ":config"):
		return "git_config_modified", "config"
	case strings.HasPrefix(key, "git:hooks"):
		return "git_hooks_modified", "hooks"
	case key == "git:worktree-list" || strings.HasPrefix(key, "git:worktree-metadata"):
		return "git_worktree_metadata_modified", "worktree-metadata"
	case strings.Contains(key, ":index-"):
		return "git_index_modified", "index"
	case strings.Contains(key, ":external-"):
		return "external_worktree_modified", "external-worktree"
	case strings.Contains(key, ":untracked:") && before == "absent":
		return "untracked_file_created", "checkout"
	case strings.Contains(key, ":untracked:") && after == "absent":
		return "untracked_file_removed", "checkout"
	case strings.Contains(key, ":ignored:") && before == "absent":
		return "ignored_untracked_file_created", "checkout"
	case strings.Contains(key, ":ignored:") && after == "absent":
		return "ignored_untracked_file_removed", "checkout"
	default:
		return "checkout_state_modified", "checkout"
	}
}

func normalizeOptions(opts Options) Options {
	opts.RepoPath = normalizePhysicalPath(opts.RepoPath)
	opts.EvidencePath = normalizePhysicalPath(opts.EvidencePath)
	opts.ContractFingerprint = strings.TrimSpace(opts.ContractFingerprint)
	cleaned := make([]string, 0, len(opts.ProjectStatePaths))
	seen := map[string]bool{}
	for _, path := range opts.ProjectStatePaths {
		path = normalizePhysicalPath(path)
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		cleaned = append(cleaned, path)
	}
	sort.Strings(cleaned)
	opts.ProjectStatePaths = cleaned
	excluded := make([]string, 0, len(opts.ExcludedPaths))
	seen = map[string]bool{}
	for _, path := range opts.ExcludedPaths {
		path = normalizePhysicalPath(path)
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		excluded = append(excluded, path)
	}
	sort.Strings(excluded)
	opts.ExcludedPaths = excluded
	return opts
}

func normalizePhysicalPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	canonical, err := pathid.Canonicalize(path)
	if err == nil && strings.TrimSpace(canonical.Identity) != "" {
		return filepath.Clean(canonical.Identity)
	}
	return filepath.Clean(path)
}

func validateOptions(opts Options) error {
	if opts.RepoPath == "." || opts.RepoPath == "" || opts.EvidencePath == "." || opts.EvidencePath == "" || opts.ContractFingerprint == "" || opts.ClaimGeneration <= 0 {
		return errors.New("repo, evidence, immutable contract fingerprint, and positive claim generation are required")
	}
	evidence, err := pathid.Canonicalize(opts.EvidencePath)
	if err != nil {
		return err
	}
	guarded := append([]string{opts.RepoPath}, opts.ProjectStatePaths...)
	for _, root := range guarded {
		rootPath, err := pathid.Canonicalize(root)
		if err != nil {
			return err
		}
		if pathWithin(rootPath.Identity, evidence.Identity) {
			return errors.New("evidence path must be outside guarded state")
		}
	}
	return nil
}

func parseWorktrees(data []byte) ([]string, error) {
	fields := bytes.Split(data, []byte{0})
	worktrees := make([]string, 0)
	for _, field := range fields {
		line := string(field)
		if !strings.HasPrefix(line, "worktree ") {
			continue
		}
		path := strings.TrimPrefix(line, "worktree ")
		if strings.TrimSpace(path) == "" {
			return nil, errors.New("git returned an empty worktree path")
		}
		worktrees = append(worktrees, path)
	}
	return worktrees, nil
}

func runGit(ctx context.Context, repoPath string, args ...string) ([]byte, error) {
	allArgs := append([]string{"-C", repoPath}, args...)
	cmd := exec.CommandContext(ctx, "git", allArgs...)
	cmd.Env = append(cleanGitEnv(os.Environ()), "GIT_OPTIONAL_LOCKS=0")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("read-only git inspection %q failed: %w", strings.Join(args, " "), err)
	}
	return stdout.Bytes(), nil
}

func cleanGitEnv(environ []string) []string {
	blocked := map[string]bool{
		"GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
		"GIT_COMMON_DIR":                   true,
		"GIT_DIR":                          true,
		"GIT_INDEX_FILE":                   true,
		"GIT_NAMESPACE":                    true,
		"GIT_OBJECT_DIRECTORY":             true,
		"GIT_WORK_TREE":                    true,
		"GIT_OPTIONAL_LOCKS":               true,
	}
	out := make([]string, 0, len(environ))
	for _, entry := range environ {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || blocked[key] || strings.HasPrefix(key, "GIT_CONFIG") {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func gitAbsolutePath(ctx context.Context, repoPath string, args ...string) (string, error) {
	allArgs := append([]string{"rev-parse", "--path-format=absolute"}, args...)
	output, err := runGit(ctx, repoPath, allArgs...)
	if err != nil {
		return "", err
	}
	path := strings.TrimSpace(string(output))
	if path == "" {
		return "", errors.New("git returned an empty authority path")
	}
	return filepath.Clean(path), nil
}

func canonicalIdentity(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

func digestPath(path string) (string, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
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
		return digestMetadata(info), nil
	}
	if !info.Mode().IsRegular() {
		return digestBytes([]byte("special\x00" + info.Mode().String() + "\x00" + fmt.Sprintf("%d", info.Size()))), nil
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	h := sha256.New()
	_, _ = io.WriteString(h, info.Mode().String())
	_, _ = io.WriteString(h, "\x00")
	if _, err := io.Copy(h, file); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func digestMetadata(info fs.FileInfo) string {
	return digestBytes([]byte(info.Mode().String()))
}

func digestBytes(data []byte) string {
	return "sha256:" + digestHex(data)
}

func digestHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func shortDigest(value string) string {
	return digestHex([]byte(value))[:16]
}

func fingerprintEntries(entries map[string]string) string {
	keys := make([]string, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, key := range keys {
		_, _ = io.WriteString(h, fmt.Sprintf("%d:%s%d:%s", len(key), key, len(entries[key]), entries[key]))
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func splitNUL(data []byte) []string {
	parts := bytes.Split(data, []byte{0})
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if len(part) > 0 {
			out = append(out, string(part))
		}
	}
	return out
}

func valueOrAbsent(values map[string]string, key string) string {
	if value, ok := values[key]; ok {
		return value
	}
	return "absent"
}

func loadRecord(path string) (evidenceRecord, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
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

func validEvidenceRecord(opts Options, record evidenceRecord) bool {
	if record.SchemaVersion != RecordSchemaVersion || record.Mode != EnforcementMode || record.ContractFingerprint != opts.ContractFingerprint || record.ClaimGeneration != opts.ClaimGeneration {
		return false
	}
	switch record.Status {
	case recordStatusBaseline, recordStatusVerified, recordStatusViolation, recordStatusInconclusive:
	default:
		return false
	}
	if !validSnapshot(record.Baseline) {
		return false
	}
	if record.PostRun != nil && !validSnapshot(*record.PostRun) {
		return false
	}
	if (record.Status == recordStatusVerified || record.Status == recordStatusViolation) && record.PostRun == nil {
		return false
	}
	if (record.Status == recordStatusBaseline || record.Status == recordStatusVerified) && len(record.Violations) != 0 {
		return false
	}
	if (record.Status == recordStatusViolation || record.Status == recordStatusInconclusive) && len(record.Violations) == 0 {
		return false
	}
	if len(record.Violations) > maxPublicViolations+1 {
		return false
	}
	for _, violation := range record.Violations {
		if !validAuditToken(violation.Code) || !validAuditToken(violation.Surface) || !validSHA256Digest(violation.TargetID) || !validViolationHash(violation.BeforeHash) || !validViolationHash(violation.AfterHash) {
			return false
		}
	}
	return true
}

func validSnapshot(snapshot Snapshot) bool {
	if !validSHA256Digest(snapshot.Fingerprint) || fingerprintEntries(snapshot.Entries) != snapshot.Fingerprint {
		return false
	}
	for _, value := range snapshot.Entries {
		if value != "absent" && value != "excluded" && !validSHA256Digest(value) {
			return false
		}
	}
	return true
}

func validSHA256Digest(value string) bool {
	if len(value) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

func validAuditToken(value string) bool {
	if value == "" || len(value) > 96 {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '-' || char == '_' {
			continue
		}
		return false
	}
	return true
}

func validViolationHash(value string) bool {
	if validSHA256Digest(value) {
		return true
	}
	switch value {
	case "absent", "excluded", "unknown", "expected", "mismatch", "verified", "relaunch-refused":
		return true
	default:
		return false
	}
}

func writeRecord(path string, record evidenceRecord) error {
	if record.Violations == nil {
		record.Violations = []Violation{}
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".read-only-enforcement-*.tmp")
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

func contractEvidenceViolation() Violation {
	return Violation{Code: "execution_contract_evidence_mismatch", Surface: "audit-state", TargetID: "sha256:" + digestHex([]byte("contract-evidence")), BeforeHash: "unknown", AfterHash: "mismatch"}
}

func evidenceFenceViolation() Violation {
	return Violation{Code: "enforcement_evidence_modified", Surface: "audit-state", TargetID: "sha256:" + digestHex([]byte("evidence-fence")), BeforeHash: "expected", AfterHash: "mismatch"}
}

func completedEvidenceViolation() Violation {
	return Violation{Code: "verified_execution_requires_reconciliation", Surface: "audit-state", TargetID: "sha256:" + digestHex([]byte("verified-execution")), BeforeHash: "verified", AfterHash: "relaunch-refused"}
}

func inconclusiveViolation(phase string) Violation {
	return Violation{Code: "verification_inconclusive", Surface: "verification", TargetID: "sha256:" + digestHex([]byte(phase)), BeforeHash: "unknown", AfterHash: "unknown"}
}

func inconclusiveAudit() Audit {
	return Audit{Mode: EnforcementMode, Verification: VerificationInconclusive, Violations: []Violation{inconclusiveViolation("pre-run")}}
}

func auditFromRecord(record evidenceRecord) Audit {
	verification := VerificationInconclusive
	if record.Status == recordStatusViolation {
		verification = VerificationViolation
	} else if record.Status == recordStatusVerified {
		verification = VerificationPassed
	}
	audit := Audit{
		Mode:                firstNonEmpty(record.Mode, EnforcementMode),
		Verification:        verification,
		BaselineFingerprint: record.Baseline.Fingerprint,
		Recovered:           record.Recovered,
		Violations:          append([]Violation(nil), record.Violations...),
	}
	if record.PostRun != nil {
		audit.PostRunFingerprint = record.PostRun.Fingerprint
	}
	return audit
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func pathWithin(parent, child string) bool {
	rel, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(child))
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func pathExcluded(path string, exclusions []string) bool {
	path = filepath.Clean(path)
	for _, exclusion := range exclusions {
		exclusion = filepath.Clean(exclusion)
		if path == exclusion || pathWithin(exclusion, path) {
			return true
		}
	}
	return false
}
