package workflowrun

import (
	"fmt"
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
		// Fail closed: filename-only hasDocsProduct is insufficient. At least one
		// docs leaf must secure-read as non-clarification substantial prose.
		// Symlink root/parent/leaf or missing nested paths must not pseudo-green.
		if !hasSubstantialDocsProduct(worktree, product) {
			if looksLikeClarification(evidence, worktree, product) {
				return fmt.Errorf("workflowrun: acceptance refused for %s: clarification/empty work is not success", workItemID)
			}
			if !hasDocsProduct(product) && len(product) == 0 {
				return fmt.Errorf("workflowrun: docs child %s produced no docs product", workItemID)
			}
			return fmt.Errorf("workflowrun: docs child %s docs product is not securely readable substantial documentation (symlink/unreadable/empty refused)", workItemID)
		}
		return nil
	}
	if looksLikeClarification(evidence, worktree, product) {
		return fmt.Errorf("workflowrun: acceptance refused for %s: clarification/empty work is not success", workItemID)
	}
	switch role {
	case RoleTests:
		// Role-specific: matching test paths only; must be secure regular leaves
		// (not filename-only, not worktree walk of pre-existing base tests).
		if !hasSecureTestProduct(worktree, product) {
			if !hasTestProductInList(product) {
				return fmt.Errorf("workflowrun: tests child %s must add/adjust real test files (*_test.go or tests/); got %v", workItemID, product)
			}
			return fmt.Errorf("workflowrun: tests child %s test product is not securely readable regular files (symlink/unreadable refused); got %v", workItemID, product)
		}
	case RoleImplement:
		// child-output-*.md alone is never enough even if worktree has base sources.
		if onlyChildOutputStubs(product) {
			return fmt.Errorf("workflowrun: implement child %s produced only child-output stubs, not product source; got %v", workItemID, product)
		}
		if !hasSecureSourceProduct(worktree, product) {
			if !hasSourceProduct(product) {
				return fmt.Errorf("workflowrun: implement child %s must produce product source (not meta/clarification only); got %v", workItemID, product)
			}
			return fmt.Errorf("workflowrun: implement child %s source product is not securely readable regular files (symlink/unreadable refused); got %v", workItemID, product)
		}
	default:
		if !hasSecureAnyProduct(worktree, product) {
			if len(product) == 0 {
				return fmt.Errorf("workflowrun: child %s success requires product files", workItemID)
			}
			return fmt.Errorf("workflowrun: child %s product is not securely readable regular files (symlink/unreadable refused); got %v", workItemID, product)
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
	// Real secure source/test product exempts implement/test paths from phrase gates.
	// Filename-only or symlink leaves do not exempt.
	if hasSecureTestProduct(worktree, product) || hasSecureSourceProduct(worktree, product) {
		return false
	}
	for _, blob := range collectSecureTextBlobs(worktree, product) {
		if isExplicitClarificationOnly(blob) {
			return true
		}
	}
	return false
}

func isTestProductRel(rel string) bool {
	cleaned, err := cleanWorktreeRelPath(rel)
	if err != nil {
		// Fall back to raw slash form for list-only classification.
		cleaned = filepath.ToSlash(strings.TrimSpace(rel))
	}
	base := filepath.Base(cleaned)
	if strings.HasPrefix(base, "child-output-") {
		return false
	}
	slash := filepath.ToSlash(cleaned)
	if strings.HasSuffix(base, "_test.go") || strings.HasSuffix(base, "_test.py") ||
		strings.HasSuffix(base, ".test.ts") || strings.HasSuffix(base, ".spec.ts") ||
		strings.Contains(slash, "/tests/") || strings.HasPrefix(slash, "tests/") ||
		strings.Contains(slash, "/testdata/") {
		return true
	}
	return false
}

func isSourceProductRel(rel string) bool {
	cleaned, err := cleanWorktreeRelPath(rel)
	if err != nil {
		cleaned = filepath.ToSlash(strings.TrimSpace(rel))
	}
	base := filepath.Base(cleaned)
	if strings.HasPrefix(base, "child-output-") {
		return false
	}
	if strings.HasSuffix(base, "_test.go") || strings.HasSuffix(base, "_test.py") {
		return false
	}
	switch {
	case strings.HasSuffix(base, ".go"), strings.HasSuffix(base, ".py"),
		strings.HasSuffix(base, ".ts"), strings.HasSuffix(base, ".tsx"),
		strings.HasSuffix(base, ".js"), strings.HasSuffix(base, ".rs"),
		strings.HasSuffix(base, ".java"), strings.HasSuffix(base, ".c"),
		strings.HasSuffix(base, ".h"), strings.HasSuffix(base, ".cpp"):
		return true
	}
	return false
}

func hasTestProductInList(product []string) bool {
	for _, f := range product {
		if isTestProductRel(f) {
			return true
		}
	}
	return false
}

func hasSourceProduct(product []string) bool {
	for _, f := range product {
		if isSourceProductRel(f) {
			return true
		}
	}
	return false
}

// hasSecureRegularMatchingProduct is true when at least one product path both
// matches role classification and proves as a secure regular non-empty leaf
// under worktree (≥1 byte; zero-byte empty.go / empty_test.go refused).
func hasSecureRegularMatchingProduct(worktree string, product []string, match func(string) bool) bool {
	if worktree == "" || len(product) == 0 {
		return false
	}
	wtAbs, err := filepath.Abs(worktree)
	if err != nil {
		return false
	}
	if err := requireNonSymlinkDir(wtAbs); err != nil {
		return false
	}
	seen := map[string]bool{}
	for _, rel := range product {
		cleaned, cerr := cleanWorktreeRelPath(rel)
		if cerr != nil || seen[cleaned] {
			continue
		}
		seen[cleaned] = true
		if !match(cleaned) {
			continue
		}
		if err := proveSecureRegularProduct(wtAbs, cleaned); err == nil {
			return true
		}
	}
	return false
}

func hasSecureSourceProduct(worktree string, product []string) bool {
	return hasSecureRegularMatchingProduct(worktree, product, isSourceProductRel)
}

func hasSecureTestProduct(worktree string, product []string) bool {
	return hasSecureRegularMatchingProduct(worktree, product, isTestProductRel)
}

// hasSecureAnyProduct requires at least one listed product path that is a
// secure regular leaf (generic role — no suffix specialization).
func hasSecureAnyProduct(worktree string, product []string) bool {
	return hasSecureRegularMatchingProduct(worktree, product, func(string) bool { return true })
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

// isDocsProductRel reports whether a clean relative path is a docs product leaf
// (markdown or under docs/), excluding child-output stubs.
func isDocsProductRel(rel string) bool {
	cleaned, err := cleanWorktreeRelPath(rel)
	if err != nil {
		return false
	}
	base := filepath.Base(cleaned)
	if strings.HasPrefix(base, "child-output-") {
		return false
	}
	if strings.HasPrefix(filepath.ToSlash(cleaned), "docs/") {
		return true
	}
	return strings.HasSuffix(strings.ToLower(base), ".md")
}

// hasSubstantialDocsProduct is true when at least one docs product leaf can be
// secure-read (non-symlink root/parents/leaf, clean relative path) and the body
// after scaffold strip is non-clarification substantial prose. Filename presence
// alone (hasDocsProduct) never greens — unreadable/symlink candidates fail closed.
func hasSubstantialDocsProduct(worktree string, product []string) bool {
	check := func(raw []byte) bool {
		if len(raw) == 0 {
			return false
		}
		if isExplicitClarificationOnly(string(raw)) {
			return false
		}
		body := strings.TrimSpace(bodyAfterMaterializeScaffold(string(raw)))
		return len(body) >= 80
	}
	if worktree == "" {
		return false
	}
	wtAbs, err := filepath.Abs(worktree)
	if err != nil {
		return false
	}
	if err := requireNonSymlinkDir(wtAbs); err != nil {
		return false
	}
	seen := map[string]bool{}
	try := func(rel string) bool {
		cleaned, err := cleanWorktreeRelPath(rel)
		if err != nil || seen[cleaned] {
			return false
		}
		seen[cleaned] = true
		raw, ok := readRegularFindingsFile(wtAbs, cleaned)
		return ok && check(raw)
	}
	for _, rel := range product {
		if isDocsProductRel(rel) && try(rel) {
			return true
		}
	}
	// Materialize leaf at worktree root (writeProductFileSecurely docs-notes.md).
	if try("docs-notes.md") {
		return true
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
// Preserves clean relative paths (docs/findings.md stays nested); never collapses
// to filepath.Base. Rejects abs/../empty and symlink root/parents via secure reader.
func anySecureLeaf(worktree string, product []string, names []string, includeChildOutput bool, check func([]byte) bool) bool {
	if worktree == "" {
		return false
	}
	wtAbs, err := filepath.Abs(worktree)
	if err != nil {
		return false
	}
	if err := requireNonSymlinkDir(wtAbs); err != nil {
		return false
	}
	wantBase := map[string]bool{}
	for _, n := range names {
		if b := filepath.Base(strings.TrimSpace(n)); b != "" {
			wantBase[b] = true
		}
	}
	seen := map[string]bool{}
	try := func(rel string) bool {
		cleaned, err := cleanWorktreeRelPath(rel)
		if err != nil || seen[cleaned] {
			return false
		}
		seen[cleaned] = true
		raw, ok := readRegularFindingsFile(wtAbs, cleaned)
		return ok && check(raw)
	}
	for _, rel := range product {
		cleaned, err := cleanWorktreeRelPath(rel)
		if err != nil {
			continue
		}
		base := filepath.Base(cleaned)
		if wantBase[base] && try(cleaned) {
			return true
		}
		if includeChildOutput && strings.HasPrefix(base, "child-output-") && try(cleaned) {
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
	// Filename hint on product list only if the nested leaf also securely reads as substantial.
	// Preserve clean relative path (docs/review.md), never filepath.Base collapse.
	if worktree != "" {
		for _, f := range product {
			cleaned, err := cleanWorktreeRelPath(f)
			if err != nil {
				continue
			}
			base := strings.ToLower(filepath.Base(cleaned))
			if strings.Contains(base, "verif") || strings.Contains(base, "verdict") || strings.Contains(base, "review") {
				if raw, ok := readRegularFindingsFile(worktree, cleaned); ok && check(raw) {
					return true
				}
			}
		}
	}
	// Evidence digest alone is insufficient without review artifact for verify role.
	return false
}
