package defaults_test

import (
	"context"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/config"
	lcdefaults "github.com/jasonhnd/loopcoder/internal/defaults"
	"github.com/jasonhnd/loopcoder/internal/loopreview"
	"github.com/jasonhnd/loopcoder/internal/statebranch"
	"github.com/jasonhnd/loopcoder/internal/supervisedexec"
	gh "github.com/jasonhnd/loopcoder/internal/vcs/github"
	"github.com/jasonhnd/loopcoder/internal/worker"
)

func TestCurrentInventory(t *testing.T) {
	got := lcdefaults.Current()

	if got.BaseBranch != "main" || got.PreProdBranch != "pre-prod" {
		t.Fatalf("branch defaults = %q/%q", got.BaseBranch, got.PreProdBranch)
	}
	if got.DispatchWaveThrottleLimit != 4 {
		t.Fatalf("DispatchWaveThrottleLimit = %d, want 4", got.DispatchWaveThrottleLimit)
	}
	if got.NestedSchedulerMaxConcurrency != 3 {
		t.Fatalf("NestedSchedulerMaxConcurrency = %d, want 3", got.NestedSchedulerMaxConcurrency)
	}
	if got.WorkerHardCap != 45*time.Minute || got.WorkerStallTimeout != 5*time.Minute {
		t.Fatalf("worker watchdog defaults = %s/%s", got.WorkerHardCap, got.WorkerStallTimeout)
	}
	if got.WorkerHeartbeatIntervalSeconds != 15 || got.WorkerStaleAfterSeconds != 120 ||
		got.WorkerHungAfterSeconds != 300 || got.WorkerMaxAttempts != 3 ||
		got.WorkerHardCapSeconds != 2700 || got.WorkerStallTimeoutSeconds != 300 {
		t.Fatalf("worker resilience defaults = %#v", got)
	}
	if !reflect.DeepEqual(got.WorkerRetryBackoffSeconds, []int{10, 30, 120}) {
		t.Fatalf("WorkerRetryBackoffSeconds = %v, want [10 30 120]", got.WorkerRetryBackoffSeconds)
	}
	if got.VerifierHardCapSeconds != 900 || got.VerifierStallTimeoutSeconds != 300 ||
		got.VerifierTimeout != 15*time.Minute || got.VerifierStallTimeout != 5*time.Minute {
		t.Fatalf("verifier defaults = %#v", got)
	}
	if got.GitCommandHardCap != time.Minute || got.DoctorCommandHardCap != time.Minute ||
		got.ScaffoldGitHubCommandCap != time.Minute || got.UpgradeCommandHardCap != time.Minute ||
		got.VerifyCommandHardCap != 15*time.Minute || got.CompileGoListHardCap != 120*time.Second ||
		got.GitHubCommandHardCap != time.Minute || got.SupervisedExecHardCap != 30*time.Minute ||
		got.ProcessLivenessCommandCap != 5*time.Second {
		t.Fatalf("command hard caps = %#v", got)
	}
	if got.ReviewPacketDiffBudgetBytes != 80*1024 ||
		got.ReviewPacketDocumentationBodyFileBytes != 64*1024 ||
		got.ReviewPacketDocumentationBodyTotalBytes != 96*1024 ||
		got.ReviewPacketDocumentationBodyMaxFiles != 3 ||
		got.ReviewPacketTotalPromptBudgetBytes != 160*1024 ||
		got.RenderedArtifactFileBudgetBytes != 8*1024 ||
		got.RenderedArtifactMaxDirectoryFiles != 32 ||
		got.RenderedArtifactProducerTimeout != 5*time.Minute {
		t.Fatalf("loopreview budgets = %#v", got)
	}
	if got.WorktreeLivenessMaxFiles != 20000 || got.GitHubListLimit != 1000 ||
		got.HookInputMaxBytes != 1<<20 || got.RunStatusMaxDirectoryEntries != 4096 ||
		got.RelayGateMaxRecordSize != 256*1024 {
		t.Fatalf("local bounds = %#v", got)
	}
	if got.StateBranchDefaultBranch != "loopcoder/state" || got.StateBranchDefaultRemote != "origin" ||
		got.StateBranchDefaultTTLSeconds != 600 || got.StateBranchLogTailLines != 50 {
		t.Fatalf("state branch defaults = %#v", got)
	}
}

