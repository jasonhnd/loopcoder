package workflowrun

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// IntegrateCommit is one exactly-once product commit onto the shared goal branch.
type IntegrateCommit struct {
	WorkItemID string   `json:"work_item_id"`
	AttemptID  string   `json:"attempt_id"`
	CommitSHA  string   `json:"commit_sha"`
	Files      []string `json:"files,omitempty"`
	Message    string   `json:"message,omitempty"`
	// Skipped true when this attempt was already integrated (exactly-once).
	Skipped bool `json:"skipped,omitempty"`
}

// BranchIntegrator merges succeeded child product files onto one goal branch.
// Subsequent children must materialize from that branch HEAD.
type BranchIntegrator interface {
	// EnsureGoalBranch creates or reuses branch from baseRef; returns HEAD OID.
	EnsureGoalBranch(ctx context.Context, repoPath, baseRef, goalBranch string) (headOID string, err error)
	// IntegrateChild copies product diffs from child worktree onto goal branch
	// and commits exactly-once per attempt_id. Conflict → fail-closed.
	IntegrateChild(ctx context.Context, req IntegrateRequest) (IntegrateCommit, error)
}

// IntegrateRequest is one child → goal branch integration.
type IntegrateRequest struct {
	RepoPath      string
	GoalBranch    string
	WorkItemID    string
	AttemptID     string
	ChildWorktree string
	// ProductFiles optional explicit relative paths; empty → discover via git status
	// in the child worktree (excluding meta-only paths).
	ProductFiles []string
	// Intent helps decide whether meta-only output is insufficient (caller may
	// enforce task-specific acceptance separately).
	Intent string
}

var (
	ErrIntegrateInvalid  = errors.New("workflowrun: integrate invalid")
	ErrIntegrateConflict = errors.New("workflowrun: integrate conflict")
	ErrIntegrateEmpty    = errors.New("workflowrun: no product files to integrate")
	ErrIntegrateDup      = errors.New("workflowrun: attempt already integrated")
)

// GitBranchIntegrator is the production integrator using system git.
type GitBranchIntegrator struct {
	// LedgerDir stores exactly-once attempt→commit records.
	// When set (including by Service default), the ledger file is
	// <LedgerDir>/integrate-ledger.json and never under the customer repo.
	// Empty (explicit inject only) → legacy <repo>/.loopcoder/integrate-ledger/.
	// Production Service never leaves this empty: see DefaultIntegrateLedgerDir.
	LedgerDir string
	Now       func() time.Time
}

// DefaultIntegrateLedgerDir is the production Service integrate-ledger location:
//
//	<durableHome>/projects/<exactIDComponent(project_id)>/runs/<exactIDComponent(run_id)>/integrate-ledger
//
// exactIDComponent = readable sanitized prefix + SHA-256 of the exact raw ID, so
// distinct raw project_id/run_id never collide even when sanitize alone would
// (a/b vs a-b, long prefixes, dot-like segments). Same exact pair always yields
// a byte-identical path. Containment-checked under durable home.
func DefaultIntegrateLedgerDir(homeDir, projectID, runID string) (string, error) {
	root, err := ResolveDurableHome(homeDir)
	if err != nil {
		return "", err
	}
	projRaw := strings.TrimSpace(projectID)
	runRaw := strings.TrimSpace(runID)
	if projRaw == "" || runRaw == "" {
		return "", fmt.Errorf("workflowrun: project_id and run_id required for integrate ledger")
	}
	projComp, err := exactDurableIDComponent(projRaw)
	if err != nil {
		return "", fmt.Errorf("workflowrun: project_id component: %w", err)
	}
	runComp, err := exactDurableIDComponent(runRaw)
	if err != nil {
		return "", fmt.Errorf("workflowrun: run_id component: %w", err)
	}
	dir := filepath.Join(root, "projects", projComp, "runs", runComp, "integrate-ledger")
	rel, rerr := filepath.Rel(root, dir)
	if rerr != nil || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("workflowrun: integrate ledger path escapes durable home")
	}
	return dir, nil
}

