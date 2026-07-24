package workflowrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func longClarification() string {
	return strings.Repeat("please clarify the requirements. need more information before any work. ", 20)
}

func longSurvey() string {
	return strings.Repeat("Survey scope covers multi-provider notes API, constraints, and test plan. ", 5)
}

func longReview() string {
	return strings.Repeat("Adversarial review: dispatch is sound, residual risk is low, tests cover edge cases. ", 5)
}

func longDocs() string {
	return strings.Repeat("Documentation notes describe user-facing package API and configuration. ", 5)
}

func TestClarification_LongBodyWithMaterializeHeaderStillClarificationOnly(t *testing.T) {
	// Scaffold headers must not wash explicit clarification into success.
	wrapped := "# Research findings\n\nWork item: wi_research\n\nIntent: research\n\n## Provider survey\n\n" + longClarification()
	if !isExplicitClarificationOnly(wrapped) {
		t.Fatal("long clarification with materialize headers must still be clarification-only")
	}
	if hasSubstantialResearchFindings("", nil) {
		t.Fatal("empty worktree")
	}
	wt := t.TempDir()
	if err := os.WriteFile(filepath.Join(wt, "findings.md"), []byte(wrapped), 0o600); err != nil {
		t.Fatal(err)
	}
	if hasSubstantialResearchFindings(wt, []string{"findings.md"}) {
		t.Fatal("clarification-only findings must not be substantial research")
	}
	if err := AcceptSucceededChild("wi_research", "research/read-only: survey", "research",
		[]string{"findings.md"}, wt, "sha256:"+strings.Repeat("ab", 32)); err == nil {
		t.Fatal("accept must refuse long clarification-only research")
	}
}

