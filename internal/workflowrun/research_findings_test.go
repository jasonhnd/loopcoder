package workflowrun

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