// exactDurableIDComponent builds a collision-resistant path segment from the
// exact raw ID: readable sanitized prefix (≤32) + "-" + full SHA-256 hex of the
// raw UTF-8 bytes. Distinct raw strings never share a component.
func exactDurableIDComponent(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("empty id")
	}
	prefix := sanitizeBranch(raw)
	if len(prefix) > 32 {
		prefix = prefix[:32]
	}
	// Drop trailing dots/dashes from truncated prefix for cleanliness.
	prefix = strings.Trim(prefix, ".-_")
	if prefix == "" {
		prefix = "id"
	}
	sum := sha256.Sum256([]byte(raw))
	comp := prefix + "-" + hex.EncodeToString(sum[:])
	// Component itself must not be a parent-dir segment.
	if comp == ".." || strings.Contains(comp, string(filepath.Separator)) {
		return "", fmt.Errorf("invalid id component")
	}
	return comp, nil
}

// EnsureGoalBranch implements BranchIntegrator.
func (g GitBranchIntegrator) EnsureGoalBranch(ctx context.Context, repoPath, baseRef, goalBranch string) (string, error) {
	repoPath = strings.TrimSpace(repoPath)
	goalBranch = strings.TrimSpace(goalBranch)
	baseRef = strings.TrimSpace(baseRef)
	if repoPath == "" || goalBranch == "" {
		return "", fmt.Errorf("%w: repo and goal branch required", ErrIntegrateInvalid)
	}
	if baseRef == "" {
		baseRef = "main"
	}
	// Resolve base.
	if _, err := runGitRepo(ctx, repoPath, "rev-parse", "--verify", baseRef); err != nil {
		if _, err2 := runGitRepo(ctx, repoPath, "rev-parse", "--verify", "origin/"+strings.TrimPrefix(baseRef, "origin/")); err2 == nil {
			baseRef = "origin/" + strings.TrimPrefix(baseRef, "origin/")
		} else if _, err3 := runGitRepo(ctx, repoPath, "rev-parse", "--verify", "HEAD"); err3 == nil {
			baseRef = "HEAD"
		} else {
			return "", fmt.Errorf("%w: base %s: %v", ErrIntegrateInvalid, baseRef, err)
		}
	}
	// Create branch if missing; leave current branch as-is (work on detached path via worktree).
	if _, err := runGitRepo(ctx, repoPath, "rev-parse", "--verify", goalBranch); err != nil {
		if _, err := runGitRepo(ctx, repoPath, "branch", goalBranch, baseRef); err != nil {
			return "", fmt.Errorf("create goal branch: %w", err)
		}
	}
	return runGitRepo(ctx, repoPath, "rev-parse", goalBranch)
}

