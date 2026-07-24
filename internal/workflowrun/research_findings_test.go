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
