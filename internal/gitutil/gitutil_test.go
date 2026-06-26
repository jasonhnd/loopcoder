package gitutil

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestClientRunsExpectedGitCommands(t *testing.T) {
	runner := &fakeGitRunner{
		outputs: map[string][]byte{
			"repo\x00rev-parse\x00--verify\x00loop/issue-101^{commit}": []byte("abc123\n"),
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
		{"verify-wt", "fetch", "-q", "origin", "loop/issue-101"},
		{"verify-wt", "checkout", "--detach", "FETCH_HEAD"},
		{"repo", "rev-parse", "--verify", "loop/issue-101^{commit}"},
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
