package gitutil

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestClientRunsExpectedGitCommands(t *testing.T) {
	runner := &fakeGitRunner{
		outputs: map[string][]byte{
			"repo\x00rev-parse\x00--verify\x00loop/issue-101^{commit}": []byte("abc123\n"),
			"repo\x00show\x00origin/main:docs/specs/design.md":         []byte("# Design\n"),
			"repo\x00status\x00--porcelain":                            []byte(" M file.go\n"),
		},
	}
	client := NewWithRunner(runner)
	ctx := context.Background()

	if err := client.FetchOriginBase(ctx, "repo", "main"); err != nil {
		t.Fatalf("FetchOriginBase returned error: %v", err)
	}
	if err := client.WorktreeAdd(ctx, "repo", "loop/issue-101", "wt", "main"); err != nil {
		t.Fatalf("WorktreeAdd returned error: %v", err)
	}
	if err := client.WorktreeAddDetached(ctx, "repo", "verify-wt", "main"); err != nil {
		t.Fatalf("WorktreeAddDetached returned error: %v", err)
	}
	if err := client.FetchPRHead(ctx, "repo", 152); err != nil {
		t.Fatalf("FetchPRHead returned error: %v", err)
	}
	if err := client.WorktreeAddDetachedAt(ctx, "repo", "review-wt", "FETCH_HEAD"); err != nil {
		t.Fatalf("WorktreeAddDetachedAt returned error: %v", err)
	}
	if err := client.FetchOriginBranch(ctx, "verify-wt", "loop/issue-101"); err != nil {
		t.Fatalf("FetchOriginBranch returned error: %v", err)
	}
	if err := client.CheckoutDetached(ctx, "verify-wt", "FETCH_HEAD"); err != nil {
		t.Fatalf("CheckoutDetached returned error: %v", err)
	}
	commit, err := client.RevParse(ctx, "repo", "loop/issue-101^{commit}")
	if err != nil {
		t.Fatalf("RevParse returned error: %v", err)
	}
	if commit != "abc123" {
		t.Fatalf("RevParse = %q, want abc123", commit)
	}
	spec, err := client.Show(ctx, "repo", "origin/main:docs/specs/design.md")
	if err != nil {
		t.Fatalf("Show returned error: %v", err)
	}
	if spec != "# Design\n" {
		t.Fatalf("Show = %q, want # Design", spec)
	}
	status, err := client.StatusPorcelain(ctx, "repo")
	if err != nil {
		t.Fatalf("StatusPorcelain returned error: %v", err)
	}
	if status != " M file.go\n" {
		t.Fatalf("status = %q", status)
	}
	if err := client.AddAll(ctx, "wt"); err != nil {
		t.Fatalf("AddAll returned error: %v", err)
	}
	if err := client.Commit(ctx, "wt", "title (closes #101)"); err != nil {
		t.Fatalf("Commit returned error: %v", err)
	}
	if err := client.PushUpstream(ctx, "wt", "loop/issue-101"); err != nil {
		t.Fatalf("PushUpstream returned error: %v", err)
	}
	if err := client.WorktreeRemove(ctx, "repo", "wt"); err != nil {
		t.Fatalf("WorktreeRemove returned error: %v", err)
	}
	if err := client.BranchDelete(ctx, "repo", "loop/issue-101"); err != nil {
		t.Fatalf("BranchDelete returned error: %v", err)
	}

	want := [][]string{
		{"repo", "fetch", "origin", "main"},
		{"repo", "worktree", "add", "-b", "loop/issue-101", "wt", "origin/main"},
		{"repo", "worktree", "add", "--detach", "verify-wt", "origin/main"},
		{"repo", "fetch", "-q", "origin", "pull/152/head"},
		{"repo", "worktree", "add", "--detach", "review-wt", "FETCH_HEAD"},
		{"verify-wt", "fetch", "-q", "origin", "loop/issue-101"},
		{"verify-wt", "checkout", "--detach", "FETCH_HEAD"},
		{"repo", "rev-parse", "--verify", "loop/issue-101^{commit}"},
		{"repo", "show", "origin/main:docs/specs/design.md"},
		{"repo", "status", "--porcelain"},
		{"wt", "add", "-A"},
		{"wt", "commit", "-m", "title (closes #101)"},
		{"wt", "push", "-u", "origin", "loop/issue-101"},
		{"repo", "worktree", "remove", "--force", "wt"},
		{"repo", "branch", "-D", "loop/issue-101"},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("git calls = %#v, want %#v", runner.calls, want)
	}
}

