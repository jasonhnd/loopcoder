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
	// Read-only / summary-materialized roles: require durable product first, then
	// refuse clarification-only bodies even when LoopCoder headers make them long.
	switch role {
	case RoleResearch:
		if len(product) == 0 && !hasAnyFindings(worktree) {
			return fmt.Errorf("workflowrun: research child %s produced no findings product", workItemID)
		}
		if !hasSubstantialResearchFindings(worktree, product) {
			if looksLikeClarification(evidence, worktree, product) {
				return fmt.Errorf("workflowrun: acceptance refused for %s: clarification/empty work is not success", workItemID)
			}
			return fmt.Errorf("workflowrun: research child %s findings are not substantial survey product", workItemID)
		}
		return nil
	case RoleVerify:
		if !hasVerifierVerdict(product, worktree, evidence) {
			return fmt.Errorf("workflowrun: verify child %s must produce independent verdict with digest over integrated head; clarification refused", workItemID)
		}
		return nil
	case RoleDocs:
		if !hasDocsProduct(product) && len(product) == 0 {
			return fmt.Errorf("workflowrun: docs child %s produced no docs product", workItemID)
		}
		if looksLikeClarification(evidence, worktree, product) {
			return fmt.Errorf("workflowrun: acceptance refused for %s: clarification/empty work is not success", workItemID)
		}
		return nil
	}
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
	default:
		if len(product) == 0 {
			return fmt.Errorf("workflowrun: child %s success requires product files", workItemID)
		}
	}
	return nil
}

