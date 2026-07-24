package workflowrun

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// TaskRole classifies a child for task-specific acceptance.
type TaskRole string

const (
	RoleResearch  TaskRole = "research"
	RoleImplement TaskRole = "implement"
	RoleTests     TaskRole = "tests"
	RoleVerify    TaskRole = "verify"
	RoleDocs      TaskRole = "docs"
	RoleGeneric   TaskRole = "generic"
)

// ClassifyTaskRole maps work-item id/intent/owner to an acceptance role.
// Prefer stable work_item_id prefixes so goal text like "with tests" inside an
// implement intent cannot mis-route wi_implement → RoleTests (false accept).
func ClassifyTaskRole(workItemID, intent, owner string) TaskRole {
	id := strings.ToLower(strings.TrimSpace(workItemID))
	switch {
	case strings.HasPrefix(id, "wi_tests") || id == "tests":
		return RoleTests
	case strings.HasPrefix(id, "wi_verify") || id == "verify":
		return RoleVerify
	case strings.HasPrefix(id, "wi_implement") || id == "implement":
		return RoleImplement
	case strings.HasPrefix(id, "wi_research") || id == "research":
		return RoleResearch
	case strings.HasPrefix(id, "wi_docs") || id == "docs":
		return RoleDocs
	}
	// Fallback: intent/owner only when work_item_id is non-canonical.
	blob := strings.ToLower(strings.TrimSpace(intent) + " " + strings.TrimSpace(owner))
	switch {
	case strings.Contains(blob, "verif") || strings.Contains(blob, "adversarial"):
		return RoleVerify
	case strings.HasPrefix(blob, "tests:") || strings.Contains(blob, "add/adjust focused tests"):
		return RoleTests
	case strings.HasPrefix(blob, "implementation:") || strings.Contains(blob, "deliver the change"):
		return RoleImplement
	case strings.HasPrefix(blob, "research") || strings.Contains(blob, "survey scope"):
		return RoleResearch
	case strings.HasPrefix(blob, "docs:") || strings.Contains(blob, "user-facing docs"):
		return RoleDocs
	default:
		return RoleGeneric
	}
}

// AcceptSucceededChild enforces task-specific acceptance after a child claims
// success. Clarification-only text, empty worktrees, and missing tests/product
// fail closed (must not count as succeeded).
func AcceptSucceededChild(workItemID, intent, owner string, files []string, worktree, evidence string) error {
	role := ClassifyTaskRole(workItemID, intent, owner)
	product := filterProductFiles(files)
	if looksLikeClarification(evidence, worktree, product) {
		return fmt.Errorf("workflowrun: acceptance refused for %s: clarification/empty work is not success", workItemID)
	}
	switch role {
	case RoleTests:
		// Require test files in the child's FilesTouched list — do not count
		// pre-existing base *_test.go via full worktree walk (false green).
		if !hasTestProductInList(product) {
			return fmt.Errorf("workflowrun: tests child %s must add/adjust real test files (*_test.go or tests/); got %v", workItemID, product)
		}
	case RoleImplement:
		if !hasSourceProduct(product) {
			return fmt.Errorf("workflowrun: implement child %s must produce product source (not meta/clarification only); got %v", workItemID, product)
		}
		// child-output-*.md alone is never enough even if worktree has base sources.
		if onlyChildOutputStubs(product) {
			return fmt.Errorf("workflowrun: implement child %s produced only child-output stubs, not product source; got %v", workItemID, product)
		}
	case RoleVerify:
		if !hasVerifierVerdict(product, worktree, evidence) {
			return fmt.Errorf("workflowrun: verify child %s must produce independent verdict with digest over integrated head; clarification refused", workItemID)
		}
	case RoleResearch:
		if len(product) == 0 && !hasAnyFindings(worktree) {
			return fmt.Errorf("workflowrun: research child %s produced no findings product", workItemID)
		}
	case RoleDocs:
		if !hasDocsProduct(product) && len(product) == 0 {
			return fmt.Errorf("workflowrun: docs child %s produced no docs product", workItemID)
		}
	default:
		if len(product) == 0 {
			return fmt.Errorf("workflowrun: child %s success requires product files", workItemID)
		}
	}
	return nil
}