func TestClientPropagatesRunnerError(t *testing.T) {
	wantErr := errors.New("boom")
	client := NewWithRunner(&fakeGitRunner{err: wantErr})

	err := client.FetchOriginBase(context.Background(), "repo", "main")
	if !errors.Is(err, wantErr) {
		t.Fatalf("FetchOriginBase error = %v, want %v", err, wantErr)
	}
}

func TestExecRunnerCapturesOutputAndNonZeroExit(t *testing.T) {
	withTestGitCommand(t, 2*time.Second)

	output, err := (ExecRunner{}).RunGit(context.Background(), "repo", "-test.run=TestGitExecHelper", "--", "stdout", "hello")
	if err != nil {
		t.Fatalf("RunGit returned error: %v", err)
	}
	if string(output) != "hello\n" {
		t.Fatalf("output = %q, want hello newline", output)
	}

	output, err = (ExecRunner{}).RunGit(context.Background(), "repo", "-test.run=TestGitExecHelper", "--", "stderr-exit", "detail", "7")
	if err == nil {
		t.Fatal("RunGit error = nil, want non-zero exit error")
	}
	if output != nil {
		t.Fatalf("output = %q, want nil on error", output)
	}
	if !strings.Contains(err.Error(), "exit status 7") || !strings.Contains(err.Error(), "detail") {
		t.Fatalf("error = %q, want exit status and stderr detail", err.Error())
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
		t.Fatalf("error = %v, want wrapped exec.ExitError exit 7", err)
	}
}

func TestExecRunnerTimesOut(t *testing.T) {
	withTestGitCommand(t, 50*time.Millisecond)

	start := time.Now()
	_, err := (ExecRunner{}).RunGit(context.Background(), "repo", "-test.run=TestGitExecHelper", "--", "sleep", "5s")
	if err == nil {
		t.Fatal("RunGit error = nil, want timeout")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("RunGit elapsed = %s, want bounded timeout", elapsed)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %q, want timeout", err.Error())
	}
}

func withTestGitCommand(t *testing.T, hardCap time.Duration) {
	t.Helper()
	oldCommand := gitCommand
	oldArgs := buildGitArgs
	oldHardCap := gitHardCap
	gitCommand = os.Args[0]
	buildGitArgs = func(_ string, args ...string) []string {
		return append([]string(nil), args...)
	}
	gitHardCap = hardCap
	t.Setenv("GO_WANT_GITUTIL_HELPER", "1")
	t.Cleanup(func() {
		gitCommand = oldCommand
		buildGitArgs = oldArgs
		gitHardCap = oldHardCap
	})
}

func TestGitExecHelper(t *testing.T) {
	if os.Getenv("GO_WANT_GITUTIL_HELPER") != "1" {
		return
	}
	runExecHelper()
}

func runExecHelper() {
	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 || separator+1 >= len(os.Args) {
		fmt.Fprintln(os.Stderr, "missing helper mode")
		os.Exit(2)
	}
	mode := os.Args[separator+1]
	args := os.Args[separator+2:]
	switch mode {
	case "stdout":
		fmt.Fprintln(os.Stdout, args[0])
	case "stderr-exit":
		fmt.Fprintln(os.Stderr, args[0])
		os.Exit(parseHelperInt(args[1]))
	case "sleep":
		time.Sleep(parseHelperDuration(args[0]))
	default:
		fmt.Fprintf(os.Stderr, "unknown helper mode %q\n", mode)
		os.Exit(2)
	}
	os.Exit(0)
}

func parseHelperDuration(value string) time.Duration {
	duration, err := time.ParseDuration(value)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse duration %q: %v\n", value, err)
		os.Exit(2)
	}
	return duration
}

func parseHelperInt(value string) int {
	var n int
	if _, err := fmt.Sscanf(value, "%d", &n); err != nil {
		fmt.Fprintf(os.Stderr, "parse int %q: %v\n", value, err)
		os.Exit(2)
	}
	return n
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
