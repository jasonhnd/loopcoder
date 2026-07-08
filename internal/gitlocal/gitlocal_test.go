package gitlocal

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestProtectLoopcoderStateAppendsIdempotently(t *testing.T) {
	root := t.TempDir()
	repo := "repo"
	excludePath := filepath.Join(root, ".git", "info", "exclude")
	gitignorePath := filepath.Join(root, ".gitignore")
	if err := os.WriteFile(gitignorePath, []byte("kept\n"), 0o644); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}
	deps := DefaultDeps()
	deps.Git = fakeExcludePathGit(repo, excludePath)

	first, err := ProtectLoopcoderStateWithDeps(context.Background(), repo, deps)
	if err != nil {
		t.Fatalf("ProtectLoopcoderState first run: %v", err)
	}
	if filepath.Clean(first.ExcludePath) != filepath.Clean(excludePath) {
		t.Fatalf("ExcludePath = %q, want %q", first.ExcludePath, excludePath)
	}
	if first.Status != ProtectCreated {
		t.Fatalf("first status = %s, want created", first.Status)
	}
	firstData := readFile(t, first.ExcludePath)
	if got := strings.Count(string(firstData), ManagedExcludeEntry); got != 1 {
		t.Fatalf("managed entry count after first run = %d, want 1:\n%s", got, firstData)
	}
	if !bytes.Contains(firstData, []byte(ManagedExcludeComment+"\n"+ManagedExcludeEntry+"\n")) {
		t.Fatalf("managed block missing from exclude:\n%s", firstData)
	}

	second, err := ProtectLoopcoderStateWithDeps(context.Background(), repo, deps)
	if err != nil {
		t.Fatalf("ProtectLoopcoderState second run: %v", err)
	}
	if second.Status != ProtectUnchanged {
		t.Fatalf("second status = %s, want unchanged", second.Status)
	}
	secondData := readFile(t, second.ExcludePath)
	if !bytes.Equal(firstData, secondData) {
		t.Fatalf("exclude changed on idempotent run:\nfirst=%s\nsecond=%s", firstData, secondData)
	}
	if got := string(readFile(t, gitignorePath)); got != "kept\n" {
		t.Fatalf(".gitignore changed to %q", got)
	}
}

func TestProtectLoopcoderStateDoesNotDuplicateExistingEntry(t *testing.T) {
	root := t.TempDir()
	repo := "repo"
	excludePath := filepath.Join(root, ".git", "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(excludePath), 0o755); err != nil {
		t.Fatalf("create exclude dir: %v", err)
	}
	existing := []byte("# user local excludes\n.loopcoder/\n")
	if err := os.WriteFile(excludePath, existing, 0o644); err != nil {
		t.Fatalf("write existing exclude: %v", err)
	}
	deps := DefaultDeps()
	deps.Git = fakeExcludePathGit(repo, excludePath)

	result, err := ProtectLoopcoderStateWithDeps(context.Background(), repo, deps)
	if err != nil {
		t.Fatalf("ProtectLoopcoderState: %v", err)
	}
	if result.Status != ProtectUnchanged {
		t.Fatalf("status = %s, want unchanged", result.Status)
	}
	if got := readFile(t, excludePath); !bytes.Equal(got, existing) {
		t.Fatalf("existing exclude entry was rewritten:\n%s", got)
	}
}

func TestProtectLoopcoderStateUsesGitPathForLinkedWorktree(t *testing.T) {
	repo := initGitRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "hello\n")
	runGit(t, repo, "add", "README.md")
	runGit(t, repo, "-c", "user.email=loopcoder-test@example.com", "-c", "user.name=Loopcoder Test", "commit", "-m", "initial")

	parent := t.TempDir()
	worktree := filepath.Join(parent, "linked")
	runGit(t, repo, "worktree", "add", "-b", "linked-test", worktree)
	gitFile := readFile(t, filepath.Join(worktree, ".git"))
	if !bytes.HasPrefix(gitFile, []byte("gitdir:")) {
		t.Fatalf("worktree .git is not a gitdir file:\n%s", gitFile)
	}

	wantExclude := gitOutput(t, worktree, "rev-parse", "--path-format=absolute", "--git-path", "info/exclude")
	result, err := ProtectLoopcoderState(context.Background(), worktree)
	if err != nil {
		t.Fatalf("ProtectLoopcoderState worktree: %v", err)
	}
	if filepath.Clean(result.ExcludePath) != filepath.Clean(wantExclude) {
		t.Fatalf("exclude path = %q, want git-reported path %q", result.ExcludePath, wantExclude)
	}
	data := readFile(t, wantExclude)
	if got := strings.Count(string(data), ManagedExcludeEntry); got != 1 {
		t.Fatalf("managed entry count in worktree exclude = %d, want 1:\n%s", got, data)
	}
}

func TestProtectLoopcoderStateReportsNonGitRepository(t *testing.T) {
	_, err := ProtectLoopcoderStateWithDeps(context.Background(), "not-git", Deps{
		Git: &fakeGitRunner{err: errors.New("fatal: not a git repository")},
	})
	if !errors.Is(err, ErrNotGitRepository) {
		t.Fatalf("error = %v, want ErrNotGitRepository", err)
	}
}

func TestResolveExcludePathUsesGitPathCommand(t *testing.T) {
	runner := &fakeGitRunner{
		outputs: map[string][]byte{
			"repo\x00rev-parse\x00--is-inside-work-tree":                                []byte("true\n"),
			"repo\x00rev-parse\x00--path-format=absolute\x00--git-path\x00info/exclude": []byte(filepath.Join(".git", "info", "exclude") + "\n"),
		},
	}

	path, err := ResolveExcludePath(context.Background(), "repo", runner)
	if err != nil {
		t.Fatalf("ResolveExcludePath: %v", err)
	}
	if !strings.HasSuffix(filepath.ToSlash(path), "repo/.git/info/exclude") {
		t.Fatalf("path = %q, want repo/.git/info/exclude suffix", path)
	}
	wantCalls := [][]string{
		{"repo", "rev-parse", "--is-inside-work-tree"},
		{"repo", "rev-parse", "--path-format=absolute", "--git-path", "info/exclude"},
	}
	if !reflect.DeepEqual(runner.calls, wantCalls) {
		t.Fatalf("git calls = %#v, want %#v", runner.calls, wantCalls)
	}
}

func initGitRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runGit(t, repo, "init", "-b", "main")
	return repo
}

func fakeExcludePathGit(repo string, excludePath string) *fakeGitRunner {
	return &fakeGitRunner{
		outputs: map[string][]byte{
			repo + "\x00rev-parse\x00--is-inside-work-tree":                                []byte("true\n"),
			repo + "\x00rev-parse\x00--path-format=absolute\x00--git-path\x00info/exclude": []byte(excludePath + "\n"),
		},
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s in %s: %v", strings.Join(args, " "), dir, err)
	}
	return filepath.Clean(strings.TrimSpace(string(out)))
}

func writeFile(t *testing.T, path string, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

type fakeGitRunner struct {
	calls   [][]string
	outputs map[string][]byte
	err     error
}

func (f *fakeGitRunner) RunGit(_ context.Context, repoPath string, args ...string) ([]byte, error) {
	call := append([]string{repoPath}, args...)
	f.calls = append(f.calls, call)
	if f.err != nil {
		return nil, f.err
	}
	key := repoPath
	for _, arg := range args {
		key += "\x00" + arg
	}
	return f.outputs[key], nil
}
