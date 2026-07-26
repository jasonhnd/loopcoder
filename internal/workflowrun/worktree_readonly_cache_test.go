package workflowrun

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseChildWorktreeRealGitReadOnlyModuleCache(t *testing.T) {
	repo := initCleanupGitRepo(t)
	home := filepath.Join(t.TempDir(), "loopcoder-home")
	wt, err := allocateChildWorktree(
		home, "proj-cleanup", "graph-cleanup", "wi_tests", "att-cleanup-g0",
		repo, "HEAD",
	)
	if err != nil {
		t.Fatal(err)
	}
	moduleDir := filepath.Join(wt, ".cache", "gomod", "example.test", "module@v1.0.0", "nested")
	if err := os.MkdirAll(moduleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	moduleFile := filepath.Join(moduleDir, "module.go")
	if err := os.WriteFile(moduleFile, []byte("package module\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	for path := moduleDir; path != filepath.Join(wt, ".cache"); path = filepath.Dir(path) {
		if err := os.Chmod(path, 0o555); err != nil {
			t.Fatal(err)
		}
	}

	if err := releaseChildWorktree(repo, wt); err != nil {
		t.Fatalf("release read-only module cache: %v", err)
	}
	if _, err := os.Lstat(wt); !os.IsNotExist(err) {
		t.Fatalf("released worktree still exists: %v", err)
	}
	out := cleanupGit(t, repo, "worktree", "list", "--porcelain")
	if strings.Contains(out, wt) {
		t.Fatalf("released worktree still registered:\n%s", out)
	}
}

func TestOwnedModuleCacheCleanupRefusesSymlinkWithoutMutatingTarget(t *testing.T) {
	repo := initCleanupGitRepo(t)
	home := filepath.Join(t.TempDir(), "loopcoder-home")
	wt, err := allocateChildWorktree(
		home, "proj-symlink", "graph-symlink", "wi_tests", "att-symlink-g0",
		repo, "HEAD",
	)
	if err != nil {
		t.Fatal(err)
	}
	external := t.TempDir()
	externalFile := filepath.Join(external, "outside.txt")
	if err := os.WriteFile(externalFile, []byte("unchanged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cacheRoot := filepath.Join(wt, ".cache", "gomod")
	if err := os.MkdirAll(cacheRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(cacheRoot, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cacheRoot, 0o555); err != nil {
		t.Fatal(err)
	}
	defer forceRemoveTestTree(filepath.Dir(filepath.Dir(wt)))

	err = releaseChildWorktree(repo, wt)
	if err == nil || !strings.Contains(err.Error(), "symlink refused") {
		t.Fatalf("want fail-closed symlink refusal, got %v", err)
	}
	raw, readErr := os.ReadFile(externalFile)
	if readErr != nil || string(raw) != "unchanged\n" {
		t.Fatalf("external target changed: read=%v content=%q", readErr, raw)
	}
	st, statErr := os.Lstat(cacheRoot)
	if statErr != nil {
		t.Fatalf("cache root unexpectedly removed: %v", statErr)
	}
	if st.Mode().Perm() != 0o555 {
		t.Fatalf("cache permissions mutated before symlink refusal: %o", st.Mode().Perm())
	}
}

func TestOwnedModuleCacheCleanupRefusesUnmarkedPath(t *testing.T) {
	root := filepath.Join(t.TempDir(), "projects", "fake", "runs", "wf_fake", "worktree")
	cacheRoot := filepath.Join(root, ".cache", "gomod", "module")
	if err := os.MkdirAll(cacheRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheRoot, "module.go"), []byte("package module\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cacheRoot, 0o555); err != nil {
		t.Fatal(err)
	}
	defer forceRemoveTestTree(filepath.Dir(filepath.Dir(filepath.Dir(root))))

	err := releaseChildWorktree("", root)
	if err == nil || !strings.Contains(err.Error(), "ownership marker") {
		t.Fatalf("want ownership refusal, got %v", err)
	}
	st, statErr := os.Lstat(cacheRoot)
	if statErr != nil {
		t.Fatalf("unowned cache unexpectedly removed: %v", statErr)
	}
	if st.Mode().Perm() != 0o555 {
		t.Fatalf("unowned cache permissions mutated: %o", st.Mode().Perm())
	}
}

func initCleanupGitRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	cleanupGit(t, repo, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# cleanup test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cleanupGit(t, repo, "add", "README.md")
	cleanupGit(t, repo, "commit", "-m", "initial")
	return repo
}

func cleanupGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=LoopCoder Test",
		"GIT_AUTHOR_EMAIL=loopcoder-test@example.invalid",
		"GIT_COMMITTER_NAME=LoopCoder Test",
		"GIT_COMMITTER_EMAIL=loopcoder-test@example.invalid",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func forceRemoveTestTree(root string) {
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
			_ = os.Chmod(path, info.Mode()|0o700)
		}
		return nil
	})
	_ = os.RemoveAll(root)
}
