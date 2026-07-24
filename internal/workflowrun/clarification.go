package workflowrun

import (
	"path/filepath"
	"strings"
)

// strongClarificationPhrases are explicit "stop and ask human" shells.
// Presence after scaffold strip means clarification-only regardless of length
// (LoopCoder materialize headers must never wash these into success).
var strongClarificationPhrases = []string{
	"please clarify",
	"need clarification",
	"need more information",
	"awaiting clarification",
	"what should i",
	"please provide more",
	"unclear requirements",
}

// weakClarificationPhrases may appear in legitimate greenfield surveys
// ("no existing tests") — only treat as empty work when the remaining body
// after phrase removal is thin.
var weakClarificationPhrases = []string{
	"cannot find implementation",
	"no implementation",
	"no test suite",
	"repository contains no",
	"nothing to review",
	"no code changes",
	"no files changed",
}

// bodyAfterMaterializeScaffold removes LoopCoder-owned materialize headers so
// length/structure checks evaluate the provider body, not scaffolding.
func bodyAfterMaterializeScaffold(s string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		low := strings.ToLower(t)
		switch {
		case strings.HasPrefix(low, "# research findings"),
			strings.HasPrefix(low, "# verification verdict"),
			strings.HasPrefix(low, "# documentation notes"),
			strings.HasPrefix(low, "## provider survey"),
			strings.HasPrefix(low, "## adversarial review"),
			strings.HasPrefix(low, "## documentation"),
			strings.HasPrefix(low, "work item:"),
			strings.HasPrefix(low, "intent:"):
			continue
		default:
			out = append(out, t)
		}
	}
	return strings.Join(out, "\n")
}

// isExplicitClarificationOnly reports whether text is clarification-only after
// stripping LoopCoder materialize scaffolding. Strong phrases always win
// regardless of total length; weak phrases only when remaining substance is thin.
func isExplicitClarificationOnly(text string) bool {
	body := strings.TrimSpace(bodyAfterMaterializeScaffold(text))
	if body == "" {
		return true
	}
	low := strings.ToLower(body)
	for _, p := range strongClarificationPhrases {
		if strings.Contains(low, p) {
			return true
		}
	}
	hits := 0
	rest := low
	for _, p := range weakClarificationPhrases {
		if strings.Contains(rest, p) {
			hits++
			rest = strings.ReplaceAll(rest, p, " ")
		}
	}
	if hits == 0 {
		return false
	}
	// Collapse whitespace for remaining-substance estimate.
	var b strings.Builder
	for _, r := range rest {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '-' || r == '*' {
			continue
		}
		b.WriteRune(r)
	}
	return b.Len() < 80
}

// collectSecureTextBlobs reads product/worktree text leaves via Lstat+SameFile only.
func collectSecureTextBlobs(worktree string, product []string) []string {
	var blobs []string
	if worktree == "" {
		return blobs
	}
	wtAbs, err := filepath.Abs(worktree)
	if err != nil {
		return blobs
	}
	seen := map[string]bool{}
	add := func(name string) {
		name = filepath.Base(name)
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		raw, ok := readRegularFindingsFile(wtAbs, name)
		if !ok || len(raw) == 0 || len(raw) > 32<<10 {
			return
		}
		blobs = append(blobs, string(raw))
	}
	for _, rel := range product {
		base := filepath.Base(rel)
		if strings.HasSuffix(base, ".md") || strings.HasSuffix(base, ".txt") {
			add(base)
		}
	}
	// Known materialize leaves + child-output stubs.
	for _, name := range []string{"findings.md", "FINDINGS.md", "verdict.md", "docs-notes.md"} {
		add(name)
	}
	entries, err := readDirSecureNames(wtAbs)
	if err == nil {
		for _, name := range entries {
			if strings.HasPrefix(name, "child-output-") || strings.HasSuffix(name, ".md") {
				add(name)
			}
		}
	}
	return blobs
}

// readDirSecureNames returns base names under worktree (dirs included as names only).
func readDirSecureNames(worktreeAbs string) ([]string, error) {
	entries, err := filepath.Glob(filepath.Join(worktreeAbs, "*"))
	if err != nil {
		return nil, err
	}
	var names []string
	for _, p := range entries {
		names = append(names, filepath.Base(p))
	}
	return names, nil
}