func looksLikeClarification(evidence, worktree string, product []string) bool {
	blobs := []string{strings.ToLower(evidence)}
	// Scan small product/md files for clarification language.
	for _, rel := range product {
		if !strings.HasSuffix(rel, ".md") && !strings.HasSuffix(rel, ".txt") {
			continue
		}
		if worktree == "" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(worktree, rel))
		if err != nil || len(raw) > 32<<10 {
			continue
		}
		blobs = append(blobs, strings.ToLower(string(raw)))
	}
	// Also check child-output stub.
	if worktree != "" {
		entries, _ := os.ReadDir(worktree)
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if strings.HasPrefix(name, "child-output-") || strings.HasSuffix(name, ".md") {
				raw, err := os.ReadFile(filepath.Join(worktree, name))
				if err == nil && len(raw) < 32<<10 {
					blobs = append(blobs, strings.ToLower(string(raw)))
				}
			}
		}
	}
	phrases := []string{
		"please clarify", "need clarification", "need more information",
		"cannot find implementation", "no implementation", "no test suite",
		"repository contains no", "nothing to review", "awaiting clarification",
		"what should i", "please provide more", "unclear requirements",
		"no code changes", "no files changed",
	}
	for _, b := range blobs {
		if b == "" {
			continue
		}
		// If text is only clarification with no concrete deliverable markers.
		for _, p := range phrases {
			if strings.Contains(b, p) {
				// Allow if product also has real tests/source.
				if hasTestProduct(product, worktree) || hasSourceProduct(product) {
					continue
				}
				return true
			}
		}
	}
	return false
}

func hasTestProductInList(product []string) bool {
	for _, f := range product {
		base := filepath.Base(f)
		if strings.HasPrefix(base, "child-output-") {
			continue
		}
		if strings.HasSuffix(base, "_test.go") || strings.HasSuffix(base, "_test.py") ||
			strings.HasSuffix(base, ".test.ts") || strings.HasSuffix(base, ".spec.ts") ||
			strings.Contains(f, "/tests/") || strings.HasPrefix(f, "tests/") ||
			strings.Contains(f, "/testdata/") {
			return true
		}
	}
	return false
}

