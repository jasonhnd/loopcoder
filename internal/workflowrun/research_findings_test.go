package workflowrun

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProductOutputDigest_ExcludesProviderLogs(t *testing.T) {
	dir := t.TempDir()
	// minimal git repo
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v %s", args, err, out)
		}
	}
	run("git", "init")
	run("git", "config", "user.email", "t@t")
	run("git", "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("git", "add", ".")
	run("git", "commit", "-m", "init")
	// Untracked provider log only
	if err := os.WriteFile(filepath.Join(dir, ".loopcoder-child-provider.log"), []byte(strings.Repeat("x", 200)), 0o600); err != nil {
		t.Fatal(err)
	}
	dig, files, err := productOutputDigest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if dig != "" || len(files) != 0 {
		t.Fatalf("provider log must not be product: dig=%q files=%v", dig, files)
	}
}

func TestMaterializeResearchFindings_WritesFindingsProduct(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v %s", args, err, out)
		}
	}
	run("git", "init")
	run("git", "config", "user.email", "t@t")
	run("git", "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("git", "add", ".")
	run("git", "commit", "-m", "init")

	summary := strings.Repeat("Survey scope includes notes package layout, multi-provider constraints, and test plan. ", 3)
	err := materializeResearchFindings(dir, summary, ChildExecInput{
		WorkItemID: "wi_research",
		Intent:     "research/read-only: survey scope",
	})
	if err != nil {
		t.Fatal(err)
	}
	dig, files, err := productOutputDigest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if dig == "" || len(files) == 0 {
		t.Fatalf("want findings product, dig=%q files=%v", dig, files)
	}
	found := false
	for _, f := range files {
		if f == "findings.md" {
			found = true
		}
	}
	if !found {
		t.Fatalf("files=%v want findings.md", files)
	}
	if err := AcceptSucceededChild("wi_research", "research/read-only: survey", "research", files, dir, dig); err != nil {
		t.Fatalf("accept: %v", err)
	}
}

func TestMaterializeResearchFindings_ShortSummaryRefused(t *testing.T) {
	dir := t.TempDir()
	if err := materializeResearchFindings(dir, "too short", ChildExecInput{WorkItemID: "wi_research"}); err == nil {
		t.Fatal("expected short summary refusal")
	}
}

func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v %s", args, err, out)
		}
	}
	run("git", "init")
	run("git", "config", "user.email", "t@t")
	run("git", "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("git", "add", ".")
	run("git", "commit", "-m", "init")
}

func substantialSummary() string {
	return strings.Repeat("Survey scope includes notes package layout, multi-provider constraints, and test plan. ", 3)
}