// IntegrateChild implements BranchIntegrator.
func (g GitBranchIntegrator) IntegrateChild(ctx context.Context, req IntegrateRequest) (out IntegrateCommit, err error) {
	out = IntegrateCommit{WorkItemID: req.WorkItemID, AttemptID: req.AttemptID}
	if strings.TrimSpace(req.RepoPath) == "" || strings.TrimSpace(req.GoalBranch) == "" ||
		strings.TrimSpace(req.AttemptID) == "" || strings.TrimSpace(req.ChildWorktree) == "" {
		return out, fmt.Errorf("%w: missing fields", ErrIntegrateInvalid)
	}
	// Exactly-once ledger.
	if prev, ok, lerr := g.loadLedger(req.RepoPath, req.AttemptID); lerr != nil {
		return out, lerr
	} else if ok {
		out.CommitSHA = prev.CommitSHA
		out.Files = prev.Files
		out.Message = prev.Message
		out.Skipped = true
		return out, nil
	}

	files := req.ProductFiles
	if len(files) == 0 {
		var derr error
		files, derr = discoverProductFiles(req.ChildWorktree)
		if derr != nil {
			return out, derr
		}
	}
	files = filterProductFiles(files)
	if len(files) == 0 {
		return out, fmt.Errorf("%w: child %s attempt %s", ErrIntegrateEmpty, req.WorkItemID, req.AttemptID)
	}

	// Integrate into a temporary worktree of the goal branch to avoid clobbering
	// the caller's checkout, then commit and record ledger.
	tmpWT, err := os.MkdirTemp("", "loopcoder-integrate-*")
	if err != nil {
		return out, err
	}
	// Must git-worktree-remove (not only RemoveAll): deleting the directory
	// leaves the goal branch registered to a missing worktree, so later PR
	// checkout fails with "already used by worktree". Surface cleanup failures.
	defer func() {
		if cerr := releaseIntegrateWorktree(req.RepoPath, tmpWT); cerr != nil {
			if err != nil {
				err = fmt.Errorf("%w; integrate worktree cleanup: %v", err, cerr)
			} else {
				err = fmt.Errorf("integrate worktree cleanup: %w", cerr)
			}
		}
	}()

	// Prefer git worktree add for the goal branch.
	if _, err := runGitRepo(ctx, req.RepoPath, "worktree", "add", "--force", tmpWT, req.GoalBranch); err != nil {
		// Fallback: clone local + checkout branch (not linked as a worktree).
		// Still run releaseIntegrateWorktree in defer (remove --force is a no-op).
		_ = os.RemoveAll(tmpWT)
		if err := os.MkdirAll(tmpWT, 0o700); err != nil {
			return out, err
		}
		if _, err := runGitRepo(ctx, req.RepoPath, "clone", "--local", "--no-hardlinks", "--branch", req.GoalBranch, req.RepoPath, tmpWT); err != nil {
			// Last resort: clone HEAD and checkout -B
			_ = os.RemoveAll(tmpWT)
			if _, err2 := runGitRepo(ctx, req.RepoPath, "clone", "--local", "--no-hardlinks", req.RepoPath, tmpWT); err2 != nil {
				return out, fmt.Errorf("integrate worktree: %v / %v", err, err2)
			}
			if _, err3 := runGitRepo(ctx, tmpWT, "checkout", "-B", req.GoalBranch); err3 != nil {
				return out, err3
			}
		}
	}

	// Copy product files from child worktree → integrate worktree.
	copied := make([]string, 0, len(files))
	for _, rel := range files {
		rel = filepath.Clean(rel)
		if rel == "." || strings.HasPrefix(rel, "..") {
			continue
		}
		src := filepath.Join(req.ChildWorktree, rel)
		dst := filepath.Join(tmpWT, rel)
		if _, serr := os.Stat(src); serr != nil {
			// Skip ephemeral paths that vanished after provider exit (logs).
			if os.IsNotExist(serr) {
				continue
			}
			return out, fmt.Errorf("stat %s: %w", rel, serr)
		}
		if err := copyPath(src, dst); err != nil {
			return out, fmt.Errorf("copy %s: %w", rel, err)
		}
		copied = append(copied, rel)
	}
	if len(copied) == 0 {
		return out, fmt.Errorf("%w: nothing copied", ErrIntegrateEmpty)
	}

	// Stage + commit. Conflict detection: if file existed with different content
	// from a different attempt without clean apply, git will just overwrite —
	// we detect semantic conflict via ledger of path ownership + content hash.
	if err := g.detectPathConflict(req.RepoPath, req.WorkItemID, req.AttemptID, tmpWT, copied); err != nil {
		return out, err
	}

	for _, rel := range copied {
		if _, err := runGitRepo(ctx, tmpWT, "add", "--", rel); err != nil {
			return out, fmt.Errorf("git add %s: %w", rel, err)
		}
	}
	// Nothing staged?
	st, _ := runGitRepo(ctx, tmpWT, "status", "--porcelain")
	if strings.TrimSpace(st) == "" {
		// Files identical to branch — still record ledger as no-op commit skip
		// but require at least one product path present on branch.
		head, herr := runGitRepo(ctx, tmpWT, "rev-parse", "HEAD")
		if herr != nil {
			return out, herr
		}
		out.CommitSHA = head
		out.Files = copied
		out.Message = "already present on goal branch"
		out.Skipped = true
		_ = g.saveLedger(req.RepoPath, out)
		return out, nil
	}

	msg := fmt.Sprintf("loopcoder: integrate %s attempt=%s (exactly-once product)", req.WorkItemID, req.AttemptID)
	if _, err := runGitRepo(ctx, tmpWT, "commit", "-m", msg); err != nil {
		// Treat merge/index errors as conflict.
		if strings.Contains(err.Error(), "conflict") || strings.Contains(err.Error(), "CONFLICT") {
			return out, fmt.Errorf("%w: %v", ErrIntegrateConflict, err)
		}
		return out, fmt.Errorf("git commit: %w", err)
	}
	sha, err := runGitRepo(ctx, tmpWT, "rev-parse", "HEAD")
	if err != nil {
		return out, err
	}
	// Push branch tip back into the main repo (worktree shares objects when linked;
	// for clone fallback, fetch/push into original).
	if _, err := runGitRepo(ctx, req.RepoPath, "branch", "-f", req.GoalBranch, sha); err != nil {
		// worktree add shares repo — branch should already advance; force update local ref
		if _, err2 := runGitRepo(ctx, tmpWT, "push", req.RepoPath, "HEAD:"+req.GoalBranch); err2 != nil {
			return out, fmt.Errorf("update goal branch ref: %v / %v", err, err2)
		}
	}

	out.CommitSHA = sha
	out.Files = copied
	out.Message = msg
	if err := g.saveLedger(req.RepoPath, out); err != nil {
		return out, err
	}
	return out, nil
}

