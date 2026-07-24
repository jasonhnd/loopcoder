package workflowrun_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/workflowrun"
)

func TestAcceptTestsRequiresTestFiles(t *testing.T) {
	wt := t.TempDir()
	err := workflowrun.AcceptSucceededChild("wi_tests", "tests: add focused tests", "worker",
		[]string{"notes/notes.go"}, wt, "sha256:abc")
	if err == nil {
		t.Fatal("expected fail without test files")
	}
	// with test file
	_ = os.MkdirAll(filepath.Join(wt, "notes"), 0o700)
	_ = os.WriteFile(filepath.Join(wt, "notes/notes_test.go"), []byte("package notes\n"), 0o600)
	err = workflowrun.AcceptSucceededChild("wi_tests", "tests: add focused tests", "worker",
		[]string{"notes/notes_test.go"}, wt, "sha256:abc")
	if err != nil {
		t.Fatal(err)
	}
}

func TestAcceptVerifyRejectsNoImplementation(t *testing.T) {
	wt := t.TempDir()
	_ = os.WriteFile(filepath.Join(wt, "child-output-wi_verify.md"), []byte(
		"# Review\n\nThe repository contains no implementation or test suite.\n"), 0o600)
	err := workflowrun.AcceptSucceededChild("wi_verify", "independent verification", "verifier",
		[]string{"child-output-wi_verify.md"}, wt, "sha256:dead")
	if err == nil {
		t.Fatal("expected clarification refusal")
	}
}

func TestAcceptImplementRejectsMetaOnly(t *testing.T) {
	err := workflowrun.AcceptSucceededChild("wi_implement", "implementation: deliver change", "worker",
		[]string{".loopcoder/child-evidence/x.json", "child-output-wi_implement.md"}, "", "sha256:x")
	if err == nil {
		t.Fatal("meta-only implement must fail")
	}
	// Filename alone is insufficient — need a secure regular source leaf.
	wt := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wt, "pkg"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "pkg", "foo.go"), []byte("package pkg\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = workflowrun.AcceptSucceededChild("wi_implement", "implementation: deliver change", "worker",
		[]string{"pkg/foo.go"}, wt, "sha256:x")
	if err != nil {
		t.Fatal(err)
	}
}

func TestAcceptClarificationPhrase(t *testing.T) {
	wt := t.TempDir()
	_ = os.WriteFile(filepath.Join(wt, "notes.md"), []byte("Please clarify the requirements before I proceed.\n"), 0o600)
	err := workflowrun.AcceptSucceededChild("wi_research", "research survey", "research",
		[]string{"notes.md"}, wt, "sha256:y")
	if err == nil || !strings.Contains(err.Error(), "clarification") {
		t.Fatalf("want clarification refuse, got %v", err)
	}
}

func TestClassifyTaskRole(t *testing.T) {
	if workflowrun.ClassifyTaskRole("wi_tests", "tests: add", "worker") != workflowrun.RoleTests {
		t.Fatal("tests")
	}
	if workflowrun.ClassifyTaskRole("wi_verify", "independent verification", "verifier") != workflowrun.RoleVerify {
		t.Fatal("verify")
	}
	// Goal text embedding "with tests" must not reclassify implement.
	if workflowrun.ClassifyTaskRole("wi_implement",
		"implementation: deliver the change for: multi-provider notes with tests and independent verification",
		"worker") != workflowrun.RoleImplement {
		t.Fatal("implement misclassified by goal text")
	}
	// Implement with only child-output stub fails.
	err := workflowrun.AcceptSucceededChild("wi_implement",
		"implementation: deliver the change for: multi-provider notes with tests",
		"worker",
		[]string{"child-output-wi_implement.md"}, "", "sha256:x")
	if err == nil {
		t.Fatal("implement child-output-only must fail under correct role")
	}
}