// looksLikeClarification scans durable product text for empty-work shells.
// Never follows symlinks (secure Lstat+SameFile reads only). Never treats the
// evidence digest string as prose. LoopCoder materialize headers are stripped
// before evaluation so long scaffolding cannot wash clarification-only bodies.
func looksLikeClarification(evidence, worktree string, product []string) bool {
	_ = evidence // digest is not prose; ignore for phrase matching
	// Real tests/source product exempts implement/test paths from phrase gates.
	if hasTestProduct(product, worktree) || hasSourceProduct(product) {
		return false
	}
	for _, blob := range collectSecureTextBlobs(worktree, product) {
		if isExplicitClarificationOnly(blob) {
			return true
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

// hasSubstantialResearchFindings is true when durable research product has a
// real survey body. LoopCoder "## Provider survey" headers alone are insufficient:
// the body after scaffold strip must not be clarification-only.
func hasSubstantialResearchFindings(worktree string, product []string) bool {
	check := func(raw []byte) bool {
		if isExplicitClarificationOnly(string(raw)) {
			return false
		}
		body := strings.TrimSpace(bodyAfterMaterializeScaffold(string(raw)))
		if len(body) < 80 {
			return false
		}
		low := strings.ToLower(string(raw))
		if strings.Contains(low, "## provider survey") {
			return true
		}
		// Long structured findings without the exact header still count.
		return strings.Count(strings.ToLower(body), "\n") >= 8 && len(body) >= 200 &&
			(strings.Contains(strings.ToLower(body), "scope") || strings.Contains(strings.ToLower(body), "constraint") ||
				strings.Contains(strings.ToLower(body), "survey") || strings.Contains(strings.ToLower(body), "findings"))
	}
	return anySecureLeaf(worktree, product, []string{"findings.md", "FINDINGS.md"}, true, check)
}

// anySecureLeaf evaluates check on secure-read product leaves (optional child-output).
func anySecureLeaf(worktree string, product []string, names []string, includeChildOutput bool, check func([]byte) bool) bool {
	if worktree == "" {
		return false
	}
	wtAbs, err := filepath.Abs(worktree)
	if err != nil {
		return false
	}
	seen := map[string]bool{}
	try := func(name string) bool {
		name = filepath.Base(name)
		if name == "" || seen[name] {
			return false
		}
		seen[name] = true
		raw, ok := readRegularFindingsFile(wtAbs, name)
		return ok && check(raw)
	}
	for _, rel := range product {
		base := filepath.Base(rel)
		for _, n := range names {
			if base == n && try(base) {
				return true
			}
		}
		if includeChildOutput && strings.HasPrefix(base, "child-output-") && try(base) {
			return true
		}
	}
	for _, n := range names {
		if try(n) {
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
		if raw, ok := readRegularFindingsFile(wtAbs, name); ok && len(raw) > 40 && !isExplicitClarificationOnly(string(raw)) {
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
// symlinks, directories, FIFOs, and any path escape.
//
// Identity chain (fail closed on race):
//  1. preLstat path — must be regular non-symlink under worktree
//  2. Open path
//  3. f.Stat() — must be regular; os.SameFile(preLstat, fdStat)
//  4. read from fd
//  5. postLstat path — must be regular non-symlink; os.SameFile(fdStat, postLstat)
//
// Without SameFile, Lstat→Open can follow a swapped symlink and leak external
// content into acceptance while path Lstats still look clean.
func readRegularFindingsFile(worktreeAbs, name string) ([]byte, bool) {
	return readRegularFindingsFileChecked(worktreeAbs, name)
}

// readRegularFindingsFileChecked is the testable implementation. Returns
// (nil, false) on any identity/safety failure.
func readRegularFindingsFileChecked(worktreeAbs, name string) ([]byte, bool) {
	raw, err := readRegularFindingsFileErr(worktreeAbs, name)
	return raw, err == nil
}

// readRegularFindingsFileErr returns a non-nil error describing identity/safety
// failures (used by unit tests). Callers that only need bool use Checked.
func readRegularFindingsFileErr(worktreeAbs, name string) ([]byte, error) {
	name = filepath.Base(strings.TrimSpace(name))
	if name == "" || name == "." || name == ".." {
		return nil, fmt.Errorf("findings leaf name invalid")
	}
	full := filepath.Join(worktreeAbs, name)
	if err := requirePathUnderRoot(worktreeAbs, full); err != nil {
		return nil, err
	}
	pre, err := os.Lstat(full)
	if err != nil {
		return nil, err
	}
	if pre.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a symlink", name)
	}
	if !pre.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file (mode=%v)", name, pre.Mode())
	}
	const maxFindings = 1 << 20
	f, err := os.Open(full)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fdStat, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fdStat.Mode().IsRegular() {
		return nil, fmt.Errorf("%s fd is not a regular file", name)
	}
	// Opened fd must be the same inode as the pre-open Lstat (no symlink swap).
	if !os.SameFile(pre, fdStat) {
		return nil, fmt.Errorf("%s identity mismatch: pre-lstat vs open fd", name)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxFindings+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxFindings {
		return nil, fmt.Errorf("%s exceeds max findings size", name)
	}
	post, err := os.Lstat(full)
	if err != nil {
		return nil, err
	}
	if post.Mode()&os.ModeSymlink != 0 || !post.Mode().IsRegular() {
		return nil, fmt.Errorf("%s post-read node is not a regular non-symlink file", name)
	}
	// Path node after read must still be the same file as the opened fd.
	if !os.SameFile(fdStat, post) {
		return nil, fmt.Errorf("%s identity mismatch: open fd vs post-lstat", name)
	}
	return raw, nil
}

func hasVerifierVerdict(product []string, worktree, evidence string) bool {
	// Must have non-empty evidence digest and not be clarification.
	if strings.TrimSpace(evidence) == "" || !strings.HasPrefix(evidence, "sha256:") {
		return false
	}
	if looksLikeClarification(evidence, worktree, product) {
		return false
	}
	// Prefer explicit verdict.md / review artifact with substantial non-clarification body.
	check := func(raw []byte) bool {
		if isExplicitClarificationOnly(string(raw)) {
			return false
		}
		body := strings.TrimSpace(bodyAfterMaterializeScaffold(string(raw)))
		if len(body) < 80 {
			return false
		}
		low := strings.ToLower(string(raw))
		return strings.Contains(low, "## adversarial review") ||
			strings.Contains(low, "verdict") || strings.Contains(low, "review") ||
			len(body) >= 120
	}
	if anySecureLeaf(worktree, product, []string{"verdict.md"}, true, check) {
		return true
	}
	// Filename hint on product list only if the leaf also securely reads as substantial.
	for _, f := range product {
		base := strings.ToLower(filepath.Base(f))
		if strings.Contains(base, "verif") || strings.Contains(base, "verdict") || strings.Contains(base, "review") {
			if worktree != "" {
				if raw, ok := readRegularFindingsFile(worktree, filepath.Base(f)); ok && check(raw) {
					return true
				}
			}
		}
	}
	// Evidence digest alone is insufficient without review artifact for verify role.
	return false
}