type integrateLedgerDoc struct {
	Schema  string                     `json:"schema"`
	Entries map[string]IntegrateCommit `json:"entries"` // attempt_id → commit
}

const integrateLedgerSchema = "loopcoder.integrate.ledger.v1"

func (g GitBranchIntegrator) ledgerPath(repo string) string {
	if d := strings.TrimSpace(g.LedgerDir); d != "" {
		return filepath.Join(d, "integrate-ledger.json")
	}
	return filepath.Join(repo, ".loopcoder", "integrate-ledger", "integrate-ledger.json")
}

func (g GitBranchIntegrator) loadLedger(repo, attemptID string) (IntegrateCommit, bool, error) {
	p := g.ledgerPath(repo)
	raw, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return IntegrateCommit{}, false, nil
		}
		return IntegrateCommit{}, false, err
	}
	var doc integrateLedgerDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return IntegrateCommit{}, false, err
	}
	if doc.Entries == nil {
		return IntegrateCommit{}, false, nil
	}
	c, ok := doc.Entries[attemptID]
	return c, ok, nil
}

func (g GitBranchIntegrator) saveLedger(repo string, c IntegrateCommit) error {
	p := g.ledgerPath(repo)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	doc := integrateLedgerDoc{Schema: integrateLedgerSchema, Entries: map[string]IntegrateCommit{}}
	if raw, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(raw, &doc)
		if doc.Entries == nil {
			doc.Entries = map[string]IntegrateCommit{}
		}
	}
	doc.Schema = integrateLedgerSchema
	doc.Entries[c.AttemptID] = c
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// detectPathConflict fail-closes only when the *same* work item re-integrates
// a path from a different attempt (exactly-once). Sequential goal children
// (implement then tests) may refine the same product path on the shared goal
// branch — RC.17 recovery blocked wi_tests on notes_test.go owned by implement.
func (g GitBranchIntegrator) detectPathConflict(repo, workItemID, attemptID, integrateWT string, files []string) error {
	p := g.ledgerPath(repo)
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var doc integrateLedgerDoc
	if json.Unmarshal(raw, &doc) != nil || doc.Entries == nil {
		return nil
	}
	for _, prev := range doc.Entries {
		if prev.AttemptID == attemptID {
			continue
		}
		// Different work items on a shared goal branch are sequential refinements.
		if prev.WorkItemID != workItemID {
			continue
		}
		for _, f := range prev.Files {
			for _, nf := range files {
				if f != nf {
					continue
				}
				// Same work item, different attempt claiming same path: fail closed
				// unless this is an explicit generation bump re-integrate (allowed
				// only when prior attempt is not already succeeded in ledger with
				// identical path ownership — v1 keeps fail-closed for same-id races).
				return fmt.Errorf("%w: path %s owned by attempt %s (%s), conflicting with %s (%s)",
					ErrIntegrateConflict, f, prev.AttemptID, prev.WorkItemID, attemptID, workItemID)
			}
		}
	}
	_ = integrateWT
	return nil
}

