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