func TestMutableDefaultsReturnCopies(t *testing.T) {
	backoff := lcdefaults.WorkerRetryBackoffSeconds()
	backoff[0] = 99
	if got := lcdefaults.WorkerRetryBackoffSeconds(); got[0] != 10 {
		t.Fatalf("WorkerRetryBackoffSeconds leaked mutation: %v", got)
	}

	patterns := lcdefaults.ReviewPacketGeneratedPatterns()
	patterns[0] = "changed"
	if got := lcdefaults.ReviewPacketGeneratedPatterns(); got[0] != "tests/baseline/**" {
		t.Fatalf("ReviewPacketGeneratedPatterns leaked mutation: %v", got)
	}

	values := lcdefaults.Current()
	values.WorkerRetryBackoffSeconds[0] = 99
	values.ReviewPacketGeneratedPatterns[0] = "changed"
	next := lcdefaults.Current()
	if next.WorkerRetryBackoffSeconds[0] != 10 || next.ReviewPacketGeneratedPatterns[0] != "tests/baseline/**" {
		t.Fatalf("Current leaked mutable values: %#v", next)
	}
}

func TestConsumersUseCentralDefaults(t *testing.T) {
	cfg := config.Default()
	if cfg.Verification.MaxFixPasses != lcdefaults.VerificationMaxFixPasses ||
		cfg.Verification.Browser.Enabled != lcdefaults.VerificationBrowserMode ||
		cfg.Resilience.Worker.MaxAttempts != lcdefaults.WorkerMaxAttempts ||
		cfg.Resilience.Worker.HardCapSeconds != lcdefaults.WorkerHardCapSeconds ||
		cfg.Environment.PreProdBranch != lcdefaults.PreProdBranch {
		t.Fatalf("config defaults are not sourced from central defaults: %#v", cfg)
	}
	cfg.Resilience.Worker.RetryBackoffSeconds[0] = 99
	if got := config.Default().Resilience.Worker.RetryBackoffSeconds[0]; got != 10 {
		t.Fatalf("config retry defaults leaked mutation: %d", got)
	}

	if worker.WorkerHardCap != lcdefaults.WorkerHardCap ||
		worker.WorkerStallTimeout != lcdefaults.WorkerStallTimeout ||
		loopreview.DefaultVerifierTimeout != lcdefaults.VerifierTimeout ||
		loopreview.VerifierStallTimeout != lcdefaults.VerifierStallTimeout ||
		supervisedexec.DefaultHardCap != lcdefaults.SupervisedExecHardCap ||
		statebranch.DefaultBranch != lcdefaults.StateBranchDefaultBranch ||
		statebranch.DefaultRemote != lcdefaults.StateBranchDefaultRemote ||
		statebranch.DefaultTTLSeconds != lcdefaults.StateBranchDefaultTTLSeconds {
		t.Fatalf("package wrappers drifted from central defaults")
	}

	runner := &captureRunner{}
	if _, err := gh.NewWithRunner(".", runner).ListIssues(context.Background(), "open"); err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if !hasArgPair(runner.args, "--limit", strconv.Itoa(lcdefaults.GitHubListLimit)) {
		t.Fatalf("gh issue list args = %v, want --limit %d", runner.args, lcdefaults.GitHubListLimit)
	}
}

type captureRunner struct {
	args []string
}

func (r *captureRunner) Run(_ context.Context, _ string, name string, args ...string) ([]byte, error) {
	r.args = append([]string{name}, args...)
	return []byte("[]"), nil
}

func hasArgPair(args []string, key, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == key && args[i+1] == value {
			return true
		}
	}
	return false
}