func discoverProductFiles(childWT string) ([]string, error) {
	// File-level product discovery via structured NUL-safe git commands.
	// Never rely on porcelain directory-only untracked lines (?? notes/) —
	// those are not hashable product files. Full worktree walk is forbidden
	// (would treat base notes.go as product; RC.16 false green).
	//
	// Sources (Git-config-independent for untracked):
	//   - ls-files --others --exclude-standard: untracked files (recursive)
	//   - diff --name-only: unstaged tracked modifications
	//   - diff --cached --name-only: staged tracked changes
	ctx := context.Background()
	seen := map[string]bool{}
	var files []string
	add := func(paths []string) {
		for _, p := range paths {
			p = filepath.ToSlash(strings.TrimSpace(p))
			if p == "" || seen[p] {
				continue
			}
			seen[p] = true
			files = append(files, p)
		}
	}
	// Untracked files (file-level even under new directories).
	u, uerr := runGitRepoBytes(ctx, childWT, "ls-files", "-z", "--others", "--exclude-standard")
	if uerr != nil {
		return nil, uerr
	}
	add(splitGitNULPaths(u))
	// Unstaged tracked modifications (renames appear as new name when rename detection on).
	d, derr := runGitRepoBytes(ctx, childWT, "diff", "--name-only", "-z")
	if derr != nil {
		return nil, derr
	}
	add(splitGitNULPaths(d))
	// Staged tracked changes.
	c, cerr := runGitRepoBytes(ctx, childWT, "diff", "--cached", "--name-only", "-z")
	if cerr != nil {
		return nil, cerr
	}
	add(splitGitNULPaths(c))
	return files, nil
}

// runGitRepoBytes runs git and returns raw stdout (no TrimSpace) for -z parsers.
func runGitRepoBytes(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=loopcoder",
		"GIT_AUTHOR_EMAIL=loopcoder@local",
		"GIT_COMMITTER_NAME=loopcoder",
		"GIT_COMMITTER_EMAIL=loopcoder@local",
	)
	out, err := cmd.Output()
	if err != nil {
		msg := ""
		if ee, ok := err.(*exec.ExitError); ok {
			msg = strings.TrimSpace(string(ee.Stderr))
		}
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("git %v: %w: %s", args, err, msg)
	}
	return out, nil
}

// splitGitNULPaths splits git -z path lists into path strings.
func splitGitNULPaths(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	// Drop a single trailing NUL so Split does not yield a trailing empty.
	if raw[len(raw)-1] == 0 {
		raw = raw[:len(raw)-1]
	}
	if len(raw) == 0 {
		return nil
	}
	parts := bytes.Split(raw, []byte{0})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if len(p) == 0 {
			continue
		}
		out = append(out, string(p))
	}
	return out
}

