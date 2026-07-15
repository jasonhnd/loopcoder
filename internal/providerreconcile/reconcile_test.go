package providerreconcile

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/gitutil"
	"github.com/jasonhnd/loopcoder/internal/providerauthority"
	"github.com/jasonhnd/loopcoder/internal/state"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

func TestCheckDecisionTableBlocksLiveAndAmbiguousAuthority(t *testing.T) {
	attempt := state.Attempt{JobID: "job-963-1", Issue: 963, Attempt: 1, Status: "running", Phase: "codex_started"}
	baseAuthority := storage.ProviderExecutionAuthority{
		AuthorityID:     "pauth-test",
		RunID:           "run-963",
		AttemptID:       attempt.JobID,
		ProviderPID:     1234,
		ProviderPGID:    1234,
		OwnerID:         "owner",
		ClaimGeneration: 2,
		WorktreePath:    t.TempDir(),
		LogPath:         filepath.Join(t.TempDir(), "provider.log"),
	}
	tests := []struct {
		name        string
		view        providerauthority.View
		validateErr error
		wantOutcome string
		wantAction  string
		wantBlock   bool
	}{
		{
			name:        "valid live owner and provider observes",
			view:        providerauthority.View{Authority: baseAuthority, State: providerauthority.StateActive, Reason: "verified", Verified: true},
			wantOutcome: OutcomeLiveOwnerProvider,
			wantAction:  ActionObserve,
			wantBlock:   true,
		},
		{
			name:        "dead supervisor and live provider reconciles",
			view:        providerauthority.View{Authority: baseAuthority, State: providerauthority.StateActive, Reason: "verified", Verified: true},
			validateErr: storage.ErrOwnershipStale,
			wantOutcome: OutcomeLiveProviderStale,
			wantAction:  ActionReconcile,
			wantBlock:   true,
		},
		{
			name:        "ambiguous provider identity needs human",
			view:        providerauthority.View{Authority: baseAuthority, State: providerauthority.StateAmbiguous, Reason: "incomplete-process-identity"},
			wantOutcome: OutcomeAmbiguous,
			wantAction:  ActionNeedsHuman,
			wantBlock:   true,
		},
		{
			name:        "terminal provider result is reused",
			view:        providerauthority.View{Authority: terminalAuthority(baseAuthority, "succeeded"), State: providerauthority.StateTerminal, Reason: "succeeded"},
			wantOutcome: OutcomeTerminal,
			wantAction:  ActionReuseTerminal,
			wantBlock:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runtime := &fakeRuntime{view: tt.view, validateErr: tt.validateErr}
			receipt := Check(context.Background(), Options{
				RepoPath: t.TempDir(),
				RunID:    "run-963",
				Issue:    963,
				Attempts: []state.Attempt{attempt},
				Now:      time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC),
				OpenRuntime: func(context.Context, string, func() time.Time) (Runtime, func(), error) {
					return runtime, nil, nil
				},
			})
			if receipt.Outcome != tt.wantOutcome || receipt.Action != tt.wantAction || receipt.BlockRedispatch != tt.wantBlock {
				t.Fatalf("receipt = %#v, want outcome/action/block %s/%s/%v", receipt, tt.wantOutcome, tt.wantAction, tt.wantBlock)
			}
		})
	}
}

func TestCheckDeadProviderChangedWorktreeHarvestsAndCleanWorktreeRetries(t *testing.T) {
	repo := initGitRepo(t)
	attempt := state.Attempt{JobID: "job-963-2", Issue: 963, Attempt: 2, Status: "running", Phase: "codex_started"}
	authority := storage.ProviderExecutionAuthority{
		RunID:           "run-963",
		AttemptID:       attempt.JobID,
		ProviderPID:     999999,
		ProviderPGID:    999999,
		OwnerID:         "owner",
		ClaimGeneration: 1,
		WorktreePath:    repo,
		LogPath:         filepath.Join(repo, "provider.log"),
	}

	writeFile(t, filepath.Join(repo, "changed.txt"), "changed\n")
	changed := Check(context.Background(), Options{
		RepoPath: t.TempDir(),
		RunID:    "run-963",
		Issue:    963,
		Attempts: []state.Attempt{attempt},
		OpenRuntime: func(context.Context, string, func() time.Time) (Runtime, func(), error) {
			return &fakeRuntime{view: providerauthority.View{Authority: authority, State: providerauthority.StateStale, Reason: "not alive"}}, nil, nil
		},
	})
	if changed.Outcome != OutcomeDeadChanged || changed.Action != ActionHarvest || !changed.BlockRedispatch {
		t.Fatalf("changed receipt = %#v, want harvest block", changed)
	}

	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-m", "clean")
	clean := Check(context.Background(), Options{
		RepoPath: t.TempDir(),
		RunID:    "run-963",
		Issue:    963,
		Attempts: []state.Attempt{attempt},
		OpenRuntime: func(context.Context, string, func() time.Time) (Runtime, func(), error) {
			return &fakeRuntime{view: providerauthority.View{Authority: authority, State: providerauthority.StateStale, Reason: "not alive"}}, nil, nil
		},
	})
	if clean.Outcome != OutcomeDeadNoMaterialWork || clean.Action != ActionRetry || clean.BlockRedispatch || !clean.RetryAllowed {
		t.Fatalf("clean receipt = %#v, want retry allowed", clean)
	}
}

func TestCheckMissingAuthorityDuringLaunchFailsClosed(t *testing.T) {
	receipt := Check(context.Background(), Options{
		RepoPath: t.TempDir(),
		RunID:    "run-963",
		Issue:    963,
		Attempts: []state.Attempt{{JobID: "job-launch", Issue: 963, Attempt: 1, Status: "running", Phase: "codex_launching"}},
		OpenRuntime: func(context.Context, string, func() time.Time) (Runtime, func(), error) {
			return &fakeRuntime{view: providerauthority.Missing("run-963", "job-launch")}, nil, nil
		},
	})
	if receipt.Outcome != OutcomeLaunchAmbiguous || !receipt.BlockRedispatch || !receipt.NeedsHuman {
		t.Fatalf("receipt = %#v, want launch ambiguity needs-human", receipt)
	}
}

type fakeRuntime struct {
	unregistered bool
	view         providerauthority.View
	validateErr  error
}

func (r *fakeRuntime) Registered() bool {
	return !r.unregistered
}

func (r *fakeRuntime) Load(context.Context, string, string) (providerauthority.View, error) {
	return r.view, nil
}

func (r *fakeRuntime) ValidateOwnership(context.Context, providerauthority.View, time.Time) error {
	return r.validateErr
}

func terminalAuthority(authority storage.ProviderExecutionAuthority, terminal string) storage.ProviderExecutionAuthority {
	authority.TerminalState = terminal
	authority.CompletedAt = "2026-07-15T00:00:00Z"
	return authority
}

func initGitRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runGit(t, repo, "init")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Test User")
	writeFile(t, filepath.Join(repo, "README.md"), "test\n")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-m", "initial")
	return repo
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitutil.CleanEnv(os.Environ())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

var _ Runtime = (*fakeRuntime)(nil)