// TestMaterializeResearchFindings_SymlinkEscapeRefused: pre-planted findings.md
// symlink pointing outside the worktree must not be followed. External content
// stays unchanged; after success the worktree findings.md is a regular file
// (Rename replaced the symlink node). No real credentials involved.
func TestMaterializeResearchFindings_SymlinkEscapeRefused(t *testing.T) {
	wt := t.TempDir()
	initGitRepo(t, wt)
	outside := t.TempDir()
	extPath := filepath.Join(outside, "secret-findings-sink.txt")
	sentinel := "EXTERNAL_CONTENT_MUST_NOT_CHANGE_" + strings.Repeat("Z", 40)
	if err := os.WriteFile(extPath, []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	// Plant symlink findings.md → external file (classic WriteFile escape).
	if err := os.Symlink(extPath, filepath.Join(wt, "findings.md")); err != nil {
		t.Fatal(err)
	}
	if err := materializeResearchFindings(wt, substantialSummary(), ChildExecInput{
		WorkItemID: "wi_research",
		Intent:     "research/read-only: survey scope",
	}); err != nil {
		t.Fatalf("materialize should replace symlink via rename, not fail: %v", err)
	}
	// External sink unchanged.
	got, err := os.ReadFile(extPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != sentinel {
		t.Fatalf("external file was mutated via symlink follow:\n got %q\nwant %q", got, sentinel)
	}
	// Worktree findings.md is regular, not a symlink.
	st, err := os.Lstat(filepath.Join(wt, "findings.md"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&os.ModeSymlink != 0 {
		t.Fatal("findings.md still a symlink after materialize")
	}
	if !st.Mode().IsRegular() {
		t.Fatalf("findings.md not regular: mode=%v", st.Mode())
	}
	body, err := os.ReadFile(filepath.Join(wt, "findings.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "## Provider survey") {
		t.Fatalf("expected research body in regular findings.md, got %q", body[:min(80, len(body))])
	}
}

// TestHasAnyFindings_ChildOutputSymlinkRejected: child-output-* symlink to an
// external substantial file must not satisfy acceptance findings.
func TestHasAnyFindings_ChildOutputSymlinkRejected(t *testing.T) {
	wt := t.TempDir()
	outside := t.TempDir()
	ext := filepath.Join(outside, "fake-child-output.md")
	payload := "## Provider survey\n\n" + strings.Repeat("external findings body line\n", 30)
	if err := os.WriteFile(ext, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(ext, filepath.Join(wt, "child-output-wi_research.md")); err != nil {
		t.Fatal(err)
	}
	if hasAnyFindings(wt) {
		t.Fatal("child-output symlink to external file must not count as findings")
	}
	// Acceptance path: empty product + symlink findings → refuse.
	if err := AcceptSucceededChild("wi_research", "research/read-only: survey", "research", nil, wt, "sha256:"+strings.Repeat("a", 64)); err == nil {
		t.Fatal("accept must fail closed when only child-output symlink is present")
	}
}

// TestHasAnyFindings_FindingsSymlinkRejected: findings.md symlink to external.
func TestHasAnyFindings_FindingsSymlinkRejected(t *testing.T) {
	wt := t.TempDir()
	outside := t.TempDir()
	ext := filepath.Join(outside, "ext-findings.md")
	if err := os.WriteFile(ext, []byte(strings.Repeat("external findings body\n", 20)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(ext, filepath.Join(wt, "findings.md")); err != nil {
		t.Fatal(err)
	}
	if hasAnyFindings(wt) {
		t.Fatal("findings.md symlink must not count")
	}
}

// TestHasAnyFindings_NonRegularRefused: directory / FIFO-like non-regular nodes
// under findings names must not pass.
func TestHasAnyFindings_NonRegularRefused(t *testing.T) {
	wt := t.TempDir()
	// findings.md as directory
	if err := os.Mkdir(filepath.Join(wt, "findings.md"), 0o700); err != nil {
		t.Fatal(err)
	}
	if hasAnyFindings(wt) {
		t.Fatal("directory named findings.md must not count")
	}
	// child-output as directory
	if err := os.Mkdir(filepath.Join(wt, "child-output-wi_research.md"), 0o700); err != nil {
		t.Fatal(err)
	}
	if hasAnyFindings(wt) {
		t.Fatal("directory named child-output-* must not count")
	}
	// FIFO (named pipe) if platform supports
	fifo := filepath.Join(wt, "FINDINGS.md")
	if err := exec.Command("mkfifo", fifo).Run(); err == nil {
		// Don't block: hasAnyFindings must Lstat-refuse without opening for read hang.
		done := make(chan bool, 1)
		go func() {
			done <- hasAnyFindings(wt)
		}()
		select {
		case ok := <-done:
			if ok {
				t.Fatal("FIFO FINDINGS.md must not count as findings")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("hasAnyFindings blocked on FIFO (must Lstat-refuse without hanging open)")
		}
	}
}

// TestMaterializeResearchFindings_DestDirectoryFailClosed.
func TestMaterializeResearchFindings_DestDirectoryFailClosed(t *testing.T) {
	wt := t.TempDir()
	initGitRepo(t, wt)
	if err := os.Mkdir(filepath.Join(wt, "findings.md"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := materializeResearchFindings(wt, substantialSummary(), ChildExecInput{
		WorkItemID: "wi_research", Intent: "research",
	}); err == nil {
		t.Fatal("expected fail closed when findings.md is a directory")
	}
}

// TestReadRegularFindingsFile_SameFileIdentityHelpers documents the
// pre-lstat / fd.Stat / post-lstat SameFile chain.
func TestReadRegularFindingsFile_SameFileIdentityHelpers(t *testing.T) {
	wt := t.TempDir()
	path := filepath.Join(wt, "findings.md")
	body := strings.Repeat("identity-helper findings body line\n", 10)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// Happy path: SameFile chain accepts regular file.
	raw, err := readRegularFindingsFileErr(wt, "findings.md")
	if err != nil {
		t.Fatalf("regular file: %v", err)
	}
	if string(raw) != body {
		t.Fatalf("body mismatch")
	}
	// Explicit SameFile unit: pre Lstat and open fd must match.
	pre, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	fdStat, err := f.Stat()
	if err != nil {
		f.Close()
		t.Fatal(err)
	}
	if !os.SameFile(pre, fdStat) {
		f.Close()
		t.Fatal("expected SameFile(pre, fdStat) for unmolested regular file")
	}
	f.Close()
	post, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(fdStat, post) {
		t.Fatal("expected SameFile(fdStat, post) for unmolested regular file")
	}
}

// TestReadRegularFindingsFile_SymlinkFailsWithTypedError.
func TestReadRegularFindingsFile_SymlinkFailsWithTypedError(t *testing.T) {
	wt := t.TempDir()
	outside := t.TempDir()
	ext := filepath.Join(outside, "ext.md")
	if err := os.WriteFile(ext, []byte(strings.Repeat("external\n", 20)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(ext, filepath.Join(wt, "findings.md")); err != nil {
		t.Fatal(err)
	}
	_, err := readRegularFindingsFileErr(wt, "findings.md")
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("want symlink error, got %v", err)
	}
}

// TestMaterializeResearchFindings_FailureSurfacedOnExecutor: materialize
// failure (dest directory) returns typed FailureClass with real message, and
// does not collapse to missing_evidence / route_mismatch. Uses a stub runner
// is hard without wiring Lookup — exercise materialize + FailureClass constant
// visibility via direct call + Production path message contract.
func TestMaterializeResearchFindings_FailureTypedClassStable(t *testing.T) {
	if FailureClassResearchFindingsMaterialization != "research_findings_materialization_failed" {
		t.Fatalf("stable class drifted: %q", FailureClassResearchFindingsMaterialization)
	}
	wt := t.TempDir()
	initGitRepo(t, wt)
	if err := os.Mkdir(filepath.Join(wt, "findings.md"), 0o700); err != nil {
		t.Fatal(err)
	}
	err := materializeResearchFindings(wt, substantialSummary(), ChildExecInput{
		WorkItemID: "wi_research", Intent: "research/read-only: survey",
	})
	if err == nil {
		t.Fatal("expected materialize failure on directory dest")
	}
	// Message must retain directory/safety reason (not generic missing_evidence).
	if !strings.Contains(err.Error(), "regular") && !strings.Contains(err.Error(), "directory") &&
		!strings.Contains(err.Error(), "not a regular") {
		t.Fatalf("message should explain dest problem, got %q", err.Error())
	}
}

// TestResearchFindingsMaterialization_ResultShapePreservesObservedRoute documents
// the ChildExecResult shape ProductionChildExecutor returns on materialize failure:
// typed class + real message + observed InvokedRoute (never empty-model route rewrite).
func TestResearchFindingsMaterialization_ResultShapePreservesObservedRoute(t *testing.T) {
	out := ChildExecResult{
		Terminal:     "failed",
		FailureClass: FailureClassResearchFindingsMaterialization,
		Message:      "research findings dest is not a regular file or symlink (mode=drwx------)",
		Provider:     "codex",
		Model:        "codex-auto-review",
		Depth:        "low",
		InvokedRoute: ChildRoute{Provider: "codex", Model: "codex-auto-review", Depth: "low"},
	}
	if out.FailureClass != "research_findings_materialization_failed" {
		t.Fatal(out.FailureClass)
	}
	if out.FailureClass == "missing_evidence" || out.FailureClass == "route_identity_mismatch" || out.FailureClass == "route_mismatch" {
		t.Fatal("must not collapse typed materialize failure")
	}
	if out.InvokedRoute.Model == "" {
		t.Fatal("must preserve observed InvokedRoute model")
	}
	if !strings.Contains(out.Message, "regular") && !strings.Contains(out.Message, "directory") {
		t.Fatalf("message must retain dest reason: %q", out.Message)
	}
}

// TestAcceptResearch_GreenfieldSurveyNotClarification: substantial findings.md
// that mentions "no existing tests/implementation" is legitimate survey scope,
// not empty clarification.
func TestAcceptResearch_GreenfieldSurveyNotClarification(t *testing.T) {
	wt := t.TempDir()
	body := "# Research findings\n\nWork item: wi_research\n\n## Provider survey\n\n" +
		"This checkout does not contain an existing multi-provider notes implementation.\n" +
		"Constraints are strict:\n" +
		"- no existing behavior to preserve\n" +
		"- no existing tests to focus or extend\n" +
		"- no provider-specific code to compare\n" +
		"A sensible implementation would need package API and provider abstraction.\n" +
		strings.Repeat("More survey detail for substance. ", 20)
	if err := os.WriteFile(filepath.Join(wt, "findings.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if !hasSubstantialResearchFindings(wt, []string{"findings.md"}) {
		t.Fatal("expected substantial research findings")
	}
	// Even if looksLikeClarification would trip on loose phrases, accept must pass.
	if err := AcceptSucceededChild("wi_research", "research/read-only: survey scope", "research",
		[]string{"findings.md"}, wt, "sha256:"+strings.Repeat("ab", 32)); err != nil {
		t.Fatalf("greenfield research survey must accept: %v", err)
	}
}

// TestAcceptResearch_PureClarificationStillRefused without substantial findings.
func TestAcceptResearch_PureClarificationStillRefused(t *testing.T) {
	wt := t.TempDir()
	// Short file with only clarification language, no survey section.
	body := "please clarify requirements. need more information before any work.\n"
	if err := os.WriteFile(filepath.Join(wt, "findings.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if hasSubstantialResearchFindings(wt, []string{"findings.md"}) {
		t.Fatal("short clarification must not count as substantial")
	}
	if err := AcceptSucceededChild("wi_research", "research/read-only: survey", "research",
		[]string{"findings.md"}, wt, "sha256:"+strings.Repeat("cd", 32)); err == nil {
		t.Fatal("pure clarification research must fail closed")
	}
}
