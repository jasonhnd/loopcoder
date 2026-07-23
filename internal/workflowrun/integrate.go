package workflowrun

import (
	"context"
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
	// LedgerDir stores exactly-once attempt→commit records under the repo.
	// Empty → <repo>/.loopcoder/integrate-ledger/
	LedgerDir string
	Now       func() time.Time
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
func (g GitBranchIntegrator) IntegrateChild(ctx context.Context, req IntegrateRequest) (IntegrateCommit, error) {
	out := IntegrateCommit{WorkItemID: req.WorkItemID, AttemptID: req.AttemptID}
	if strings.TrimSpace(req.RepoPath) == "" || strings.TrimSpace(req.GoalBranch) == "" ||
		strings.TrimSpace(req.AttemptID) == "" || strings.TrimSpace(req.ChildWorktree) == "" {
		return out, fmt.Errorf("%w: missing fields", ErrIntegrateInvalid)
	}
	// Exactly-once ledger.
	if prev, ok, err := g.loadLedger(req.RepoPath, req.AttemptID); err != nil {
		return out, err
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
	defer func() { _ = os.RemoveAll(tmpWT) }()

	// Prefer git worktree add for the goal branch.
	if _, err := runGitRepo(ctx, req.RepoPath, "worktree", "add", "--force", tmpWT, req.GoalBranch); err != nil {
		// Fallback: clone local + checkout branch.
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

// detectPathConflict fail-closes when a product path was already integrated by a
// *different* attempt with different content (true merge conflict).
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
	// Map path → prior attempt content hash from files on integrate WT vs child
	for _, prev := range doc.Entries {
		if prev.AttemptID == attemptID {
			continue
		}
		for _, f := range prev.Files {
			for _, nf := range files {
				if f != nf {
					continue
				}
				// Same path claimed by different attempt — fail closed (v1: no
				// silent overwrite across work items).
				if prev.WorkItemID != workItemID {
					return fmt.Errorf("%w: path %s owned by attempt %s (%s), conflicting with %s (%s)",
						ErrIntegrateConflict, f, prev.AttemptID, prev.WorkItemID, attemptID, workItemID)
				}
				_ = integrateWT // reserved for future content-equal allowlist
			}
		}
	}
	return nil
}

func discoverProductFiles(childWT string) ([]string, error) {
	// Only git-status changes count as product (added/modified/untracked).
	// Full worktree walk would treat base notes.go as "product" and let
	// implement accept without writing real source (RC.16 false green).
	st, err := runGitRepo(context.Background(), childWT, "status", "--porcelain")
	if err == nil {
		var files []string
		for _, line := range strings.Split(st, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			// porcelain: XY PATH or XY ORIG -> PATH
			path := line
			if len(line) > 3 {
				path = strings.TrimSpace(line[3:])
			}
			if i := strings.Index(path, " -> "); i >= 0 {
				path = path[i+4:]
			}
			path = strings.Trim(path, "\"")
			if path != "" {
				files = append(files, path)
			}
		}
		return files, nil
	}
	// No walk fallback: listing the whole tree falsely treats base product files
	// as child output. Prefer empty + acceptance failure over false green.
	return nil, err
}

func filterProductFiles(files []string) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		f = filepath.ToSlash(filepath.Clean(f))
		if f == "." || f == "" || strings.HasPrefix(f, "../") {
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
