package verify

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRunNoCommandsConfigured(t *testing.T) {
	repo := t.TempDir()
	git := &verifyFakeGit{}
	runner := &verifyFakeRunner{}

	result := Run(context.Background(), Options{
		RepoPath: repo,
		Branch:   "loop/issue-105",
	}, Deps{
		Git:    git,
		Runner: runner,
		Now:    verifyFixedNow,
	})

	if result.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", result.ExitCode)
	}
	if result.Summary.LocalCommandGates != "not-configured" || result.Summary.Verdict != StatusPass {
		t.Fatalf("summary = %#v, want not-configured pass", result.Summary)
	}
	if len(git.calls) != 0 {
		t.Fatalf("git calls = %#v, want none", git.calls)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runner calls = %#v, want none", runner.calls)
	}

	var rendered bytes.Buffer
	if err := Render(&rendered, result); err != nil {
		t.Fatalf("Render returned error: %v", err)
	}
	for _, want := range []string{"no local command gates configured", "LOCAL VERIFICATION SUMMARY", "JSON SUMMARY"} {
		if !strings.Contains(rendered.String(), want) {
			t.Fatalf("rendered output missing %q:\n%s", want, rendered.String())
		}
	}
}

func TestRunPassingCommand(t *testing.T) {
	repo := repoWithDelivery(t, "ci:\n  tests:\n    - go test ./...\n")
	runner := &verifyFakeRunner{
		results: []CommandRunResult{{ExitCode: 0, Output: []string{"ok"}}},
	}

	result := Run(context.Background(), Options{
		RepoPath: repo,
		Branch:   "loop/issue-105",
	}, verifyDeps(t, runner, nil))

	if result.ExitCode != 0 || result.Summary.Verdict != StatusPass {
		t.Fatalf("result = %#v, want pass exit 0", result)
	}
	if len(result.Summary.Groups) != 1 || result.Summary.Groups[0].Group != "tests" {
		t.Fatalf("groups = %#v, want tests group", result.Summary.Groups)
	}
	command := result.Summary.Groups[0].Commands[0]
	if command.Status != StatusPass || command.ExitCode != 0 || !reflect.DeepEqual(command.LogTail, []string{"ok"}) {
		t.Fatalf("command result = %#v, want passing command", command)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("runner calls = %#v, want one shell command", runner.calls)
	}
	wantShell, wantShellArgs := expectedShell("go test ./...")
	if runner.calls[0].File != wantShell || !reflect.DeepEqual(runner.calls[0].Args, wantShellArgs) {
		t.Fatalf("runner shell = %s %#v, want %s %#v", runner.calls[0].File, runner.calls[0].Args, wantShell, wantShellArgs)
	}
	if runner.calls[0].WorkingDirectory != result.Summary.Worktree {
		t.Fatalf("runner dir = %q, want worktree %q", runner.calls[0].WorkingDirectory, result.Summary.Worktree)
	}
	if len(result.Summary.Cleanup) != 2 {
		t.Fatalf("cleanup = %#v, want worktree and scratch cleanup notes", result.Summary.Cleanup)
	}
}

func TestRunFailingCommand(t *testing.T) {
	repo := repoWithDelivery(t, "ci:\n  build:\n    - go test ./...\n")
	runner := &verifyFakeRunner{
		results: []CommandRunResult{{ExitCode: 1, Output: []string{"FAIL: TestThing"}}},
	}

	result := Run(context.Background(), Options{
		RepoPath: repo,
		Branch:   "loop/issue-105",
	}, verifyDeps(t, runner, nil))

	if result.ExitCode != 1 || result.Summary.Verdict != StatusFail {
		t.Fatalf("result = %#v, want fail exit 1", result)
	}
	command := result.Summary.Groups[0].Commands[0]
	if command.Status != StatusFail || command.Reason != "command-exit-nonzero" {
		t.Fatalf("command result = %#v, want command-exit-nonzero fail", command)
	}
}

func TestRunNeedsHumanForUnrunnableCommand(t *testing.T) {
	repo := repoWithDelivery(t, "ci:\n  typecheck:\n    - missing-tool --version\n")
	runner := &verifyFakeRunner{
		results: []CommandRunResult{{ExitCode: 127, Output: []string{"missing-tool: command not found"}}},
	}

	result := Run(context.Background(), Options{
		RepoPath: repo,
		Branch:   "loop/issue-105",
	}, verifyDeps(t, runner, nil))

	if result.ExitCode != 2 || result.Summary.Verdict != StatusNeedsHuman {
		t.Fatalf("result = %#v, want needs-human exit 2", result)
	}
	command := result.Summary.Groups[0].Commands[0]
	if command.Status != StatusNeedsHuman || command.Reason != "missing-tool" {
		t.Fatalf("command result = %#v, want missing-tool needs-human", command)
	}
}

