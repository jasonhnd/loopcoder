package workflowrun

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectWorktreeEscapesFindsProjectRootFiles(t *testing.T) {
	root := t.TempDir()
	// Simulate .../projects/pid/runs/wf_x/worktree
	wt := filepath.Join(root, "runs", "wf_abc", "worktree")
	if err := os.MkdirAll(wt, 0o700); err != nil {
		t.Fatal(err)
	}
	// Product file escaped to project root
	escape := filepath.Join(root, "NOTES.md")
	if err := os.WriteFile(escape, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Legitimate file inside worktree
	if err := os.WriteFile(filepath.Join(wt, "ok.go"), []byte("package ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := detectWorktreeEscapes(wt)
	if len(got) == 0 {
		t.Fatal("expected escape detection")
	}
	found := false
	for _, p := range got {
		if filepath.Base(p) == "NOTES.md" {
			found = true
		}
	}
	if !found {
		t.Fatalf("NOTES.md not in escapes: %v", got)
	}
}
