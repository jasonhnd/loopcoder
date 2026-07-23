package workflowrun

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiscoverProductFiles_UnstagedModified(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	run("git", "init")
	_ = os.WriteFile(filepath.Join(dir, "notes.go"), []byte("package notes\n"), 0o600)
	run("git", "add", "notes.go")
	run("git", "commit", "-m", "init")
	_ = os.WriteFile(filepath.Join(dir, "notes.go"), []byte("package notes\nfunc X(){}\n"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "child-output-x.md"), []byte("# x\n"), 0o600)
	run("git", "add", "child-output-x.md")

	files, err := discoverProductFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(files, ",")
	if !strings.Contains(joined, "notes.go") {
		t.Fatalf("want notes.go in discovered %v", files)
	}
	if !strings.Contains(joined, "child-output-x.md") {
		t.Fatalf("want child-output in discovered %v", files)
	}
	for _, f := range files {
		if f == "otes.go" || strings.HasPrefix(f, "otes.") {
			t.Fatalf("porcelain TrimSpace bug still present: %v", files)
		}
	}
}

func TestDiffDirTree_IgnoresGit(t *testing.T) {
	before := dirSnap{}
	after := dirSnap{".git/objects/ab/cd": "deadbeef", "notes.go": "cafe"}
	mut := diffDirTree(before, after, "")
	if len(mut) != 1 || mut[0] != "notes.go" {
		t.Fatalf("mut=%v", mut)
	}
}