func TestExecRunnerCapturesOutputAndNonZeroExit(t *testing.T) {
	withTestCommandCap(t, 2*time.Second)

	run := (ExecRunner{}).Run(context.Background(), CommandInvocation{
		File: os.Args[0],
		Args: []string{"-test.run=TestVerifyExecHelper", "--", "stdout", "ok"},
	})
	if run.Err != nil || run.ExitCode != 0 || !reflect.DeepEqual(run.Output, []string{"ok"}) {
		t.Fatalf("Run = %#v, want output ok exit 0", run)
	}

	run = (ExecRunner{}).Run(context.Background(), CommandInvocation{
		File: os.Args[0],
		Args: []string{"-test.run=TestVerifyExecHelper", "--", "combined-exit", "stdout", "stderr", "7"},
	})
	if run.Err != nil || run.ExitCode != 7 {
		t.Fatalf("Run = %#v, want exit 7 without runner error", run)
	}
	text := strings.Join(run.Output, "\n")
	if !strings.Contains(text, "stdout") || !strings.Contains(text, "stderr") {
		t.Fatalf("output = %#v, want combined stdout and stderr", run.Output)
	}
}

func TestExecRunnerTimesOut(t *testing.T) {
	withTestCommandCap(t, 50*time.Millisecond)

	start := time.Now()
	run := (ExecRunner{}).Run(context.Background(), CommandInvocation{
		File: os.Args[0],
		Args: []string{"-test.run=TestVerifyExecHelper", "--", "sleep", "5s"},
	})
	if run.Err == nil {
		t.Fatal("Run Err = nil, want timeout")
	}
	if run.ExitCode != -1 {
		t.Fatalf("ExitCode = %d, want -1", run.ExitCode)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Run elapsed = %s, want bounded timeout", elapsed)
	}
	if !strings.Contains(run.Err.Error(), "timed out") {
		t.Fatalf("error = %q, want timeout", run.Err.Error())
	}
}

func withTestCommandCap(t *testing.T, hardCap time.Duration) {
	t.Helper()
	oldHardCap := commandHardCap
	commandHardCap = hardCap
	t.Setenv("GO_WANT_VERIFY_HELPER", "1")
	t.Cleanup(func() {
		commandHardCap = oldHardCap
	})
}

func TestVerifyExecHelper(t *testing.T) {
	if os.Getenv("GO_WANT_VERIFY_HELPER") != "1" {
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
	case "combined-exit":
		fmt.Fprintln(os.Stdout, args[0])
		fmt.Fprintln(os.Stderr, args[1])
		os.Exit(parseHelperInt(args[2]))
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

func repoWithDelivery(t *testing.T, content string) string {
	t.Helper()
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".delivery.yml"), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile .delivery.yml: %v", err)
	}
	return repo
}

func verifyDeps(t *testing.T, runner *verifyFakeRunner, git *verifyFakeGit) Deps {
	t.Helper()
	if git == nil {
		git = &verifyFakeGit{}
	}
	scratchRoot := t.TempDir()
	return Deps{
		Git:    git,
		Runner: runner,
		Now:    verifyFixedNow,
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
		RemoveAll: os.RemoveAll,
	}
}

func verifyFixedNow() time.Time {
	return time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
}

func expectedShell(command string) (string, []string) {
	if runtime.GOOS == "windows" {
		return "cmd", []string{"/c", command}
	}
	return "sh", []string{"-c", command}
}

type verifyFakeRunner struct {
	calls   []CommandInvocation
	results []CommandRunResult
}

func (f *verifyFakeRunner) Run(_ context.Context, invocation CommandInvocation) CommandRunResult {
	f.calls = append(f.calls, invocation)
	if len(f.results) == 0 {
		return CommandRunResult{}
	}
	result := f.results[0]
	f.results = f.results[1:]
	return result
}

type verifyFakeGit struct {
	calls          []string
	fetchBranchErr error
	rev            string
	err            error
}

func (f *verifyFakeGit) FetchOriginBase(context.Context, string, string) error {
	f.calls = append(f.calls, "fetch-base")
	return f.err
}

func (f *verifyFakeGit) WorktreeAddDetached(_ context.Context, _ string, worktreePath string, _ string) error {
	f.calls = append(f.calls, "worktree-add-detached")
	if f.err != nil {
		return f.err
	}
	return os.MkdirAll(worktreePath, 0o755)
}

func (f *verifyFakeGit) WorktreeRemove(context.Context, string, string) error {
	f.calls = append(f.calls, "worktree-remove")
	return nil
}

func (f *verifyFakeGit) FetchOriginBranch(context.Context, string, string) error {
	f.calls = append(f.calls, "fetch-branch")
	if f.fetchBranchErr != nil {
		return f.fetchBranchErr
	}
	return f.err
}

func (f *verifyFakeGit) CheckoutDetached(context.Context, string, string) error {
	f.calls = append(f.calls, "checkout-detached")
	return f.err
}

func (f *verifyFakeGit) RevParse(context.Context, string, string) (string, error) {
	f.calls = append(f.calls, "rev-parse")
	if f.err != nil {
		return "", f.err
	}
	if f.rev != "" {
		return f.rev, nil
	}
	return "abc123", nil
}