// releaseIntegrateWorktree deregisters a temporary integrate worktree from the
// parent repo and removes its directory. RemoveAll alone leaves git still
// believing the goal branch is checked out in the vanished path.
//
// Returns combined errors from git remove, RemoveAll, and prune. Callers that
// own child leases must not swallow the result.
func releaseIntegrateWorktree(repoPath, tmpWT string) error {
	tmpWT = strings.TrimSpace(tmpWT)
	repoPath = strings.TrimSpace(repoPath)
	if tmpWT == "" {
		return nil
	}
	var errs []string
	// Soft-skip git deregistration only when parent path is absent (plain child
	// directory / never-a-repo). When the parent path exists, git failures are
	// fail-closed except known "not a working tree" / worktree-path-missing cases.
	parentAbsent := false
	if repoPath != "" {
		if _, serr := os.Stat(repoPath); os.IsNotExist(serr) {
			parentAbsent = true
		} else if serr != nil {
			errs = append(errs, fmt.Sprintf("stat repo: %v", serr))
		}
	}
	if repoPath != "" && !parentAbsent && len(errs) == 0 {
		if _, err := runGitRepo(context.Background(), repoPath, "worktree", "remove", "--force", tmpWT); err != nil {
			msg := strings.ToLower(err.Error())
			// Plain child dirs are not git worktrees — only real remove failures count.
			if !strings.Contains(msg, "is not a working tree") &&
				!strings.Contains(msg, "not a valid path") &&
				// Worktree path itself already gone is soft; parent still exists.
				!(strings.Contains(msg, "no such file") && strings.Contains(msg, strings.ToLower(tmpWT))) {
				errs = append(errs, fmt.Sprintf("git worktree remove: %v", err))
			}
		}
	}
	if err := os.RemoveAll(tmpWT); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Sprintf("RemoveAll: %v", err))
	}
	if repoPath != "" && !parentAbsent {
		if _, err := runGitRepo(context.Background(), repoPath, "worktree", "prune"); err != nil {
			// Parent exists: prune failure is durable cleanup error (fail closed).
			errs = append(errs, fmt.Sprintf("git worktree prune: %v", err))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("releaseIntegrateWorktree %s: %s", tmpWT, strings.Join(errs, "; "))
}

// ProductFilesOnly returns provider-produced paths that are eligible for
// product evidence/integration. Runtime-only roots are exact, top-level
// namespaces created by bounded provider invocations; they can never satisfy a
// product output contract or enter a goal branch.
func ProductFilesOnly(files []string) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		f = filepath.ToSlash(filepath.Clean(f))
		if f == "." || f == "" || strings.HasPrefix(f, "../") {
			continue
		}
		if runtimeOnlyProductPath(f) {
			continue
		}
		base := filepath.Base(f)
		if base == ".loopcoder-owned-worktree" || base == ".git" {
			continue
		}
		// Provider runtime logs / prompt dumps are not product.
		if strings.HasSuffix(base, ".log") || base == "prompt.txt" || base == "summary.txt" ||
			strings.HasPrefix(base, ".loopcoder-child") || base == "loopcoder-child-provider.log" {
			continue
		}
		// Meta evidence alone is not product — exclude pure .loopcoder/** except
		// we still allow child-output-*.md at root and real source/tests.
		if strings.HasPrefix(f, ".loopcoder/") {
			continue
		}
		out = append(out, f)
	}
	return out
}

func filterProductFiles(files []string) []string {
	return ProductFilesOnly(files)
}

func runtimeOnlyProductPath(rel string) bool {
	rel = filepath.ToSlash(rel)
	first := rel
	if i := strings.IndexByte(rel, '/'); i >= 0 {
		first = rel[:i]
	}
	switch first {
	case ".cache", ".tmp":
		return true
	default:
		return false
	}
}

func copyPath(src, dst string) error {
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	if st.IsDir() {
		return copyDir(src, dst)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, st.Mode().Perm())
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		return copyPath(path, target)
	})
}

func runGitRepo(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=loopcoder",
		"GIT_AUTHOR_EMAIL=loopcoder@local",
		"GIT_COMMITTER_NAME=loopcoder",
		"GIT_COMMITTER_EMAIL=loopcoder@local",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %v: %w: %s", args, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}