func hasTestProduct(product []string, worktree string) bool {
	if hasTestProductInList(product) {
		return true
	}
	if worktree == "" {
		return false
	}
	found := false
	_ = filepath.Walk(worktree, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			if info != nil && info.IsDir() && (filepath.Base(path) == ".git" || filepath.Base(path) == ".loopcoder") {
				return filepath.SkipDir
			}
			return nil
		}
		base := filepath.Base(path)
		if strings.HasSuffix(base, "_test.go") || strings.HasSuffix(base, "_test.py") {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

func hasSourceProduct(product []string) bool {
	for _, f := range product {
		base := filepath.Base(f)
		if strings.HasPrefix(base, "child-output-") {
			continue
		}
		if strings.HasSuffix(base, "_test.go") {
			continue
		}
		switch {
		case strings.HasSuffix(base, ".go"), strings.HasSuffix(base, ".py"),
			strings.HasSuffix(base, ".ts"), strings.HasSuffix(base, ".tsx"),
			strings.HasSuffix(base, ".js"), strings.HasSuffix(base, ".rs"),
			strings.HasSuffix(base, ".java"), strings.HasSuffix(base, ".c"),
			strings.HasSuffix(base, ".h"), strings.HasSuffix(base, ".cpp"):
			return true
		}
	}
	return false
}

// onlyChildOutputStubs is true when every product path is a child-output-*.md stub.
func onlyChildOutputStubs(product []string) bool {
	if len(product) == 0 {
		return false
	}
	for _, f := range product {
		base := filepath.Base(f)
		if !strings.HasPrefix(base, "child-output-") {
			return false
		}
	}
	return true
}

func hasDocsProduct(product []string) bool {
	for _, f := range product {
		if strings.HasSuffix(f, ".md") && !strings.HasPrefix(filepath.Base(f), "child-output-") {
			return true
		}
		if strings.HasPrefix(f, "docs/") {
			return true
		}
	}
	return false
}

func hasAnyFindings(worktree string) bool {
	if worktree == "" {
		return false
	}
	wtAbs, err := filepath.Abs(worktree)
	if err != nil {
		return false
	}
	// findings.md or child-output with substantial body (not route-metadata stubs alone).
	// Security: Lstat only — never follow symlinks (external file must not count).
	for _, name := range []string{"findings.md", "FINDINGS.md"} {
		if raw, ok := readRegularFindingsFile(wtAbs, name); ok && len(raw) > 40 {
			return true
		}
	}
	entries, err := os.ReadDir(wtAbs)
	if err != nil {
		return false
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "child-output-") {
			continue
		}
		raw, ok := readRegularFindingsFile(wtAbs, name)
		if !ok || len(raw) < 200 {
			continue
		}
		// Require survey body beyond the short writeChildEvidence route stub.
		low := strings.ToLower(string(raw))
		if strings.Contains(low, "## provider survey") || strings.Contains(low, "findings") ||
			(strings.Count(low, "\n") >= 8 && len(raw) > 400) {
			return true
		}
	}
	return false
}

// readRegularFindingsFile Lstats then reads a leaf under worktree. Rejects
// symlinks, directories, FIFOs, and any path escape. Re-Lstats after read to
// refuse races that swap a regular file for a non-regular node.
func readRegularFindingsFile(worktreeAbs, name string) ([]byte, bool) {
	name = filepath.Base(strings.TrimSpace(name))
	if name == "" || name == "." || name == ".." {
		return nil, false
	}
	full := filepath.Join(worktreeAbs, name)
	if err := requirePathUnderRoot(worktreeAbs, full); err != nil {
		return nil, false
	}
	st, err := os.Lstat(full)
	if err != nil {
		return nil, false
	}
	if st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
		return nil, false
	}
	// Cap read size to avoid loading huge external-looking artifacts.
	const maxFindings = 1 << 20
	f, err := os.Open(full)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	// Post-open: still a regular non-symlink at the path (Lstat, not Stat).
	if st2, err := os.Lstat(full); err != nil || st2.Mode()&os.ModeSymlink != 0 || !st2.Mode().IsRegular() {
		return nil, false
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxFindings+1))
	if err != nil || len(raw) > maxFindings {
		return nil, false
	}
	// Final Lstat after read — refuse if node type flipped.
	if st3, err := os.Lstat(full); err != nil || st3.Mode()&os.ModeSymlink != 0 || !st3.Mode().IsRegular() {
		return nil, false
	}
	return raw, true
}

func hasVerifierVerdict(product []string, worktree, evidence string) bool {
	// Must have non-empty evidence digest and not be clarification.
	if strings.TrimSpace(evidence) == "" || !strings.HasPrefix(evidence, "sha256:") {
		return false
	}
	if looksLikeClarification(evidence, worktree, product) {
		return false
	}
	// Prefer an explicit verification artifact when present.
	for _, f := range product {
		base := strings.ToLower(filepath.Base(f))
		if strings.Contains(base, "verif") || strings.Contains(base, "verdict") || strings.Contains(base, "review") {
			return true
		}
	}
	// child-output with adversarial review content + digest is acceptable when
	// integrated head was available (caller ensures materialize from goal branch).
	if worktree != "" {
		entries, _ := os.ReadDir(worktree)
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "child-output-") {
				raw, err := os.ReadFile(filepath.Join(worktree, e.Name()))
				if err != nil {
					continue
				}
				low := strings.ToLower(string(raw))
				if strings.Contains(low, "no implementation") || strings.Contains(low, "nothing to review") {
					return false
				}
				if len(raw) > 80 {
					return true
				}
			}
		}
	}
	// Evidence digest alone is insufficient without review artifact for verify role.
	return false
}