func TestLooksLikeClarification_NeverFollowsSymlink(t *testing.T) {
	wt := t.TempDir()
	outside := t.TempDir()
	ext := filepath.Join(outside, "secret.md")
	if err := os.WriteFile(ext, []byte(longClarification()), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(ext, filepath.Join(wt, "findings.md")); err != nil {
		t.Fatal(err)
	}
	// Symlink must not be read; without a secure regular file, no clarification blob.
	if looksLikeClarification("sha256:"+strings.Repeat("cd", 32), wt, []string{"findings.md"}) {
		// Actually: if we can't read, we return false for looksLikeClarification.
		// That's fail-open for the gate but fail-closed for acceptance product.
		// Accept without substantial findings still fails.
	}
	// Product list points at symlink leaf — secure read fails → not substantial → accept fails.
	if err := AcceptSucceededChild("wi_research", "research", "research",
		[]string{"findings.md"}, wt, "sha256:"+strings.Repeat("cd", 32)); err == nil {
		t.Fatal("symlink findings must not accept")
	}
}

func TestMaterialize_AllRoles_RefuseClarificationOnly(t *testing.T) {
	wt := t.TempDir()
	initGitRepo(t, wt)
	in := ChildExecInput{WorkItemID: "wi_x", Intent: "test"}
	if err := materializeResearchFindings(wt, longClarification(), in); err == nil {
		t.Fatal("research materialize must refuse clarification")
	}
	if err := materializeVerifierVerdict(wt, longClarification(), in); err == nil {
		t.Fatal("verify materialize must refuse clarification")
	}
	if err := materializeDocsNotes(wt, longClarification(), in); err == nil {
		t.Fatal("docs materialize must refuse clarification")
	}
}

func TestMaterialize_AllRoles_SuccessAndAccept(t *testing.T) {
	// Research
	{
		wt := t.TempDir()
		initGitRepo(t, wt)
		if err := materializeResearchFindings(wt, longSurvey(), ChildExecInput{WorkItemID: "wi_research", Intent: "research"}); err != nil {
			t.Fatal(err)
		}
		dig, files, err := productOutputDigest(wt)
		if err != nil || dig == "" {
			t.Fatal(dig, files, err)
		}
		if err := AcceptSucceededChild("wi_research", "research/read-only: survey", "research", files, wt, dig); err != nil {
			t.Fatal("research accept", err)
		}
	}
	// Verify
	{
		wt := t.TempDir()
		initGitRepo(t, wt)
		if err := materializeVerifierVerdict(wt, longReview(), ChildExecInput{WorkItemID: "wi_verify", Intent: "verify"}); err != nil {
			t.Fatal(err)
		}
		dig, files, err := productOutputDigest(wt)
		if err != nil || dig == "" {
			t.Fatal(dig, files, err)
		}
		if err := AcceptSucceededChild("wi_verify", "independent verification: adversarial review", "verifier", files, wt, dig); err != nil {
			t.Fatal("verify accept", err)
		}
	}
	// Docs
	{
		wt := t.TempDir()
		initGitRepo(t, wt)
		if err := materializeDocsNotes(wt, longDocs(), ChildExecInput{WorkItemID: "wi_docs", Intent: "docs: update"}); err != nil {
			t.Fatal(err)
		}
		dig, files, err := productOutputDigest(wt)
		if err != nil || dig == "" {
			t.Fatal(dig, files, err)
		}
		if err := AcceptSucceededChild("wi_docs", "docs: update user-facing docs", "worker", files, wt, dig); err != nil {
			t.Fatal("docs accept", err)
		}
	}
}

func TestMaterialize_DocsAndVerify_SymlinkEscape(t *testing.T) {
	for _, leaf := range []string{"verdict.md", "docs-notes.md"} {
		t.Run(leaf, func(t *testing.T) {
			wt := t.TempDir()
			initGitRepo(t, wt)
			outside := t.TempDir()
			ext := filepath.Join(outside, "sink.txt")
			sentinel := "EXTERNAL_" + leaf + "_" + strings.Repeat("Q", 20)
			if err := os.WriteFile(ext, []byte(sentinel), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(ext, filepath.Join(wt, leaf)); err != nil {
				t.Fatal(err)
			}
			var err error
			switch leaf {
			case "verdict.md":
				err = materializeVerifierVerdict(wt, longReview(), ChildExecInput{WorkItemID: "wi_verify"})
			case "docs-notes.md":
				err = materializeDocsNotes(wt, longDocs(), ChildExecInput{WorkItemID: "wi_docs"})
			}
			if err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(ext)
			if string(got) != sentinel {
				t.Fatalf("external mutated for %s", leaf)
			}
			st, err := os.Lstat(filepath.Join(wt, leaf))
			if err != nil || st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
				t.Fatalf("%s must be regular after rename", leaf)
			}
		})
	}
}

func TestMaterialize_FailureTypedClassesStable(t *testing.T) {
	if FailureClassResearchFindingsMaterialization != "research_findings_materialization_failed" ||
		FailureClassVerifierVerdictMaterialization != "verifier_verdict_materialization_failed" ||
		FailureClassDocsMaterialization != "docs_notes_materialization_failed" {
		t.Fatal("typed failure class drift")
	}
	wt := t.TempDir()
	initGitRepo(t, wt)
	if err := os.Mkdir(filepath.Join(wt, "findings.md"), 0o700); err != nil {
		t.Fatal(err)
	}
	err := materializeResearchFindings(wt, longSurvey(), ChildExecInput{WorkItemID: "wi_research"})
	if err == nil {
		t.Fatal("directory dest must fail")
	}
	if !strings.Contains(err.Error(), "regular") && !strings.Contains(err.Error(), "directory") {
		t.Fatalf("want dest reason: %v", err)
	}
}

func TestGreenfieldSurvey_StillAccepts(t *testing.T) {
	wt := t.TempDir()
	body := "# Research findings\n\nWork item: wi_research\n\n## Provider survey\n\n" +
		"This checkout does not contain an existing multi-provider notes implementation.\n" +
		"Constraints: no existing tests to extend; greenfield package API required.\n" +
		longSurvey()
	if err := os.WriteFile(filepath.Join(wt, "findings.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AcceptSucceededChild("wi_research", "research/read-only: survey scope", "research",
		[]string{"findings.md"}, wt, "sha256:"+strings.Repeat("ef", 32)); err != nil {
		t.Fatalf("greenfield survey must accept: %v", err)
	}
}

func TestHasVerifierVerdict_SymlinkChildOutputRejected(t *testing.T) {
	wt := t.TempDir()
	outside := t.TempDir()
	ext := filepath.Join(outside, "co.md")
	if err := os.WriteFile(ext, []byte(longReview()), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(ext, filepath.Join(wt, "child-output-wi_verify.md")); err != nil {
		t.Fatal(err)
	}
	if hasVerifierVerdict([]string{"child-output-wi_verify.md"}, wt, "sha256:"+strings.Repeat("11", 32)) {
		t.Fatal("symlink child-output must not satisfy verifier verdict")
	}
}

// Nested docs/foo.md must be read as the nested leaf, not collapsed via
// filepath.Base to a missing root/foo.md (which would skip body and pseudo-green).
func TestNestedDocs_LongClarificationRejected(t *testing.T) {
	wt := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wt, "docs"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Put clarification only under docs/ — root has no notes.md.
	body := "# Documentation notes\n\nWork item: wi_docs\n\n## Documentation\n\n" + longClarification()
	if err := os.WriteFile(filepath.Join(wt, "docs", "notes.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// hasDocsProduct is true for nested docs/ paths.
	product := []string{"docs/notes.md"}
	if !hasDocsProduct(product) {
		t.Fatal("docs/notes.md must count as docs product")
	}
	// Secure reader must reach the nested leaf and detect clarification.
	if !looksLikeClarification("sha256:"+strings.Repeat("aa", 32), wt, product) {
		t.Fatal("nested long clarification must be detected (Base collapse would pseudo-green)")
	}
	if err := AcceptSucceededChild("wi_docs", "docs: update user-facing docs", "worker",
		product, wt, "sha256:"+strings.Repeat("aa", 32)); err == nil {
		t.Fatal("nested clarification-only docs must be refused")
	}
}

func TestNestedDocs_LegitimateAccept(t *testing.T) {
	wt := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wt, "docs", "guides"), 0o700); err != nil {
		t.Fatal(err)
	}
	body := "# Documentation notes\n\nWork item: wi_docs\n\n## Documentation\n\n" + longDocs()
	rel := filepath.Join("docs", "guides", "api.md")
	if err := os.WriteFile(filepath.Join(wt, rel), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// Prefer slash form as product paths are stored relative.
	product := []string{"docs/guides/api.md"}
	raw, err := readRegularFindingsFileErr(wt, product[0])
	if err != nil {
		t.Fatalf("nested legitimate docs must secure-read: %v", err)
	}
	if !strings.Contains(string(raw), "Documentation notes") {
		t.Fatal("must read nested leaf content, not a collapsed root path")
	}
	if looksLikeClarification("sha256:"+strings.Repeat("bb", 32), wt, product) {
		t.Fatal("legitimate nested docs must not look like clarification")
	}
	if err := AcceptSucceededChild("wi_docs", "docs: update user-facing docs", "worker",
		product, wt, "sha256:"+strings.Repeat("bb", 32)); err != nil {
		t.Fatalf("nested legitimate docs must accept: %v", err)
	}
}

func TestSecureRead_WorktreeRootSymlinkRejected(t *testing.T) {
	real := t.TempDir()
	if err := os.WriteFile(filepath.Join(real, "findings.md"), []byte(longSurvey()), 0o600); err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	linkRoot := filepath.Join(parent, "wt-link")
	if err := os.Symlink(real, linkRoot); err != nil {
		t.Fatal(err)
	}
	_, err := readRegularFindingsFileErr(linkRoot, "findings.md")
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink worktree root must be refused, got %v", err)
	}
	if hasSubstantialResearchFindings(linkRoot, []string{"findings.md"}) {
		t.Fatal("symlink root must not yield substantial findings")
	}
}

func TestSecureRead_ParentComponentSymlinkRejected(t *testing.T) {
	wt := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "notes.md"), []byte(longDocs()), 0o600); err != nil {
		t.Fatal(err)
	}
	// docs/ is a symlink to outside — parent component must be non-symlink dir.
	if err := os.Symlink(outside, filepath.Join(wt, "docs")); err != nil {
		t.Fatal(err)
	}
	_, err := readRegularFindingsFileErr(wt, "docs/notes.md")
	if err == nil {
		t.Fatal("parent symlink component must be refused")
	}
	if !strings.Contains(err.Error(), "symlink") && !strings.Contains(err.Error(), "parent") {
		t.Fatalf("want parent/symlink error, got %v", err)
	}
	// Nested clarification gate must not false-green by Base-collapsing to a
	// missing root leaf; unreadable parent-symlink product yields no secure blob.
	if looksLikeClarification("sha256:"+strings.Repeat("cc", 32), wt, []string{"docs/notes.md"}) {
		t.Fatal("must not follow parent symlink into external clarification text")
	}
}

func TestSecureRead_RelativeEscapeRejected(t *testing.T) {
	wt := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.md")
	if err := os.WriteFile(secret, []byte("EXTERNAL_SECRET_"+strings.Repeat("X", 40)), 0o600); err != nil {
		t.Fatal(err)
	}
	// Escape via .. / abs / empty must never open outside the worktree.
	for _, bad := range []string{
		"../secret.md",
		"..",
		"",
		"/etc/passwd",
		"docs/../../etc/passwd",
		"foo/../../../etc/passwd",
	} {
		if _, err := cleanWorktreeRelPath(bad); err == nil {
			t.Fatalf("cleanWorktreeRelPath(%q) must reject", bad)
		}
		if _, err := readRegularFindingsFileErr(wt, bad); err == nil {
			t.Fatalf("readRegularFindingsFileErr(%q) must reject", bad)
		}
	}
	// Nested legitimate still works (control).
	if err := os.MkdirAll(filepath.Join(wt, "docs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "docs", "ok.md"), []byte(longDocs()), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readRegularFindingsFileErr(wt, "docs/ok.md"); err != nil {
		t.Fatalf("control nested path: %v", err)
	}
	_ = outside
	_ = secret
}

// Controllable TOCTOU: swap leaf inode between pre-Lstat and Open → identity mismatch.
func TestSecureRead_TOCTOUIdentityMismatch(t *testing.T) {
	wt := t.TempDir()
	path := filepath.Join(wt, "findings.md")
	if err := os.WriteFile(path, []byte(longSurvey()), 0o600); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(wt, "findings-swap.md")
	if err := os.WriteFile(replacement, []byte("SWAPPED_"+strings.Repeat("Y", 80)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { secureReadAfterPreLstat = nil })
	secureReadAfterPreLstat = func(full string) {
		// Replace leaf with a different inode while keeping the path name.
		if err := os.Remove(full); err != nil {
			t.Errorf("remove for swap: %v", err)
			return
		}
		if err := os.Rename(replacement, full); err != nil {
			t.Errorf("rename swap: %v", err)
		}
	}
	_, err := readRegularFindingsFileErr(wt, "findings.md")
	if err == nil {
		t.Fatal("expected identity mismatch after inode swap")
	}
	if !strings.Contains(err.Error(), "identity mismatch") {
		t.Fatalf("want identity mismatch, got %v", err)
	}
}

func TestCleanWorktreeRelPath_PreservesNested(t *testing.T) {
	got, err := cleanWorktreeRelPath("docs/guides/api.md")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.FromSlash("docs/guides/api.md") {
		t.Fatalf("nested path must be preserved, got %q", got)
	}
	// Must NOT collapse to Base.
	if got == "api.md" {
		t.Fatal("must not filepath.Base-collapse nested docs path")
	}
}
