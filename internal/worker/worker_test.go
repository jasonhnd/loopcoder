package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/agent"
	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/progress"
	"github.com/jasonhnd/loopcoder/internal/reporter"
	"github.com/jasonhnd/loopcoder/internal/runtimecap"
	"github.com/jasonhnd/loopcoder/internal/runtimepath"
	"github.com/jasonhnd/loopcoder/internal/state"
	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/supervisedexec"
	gh "github.com/jasonhnd/loopcoder/internal/vcs/github"
)

func TestWorkerWatchdogDefaultsAreRaised(t *testing.T) {
	if WorkerHardCap != 45*time.Minute || WorkerStallTimeout != 5*time.Minute {
		t.Fatalf("worker watchdog defaults = hard cap %s stall %s, want 45m0s/5m0s", WorkerHardCap, WorkerStallTimeout)
	}
}

func TestBuildPromptWithAndWithoutRecoveryContext(t *testing.T) {
	base := BuildPrompt(PromptOptions{
		IssueNumber: 101,
		IssueTitle:  "Implement dispatch",
		IssueBody:   "Details here",
		Branch:      "loop/issue-101",
	})
	for _, want := range []string{
		"You are implementing GitHub issue #101.",
		"fresh git worktree on branch loop/issue-101",
		"# Title\nImplement dispatch",
		"# Details\nDetails here",
		"do NOT run git commit or git push",
		"final summary in English",
	} {
		if !strings.Contains(base, want) {
			t.Fatalf("prompt missing %q:\n%s", want, base)
		}
	}
	if strings.Contains(base, "Recovery context from a prior failed attempt") {
		t.Fatalf("prompt unexpectedly included recovery context:\n%s", base)
	}
	if strings.Contains(base, "Repo-local skills") {
		t.Fatalf("prompt unexpectedly included repo skills:\n%s", base)
	}

	withRepoSkills := BuildPrompt(PromptOptions{
		IssueNumber: 101,
		IssueTitle:  "Implement dispatch",
		Branch:      "loop/issue-101",
		RepoSkills:  "## Repo-local skills\nSummary: project conventions\n",
	})
	for _, want := range []string{
		"## Repo-local skills",
		"Summary: project conventions",
	} {
		if !strings.Contains(withRepoSkills, want) {
			t.Fatalf("repo-skill prompt missing %q:\n%s", want, withRepoSkills)
		}
	}

	withRecovery := BuildPrompt(PromptOptions{
		IssueNumber:     101,
		IssueTitle:      "Implement dispatch",
		Branch:          "loop/issue-101",
		RecoveryContext: "Previous failure details",
	})
	for _, want := range []string{
		"## Recovery context from a prior failed attempt",
		"Previous failure details",
	} {
		if !strings.Contains(withRecovery, want) {
			t.Fatalf("recovery prompt missing %q:\n%s", want, withRecovery)
		}
	}
}

func TestDispatchSuccessWritesStateAndReturnsParityJSONFields(t *testing.T) {
	repo := t.TempDir()
	scratchRoot := t.TempDir()
	var warnings strings.Builder
	fakeGit := &workerFakeGit{status: " M internal/worker/worker.go\n"}
	fakeAgent := &workerFakeAgent{
		resultSet: true,
		result:    validWorkerAgentResult("Implemented dispatch.", 0),
		log:       "codex ok\n",
	}
	fakeGitHub := &workerFakeGitHub{prURL: "https://github.com/owner/repo/pull/101"}

	result, err := Dispatch(context.Background(), Options{
		RepoPath:    repo,
		IssueNumber: 101,
		IssueTitle:  "Implement dispatch",
		IssueBody:   "Body",
		RunID:       "run-test",
		ProviderKey: "child-run:run-test-child",
		Provider:    "codex",
		Stderr:      &warnings,
	}, Deps{
		Git: fakeGit,
		GitHub: func(string) GitHubClient {
			return fakeGitHub
		},
		AgentLookup: func(provider string) (agent.Runner, error) {
			if provider != "codex" {
				t.Fatalf("provider = %q, want codex", provider)
			}
			return fakeAgent, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &workerFakeLock{}, nil
		},
		Now: fixedNow,
		PID: func() int {
			return 4321
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
		RemoveAll: os.RemoveAll,
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}

	if !result.OK || result.Issue != 101 || result.Branch != "loop/issue-101" || result.RunID != "run-test" {
		t.Fatalf("result has wrong identity fields: %#v", result)
	}
	if result.PR != "https://github.com/owner/repo/pull/101" {
		t.Fatalf("PR = %q", result.PR)
	}
	if result.Summary != "Implemented dispatch." || result.Status != "succeeded" || result.ExitCode != 0 || result.LogBytes == 0 {
		t.Fatalf("result has wrong status fields: %#v", result)
	}
	if result.AttemptPath != filepath.Join(repo, ".loopcoder", "runs", "run-test", "workers", "job-101-4321.attempt.json") {
		t.Fatalf("AttemptPath = %q", result.AttemptPath)
	}
	if fakeAgent.invocation.ProviderKey != "child-run:run-test-child" {
		t.Fatalf("agent provider key = %q, want child-run:run-test-child", fakeAgent.invocation.ProviderKey)
	}
	if !strings.Contains(fakeAgent.invocation.Prompt, "Provider idempotency key: child-run:run-test-child") {
		t.Fatalf("agent prompt missing provider idempotency key:\n%s", fakeAgent.invocation.Prompt)
	}
	if result.Report == nil {
		t.Fatal("result missing report")
	}
	if err := result.Report.Validate(); err != nil {
		t.Fatalf("result report does not validate: %v", err)
	}
	canonicalReport, err := result.Report.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON returned error: %v", err)
	}
	if want := "Closes #101\n\nImplemented dispatch."; fakeGitHub.lastPRBody != want {
		t.Fatalf("PR body = %q, want %q", fakeGitHub.lastPRBody, want)
	}
	for _, forbidden := range []string{
		result.Report.Header(),
		string(canonicalReport),
		"[attestation]",
		"```json\n",
		`"role":"worker"`,
		`"provider":"codex"`,
		`"model_source":"parsed"`,
		`"permission":"write"`,
		`"action":"implement issue #101"`,
		`"usage":{"input_tokens":120,"output_tokens":34,"total_tokens":154}`,
	} {
		if strings.Contains(fakeGitHub.lastPRBody, forbidden) {
			t.Fatalf("PR body contains forbidden report text %q:\n%s", forbidden, fakeGitHub.lastPRBody)
		}
	}
	if fakeGit.lastCommitMessage != "Implement dispatch (closes #101)" {
		t.Fatalf("commit message = %q", fakeGit.lastCommitMessage)
	}

	data, err := MarshalResult(result)
	if err != nil {
		t.Fatalf("MarshalResult returned error: %v", err)
	}
	var jsonFields map[string]any
	if err := json.Unmarshal(data, &jsonFields); err != nil {
		t.Fatalf("result JSON invalid: %v", err)
	}
	for _, key := range []string{"ok", "issue", "branch", "run_id", "pr", "summary", "attempt_path", "status", "exit_code", "log_bytes"} {
		if _, ok := jsonFields[key]; !ok {
			t.Fatalf("success JSON missing field %q: %s", key, string(data))
		}
	}
	reportField, ok := jsonFields["report"]
	if !ok {
		t.Fatalf("success JSON missing report field: %s", string(data))
	}
	reportBytes, err := json.Marshal(reportField)
	if err != nil {
		t.Fatalf("marshal report field: %v", err)
	}
	var renderedReport reporter.Report
	if err := json.Unmarshal(reportBytes, &renderedReport); err != nil {
		t.Fatalf("report field invalid: %v", err)
	}
	if err := renderedReport.Validate(); err != nil {
		t.Fatalf("report field does not validate: %v", err)
	}
	if renderedReport.Header() != result.Report.Header() {
		t.Fatalf("report JSON header = %q, want %q", renderedReport.Header(), result.Report.Header())
	}

	attempts, err := state.LoadAttempts(repo, "run-test")
	if err != nil {
		t.Fatalf("LoadAttempts returned error: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("LoadAttempts returned %d attempts, want 1", len(attempts))
	}
	if attempts[0].Phase != "cleanup" || attempts[0].Status != "succeeded" {
		t.Fatalf("final attempt = %#v", attempts[0])
	}
	if attempts[0].ExitCode == nil || *attempts[0].ExitCode != 0 {
		t.Fatalf("attempt exit code = %#v, want 0", attempts[0].ExitCode)
	}
	if attempts[0].Usage == nil || attempts[0].Usage.TotalTokens == nil || *attempts[0].Usage.TotalTokens != 154 {
		t.Fatalf("attempt usage = %#v, want total tokens 154", attempts[0].Usage)
	}
	if attempts[0].Report == nil {
		t.Fatal("attempt missing persisted report")
	}
	if err := attempts[0].Report.Validate(); err != nil {
		t.Fatalf("attempt report does not validate: %v", err)
	}
	if attempts[0].Report.Header() != result.Report.Header() {
		t.Fatalf("attempt report header = %q, want %q", attempts[0].Report.Header(), result.Report.Header())
	}
	if attempts[0].ArtifactDecision == nil ||
		attempts[0].ArtifactDecision.State != artifactDecisionCleanupCompleted ||
		attempts[0].ArtifactDecision.OwnerID != "worker:run-test:job-101-4321:1" ||
		attempts[0].ArtifactDecision.Generation != 1 ||
		len(attempts[0].ArtifactDecision.CleanupErrors) != 0 {
		t.Fatalf("artifact decision = %#v, want completed cleanup decision", attempts[0].ArtifactDecision)
	}
	eventCount, err := state.CountEvents(repo, "run-test")
	if err != nil {
		t.Fatalf("CountEvents returned error: %v", err)
	}
	if eventCount != 11 {
		t.Fatalf("event count = %d, want 11", eventCount)
	}
	lifecycle, err := state.LoadLifecycle(repo, "run-test")
	if err != nil {
		t.Fatalf("LoadLifecycle returned error: %v", err)
	}
	if lifecycle.State != state.StateSucceeded || lifecycle.Source != "lifecycle" {
		t.Fatalf("lifecycle = %#v, want explicit succeeded", lifecycle)
	}
	if len(lifecycle.History) != 2 || lifecycle.History[0].State != state.StateRunning || lifecycle.History[1].State != state.StateSucceeded {
		t.Fatalf("lifecycle history = %#v, want running -> succeeded", lifecycle.History)
	}
	if fakeAgent.invocation.WorktreePath == "" || fakeAgent.invocation.Prompt == "" || fakeAgent.invocation.LogPath == "" {
		t.Fatalf("agent invocation missing required fields: %#v", fakeAgent.invocation)
	}
	if fakeAgent.invocation.HardCap != WorkerHardCap || fakeAgent.invocation.StallTimeout != WorkerStallTimeout {
		t.Fatalf("agent supervision = hard cap %s stall %s, want %s/%s", fakeAgent.invocation.HardCap, fakeAgent.invocation.StallTimeout, WorkerHardCap, WorkerStallTimeout)
	}
	if strings.Contains(fakeAgent.invocation.Prompt, "Repo-local skills") {
		t.Fatalf("agent prompt unexpectedly included repo skills:\n%s", fakeAgent.invocation.Prompt)
	}
}

func TestDispatchRegisteredRunEmitsProgressReceiptsFromTracker(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	homeDir := t.TempDir()
	t.Setenv("LOOPCODER_HOME", homeDir)
	t.Setenv("CODEX_THREAD_ID", "thread-worker-progress-secret-canary")
	dbPath := filepath.Join(homeDir, "data", "loopcoder.db")
	clock := newWorkerManualClock(fixedNow())
	registerWorkerProgressProject(t, ctx, dbPath, repo, clock.Now)

	scratchRoot := t.TempDir()
	var warnings strings.Builder
	fakeGit := &workerFakeGit{status: " M internal/worker/worker.go\n"}
	fakeAgent := &workerFakeAgent{
		resultSet: true,
		result:    validWorkerAgentResult("Implemented dispatch.", 0),
		log:       "codex ok\n",
	}
	fakeGitHub := &workerFakeGitHub{prURL: "https://github.com/owner/repo/pull/101"}

	result, err := Dispatch(ctx, Options{
		RepoPath:    repo,
		IssueNumber: 101,
		IssueTitle:  "Implement dispatch",
		IssueBody:   "Body",
		RunID:       "run-progress",
		ProviderKey: "child-run:run-progress-child",
		Provider:    "codex",
		Stderr:      &warnings,
	}, Deps{
		Git: fakeGit,
		GitHub: func(string) GitHubClient {
			return fakeGitHub
		},
		AgentLookup: func(string) (agent.Runner, error) {
			return fakeAgent, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &workerFakeLock{}, nil
		},
		Now: clock.Now,
		PID: func() int {
			return 4321
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
		RemoveAll:          os.RemoveAll,
		OpenProgressStore:  storage.Open,
		ProgressClock:      clock,
		ProgressMaxSilence: time.Minute,
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v\nwarnings:\n%s", err, warnings.String())
	}
	if !result.OK {
		t.Fatalf("result OK = false: %#v", result)
	}

	store, err := storage.Open(ctx, storage.Options{Path: dbPath, Now: clock.Now})
	if err != nil {
		t.Fatalf("Open progress store: %v", err)
	}
	defer store.Close()
	receipts, err := progress.ListReceipts(ctx, store, progress.ListFilter{
		ProjectID:     "proj_worker_progress",
		DeliveryRunID: "run-progress",
		CorrelationID: "job-101-4321",
	})
	if err != nil {
		t.Fatalf("ListReceipts: %v", err)
	}
	if len(receipts) < 6 {
		t.Fatalf("receipt count = %d, want lifecycle receipts from Dispatch; warnings:\n%s", len(receipts), warnings.String())
	}
	phases := map[string]bool{}
	for _, receipt := range receipts {
		phases[receipt.Phase] = true
		if receipt.Provider.ProviderID != "codex" {
			t.Fatalf("receipt provider = %q, want codex", receipt.Provider.ProviderID)
		}
	}
	for _, want := range []string{"worktree_created", "prompt_written", "codex_started", "codex_exited", "cleanup"} {
		if !phases[want] {
			t.Fatalf("progress receipts missing phase %q: %#v", want, phases)
		}
	}
	last := receipts[len(receipts)-1]
	if last.Phase != "cleanup" || last.Status != "succeeded" {
		t.Fatalf("last receipt = %s/%s, want cleanup/succeeded", last.Phase, last.Status)
	}
	if last.Provider.ModelID != progress.Unknown {
		t.Fatalf("receipt model id = %q, want unknown because receipt generation does not consume provider tokens", last.Provider.ModelID)
	}
	receiptsJSON := mustWorkerJSON(t, receipts)
	if strings.Contains(receiptsJSON, "input_tokens") || strings.Contains(receiptsJSON, "output_tokens") {
		t.Fatalf("progress receipts unexpectedly contain provider token usage: %s", receiptsJSON)
	}
	obligations, err := progress.ListDeliveryObligations(ctx, store, progress.DeliveryObligationFilter{
		ProjectID:     "proj_worker_progress",
		DeliveryRunID: "run-progress",
		Limit:         100,
	})
	if err != nil {
		t.Fatalf("ListDeliveryObligations: %v", err)
	}
	if len(obligations) != len(receipts) {
		t.Fatalf("delivery obligation count = %d, want one per receipt (%d)", len(obligations), len(receipts))
	}
	receiptIDs := map[string]bool{}
	for _, receipt := range receipts {
		receiptIDs[receipt.ProgressReceiptID] = true
	}
	for _, obligation := range obligations {
		if !receiptIDs[obligation.ProgressReceiptID] {
			t.Fatalf("delivery obligation references unknown receipt: %#v", obligation)
		}
		if obligation.OriginKind != "host-run-origin" || obligation.SinkKind != "host" {
			t.Fatalf("delivery obligation origin/sink = %s/%s, want host-run-origin/host", obligation.OriginKind, obligation.SinkKind)
		}
		if obligation.TransportContract != runtimecap.HostProgressKnownOriginReplay {
			t.Fatalf("transport contract = %q, want %q", obligation.TransportContract, runtimecap.HostProgressKnownOriginReplay)
		}
		if obligation.AckPolicy != progress.DeliveryAckPolicyNone || obligation.RequiredAck {
			t.Fatalf("ack policy = %q required=%v, want no-ack", obligation.AckPolicy, obligation.RequiredAck)
		}
		if obligation.Status != progress.DeliveryPending {
			t.Fatalf("delivery obligation status = %q, want pending without host evidence", obligation.Status)
		}
	}
	acks, err := progress.ListDeliveryAcknowledgments(ctx, store, progress.DeliveryAckFilter{
		ProjectID:     "proj_worker_progress",
		DeliveryRunID: "run-progress",
		Limit:         10,
	})
	if err != nil {
		t.Fatalf("ListDeliveryAcknowledgments: %v", err)
	}
	if len(acks) != 0 {
		t.Fatalf("delivery acknowledgments = %#v, want none without host evidence", acks)
	}
	obligationsJSON := mustWorkerJSON(t, obligations)
	for _, forbidden := range []string{"thread-worker-progress-secret-canary", repo, "Body", "Implemented dispatch.", "internal/worker/worker.go"} {
		if strings.Contains(obligationsJSON, forbidden) {
			t.Fatalf("delivery obligation leaked forbidden value %q: %s", forbidden, obligationsJSON)
		}
	}
}

func TestProgressRecorderTaskCountsTruthfulForAttemptStatuses(t *testing.T) {
	now := fixedNow()
	recorder := &progressRecorder{now: func() time.Time { return now }}
	tests := []struct {
		status string
		want   progress.TaskCounts
	}{
		{state.StatusPlanned, progress.TaskCounts{Total: 1, Ready: 1}},
		{state.StatusQueued, progress.TaskCounts{Total: 1, Ready: 1}},
		{state.StatusLaunching, progress.TaskCounts{Total: 1, Ready: 1}},
		{state.StatusRunning, progress.TaskCounts{Total: 1, Running: 1}},
		{state.StatusFinishing, progress.TaskCounts{Total: 1, Running: 1}},
		{state.StatusWaiting, progress.TaskCounts{Total: 1, Blocked: 1}},
		{state.StatusSucceeded, progress.TaskCounts{Total: 1, Succeeded: 1}},
		{state.StatusSucceededWithOptionalFailures, progress.TaskCounts{Total: 1, Succeeded: 1}},
		{state.StatusFailed, progress.TaskCounts{Total: 1, Failed: 1}},
		{state.StatusCancelled, progress.TaskCounts{Total: 1, Failed: 1}},
		{state.StatusTimedOut, progress.TaskCounts{Total: 1, Failed: 1}},
		{state.StatusAbandoned, progress.TaskCounts{Total: 1, Failed: 1}},
		{state.StatusSkipped, progress.TaskCounts{Total: 1, Failed: 1}},
		{state.StatusHung, progress.TaskCounts{Total: 1, Blocked: 1}},
		{state.StatusNeedsHuman, progress.TaskCounts{Total: 1, Blocked: 1}},
		{"unrecognized", progress.TaskCounts{Total: 1, Unknown: 1}},
	}
	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			obs := recorder.observation(state.AttemptRecord{
				JobID:          "job-counts",
				Issue:          828,
				Attempt:        1,
				Provider:       "codex",
				Phase:          "test_phase",
				Status:         tt.status,
				StartedAt:      state.FormatTimestamp(now.Add(-time.Minute)),
				HeartbeatAt:    state.FormatTimestamp(now.Add(-30 * time.Second)),
				LastProgressAt: state.FormatTimestamp(now.Add(-45 * time.Second)),
			})
			if obs.TaskCounts != tt.want {
				t.Fatalf("TaskCounts for %q = %#v, want %#v", tt.status, obs.TaskCounts, tt.want)
			}
			sum := obs.TaskCounts.Ready + obs.TaskCounts.Running + obs.TaskCounts.Succeeded + obs.TaskCounts.Failed + obs.TaskCounts.Blocked + obs.TaskCounts.Unknown
			if sum != obs.TaskCounts.Total {
				t.Fatalf("TaskCounts sum = %d, total = %d for %#v", sum, obs.TaskCounts.Total, obs.TaskCounts)
			}
		})
	}
}

func TestProgressRecorderRetryHonorsStaleWorkerOwnership(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	homeDir := t.TempDir()
	dbPath := filepath.Join(homeDir, "data", "loopcoder.db")
	clock := newWorkerManualClock(fixedNow())
	registerWorkerProgressProject(t, ctx, dbPath, repo, clock.Now)

	ownershipStore, err := storage.Open(ctx, storage.Options{Path: dbPath, Now: clock.Now})
	if err != nil {
		t.Fatalf("Open ownership store: %v", err)
	}
	defer ownershipStore.Close()
	lease, err := storage.AcquireAgentOwnershipLease(ctx, ownershipStore, storage.AgentOwnershipLeaseRequest{
		ProjectID:     "proj_worker_progress",
		DeliveryRunID: "run-progress",
		RunID:         "run-progress",
		OwnerID:       "worker:run-progress:job-ownership-retry:1",
		Now:           clock.Now(),
		LeaseUntil:    clock.Now().Add(time.Hour),
		Resources: []storage.AgentOwnershipResource{
			{ResourceKind: "repo-path", ResourceKey: "."},
		},
	})
	if err != nil {
		t.Fatalf("AcquireAgentOwnershipLease: %v", err)
	}

	var failing *workerFailingWriteStore
	validateCalls := 0
	var validateMu sync.Mutex
	recorder, err := newProgressRecorder(ctx, Options{
		IssueNumber: 828,
		RunID:       "run-progress",
		Attempt:     1,
		Provider:    "codex",
	}, Deps{
		Now: clock.Now,
		OpenProgressStore: func(ctx context.Context, opts storage.Options) (storage.Store, error) {
			store, err := storage.Open(ctx, opts)
			if err != nil {
				return nil, err
			}
			failing = &workerFailingWriteStore{Store: store, skip: 1, failures: 100}
			return failing, nil
		},
		ProgressClock:      clock,
		ProgressMaxSilence: 20 * time.Second,
	}, runtimepath.Roots{
		Registered:   true,
		ProjectID:    "proj_worker_progress",
		DatabasePath: dbPath,
	}, "job-ownership-retry", io.Discard, func(ctx context.Context) error {
		validateMu.Lock()
		validateCalls++
		validateMu.Unlock()
		return storage.ValidateAgentOwnershipFence(ctx, ownershipStore, lease)
	})
	if err != nil {
		t.Fatalf("newProgressRecorder: %v", err)
	}
	defer recorder.Stop()

	recorder.RecordAttempt(state.AttemptRecord{
		JobID:          "job-ownership-retry",
		Issue:          828,
		Attempt:        1,
		Provider:       "codex",
		Phase:          "codex_started",
		Status:         state.StatusRunning,
		StartedAt:      state.FormatTimestamp(clock.Now()),
		HeartbeatAt:    state.FormatTimestamp(clock.Now()),
		LastProgressAt: state.FormatTimestamp(clock.Now()),
	}, false)
	waitForWorkerWriteAttempts(t, failing, 1)

	clock.Advance(20 * time.Second)
	waitForWorkerWriteAttempts(t, failing, 2)
	if err := storage.ReleaseAgentOwnershipLease(ctx, ownershipStore, lease, clock.Now().Add(time.Second)); err != nil {
		t.Fatalf("ReleaseAgentOwnershipLease: %v", err)
	}
	clock.Advance(10 * time.Second)
	waitForWorkerValidationCalls(t, &validateMu, &validateCalls, 3)
	if got := failing.Attempts(); got != 2 {
		t.Fatalf("write attempts after stale retry = %d, want stale ownership to block before persistence", got)
	}

	verifyStore, err := storage.Open(ctx, storage.Options{Path: dbPath, Now: clock.Now})
	if err != nil {
		t.Fatalf("Open verify store: %v", err)
	}
	defer verifyStore.Close()
	receipts, err := progress.ListReceipts(ctx, verifyStore, progress.ListFilter{
		ProjectID:     "proj_worker_progress",
		DeliveryRunID: "run-progress",
		CorrelationID: "job-ownership-retry",
	})
	if err != nil {
		t.Fatalf("ListReceipts: %v", err)
	}
	if len(receipts) != 1 {
		t.Fatalf("receipt count after stale retry = %d, want only initial durable receipt", len(receipts))
	}
}

func TestDispatchRecordsCancelledFailureState(t *testing.T) {
	repo := t.TempDir()
	scratchRoot := t.TempDir()
	var warnings strings.Builder

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Dispatch(ctx, Options{
		RepoPath:    repo,
		IssueNumber: 110,
		IssueTitle:  "Cancelled worker",
		RunID:       "run-cancelled",
		Provider:    "codex",
		Stderr:      &warnings,
	}, Deps{
		Git: &workerFakeGit{status: " M file.go\n"},
		GitHub: func(string) GitHubClient {
			return &workerFakeGitHub{}
		},
		AgentLookup: func(string) (agent.Runner, error) {
			return workerContextErrAgent{}, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &workerFakeLock{}, nil
		},
		Now: fixedNow,
		PID: func() int {
			return 4321
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
		RemoveAll: os.RemoveAll,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Dispatch error = %v, want context.Canceled", err)
	}
	attempts, err := state.LoadAttempts(repo, "run-cancelled")
	if err != nil {
		t.Fatalf("LoadAttempts returned error: %v", err)
	}
	if len(attempts) != 1 || attempts[0].Status != state.StatusCancelled {
		t.Fatalf("cancelled attempt = %#v", attempts)
	}
	brief, err := os.ReadFile(state.RecoveryBriefPath(repo, "run-cancelled", "job-110-4321"))
	if err != nil {
		t.Fatalf("read recovery brief: %v", err)
	}
	if !strings.Contains(string(brief), "- Status: cancelled") || !strings.Contains(string(brief), "context canceled") {
		t.Fatalf("cancelled recovery brief missing status/error:\n%s", string(brief))
	}
}

func TestDispatchRecordsTimedOutFailureState(t *testing.T) {
	repo := t.TempDir()
	scratchRoot := t.TempDir()
	var warnings strings.Builder

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	_, err := Dispatch(ctx, Options{
		RepoPath:    repo,
		IssueNumber: 111,
		IssueTitle:  "Timed out worker",
		RunID:       "run-timeout",
		Provider:    "codex",
		Stderr:      &warnings,
	}, Deps{
		Git: &workerFakeGit{status: " M file.go\n"},
		GitHub: func(string) GitHubClient {
			return &workerFakeGitHub{}
		},
		AgentLookup: func(string) (agent.Runner, error) {
			return workerContextErrAgent{}, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &workerFakeLock{}, nil
		},
		Now: fixedNow,
		PID: func() int {
			return 4321
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
		RemoveAll: os.RemoveAll,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Dispatch error = %v, want context deadline exceeded", err)
	}
	attempts, err := state.LoadAttempts(repo, "run-timeout")
	if err != nil {
		t.Fatalf("LoadAttempts returned error: %v", err)
	}
	if len(attempts) != 1 || attempts[0].Status != state.StatusTimedOut {
		t.Fatalf("timed out attempt = %#v", attempts)
	}
}

func TestDispatchInjectsRepoSkillsFromWorktree(t *testing.T) {
	repo := t.TempDir()
	scratchRoot := t.TempDir()
	var warnings strings.Builder
	fakeGit := &workerFakeGit{
		status: " M internal/worker/worker.go\n",
		worktreeSetup: func(worktreePath string) error {
			skillDir := filepath.Join(worktreePath, ".claude", "skills", "go-style")
			if err := os.MkdirAll(skillDir, 0o755); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(`---
name: go-style
description: Repository Go conventions
---

# Go Style

This implementation detail should stay out of the prompt.
`), 0o644)
		},
	}
	fakeAgent := &workerFakeAgent{
		resultSet: true,
		result:    validWorkerAgentResult("Implemented dispatch.", 0),
		log:       "codex ok\n",
	}

	_, err := Dispatch(context.Background(), Options{
		RepoPath:    repo,
		IssueNumber: 101,
		IssueTitle:  "Implement dispatch",
		IssueBody:   "Body",
		RunID:       "run-test",
		Provider:    "codex",
		Stderr:      &warnings,
	}, Deps{
		Git: fakeGit,
		GitHub: func(string) GitHubClient {
			return &workerFakeGitHub{prURL: "https://github.com/owner/repo/pull/101"}
		},
		AgentLookup: func(provider string) (agent.Runner, error) {
			return fakeAgent, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &workerFakeLock{}, nil
		},
		Now: fixedNow,
		PID: func() int {
			return 4321
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
		RemoveAll: os.RemoveAll,
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	for _, want := range []string{
		"## Repo-local skills",
		"Path: `.claude/skills/go-style/SKILL.md`",
		"Summary: Repository Go conventions",
		"- # Go Style",
	} {
		if !strings.Contains(fakeAgent.invocation.Prompt, want) {
			t.Fatalf("agent prompt missing %q:\n%s", want, fakeAgent.invocation.Prompt)
		}
	}
	if strings.Contains(fakeAgent.invocation.Prompt, "This implementation detail should stay out of the prompt") {
		t.Fatalf("agent prompt included skill body text:\n%s", fakeAgent.invocation.Prompt)
	}
}

func TestDispatchInjectsDomainConfiguredSkillsFromWorktree(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".delivery.yml"), []byte(`
domain:
  skills:
    paths:
      - governance/**/skill.md
    machine_library:
      paths:
        - .loopcoder/skill-library/**/*.md
    select:
      - governance
      - disclosure
    prompt_budget_bytes: 2048
`), 0o644); err != nil {
		t.Fatalf("write delivery config: %v", err)
	}
	scratchRoot := t.TempDir()
	fakeGit := &workerFakeGit{
		status: " M internal/worker/worker.go\n",
		worktreeSetup: func(worktreePath string) error {
			if err := os.MkdirAll(filepath.Join(worktreePath, ".claude", "skills", "go-style"), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(worktreePath, ".claude", "skills", "go-style", "SKILL.md"), []byte(`---
name: go-style
description: Repository Go conventions
---

# Go Style
`), 0o644); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Join(worktreePath, "governance", "review"), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(worktreePath, "governance", "review", "skill.md"), []byte(`---
name: Governance Review
description: Governance review conventions
tags: [governance]
---

# Governance Review

Full governance body should stay out of the prompt.
`), 0o644); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Join(worktreePath, ".loopcoder", "skill-library"), 0o755); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(worktreePath, ".loopcoder", "skill-library", "disclosure.md"), []byte(`---
description: Disclosure library metadata
---

# Disclosure Library
`), 0o644)
		},
	}
	fakeAgent := &workerFakeAgent{
		resultSet: true,
		result:    validWorkerAgentResult("Implemented domain skills.", 0),
		log:       "codex ok\n",
	}

	_, err := Dispatch(context.Background(), Options{
		RepoPath:    repo,
		IssueNumber: 463,
		IssueTitle:  "Configurable skill discovery",
		IssueBody:   "Body",
		RunID:       "run-test",
		Provider:    "codex",
	}, Deps{
		Git: fakeGit,
		GitHub: func(string) GitHubClient {
			return &workerFakeGitHub{prURL: "https://github.com/owner/repo/pull/463"}
		},
		AgentLookup: func(provider string) (agent.Runner, error) {
			return fakeAgent, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &workerFakeLock{}, nil
		},
		Now: fixedNow,
		PID: func() int {
			return 4321
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
		RemoveAll: os.RemoveAll,
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	for _, want := range []string{
		"Discovery paths:",
		"- skills: `governance/**/skill.md`",
		"- machine_library: `.loopcoder/skill-library/**/*.md`",
		"Selection filter: governance, disclosure.",
		"Budget: 2048 bytes.",
		"Path: `governance/review/skill.md`",
		"Summary: Governance review conventions",
		"Tags: `governance`",
		"Path: `.loopcoder/skill-library/disclosure.md`",
		"Summary: Disclosure library metadata",
	} {
		if !strings.Contains(fakeAgent.invocation.Prompt, want) {
			t.Fatalf("agent prompt missing %q:\n%s", want, fakeAgent.invocation.Prompt)
		}
	}
	for _, notWant := range []string{
		"Path: `.claude/skills/go-style/SKILL.md`",
		"Full governance body should stay out of the prompt",
	} {
		if strings.Contains(fakeAgent.invocation.Prompt, notWant) {
			t.Fatalf("agent prompt contained %q:\n%s", notWant, fakeAgent.invocation.Prompt)
		}
	}
}

func TestDispatchPassesConfiguredWorkerMCPServers(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".delivery.yml"), []byte(`
mcp:
  servers:
    - name: worker-index
      transport: stdio
      command: ./tools/worker-index
      args: ["--root", "."]
      roles: [worker]
      read_only: false
    - name: shared-read
      transport: http
      url: https://mcp.example.com/shared
      auth:
        header: Authorization
        env: SHARED_MCP_TOKEN
      roles: [worker, verifier]
      read_only: true
    - name: verifier-only
      transport: stdio
      command: ./tools/verifier-only
      roles: [verifier]
      read_only: true
`), 0o644); err != nil {
		t.Fatalf("write delivery config: %v", err)
	}
	scratchRoot := t.TempDir()
	fakeGit := &workerFakeGit{status: " M file.go\n"}
	fakeAgent := &workerFakeAgent{
		resultSet: true,
		result:    validWorkerAgentResult("Implemented MCP.", 0),
		log:       "codex ok\n",
	}

	_, err := Dispatch(context.Background(), Options{
		RepoPath:    repo,
		IssueNumber: 465,
		IssueTitle:  "MCP invocation contract",
		IssueBody:   "Body",
		RunID:       "run-test",
		Provider:    "codex",
	}, Deps{
		Git: fakeGit,
		GitHub: func(string) GitHubClient {
			return &workerFakeGitHub{prURL: "https://github.com/owner/repo/pull/465"}
		},
		AgentLookup: func(string) (agent.Runner, error) {
			return fakeAgent, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &workerFakeLock{}, nil
		},
		Now: fixedNow,
		PID: func() int {
			return 4321
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
		RemoveAll: os.RemoveAll,
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}

	servers := fakeAgent.invocation.MCPServers
	if len(servers) != 2 {
		t.Fatalf("MCPServers = %#v, want worker-index and shared-read", servers)
	}
	if servers[0].Name != "worker-index" || servers[0].Command != "./tools/worker-index" || servers[0].ReadOnly {
		t.Fatalf("first MCP server = %#v, want writable worker-index", servers[0])
	}
	if len(servers[0].Args) != 2 || servers[0].Args[0] != "--root" || servers[0].Args[1] != "." {
		t.Fatalf("worker-index args = %#v, want --root .", servers[0].Args)
	}
	if servers[1].Name != "shared-read" || servers[1].URL != "https://mcp.example.com/shared" || !servers[1].ReadOnly {
		t.Fatalf("second MCP server = %#v, want read-only shared-read", servers[1])
	}
	if servers[1].Auth.Header != "Authorization" || servers[1].Auth.Env != "SHARED_MCP_TOKEN" {
		t.Fatalf("shared-read auth = %#v, want env-backed Authorization", servers[1].Auth)
	}
}

func TestDispatchPassesDomainLivenessLogOnlyToAgent(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".delivery.yml"), []byte(`
domain:
  liveness:
    mode: log-only
`), 0o644); err != nil {
		t.Fatalf("write delivery config: %v", err)
	}
	scratchRoot := t.TempDir()
	fakeGit := &workerFakeGit{status: " M file.go\n"}
	fakeAgent := &workerFakeAgent{
		resultSet: true,
		result:    validWorkerAgentResult("Implemented liveness.", 0),
		log:       "codex ok\n",
	}

	_, err := Dispatch(context.Background(), Options{
		RepoPath:    repo,
		IssueNumber: 469,
		IssueTitle:  "Domain liveness",
		IssueBody:   "Body",
		RunID:       "run-test",
		Provider:    "codex",
	}, Deps{
		Git: fakeGit,
		GitHub: func(string) GitHubClient {
			return &workerFakeGitHub{prURL: "https://github.com/owner/repo/pull/469"}
		},
		AgentLookup: func(string) (agent.Runner, error) {
			return fakeAgent, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &workerFakeLock{}, nil
		},
		Now: fixedNow,
		PID: func() int {
			return 4321
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
		RemoveAll: os.RemoveAll,
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if fakeAgent.invocation.LivenessMode != "log-only" {
		t.Fatalf("LivenessMode = %q, want log-only", fakeAgent.invocation.LivenessMode)
	}
	if fakeAgent.invocation.LivenessCommand != "" {
		t.Fatalf("LivenessCommand = %q, want empty", fakeAgent.invocation.LivenessCommand)
	}
}

func TestDispatchPassesDomainCustomLivenessCommandToAgent(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".delivery.yml"), []byte(`
domain:
  liveness:
    mode: custom
    command: echo alive
`), 0o644); err != nil {
		t.Fatalf("write delivery config: %v", err)
	}
	scratchRoot := t.TempDir()
	fakeGit := &workerFakeGit{status: " M file.go\n"}
	fakeAgent := &workerFakeAgent{
		resultSet: true,
		result:    validWorkerAgentResult("Implemented custom liveness.", 0),
		log:       "codex ok\n",
	}

	_, err := Dispatch(context.Background(), Options{
		RepoPath:    repo,
		IssueNumber: 469,
		IssueTitle:  "Domain liveness",
		IssueBody:   "Body",
		RunID:       "run-test",
		Provider:    "codex",
	}, Deps{
		Git: fakeGit,
		GitHub: func(string) GitHubClient {
			return &workerFakeGitHub{prURL: "https://github.com/owner/repo/pull/469"}
		},
		AgentLookup: func(string) (agent.Runner, error) {
			return fakeAgent, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &workerFakeLock{}, nil
		},
		Now: fixedNow,
		PID: func() int {
			return 4321
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
		RemoveAll: os.RemoveAll,
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if fakeAgent.invocation.LivenessMode != "custom" {
		t.Fatalf("LivenessMode = %q, want custom", fakeAgent.invocation.LivenessMode)
	}
	if fakeAgent.invocation.LivenessCommand != "echo alive" {
		t.Fatalf("LivenessCommand = %q, want echo alive", fakeAgent.invocation.LivenessCommand)
	}
}

func TestDispatchPassesDomainCustomLivenessArgvToAgent(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".delivery.yml"), []byte(`
domain:
  liveness:
    mode: custom
    argv: ["./tools/liveness", "--literal", "alive && exit 9"]
`), 0o644); err != nil {
		t.Fatalf("write delivery config: %v", err)
	}
	scratchRoot := t.TempDir()
	fakeGit := &workerFakeGit{status: " M file.go\n"}
	fakeAgent := &workerFakeAgent{
		resultSet: true,
		result:    validWorkerAgentResult("Implemented custom liveness argv.", 0),
		log:       "codex ok\n",
	}

	_, err := Dispatch(context.Background(), Options{
		RepoPath:    repo,
		IssueNumber: 490,
		IssueTitle:  "Domain liveness argv",
		IssueBody:   "Body",
		RunID:       "run-test",
		Provider:    "codex",
	}, Deps{
		Git: fakeGit,
		GitHub: func(string) GitHubClient {
			return &workerFakeGitHub{prURL: "https://github.com/owner/repo/pull/490"}
		},
		AgentLookup: func(string) (agent.Runner, error) {
			return fakeAgent, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &workerFakeLock{}, nil
		},
		Now: fixedNow,
		PID: func() int {
			return 4321
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
		RemoveAll: os.RemoveAll,
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if fakeAgent.invocation.LivenessMode != "custom" {
		t.Fatalf("LivenessMode = %q, want custom", fakeAgent.invocation.LivenessMode)
	}
	wantCommand := supervisedexec.EncodeLivenessArgv([]string{"./tools/liveness", "--literal", "alive && exit 9"})
	if fakeAgent.invocation.LivenessCommand != wantCommand {
		t.Fatalf("LivenessCommand = %q, want encoded argv command %q", fakeAgent.invocation.LivenessCommand, wantCommand)
	}
}

func TestGitHubBoundTextBuildersHaveZeroReportFootprint(t *testing.T) {
	record := buildWorkerReport(Options{
		IssueNumber: 101,
		IssueTitle:  "Implement dispatch",
		Provider:    "codex",
	}, validWorkerAgentResult("Implemented dispatch.", 0))
	if err := record.Validate(); err != nil {
		t.Fatalf("test report does not validate: %v", err)
	}

	surfaces := map[string]string{
		"pr body":        buildPRBody(101, "Implemented dispatch."),
		"commit message": buildCommitMessage("Implement dispatch", 101),
	}
	for name, text := range surfaces {
		assertNoReportFootprint(t, name, text, record)
	}
	if surfaces["pr body"] != "Closes #101\n\nImplemented dispatch." {
		t.Fatalf("PR body surface = %q", surfaces["pr body"])
	}
	if surfaces["commit message"] != "Implement dispatch (closes #101)" {
		t.Fatalf("commit message surface = %q", surfaces["commit message"])
	}
}

func TestBuildWorkerReportAllowsAntigravitySelfReportedNoUsage(t *testing.T) {
	record := buildWorkerReport(Options{
		IssueNumber: 559,
		Provider:    "antigravity",
	}, agent.Result{
		ExitCode:   0,
		Summary:    "Implemented antigravity runner.",
		Model:      "Gemini 3.1 Pro (High)",
		Effort:     "High",
		StartedAt:  "2026-06-28T00:00:00Z",
		EndedAt:    "2026-06-28T00:00:02Z",
		DurationMS: 2000,
	})

	if record.ModelSource != reporter.ModelSourceSelfReported {
		t.Fatalf("ModelSource = %q, want self-reported", record.ModelSource)
	}
	if record.Usage.TotalTokens != nil || record.Usage.InputTokens != nil || record.Usage.OutputTokens != nil {
		t.Fatalf("Usage = %#v, want empty", record.Usage)
	}
	if err := record.Validate(); err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
}

func TestBuildWorkerReportIncludesGrokAdapterAttribution(t *testing.T) {
	record := buildWorkerReport(Options{
		IssueNumber: 834,
		Provider:    "grok",
		Branch:      "loop/issue-834",
		RunID:       "run-834",
	}, agent.Result{
		ExitCode:           0,
		Model:              "grok-4.5",
		AdapterVersion:     "0.1.211",
		ExternalSessionRef: "session-abc",
		StartedAt:          "2026-07-13T00:00:00Z",
		EndedAt:            "2026-07-13T00:00:01Z",
		DurationMS:         1000,
	})

	want := "implement issue #834 [adapter=0.1.211 attempt=run-834 session=session-abc]"
	if record.Action != want {
		t.Fatalf("Action = %q, want %q", record.Action, want)
	}
	if err := record.Validate(); err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
}

func TestDispatchFailureWritesRecoveryBriefAndPreservesArtifacts(t *testing.T) {
	repo := t.TempDir()
	scratchRoot := t.TempDir()
	var warnings strings.Builder
	fakeGit := &workerFakeGit{status: " M file.go\n"}
	fakeAgent := &workerFakeAgent{
		exitCode: 7,
		log:      "Authorization: Bearer abc.def\npassword=hunter2\nlast line\n",
	}
	fakeGitHub := &workerFakeGitHub{
		prs: []gh.PullRequestReference{{Number: 11, URL: "https://github.com/owner/repo/pull/11"}},
	}

	_, err := Dispatch(context.Background(), Options{
		RepoPath:    repo,
		IssueNumber: 101,
		IssueTitle:  "Implement dispatch",
		RunID:       "run-test",
		Provider:    "codex",
		Stderr:      &warnings,
	}, Deps{
		Git: fakeGit,
		GitHub: func(string) GitHubClient {
			return fakeGitHub
		},
		AgentLookup: func(provider string) (agent.Runner, error) {
			if provider != "codex" {
				t.Fatalf("provider = %q, want codex", provider)
			}
			return fakeAgent, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &workerFakeLock{}, nil
		},
		Now: fixedNow,
		PID: func() int {
			return 4321
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
		RemoveAll: os.RemoveAll,
	})
	if err == nil {
		t.Fatal("Dispatch returned nil error, want failure")
	}
	if !strings.Contains(err.Error(), "codex exec failed (exit 7)") {
		t.Fatalf("error = %v", err)
	}
	if !strings.Contains(warnings.String(), "preserved failed attempt artifacts") {
		t.Fatalf("warnings missing artifact preservation note: %q", warnings.String())
	}

	briefPath := state.RecoveryBriefPath(repo, "run-test", "job-101-4321")
	brief, err := os.ReadFile(briefPath)
	if err != nil {
		t.Fatalf("ReadFile recovery brief: %v", err)
	}
	briefText := string(brief)
	for _, want := range []string{
		"# Recovery context for issue #101",
		"- Last phase: codex_exited",
		" M file.go",
		"#11 https://github.com/owner/repo/pull/11",
		"Bearer [REDACTED_TOKEN]",
		"password=[REDACTED_SECRET]",
	} {
		if !strings.Contains(briefText, want) {
			t.Fatalf("recovery brief missing %q:\n%s", want, briefText)
		}
	}
	for _, leaked := range []string{"abc.def", "hunter2"} {
		if strings.Contains(briefText, leaked) {
			t.Fatalf("recovery brief leaked %q:\n%s", leaked, briefText)
		}
	}

	attempts, err := state.LoadAttempts(repo, "run-test")
	if err != nil {
		t.Fatalf("LoadAttempts returned error: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("LoadAttempts returned %d attempts, want 1", len(attempts))
	}
	got := attempts[0]
	if got.Phase != "codex_exited" || got.Status != "failed" {
		t.Fatalf("failed attempt = %#v", got)
	}
	if got.ExitCode == nil || *got.ExitCode != 7 {
		t.Fatalf("failed attempt exit code = %#v, want 7", got.ExitCode)
	}
	if got.ArtifactDecision == nil || got.ArtifactDecision.State != artifactDecisionPreserveSelected ||
		got.ArtifactDecision.WorktreePath == "" || got.ArtifactDecision.ScratchPath == "" ||
		got.ArtifactDecision.AttemptPath == "" || got.ArtifactDecision.ManifestPath == "" {
		t.Fatalf("failed artifact decision = %#v, want preserve-selected with existing paths", got.ArtifactDecision)
	}
}

func TestPreservationManifestWriteFailureReportsPartialWithoutClaimingManifest(t *testing.T) {
	repo := t.TempDir()
	scratchRoot := t.TempDir()
	var warnings strings.Builder
	fakeGit := &workerFakeGit{status: " M file.go\n"}
	fakeAgent := &workerFakeAgent{
		resultSet: true,
		result:    validWorkerAgentResult("Could not finish.", 7),
		log:       "last line\n",
	}

	_, err := Dispatch(context.Background(), Options{
		RepoPath:    repo,
		IssueNumber: 121,
		IssueTitle:  "Preserve failed attempt",
		RunID:       "run-preserve-partial",
		Provider:    "codex",
		Stderr:      &warnings,
	}, Deps{
		Git: fakeGit,
		GitHub: func(string) GitHubClient {
			return &workerFakeGitHub{}
		},
		AgentLookup: func(string) (agent.Runner, error) {
			return fakeAgent, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &workerFakeLock{}, nil
		},
		Now: fixedNow,
		PID: func() int {
			return 7777
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
		WriteFile: func(path string, data []byte, perm os.FileMode) error {
			if strings.HasSuffix(path, "-preserved.json") {
				return errors.New("deterministic Windows open-file fixture: file is in use")
			}
			return os.WriteFile(path, data, perm)
		},
		RemoveAll: os.RemoveAll,
	})
	if err == nil {
		t.Fatal("Dispatch returned nil error, want failure")
	}
	if !strings.Contains(warnings.String(), "failed to write preservation manifest") ||
		!strings.Contains(warnings.String(), "preservation incomplete") {
		t.Fatalf("warnings missing partial preservation failure:\n%s", warnings.String())
	}
	if strings.Contains(warnings.String(), "preserved manifest:") {
		t.Fatalf("warnings claimed failed manifest was preserved:\n%s", warnings.String())
	}
	assertPathMissing(t, state.PreservationManifestPath(repo, "run-preserve-partial", "job-121-7777"))
	attempts, err := state.LoadAttempts(repo, "run-preserve-partial")
	if err != nil {
		t.Fatalf("LoadAttempts returned error: %v", err)
	}
	if len(attempts) != 1 || attempts[0].ArtifactDecision == nil {
		t.Fatalf("attempts = %#v, want artifact decision", attempts)
	}
	decision := attempts[0].ArtifactDecision
	if decision.State != artifactDecisionPreserveSelected || decision.ManifestPath != "" ||
		!strings.Contains(strings.Join(decision.PreservationErrors, "\n"), "file is in use") {
		t.Fatalf("artifact decision = %#v, want partial preserve without manifest path", decision)
	}
}

func TestDispatchHungWithEmptyWorktreeWritesHungStateAndNoReport(t *testing.T) {
	repo := t.TempDir()
	scratchRoot := t.TempDir()
	var warnings strings.Builder
	fakeGit := &workerFakeGit{}
	fakeGitHub := &workerFakeGitHub{}
	fakeAgent := &workerFakeAgent{
		resultSet: true,
		result: agent.Result{
			ExitCode:   -1,
			Hung:       true,
			HungReason: agent.HungReasonStall,
		},
		log: "provider stopped producing output\n",
	}

	result, err := Dispatch(context.Background(), Options{
		RepoPath:    repo,
		IssueNumber: 101,
		IssueTitle:  "Implement dispatch",
		RunID:       "run-test",
		Provider:    "codex",
		Stderr:      &warnings,
	}, Deps{
		Git: fakeGit,
		GitHub: func(string) GitHubClient {
			return fakeGitHub
		},
		AgentLookup: func(provider string) (agent.Runner, error) {
			return fakeAgent, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &workerFakeLock{}, nil
		},
		Now: fixedNow,
		PID: func() int {
			return 4321
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
		RemoveAll: os.RemoveAll,
	})
	if err == nil {
		t.Fatal("Dispatch returned nil error, want hung failure")
	}
	if result.Status != "hung" || result.OK || result.Report != nil {
		t.Fatalf("hung result = %#v, want status hung, not ok, no report", result)
	}
	if fakeGit.addAllCalls != 0 || fakeGit.commitCalls != 0 || fakeGit.pushCalls != 0 || fakeGit.forcePushCalls != 0 {
		t.Fatalf("hung empty worktree made harvest git calls: add=%d commit=%d push=%d forcePush=%d", fakeGit.addAllCalls, fakeGit.commitCalls, fakeGit.pushCalls, fakeGit.forcePushCalls)
	}
	if fakeGitHub.createPRCalls != 0 {
		t.Fatalf("CreatePR calls = %d, want 0", fakeGitHub.createPRCalls)
	}
	for _, want := range []string{"exec hung", "reason=hung", "hung_reason=stall"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("hung error missing %q: %v", want, err)
		}
	}

	attempts, err := state.LoadAttempts(repo, "run-test")
	if err != nil {
		t.Fatalf("LoadAttempts returned error: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("LoadAttempts returned %d attempts, want 1", len(attempts))
	}
	got := attempts[0]
	if got.Status != "hung" || got.Report != nil {
		t.Fatalf("hung attempt = %#v, want status hung and no report", got)
	}
	if !strings.Contains(got.Error, "reason=hung") {
		t.Fatalf("hung attempt error = %q, want reason=hung", got.Error)
	}

	brief, err := os.ReadFile(state.RecoveryBriefPath(repo, "run-test", "job-101-4321"))
	if err != nil {
		t.Fatalf("ReadFile recovery brief: %v", err)
	}
	if !strings.Contains(string(brief), "- Status: hung") || !strings.Contains(string(brief), "reason=hung") {
		t.Fatalf("hung recovery brief missing status/reason:\n%s", string(brief))
	}

	events, err := os.ReadFile(state.EventsPath(repo, "run-test"))
	if err != nil {
		t.Fatalf("ReadFile events: %v", err)
	}
	for _, want := range []string{`"status":"hung"`, `"event":"worker_hung"`, `"outcome":"hung"`, `"reason":"hung"`} {
		if !strings.Contains(string(events), want) {
			t.Fatalf("events missing %q:\n%s", want, string(events))
		}
	}
}

func TestCleanupPartialFailuresPersistDecisionAndLeaveEvidence(t *testing.T) {
	repo := t.TempDir()
	scratchRoot := t.TempDir()
	var warnings strings.Builder
	fakeGit := &workerFakeGit{
		status:            " M internal/worker/worker.go\n",
		worktreeRemoveErr: errors.New("unix permission fixture: permission denied"),
		branchDeleteErr:   errors.New("branch is checked out elsewhere"),
	}
	fakeAgent := &workerFakeAgent{
		resultSet: true,
		result:    validWorkerAgentResult("Implemented dispatch.", 0),
		log:       "codex ok\n",
	}
	removeErr := errors.New("deterministic Windows open-file fixture: file is in use")

	result, err := Dispatch(context.Background(), Options{
		RepoPath:    repo,
		IssueNumber: 122,
		IssueTitle:  "Cleanup partial failures",
		RunID:       "run-cleanup-partial",
		Provider:    "codex",
		Stderr:      &warnings,
	}, Deps{
		Git: fakeGit,
		GitHub: func(string) GitHubClient {
			return &workerFakeGitHub{prURL: "https://github.com/owner/repo/pull/122"}
		},
		AgentLookup: func(string) (agent.Runner, error) {
			return fakeAgent, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &workerFakeLock{}, nil
		},
		Now: fixedNow,
		PID: func() int {
			return 8888
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
		RemoveAll: func(string) error {
			return removeErr
		},
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if !result.OK || result.Status != "succeeded" {
		t.Fatalf("result = %#v, want succeeded despite cleanup warning", result)
	}
	for _, want := range []string{"failed to remove worktree", "failed to delete local branch", "failed to remove scratch directory"} {
		if !strings.Contains(warnings.String(), want) {
			t.Fatalf("warnings missing %q:\n%s", want, warnings.String())
		}
	}
	assertPathExists(t, fakeGit.worktreePath)
	attempts, err := state.LoadAttempts(repo, "run-cleanup-partial")
	if err != nil {
		t.Fatalf("LoadAttempts returned error: %v", err)
	}
	if len(attempts) != 1 || attempts[0].ArtifactDecision == nil {
		t.Fatalf("attempts = %#v, want artifact cleanup decision", attempts)
	}
	decision := attempts[0].ArtifactDecision
	if decision.State != artifactDecisionCleanupPartial || len(decision.CleanupErrors) != 3 ||
		decision.OwnerID != "worker:run-cleanup-partial:job-122-8888:1" {
		t.Fatalf("artifact cleanup decision = %#v, want partial cleanup with three errors", decision)
	}
}

func TestDispatchHungWithDirtyWorktreeHarvestsNeedsHumanPR(t *testing.T) {
	repo := t.TempDir()
	scratchRoot := t.TempDir()
	var warnings strings.Builder
	fakeGit := &workerFakeGit{status: " M file.go\n?? new.go\n"}
	totalTokens := int64(77)
	fakeAgent := &workerFakeAgent{
		resultSet: true,
		result: agent.Result{
			ExitCode:   -1,
			Hung:       true,
			HungReason: agent.HungReasonStall,
			Model:      "gpt-worker",
			Effort:     "high",
			Usage: reporter.Usage{
				TotalTokens: &totalTokens,
			},
			StartedAt:  "2026-06-28T00:00:00Z",
			EndedAt:    "2026-06-28T00:02:00Z",
			DurationMS: 120000,
		},
		log: "provider stopped producing output\nlast useful log line\n",
	}
	fakeGitHub := &workerFakeGitHub{prURL: "https://github.com/owner/repo/pull/101"}

	result, err := Dispatch(context.Background(), Options{
		RepoPath:    repo,
		IssueNumber: 101,
		IssueTitle:  "Implement dispatch",
		RunID:       "run-test",
		Attempt:     2,
		Branch:      "loop/issue-101-retry-2",
		Provider:    "codex",
		Model:       "gpt-conductor",
		Effort:      "xhigh",
		Stderr:      &warnings,
	}, Deps{
		Git: fakeGit,
		GitHub: func(string) GitHubClient {
			return fakeGitHub
		},
		AgentLookup: func(provider string) (agent.Runner, error) {
			return fakeAgent, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &workerFakeLock{}, nil
		},
		Now: fixedNow,
		PID: func() int {
			return 4321
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
		RemoveAll: os.RemoveAll,
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if !result.OK || result.Status != "needs-human" || result.Branch != "loop/issue-101-retry-2" || result.PR != "https://github.com/owner/repo/pull/101" {
		t.Fatalf("harvest result = %#v", result)
	}
	if result.Report == nil {
		t.Fatal("harvest result missing report")
	}
	if result.Report.Role != reporter.RoleConductor || result.Report.Permission != reporter.PermissionOrchestrate || result.Report.Verified {
		t.Fatalf("harvest report = %#v, want unverified conductor orchestrate", result.Report)
	}
	if err := result.Report.Validate(); err != nil {
		t.Fatalf("harvest report does not validate: %v", err)
	}
	if fakeGit.addAllCalls != 1 || fakeGit.commitCalls != 1 || fakeGit.pushCalls != 0 || fakeGit.forcePushCalls != 1 {
		t.Fatalf("harvest git calls add=%d commit=%d push=%d forcePush=%d, want add/commit/force only", fakeGit.addAllCalls, fakeGit.commitCalls, fakeGit.pushCalls, fakeGit.forcePushCalls)
	}
	if fakeGit.removeCalls != 0 || fakeGit.branchDeleteCalls != 0 {
		t.Fatalf("harvest cleanup removed preserved artifacts: worktreeRemove=%d branchDelete=%d", fakeGit.removeCalls, fakeGit.branchDeleteCalls)
	}
	assertPathExists(t, filepath.Dir(fakeGit.worktreePath))
	assertPathExists(t, fakeGit.worktreePath)
	if fakeGit.lastForcePushBranch != "loop/issue-101-retry-2" {
		t.Fatalf("force push branch = %q", fakeGit.lastForcePushBranch)
	}
	if strings.Contains(strings.ToLower(fakeGit.lastCommitMessage), "closes") || !strings.Contains(fakeGit.lastCommitMessage, "Harvest issue #101 from hung worker") {
		t.Fatalf("harvest commit message = %q", fakeGit.lastCommitMessage)
	}
	if fakeGitHub.createPRCalls != 1 || fakeGitHub.lastPRHead != "loop/issue-101-retry-2" || fakeGitHub.lastPRBase != "main" {
		t.Fatalf("CreatePR calls/head/base = %d/%q/%q", fakeGitHub.createPRCalls, fakeGitHub.lastPRHead, fakeGitHub.lastPRBase)
	}
	for _, want := range []string{
		"Refs #101",
		"needs-human",
		"harvested from a hung/killed worker",
		"possibly incomplete",
		"## Recovery brief",
		" M file.go",
		"?? new.go",
		"last useful log line",
		"## Hung worker partial report",
		"role=worker",
		"provider=codex",
		"model=gpt-worker",
		"total_tokens=77",
	} {
		if !strings.Contains(fakeGitHub.lastPRBody, want) {
			t.Fatalf("harvest PR body missing %q:\n%s", want, fakeGitHub.lastPRBody)
		}
	}
	for _, forbidden := range []string{"Part of #101", "Closes #101", "closes #101", `"role":"worker"`} {
		if strings.Contains(fakeGitHub.lastPRBody, forbidden) {
			t.Fatalf("harvest PR body contains forbidden %q:\n%s", forbidden, fakeGitHub.lastPRBody)
		}
	}

	attempts, err := state.LoadAttempts(repo, "run-test")
	if err != nil {
		t.Fatalf("LoadAttempts returned error: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("LoadAttempts returned %d attempts, want 1", len(attempts))
	}
	got := attempts[0]
	if got.Status != "needs-human" || got.Branch != "loop/issue-101-retry-2" {
		t.Fatalf("harvest attempt = %#v, want needs-human retry branch", got)
	}
	if got.Report == nil || got.Report.Role != reporter.RoleConductor {
		t.Fatalf("harvest attempt report = %#v, want conductor", got.Report)
	}
	if got.ArtifactDecision == nil || got.ArtifactDecision.State != artifactDecisionPreserveSelected ||
		got.ArtifactDecision.OwnerID != "worker:run-test:job-101-4321:2" ||
		got.ArtifactDecision.Generation != 1 ||
		got.ArtifactDecision.WorktreePath != fakeGit.worktreePath ||
		got.ArtifactDecision.ScratchPath != filepath.Dir(fakeGit.worktreePath) ||
		got.ArtifactDecision.ManifestPath == "" {
		t.Fatalf("harvest artifact decision = %#v, want preserve-selected exact paths", got.ArtifactDecision)
	}
	manifestPath := state.PreservationManifestPath(repo, "run-test", "job-101-4321")
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("ReadFile preservation manifest: %v", err)
	}
	var manifest preservationManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatalf("preservation manifest is invalid JSON: %v\n%s", err, string(manifestData))
	}
	if manifest.WorktreePath != fakeGit.worktreePath || manifest.ScratchPath != filepath.Dir(fakeGit.worktreePath) ||
		manifest.AttemptPath != filepath.Join(repo, ".loopcoder", "runs", "run-test", "workers", "job-101-4321.attempt.json") ||
		len(manifest.PartialArtifactPaths) == 0 ||
		!strings.Contains(manifest.DisposalGuidance, "run_id and job_id") {
		t.Fatalf("preservation manifest = %#v", manifest)
	}
	for _, want := range []string{"preserved worktree:", "preserved scratch:", "preserved manifest:", "preserved partial artifacts:", "disposal:"} {
		if !strings.Contains(warnings.String(), want) {
			t.Fatalf("warnings missing %q:\n%s", want, warnings.String())
		}
	}
}

func TestDispatchHungReportOnlyPreservesPartialWorkWithoutHarvestPR(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".delivery.yml"), []byte(`
domain:
  partial_work:
    mode: report-only
`), 0o644); err != nil {
		t.Fatalf("write delivery config: %v", err)
	}
	scratchRoot := t.TempDir()
	var warnings strings.Builder
	fakeGit := &workerFakeGit{status: " M file.go\n?? new.go\n"}
	fakeAgent := &workerFakeAgent{
		resultSet: true,
		result: agent.Result{
			ExitCode:   -1,
			Hung:       true,
			HungReason: agent.HungReasonStall,
		},
		log: "provider stopped producing output\nlast useful log line\n",
	}
	fakeGitHub := &workerFakeGitHub{prURL: "https://github.com/owner/repo/pull/101"}

	result, err := Dispatch(context.Background(), Options{
		RepoPath:    repo,
		IssueNumber: 101,
		IssueTitle:  "Implement dispatch",
		RunID:       "run-test",
		Attempt:     2,
		Provider:    "codex",
		Stderr:      &warnings,
	}, Deps{
		Git: fakeGit,
		GitHub: func(string) GitHubClient {
			return fakeGitHub
		},
		AgentLookup: func(provider string) (agent.Runner, error) {
			return fakeAgent, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &workerFakeLock{}, nil
		},
		Now: fixedNow,
		PID: func() int {
			return 4321
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
		RemoveAll: os.RemoveAll,
	})
	if err == nil {
		t.Fatal("Dispatch returned nil error, want hung failure")
	}
	if result.OK || result.Status != "hung" || result.PR != "" || result.Report != nil {
		t.Fatalf("report-only hung result = %#v, want hung without PR or report", result)
	}
	if fakeGit.addAllCalls != 0 || fakeGit.commitCalls != 0 || fakeGit.pushCalls != 0 || fakeGit.forcePushCalls != 0 {
		t.Fatalf("report-only made harvest git calls: add=%d commit=%d push=%d force=%d", fakeGit.addAllCalls, fakeGit.commitCalls, fakeGit.pushCalls, fakeGit.forcePushCalls)
	}
	if fakeGitHub.createPRCalls != 0 {
		t.Fatalf("CreatePR calls = %d, want 0", fakeGitHub.createPRCalls)
	}
	for _, want := range []string{"report-only", "preserved partial work", "no harvest PR opened"} {
		if !strings.Contains(warnings.String(), want) {
			t.Fatalf("warnings missing %q:\n%s", want, warnings.String())
		}
	}

	attempts, err := state.LoadAttempts(repo, "run-test")
	if err != nil {
		t.Fatalf("LoadAttempts returned error: %v", err)
	}
	if len(attempts) != 1 || attempts[0].Status != "hung" || attempts[0].Report != nil {
		t.Fatalf("report-only attempt = %#v, want hung with no report", attempts)
	}
	brief, err := os.ReadFile(state.RecoveryBriefPath(repo, "run-test", "job-101-4321"))
	if err != nil {
		t.Fatalf("ReadFile recovery brief: %v", err)
	}
	for _, want := range []string{" M file.go", "?? new.go", "last useful log line"} {
		if !strings.Contains(string(brief), want) {
			t.Fatalf("recovery brief missing %q:\n%s", want, string(brief))
		}
	}
	events, err := os.ReadFile(state.EventsPath(repo, "run-test"))
	if err != nil {
		t.Fatalf("ReadFile events: %v", err)
	}
	for _, want := range []string{`"event":"worker_partial_work_reported"`, `"mode":"report-only"`} {
		if !strings.Contains(string(events), want) {
			t.Fatalf("events missing %q:\n%s", want, string(events))
		}
	}
}

func TestDispatchHungHarvestWarnsAndContinuesWhenDedupCheckFails(t *testing.T) {
	repo := t.TempDir()
	scratchRoot := t.TempDir()
	var warnings strings.Builder
	fakeGit := &workerFakeGit{status: " M file.go\n"}
	fakeAgent := &workerFakeAgent{
		resultSet: true,
		result: agent.Result{
			ExitCode:   -1,
			Hung:       true,
			HungReason: agent.HungReasonStall,
		},
		log: "hung\n",
	}
	fakeGitHub := &workerFakeGitHub{
		prURL:       "https://github.com/owner/repo/pull/101",
		listHeadErr: errors.New("head PR lookup unavailable"),
		listOpenErr: errors.New("github API temporarily unavailable"),
	}

	result, err := Dispatch(context.Background(), Options{
		RepoPath:    repo,
		IssueNumber: 101,
		IssueTitle:  "Implement dispatch",
		RunID:       "run-test",
		Attempt:     2,
		Provider:    "codex",
		Stderr:      &warnings,
	}, Deps{
		Git: fakeGit,
		GitHub: func(string) GitHubClient {
			return fakeGitHub
		},
		AgentLookup: func(provider string) (agent.Runner, error) {
			return fakeAgent, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &workerFakeLock{}, nil
		},
		Now: fixedNow,
		PID: func() int {
			return 4321
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
		RemoveAll: os.RemoveAll,
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if !result.OK || result.Status != "needs-human" || result.PR != "https://github.com/owner/repo/pull/101" {
		t.Fatalf("harvest result = %#v", result)
	}
	if fakeGit.addAllCalls != 1 || fakeGit.commitCalls != 1 || fakeGit.forcePushCalls != 1 || fakeGitHub.createPRCalls != 1 {
		t.Fatalf("harvest did not continue after dedup warning: add=%d commit=%d force=%d createPR=%d", fakeGit.addAllCalls, fakeGit.commitCalls, fakeGit.forcePushCalls, fakeGitHub.createPRCalls)
	}
	for _, want := range []string{"warning", "harvest idempotency check", "duplicate needs-human PR may result"} {
		if !strings.Contains(warnings.String(), want) {
			t.Fatalf("warnings missing %q:\n%s", want, warnings.String())
		}
	}
}

func TestDispatchHungHarvestUsesForceWithLeaseForPreExistingRemoteBranch(t *testing.T) {
	repo := t.TempDir()
	scratchRoot := t.TempDir()
	fakeGit := &workerFakeGit{status: " M file.go\n", remoteBranchExists: true}
	fakeAgent := &workerFakeAgent{
		resultSet: true,
		result: agent.Result{
			ExitCode:   -1,
			Hung:       true,
			HungReason: agent.HungReasonStall,
		},
		log: "hung\n",
	}

	_, err := Dispatch(context.Background(), Options{
		RepoPath:    repo,
		IssueNumber: 101,
		IssueTitle:  "Implement dispatch",
		RunID:       "run-test",
		Attempt:     2,
		Provider:    "codex",
		Stderr:      io.Discard,
	}, Deps{
		Git: fakeGit,
		GitHub: func(string) GitHubClient {
			return &workerFakeGitHub{prURL: "https://github.com/owner/repo/pull/101"}
		},
		AgentLookup: func(provider string) (agent.Runner, error) {
			return fakeAgent, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &workerFakeLock{}, nil
		},
		Now: fixedNow,
		PID: func() int {
			return 4321
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
		RemoveAll: os.RemoveAll,
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if fakeGit.forcePushCalls != 1 || !fakeGit.remoteBranchExists {
		t.Fatalf("force-with-lease path not exercised: calls=%d remoteExists=%v", fakeGit.forcePushCalls, fakeGit.remoteBranchExists)
	}
}

func TestDispatchHungHarvestNoOpsWhenHarvestPRExistsForDifferentAttempt(t *testing.T) {
	repo := t.TempDir()
	scratchRoot := t.TempDir()
	fakeGit := &workerFakeGit{status: " M file.go\n"}
	fakeAgent := &workerFakeAgent{
		resultSet: true,
		result: agent.Result{
			ExitCode:   -1,
			Hung:       true,
			HungReason: agent.HungReasonStall,
		},
		log: "hung\n",
	}
	fakeGitHub := &workerFakeGitHub{
		openPRs: []gh.PullRequest{{
			Number:      77,
			Title:       "[needs-human] Harvested issue #101 from hung/killed worker",
			URL:         "https://github.com/owner/repo/pull/77",
			HeadRefName: "loop/issue-101-retry-1",
		}},
	}

	result, err := Dispatch(context.Background(), Options{
		RepoPath:    repo,
		IssueNumber: 101,
		IssueTitle:  "Implement dispatch",
		RunID:       "run-test",
		Attempt:     3,
		Provider:    "codex",
		Stderr:      io.Discard,
	}, Deps{
		Git: fakeGit,
		GitHub: func(string) GitHubClient {
			return fakeGitHub
		},
		AgentLookup: func(provider string) (agent.Runner, error) {
			return fakeAgent, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &workerFakeLock{}, nil
		},
		Now: fixedNow,
		PID: func() int {
			return 4321
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
		RemoveAll: os.RemoveAll,
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if result.Status != "needs-human" || result.PR != "https://github.com/owner/repo/pull/77" || result.Branch != "loop/issue-101-retry-1" {
		t.Fatalf("idempotent harvest result = %#v", result)
	}
	if fakeGit.addAllCalls != 0 || fakeGit.commitCalls != 0 || fakeGit.forcePushCalls != 0 || fakeGitHub.createPRCalls != 0 {
		t.Fatalf("idempotent harvest made calls add=%d commit=%d force=%d createPR=%d", fakeGit.addAllCalls, fakeGit.commitCalls, fakeGit.forcePushCalls, fakeGitHub.createPRCalls)
	}

	attempts, err := state.LoadAttempts(repo, "run-test")
	if err != nil {
		t.Fatalf("LoadAttempts returned error: %v", err)
	}
	if len(attempts) != 1 || attempts[0].Status != "needs-human" || attempts[0].Branch != "loop/issue-101-retry-1" {
		t.Fatalf("idempotent harvest attempt = %#v", attempts)
	}
}

func TestDispatchHardFailsInvalidReportBeforeDelivery(t *testing.T) {
	repo := t.TempDir()
	scratchRoot := t.TempDir()
	var warnings strings.Builder
	fakeGit := &workerFakeGit{status: " M internal/worker/worker.go\n"}
	fakeAgent := &workerFakeAgent{
		resultSet: true,
		result: agent.Result{
			ExitCode:   0,
			Summary:    "Implemented dispatch.",
			Effort:     "xhigh",
			StartedAt:  "2026-06-28T00:00:00Z",
			EndedAt:    "2026-06-28T00:00:42Z",
			DurationMS: 42000,
		},
		log: "codex ok without metadata\n",
	}
	fakeGitHub := &workerFakeGitHub{prURL: "https://github.com/owner/repo/pull/101"}

	_, err := Dispatch(context.Background(), Options{
		RepoPath:    repo,
		IssueNumber: 101,
		IssueTitle:  "Implement dispatch",
		IssueBody:   "Body",
		RunID:       "run-test",
		Provider:    "codex",
		Stderr:      &warnings,
	}, Deps{
		Git: fakeGit,
		GitHub: func(string) GitHubClient {
			return fakeGitHub
		},
		AgentLookup: func(provider string) (agent.Runner, error) {
			return fakeAgent, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &workerFakeLock{}, nil
		},
		Now: fixedNow,
		PID: func() int {
			return 4321
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
		RemoveAll: os.RemoveAll,
	})
	if err == nil {
		t.Fatal("Dispatch returned nil error, want report failure")
	}
	for _, want := range []string{"validate worker report", "model is required", "usage requires total_tokens or both input_tokens and output_tokens"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error missing %q: %v", want, err)
		}
	}
	if fakeGit.addAllCalls != 0 || fakeGit.commitCalls != 0 || fakeGit.pushCalls != 0 {
		t.Fatalf("delivery git calls after report failure: add=%d commit=%d push=%d", fakeGit.addAllCalls, fakeGit.commitCalls, fakeGit.pushCalls)
	}
	if fakeGitHub.createPRCalls != 0 {
		t.Fatalf("CreatePR calls = %d, want 0", fakeGitHub.createPRCalls)
	}
	if !strings.Contains(warnings.String(), "preserved failed attempt artifacts") {
		t.Fatalf("warnings missing artifact preservation note: %q", warnings.String())
	}

	attempts, err := state.LoadAttempts(repo, "run-test")
	if err != nil {
		t.Fatalf("LoadAttempts returned error: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("LoadAttempts returned %d attempts, want 1", len(attempts))
	}
	got := attempts[0]
	if got.Phase != "codex_exited" || got.Status != "failed" {
		t.Fatalf("failed attempt = %#v", got)
	}
	if got.ExitCode == nil || *got.ExitCode != 0 {
		t.Fatalf("failed attempt exit code = %#v, want 0", got.ExitCode)
	}
}

func TestPrepareDispatchDefaultsAndResolvesDependencies(t *testing.T) {
	repo := t.TempDir()
	fakeAgent := &workerFakeAgent{}
	fakeGitHub := &workerFakeGitHub{}
	var gotProvider string
	var gotRepoPath string

	dispatch, err := prepareDispatch(context.Background(), Options{
		RepoPath:    repo,
		IssueNumber: 509,
		IssueTitle:  "Split worker dispatch",
	}, Deps{
		AgentLookup: func(provider string) (agent.Runner, error) {
			gotProvider = provider
			return fakeAgent, nil
		},
		GitHub: func(repoPath string) GitHubClient {
			gotRepoPath = repoPath
			return fakeGitHub
		},
		Now: fixedNow,
	})
	if err != nil {
		t.Fatalf("prepareDispatch returned error: %v", err)
	}
	if gotProvider != "codex" {
		t.Fatalf("AgentLookup provider = %q, want codex", gotProvider)
	}
	if dispatch.opts.Provider != "codex" || dispatch.opts.BaseBranch != "main" || dispatch.opts.Branch != "loop/issue-509" || dispatch.opts.Attempt != 1 {
		t.Fatalf("normalized options = %#v", dispatch.opts)
	}
	if dispatch.opts.RunID != state.RunIDForIssue(509, fixedNow()) {
		t.Fatalf("RunID = %q, want generated run ID", dispatch.opts.RunID)
	}
	if !filepath.IsAbs(dispatch.repoPath) || gotRepoPath != dispatch.repoPath {
		t.Fatalf("repo path = %q, GitHub saw %q", dispatch.repoPath, gotRepoPath)
	}
	if dispatch.agentRun != fakeAgent || dispatch.github != fakeGitHub {
		t.Fatalf("resolved deps agent=%#v github=%#v", dispatch.agentRun, dispatch.github)
	}
	if dispatch.warnings == nil {
		t.Fatal("warnings writer was nil")
	}
}

func TestDispatchHelperSeamsSuccessPath(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	scratchRoot := t.TempDir()
	var removedScratch string
	fakeGit := &workerFakeGit{status: " M file.go\n"}
	fakeGitHub := &workerFakeGitHub{prURL: "https://github.com/owner/repo/pull/509"}
	fakeAgent := &workerFakeAgent{
		resultSet: true,
		result:    validWorkerAgentResult("Split dispatch into helpers.", 0),
		log:       "codex ok\n",
	}

	dispatch, err := prepareDispatch(ctx, Options{
		RepoPath:    repo,
		IssueNumber: 509,
		IssueTitle:  "Split worker dispatch",
		IssueBody:   "Body",
		RunID:       "run-seam",
		Provider:    "codex",
		Model:       "gpt-worker",
		Effort:      "high",
	}, Deps{
		Git: fakeGit,
		GitHub: func(string) GitHubClient {
			return fakeGitHub
		},
		AgentLookup: func(string) (agent.Runner, error) {
			return fakeAgent, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &workerFakeLock{}, nil
		},
		Now: fixedNow,
		PID: func() int {
			return 2468
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
		RemoveAll: func(path string) error {
			removedScratch = path
			return os.RemoveAll(path)
		},
		RepoSkills: func(repoPath string, domainSkills config.DomainSkills) (string, error) {
			if repoPath == "" {
				t.Fatal("RepoSkills repoPath was empty")
			}
			return "## Repo-local skills\nSummary: seam coverage\n", nil
		},
	})
	if err != nil {
		t.Fatalf("prepareDispatch returned error: %v", err)
	}

	if err := prepareWorktree(ctx, dispatch); err != nil {
		t.Fatalf("prepareWorktree returned error: %v", err)
	}
	if fakeGit.fetchCalls != 1 || fakeGit.fetchBase != "main" {
		t.Fatalf("fetch calls/base = %d/%q, want 1/main", fakeGit.fetchCalls, fakeGit.fetchBase)
	}
	if fakeGit.worktreeAddCalls != 1 || fakeGit.worktreeBranch != "loop/issue-509" || fakeGit.worktreeBase != "main" {
		t.Fatalf("worktree add = calls %d branch %q base %q", fakeGit.worktreeAddCalls, fakeGit.worktreeBranch, fakeGit.worktreeBase)
	}
	if dispatch.jobID != "job-509-2468" || dispatch.attemptPath != filepath.Join(repo, ".loopcoder", "runs", "run-seam", "workers", "job-509-2468.attempt.json") {
		t.Fatalf("attempt identity job=%q path=%q", dispatch.jobID, dispatch.attemptPath)
	}
	attempts, err := state.LoadAttempts(repo, "run-seam")
	if err != nil {
		t.Fatalf("LoadAttempts after prepareWorktree: %v", err)
	}
	if len(attempts) != 1 || attempts[0].Phase != "worktree_created" || attempts[0].Status != "running" {
		t.Fatalf("prepareWorktree attempt = %#v", attempts)
	}

	invocation, err := buildInvocation(ctx, dispatch)
	if err != nil {
		t.Fatalf("buildInvocation returned error: %v", err)
	}
	if invocation.WorktreePath != dispatch.worktreePath || invocation.LogPath != dispatch.logPath || invocation.RunID != "run-seam" || invocation.Role != "worker" {
		t.Fatalf("invocation identity = %#v", invocation)
	}
	if invocation.Model != "gpt-worker" || invocation.Effort != "high" || invocation.HardCap != WorkerHardCap || invocation.StallTimeout != WorkerStallTimeout {
		t.Fatalf("invocation supervision/model = %#v", invocation)
	}
	if !strings.Contains(invocation.Prompt, "## Repo-local skills") || !strings.Contains(invocation.Prompt, "Summary: seam coverage") {
		t.Fatalf("invocation prompt missing repo skills:\n%s", invocation.Prompt)
	}
	promptInfo, err := os.Stat(dispatch.promptPath)
	if err != nil {
		t.Fatalf("stat prompt: %v", err)
	}
	if runtime.GOOS != "windows" && promptInfo.Mode().Perm() != 0o600 {
		t.Fatalf("prompt mode = %#o, want 0600", promptInfo.Mode().Perm())
	}

	agentResult, agentErr := runAgent(ctx, dispatch, invocation)
	if agentErr != nil {
		t.Fatalf("runAgent returned error: %v", agentErr)
	}
	if agentResult.ExitCode != 0 || dispatch.activePhase != "codex_exited" || dispatch.tracker.exitCode == nil || *dispatch.tracker.exitCode != 0 {
		t.Fatalf("runAgent state result=%#v phase=%q exit=%#v", agentResult, dispatch.activePhase, dispatch.tracker.exitCode)
	}

	result, err := commitAndOpenPR(ctx, dispatch, agentResult)
	if err != nil {
		t.Fatalf("commitAndOpenPR returned error: %v", err)
	}
	if !result.OK || result.Status != "succeeded" || result.PR != "https://github.com/owner/repo/pull/509" || result.Summary != "Split dispatch into helpers." {
		t.Fatalf("commit result = %#v", result)
	}
	if fakeGit.addAllCalls != 1 || fakeGit.commitCalls != 1 || fakeGit.pushCalls != 1 || fakeGitHub.createPRCalls != 1 {
		t.Fatalf("delivery calls add=%d commit=%d push=%d pr=%d", fakeGit.addAllCalls, fakeGit.commitCalls, fakeGit.pushCalls, fakeGitHub.createPRCalls)
	}
	if fakeGit.lastCommitMessage != "Split worker dispatch (closes #509)" || fakeGitHub.lastPRBody != "Closes #509\n\nSplit dispatch into helpers." {
		t.Fatalf("commit/PR text = %q / %q", fakeGit.lastCommitMessage, fakeGitHub.lastPRBody)
	}

	cleanup(ctx, dispatch, nil)
	if fakeGit.removeCalls != 1 || fakeGit.branchDeleteCalls != 1 || removedScratch != dispatch.scratch {
		t.Fatalf("cleanup calls remove=%d branchDelete=%d scratch=%q want %q", fakeGit.removeCalls, fakeGit.branchDeleteCalls, removedScratch, dispatch.scratch)
	}
	attempts, err = state.LoadAttempts(repo, "run-seam")
	if err != nil {
		t.Fatalf("LoadAttempts after cleanup: %v", err)
	}
	if len(attempts) != 1 || attempts[0].Phase != "cleanup" || attempts[0].Status != "succeeded" {
		t.Fatalf("cleanup attempt = %#v", attempts)
	}
}

func TestDispatchFinalizationRetryAdoptsExistingPRWithoutProviderRerun(t *testing.T) {
	repo := t.TempDir()
	scratchRoot := t.TempDir()
	var warnings strings.Builder
	fakeGit := &workerFakeGit{status: " M file.go\n"}
	fakeGitHub := &workerFakeGitHub{createErr: errors.New("GitHub API unavailable")}
	fakeAgent := &workerFakeAgent{
		resultSet: true,
		result:    validWorkerAgentResult("Implemented finalization.", 0),
		log:       "provider completed\n",
	}
	deps := Deps{
		Git: fakeGit,
		GitHub: func(string) GitHubClient {
			return fakeGitHub
		},
		AgentLookup: func(string) (agent.Runner, error) {
			return fakeAgent, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &workerFakeLock{}, nil
		},
		Now: fixedNow,
		PID: func() int {
			return 2468
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
		RemoveAll: os.RemoveAll,
	}
	opts := Options{
		RepoPath:    repo,
		IssueNumber: 965,
		IssueTitle:  "Classify finalization",
		RunID:       "run-finalization",
		Branch:      "loop/issue-965",
		Provider:    "codex",
		Stderr:      &warnings,
	}

	first, err := Dispatch(context.Background(), opts, deps)
	if err == nil {
		t.Fatal("first Dispatch returned nil error, want delivery failure")
	}
	if first.Report == nil || first.Outcome != string(OutcomeDeliveryFailed) || first.ProviderOutcome != string(OutcomeProviderCompleted) {
		t.Fatalf("first result = %#v, want preserved report and delivery_failed/provider_completed", first)
	}
	if fakeAgent.runCalls != 1 || fakeGitHub.createPRCalls != 1 {
		t.Fatalf("first calls provider=%d createPR=%d, want 1/1", fakeAgent.runCalls, fakeGitHub.createPRCalls)
	}

	fakeGitHub.createErr = nil
	fakeGitHub.prs = []gh.PullRequestReference{{Number: 965, URL: "https://github.com/owner/repo/pull/965"}}
	second, err := Dispatch(context.Background(), opts, deps)
	if err != nil {
		t.Fatalf("second Dispatch returned error: %v", err)
	}
	if !second.OK || second.Outcome != string(OutcomePRAdopted) || second.PR != "https://github.com/owner/repo/pull/965" {
		t.Fatalf("second result = %#v, want adopted PR success", second)
	}
	if fakeAgent.runCalls != 1 {
		t.Fatalf("provider executions = %d, want exactly one across finalization retry", fakeAgent.runCalls)
	}
	if fakeGitHub.createPRCalls != 1 {
		t.Fatalf("CreatePR calls = %d, want no second create", fakeGitHub.createPRCalls)
	}
}

func TestDispatchPushConflictNeedsHumanAndDoesNotOverwriteRemote(t *testing.T) {
	repo := t.TempDir()
	scratchRoot := t.TempDir()
	fakeGit := &workerFakeGit{status: " M file.go\n", remoteBranchExists: true}
	fakeGitHub := &workerFakeGitHub{}
	fakeAgent := &workerFakeAgent{
		resultSet: true,
		result:    validWorkerAgentResult("Implemented.", 0),
		log:       "provider completed\n",
	}

	result, err := Dispatch(context.Background(), Options{
		RepoPath:    repo,
		IssueNumber: 966,
		IssueTitle:  "Protect remote branch",
		RunID:       "run-push-conflict",
		Provider:    "codex",
	}, Deps{
		Git: fakeGit,
		GitHub: func(string) GitHubClient {
			return fakeGitHub
		},
		AgentLookup: func(string) (agent.Runner, error) {
			return fakeAgent, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &workerFakeLock{}, nil
		},
		Now: fixedNow,
		PID: func() int {
			return 2468
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
		RemoveAll: os.RemoveAll,
	})
	if err == nil {
		t.Fatal("Dispatch returned nil error, want push conflict")
	}
	if result.OK || result.Status != state.StatusNeedsHuman || result.Outcome != string(OutcomePushConflict) {
		t.Fatalf("result = %#v, want needs-human push_conflict", result)
	}
	if fakeGit.forcePushCalls != 0 || fakeGitHub.createPRCalls != 0 {
		t.Fatalf("unsafe finalization calls forcePush=%d createPR=%d, want 0/0", fakeGit.forcePushCalls, fakeGitHub.createPRCalls)
	}
}

func TestDispatchPushAlreadyAppliedAdoptsBranchPR(t *testing.T) {
	repo := t.TempDir()
	scratchRoot := t.TempDir()
	fakeGit := &workerFakeGit{status: " M file.go\n", remoteBranchExists: true}
	fakeGitHub := &workerFakeGitHub{
		prsByCall: map[int][]gh.PullRequestReference{
			3: {{Number: 968, URL: "https://github.com/owner/repo/pull/968"}},
		},
	}
	fakeAgent := &workerFakeAgent{
		resultSet: true,
		result:    validWorkerAgentResult("Implemented.", 0),
		log:       "provider completed\n",
	}

	result, err := Dispatch(context.Background(), Options{
		RepoPath:    repo,
		IssueNumber: 968,
		IssueTitle:  "Adopt applied push",
		RunID:       "run-push-applied",
		Provider:    "codex",
	}, Deps{
		Git: fakeGit,
		GitHub: func(string) GitHubClient {
			return fakeGitHub
		},
		AgentLookup: func(string) (agent.Runner, error) {
			return fakeAgent, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &workerFakeLock{}, nil
		},
		Now: fixedNow,
		PID: func() int {
			return 2468
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
		RemoveAll: os.RemoveAll,
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if !result.OK || result.Outcome != string(OutcomePushAlreadyApplied) || result.PR != "https://github.com/owner/repo/pull/968" {
		t.Fatalf("result = %#v, want push_already_applied success", result)
	}
	if fakeGitHub.createPRCalls != 0 || fakeGit.forcePushCalls != 0 {
		t.Fatalf("unexpected create/overwrite calls createPR=%d forcePush=%d", fakeGitHub.createPRCalls, fakeGit.forcePushCalls)
	}
}

func TestDispatchNoChangesClassifiesExpectedAndInvalid(t *testing.T) {
	for _, tc := range []struct {
		name        string
		summary     string
		wantOK      bool
		wantOutcome Outcome
		wantErr     bool
	}{
		{name: "expected", summary: "Already implemented; no changes needed.", wantOK: true, wantOutcome: OutcomeNoChangesExpected},
		{name: "invalid", summary: "I inspected the code.", wantOK: false, wantOutcome: OutcomeNoChangesInvalid, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			scratchRoot := t.TempDir()
			fakeGit := &workerFakeGit{status: ""}
			fakeGitHub := &workerFakeGitHub{}
			fakeAgent := &workerFakeAgent{
				resultSet: true,
				result:    validWorkerAgentResult(tc.summary, 0),
				log:       "provider completed\n",
			}
			result, err := Dispatch(context.Background(), Options{
				RepoPath:    repo,
				IssueNumber: 967,
				IssueTitle:  "Classify no changes",
				RunID:       "run-no-changes-" + tc.name,
				Provider:    "codex",
			}, Deps{
				Git: fakeGit,
				GitHub: func(string) GitHubClient {
					return fakeGitHub
				},
				AgentLookup: func(string) (agent.Runner, error) {
					return fakeAgent, nil
				},
				AcquireLock: func(string, time.Duration) (Lock, error) {
					return &workerFakeLock{}, nil
				},
				Now: fixedNow,
				PID: func() int {
					return 2468
				},
				MkdirTemp: func(dir, pattern string) (string, error) {
					return os.MkdirTemp(scratchRoot, pattern)
				},
				RemoveAll: os.RemoveAll,
			})
			if (err != nil) != tc.wantErr {
				t.Fatalf("Dispatch err = %v, wantErr %t", err, tc.wantErr)
			}
			if result.OK != tc.wantOK || result.Outcome != string(tc.wantOutcome) || result.ProviderOutcome != string(OutcomeProviderCompleted) {
				t.Fatalf("result = %#v, want ok=%t outcome=%s provider_completed", result, tc.wantOK, tc.wantOutcome)
			}
			if fakeGit.addAllCalls != 0 || fakeGit.commitCalls != 0 || fakeGit.pushCalls != 0 || fakeGitHub.createPRCalls != 0 {
				t.Fatalf("no-change finalization made calls add=%d commit=%d push=%d pr=%d", fakeGit.addAllCalls, fakeGit.commitCalls, fakeGit.pushCalls, fakeGitHub.createPRCalls)
			}
		})
	}
}

func TestCleanupRefusesUnownedScratchDeletion(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	scratchRoot := t.TempDir()
	var warnings strings.Builder
	removedScratch := ""
	fakeGit := &workerFakeGit{status: " M file.go\n"}
	fakeGitHub := &workerFakeGitHub{prURL: "https://github.com/owner/repo/pull/510"}
	fakeAgent := &workerFakeAgent{
		resultSet: true,
		result:    validWorkerAgentResult("Implemented.", 0),
		log:       "codex ok\n",
	}

	dispatch, err := prepareDispatch(ctx, Options{
		RepoPath:    repo,
		IssueNumber: 510,
		IssueTitle:  "Guard scratch cleanup",
		RunID:       "run-ownership",
		Provider:    "codex",
		Stderr:      &warnings,
	}, Deps{
		Git: fakeGit,
		GitHub: func(string) GitHubClient {
			return fakeGitHub
		},
		AgentLookup: func(string) (agent.Runner, error) {
			return fakeAgent, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &workerFakeLock{}, nil
		},
		Now: func() time.Time {
			return fixedNow()
		},
		PID: func() int {
			return 2468
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
		RemoveAll: func(path string) error {
			removedScratch = path
			return os.RemoveAll(path)
		},
	})
	if err != nil {
		t.Fatalf("prepareDispatch returned error: %v", err)
	}
	if err := prepareWorktree(ctx, dispatch); err != nil {
		t.Fatalf("prepareWorktree returned error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dispatch.scratch, scratchOwnerFile), []byte(`{"version":1,"run_id":"other","job_id":"other","issue":510,"attempt":1}`), 0o600); err != nil {
		t.Fatalf("tamper scratch owner marker: %v", err)
	}
	invocation, err := buildInvocation(ctx, dispatch)
	if err != nil {
		t.Fatalf("buildInvocation returned error: %v", err)
	}
	agentResult, agentErr := runAgent(ctx, dispatch, invocation)
	if agentErr != nil {
		t.Fatalf("runAgent returned error: %v", agentErr)
	}
	if _, err := commitAndOpenPR(ctx, dispatch, agentResult); err != nil {
		t.Fatalf("commitAndOpenPR returned error: %v", err)
	}

	cleanup(ctx, dispatch, nil)
	if removedScratch != "" {
		t.Fatalf("RemoveAll called for unowned scratch %q", removedScratch)
	}
	assertPathExists(t, dispatch.scratch)
	if !strings.Contains(warnings.String(), "scratch owner marker does not match attempt") {
		t.Fatalf("warnings missing ownership mismatch:\n%s", warnings.String())
	}
}

func TestCommitAndOpenPRRejectsStaleWorkerOwnership(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	scratchRoot := t.TempDir()
	fakeGit := &workerFakeGit{status: " M file.go\n"}
	fakeGitHub := &workerFakeGitHub{prURL: "https://github.com/owner/repo/pull/511"}
	dispatch, err := prepareDispatch(ctx, Options{
		RepoPath:    repo,
		IssueNumber: 511,
		IssueTitle:  "Fence stale worker",
		RunID:       "run-stale-worker",
		Provider:    "codex",
	}, Deps{
		Git: fakeGit,
		GitHub: func(string) GitHubClient {
			return fakeGitHub
		},
		AgentLookup: func(string) (agent.Runner, error) {
			return &workerFakeAgent{}, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &workerFakeLock{}, nil
		},
		Now: fixedNow,
		PID: func() int {
			return 2468
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
	})
	if err != nil {
		t.Fatalf("prepareDispatch returned error: %v", err)
	}
	if err := prepareWorktree(ctx, dispatch); err != nil {
		t.Fatalf("prepareWorktree returned error: %v", err)
	}
	store, lease := attachReleasedWorkerOwnership(t, ctx, dispatch)
	defer store.Close()

	_, err = commitAndOpenPR(ctx, dispatch, validWorkerAgentResult("stale output", 0))
	if !errors.Is(err, storage.ErrOwnershipStale) {
		t.Fatalf("commitAndOpenPR error = %v, want ErrOwnershipStale", err)
	}
	if fakeGit.addAllCalls != 0 || fakeGit.commitCalls != 0 || fakeGit.pushCalls != 0 || fakeGitHub.createPRCalls != 0 {
		t.Fatalf("stale owner reached mutation calls add=%d commit=%d push=%d pr=%d", fakeGit.addAllCalls, fakeGit.commitCalls, fakeGit.pushCalls, fakeGitHub.createPRCalls)
	}
	if err := storage.ValidateAgentOwnershipFence(ctx, store, lease); !errors.Is(err, storage.ErrOwnershipStale) {
		t.Fatalf("stale lease validation = %v, want ErrOwnershipStale", err)
	}
}

func TestCleanupRefusesStaleWorkerOwnershipDeletion(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	scratchRoot := t.TempDir()
	var warnings strings.Builder
	fakeGit := &workerFakeGit{status: " M file.go\n"}
	dispatch, err := prepareDispatch(ctx, Options{
		RepoPath:    repo,
		IssueNumber: 512,
		IssueTitle:  "Fence cleanup",
		RunID:       "run-stale-cleanup",
		Provider:    "codex",
		Stderr:      &warnings,
	}, Deps{
		Git: fakeGit,
		GitHub: func(string) GitHubClient {
			return &workerFakeGitHub{}
		},
		AgentLookup: func(string) (agent.Runner, error) {
			return &workerFakeAgent{}, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &workerFakeLock{}, nil
		},
		Now: fixedNow,
		PID: func() int {
			return 2468
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
	})
	if err != nil {
		t.Fatalf("prepareDispatch returned error: %v", err)
	}
	if err := prepareWorktree(ctx, dispatch); err != nil {
		t.Fatalf("prepareWorktree returned error: %v", err)
	}
	_, _ = attachReleasedWorkerOwnership(t, ctx, dispatch)
	dispatch.dispatchSucceeded = true
	cleanup(ctx, dispatch, nil)
	if fakeGit.removeCalls != 0 || fakeGit.branchDeleteCalls != 0 {
		t.Fatalf("stale cleanup removed worktree/branch remove=%d branchDelete=%d", fakeGit.removeCalls, fakeGit.branchDeleteCalls)
	}
	assertPathExists(t, dispatch.scratch)
	if !strings.Contains(warnings.String(), "refused cleanup without active ownership fence") {
		t.Fatalf("warnings missing stale cleanup refusal:\n%s", warnings.String())
	}
}

func TestBuildInvocationUsesPerRunTimeoutOverride(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".delivery.yml"), []byte("version: 1\n"), 0o644); err != nil {
		t.Fatalf("write delivery config: %v", err)
	}
	dispatch, err := prepareDispatch(ctx, Options{
		RepoPath:       repo,
		IssueNumber:    601,
		IssueTitle:     "Timeout override",
		RunID:          "run-timeout-override",
		Provider:       "codex",
		Timeout:        2 * time.Minute,
		ConfigFromBase: false,
	}, Deps{
		Git: &workerFakeGit{},
		GitHub: func(string) GitHubClient {
			return &workerFakeGitHub{}
		},
		AgentLookup: func(string) (agent.Runner, error) {
			return &workerFakeAgent{}, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &workerFakeLock{}, nil
		},
		Now:       fixedNow,
		PID:       func() int { return 2468 },
		MkdirTemp: os.MkdirTemp,
		RemoveAll: os.RemoveAll,
		RepoSkills: func(string, config.DomainSkills) (string, error) {
			return "", nil
		},
	})
	if err != nil {
		t.Fatalf("prepareDispatch returned error: %v", err)
	}
	if err := prepareWorktree(ctx, dispatch); err != nil {
		t.Fatalf("prepareWorktree returned error: %v", err)
	}
	invocation, err := buildInvocation(ctx, dispatch)
	if err != nil {
		t.Fatalf("buildInvocation returned error: %v", err)
	}
	if invocation.HardCap != 2*time.Minute {
		t.Fatalf("HardCap = %s, want timeout override 2m0s", invocation.HardCap)
	}
	if invocation.StallTimeout != WorkerStallTimeout {
		t.Fatalf("StallTimeout = %s, want configured default %s", invocation.StallTimeout, WorkerStallTimeout)
	}
}

func TestHandleHungReportOnlyAndWriteRecoveryHelpers(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	scratchRoot := t.TempDir()
	var warnings strings.Builder
	fakeGit := &workerFakeGit{status: " M partial.go\n"}
	fakeGitHub := &workerFakeGitHub{}

	dispatch, err := prepareDispatch(ctx, Options{
		RepoPath:    repo,
		IssueNumber: 509,
		IssueTitle:  "Split worker dispatch",
		RunID:       "run-hung",
		Provider:    "codex",
		Stderr:      &warnings,
	}, Deps{
		Git: fakeGit,
		GitHub: func(string) GitHubClient {
			return fakeGitHub
		},
		AgentLookup: func(string) (agent.Runner, error) {
			return &workerFakeAgent{}, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &workerFakeLock{}, nil
		},
		Now: fixedNow,
		PID: func() int {
			return 1357
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
		RemoveAll: os.RemoveAll,
	})
	if err != nil {
		t.Fatalf("prepareDispatch returned error: %v", err)
	}
	if err := prepareWorktree(ctx, dispatch); err != nil {
		t.Fatalf("prepareWorktree returned error: %v", err)
	}
	dispatch.domainPolicy = domainWorkerPolicy{PartialWorkMode: partialWorkModeReportOnly}
	if err := os.WriteFile(dispatch.logPath, []byte("provider stalled\nlast useful line\n"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	agentResult := agent.Result{
		ExitCode:   -1,
		Hung:       true,
		HungReason: agent.HungReasonStall,
	}

	result, err := handleHungOrPartialWork(ctx, dispatch, agentResult)
	if err == nil {
		t.Fatal("handleHungOrPartialWork returned nil error, want hung error")
	}
	if result.OK || result.Status != "hung" || result.Branch != "loop/issue-509" || result.Report != nil {
		t.Fatalf("hung helper result = %#v", result)
	}
	if fakeGit.addAllCalls != 0 || fakeGit.commitCalls != 0 || fakeGit.forcePushCalls != 0 || fakeGitHub.createPRCalls != 0 {
		t.Fatalf("report-only helper made delivery calls add=%d commit=%d force=%d pr=%d", fakeGit.addAllCalls, fakeGit.commitCalls, fakeGit.forcePushCalls, fakeGitHub.createPRCalls)
	}
	if !strings.Contains(warnings.String(), "report-only preserved partial work") {
		t.Fatalf("warnings missing report-only note:\n%s", warnings.String())
	}

	writeRecovery(ctx, dispatch, err)
	brief, err := os.ReadFile(state.RecoveryBriefPath(repo, "run-hung", "job-509-1357"))
	if err != nil {
		t.Fatalf("read recovery brief: %v", err)
	}
	briefInfo, err := os.Stat(state.RecoveryBriefPath(repo, "run-hung", "job-509-1357"))
	if err != nil {
		t.Fatalf("stat recovery brief: %v", err)
	}
	if runtime.GOOS != "windows" && briefInfo.Mode().Perm() != 0o600 {
		t.Fatalf("recovery brief mode = %#o, want 0600", briefInfo.Mode().Perm())
	}
	for _, want := range []string{"- Status: hung", "- Last phase: worktree_created", " M partial.go", "last useful line"} {
		if !strings.Contains(string(brief), want) {
			t.Fatalf("recovery brief missing %q:\n%s", want, string(brief))
		}
	}
	attempts, err := state.LoadAttempts(repo, "run-hung")
	if err != nil {
		t.Fatalf("LoadAttempts returned error: %v", err)
	}
	if len(attempts) != 1 || attempts[0].Status != "hung" || attempts[0].Report != nil {
		t.Fatalf("hung recovery attempt = %#v", attempts)
	}
	events, err := os.ReadFile(state.EventsPath(repo, "run-hung"))
	if err != nil {
		t.Fatalf("ReadFile events: %v", err)
	}
	for _, want := range []string{`"event":"worker_hung"`, `"event":"worker_partial_work_reported"`, `"status":"hung"`} {
		if !strings.Contains(string(events), want) {
			t.Fatalf("events missing %q:\n%s", want, string(events))
		}
	}
}

func fixedNow() time.Time {
	return time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
}

func validWorkerAgentResult(summary string, exitCode int) agent.Result {
	inputTokens := int64(120)
	outputTokens := int64(34)
	totalTokens := int64(154)
	return agent.Result{
		ExitCode: exitCode,
		Summary:  summary,
		Model:    "gpt-5.5",
		Effort:   "xhigh",
		Usage: reporter.Usage{
			InputTokens:  &inputTokens,
			OutputTokens: &outputTokens,
			TotalTokens:  &totalTokens,
		},
		StartedAt:  "2026-06-28T00:00:00Z",
		EndedAt:    "2026-06-28T00:00:42Z",
		DurationMS: 42000,
	}
}

func registerWorkerProgressProject(t *testing.T, ctx context.Context, dbPath, repo string, now func() time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("mkdir db dir: %v", err)
	}
	absRepo, err := filepath.Abs(repo)
	if err != nil {
		t.Fatalf("abs repo: %v", err)
	}
	store, err := storage.Open(ctx, storage.Options{Path: dbPath, Now: now})
	if err != nil {
		t.Fatalf("Open registry store: %v", err)
	}
	defer store.Close()
	ts := state.FormatTimestamp(now())
	if err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO projects(id, local_path, local_path_canonical, display_name, identity_source, created_at, updated_at)
			VALUES (?, ?, ?, 'repo', 'local-path', ?, ?)
			ON CONFLICT(id) DO UPDATE SET local_path = excluded.local_path, local_path_canonical = excluded.local_path_canonical, detached_at = ''`,
			"proj_worker_progress", absRepo, absRepo, ts, ts)
		return err
	}); err != nil {
		t.Fatalf("insert project: %v", err)
	}
}

func mustWorkerJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal JSON: %v", err)
	}
	return string(data)
}

type workerManualClock struct {
	mu  sync.Mutex
	now time.Time
	ch  chan time.Time
}

func newWorkerManualClock(now time.Time) *workerManualClock {
	return &workerManualClock{now: now.UTC(), ch: make(chan time.Time, 16)}
}

func (c *workerManualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *workerManualClock) NewTicker(time.Duration) progress.Ticker {
	return workerManualTicker{ch: c.ch}
}

func (c *workerManualClock) Advance(duration time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(duration)
	now := c.now
	c.mu.Unlock()
	c.ch <- now
}

type workerManualTicker struct {
	ch <-chan time.Time
}

func (t workerManualTicker) C() <-chan time.Time { return t.ch }
func (t workerManualTicker) Stop()               {}

type workerFailingWriteStore struct {
	storage.Store
	mu       sync.Mutex
	attempts int
	skip     int
	failures int
}

func (s *workerFailingWriteStore) WithWriteTx(ctx context.Context, fn func(storage.Tx) error) error {
	s.mu.Lock()
	s.attempts++
	attempt := s.attempts
	shouldFail := attempt > s.skip && s.failures > 0
	if shouldFail {
		s.failures--
	}
	s.mu.Unlock()
	if shouldFail {
		return errors.New("injected progress write failure")
	}
	return s.Store.WithWriteTx(ctx, fn)
}

func (s *workerFailingWriteStore) Attempts() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts
}

func waitForWorkerWriteAttempts(t *testing.T, store *workerFailingWriteStore, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if store != nil && store.Attempts() >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	if store == nil {
		t.Fatalf("write attempts = 0, want at least %d", want)
	}
	t.Fatalf("write attempts = %d, want at least %d", store.Attempts(), want)
}

func waitForWorkerValidationCalls(t *testing.T, mu *sync.Mutex, calls *int, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := *calls
		mu.Unlock()
		if got >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	got := *calls
	mu.Unlock()
	t.Fatalf("ownership validation calls = %d, want at least %d", got, want)
}

func assertNoReportFootprint(t *testing.T, surface, text string, record reporter.Report) {
	t.Helper()
	canonical, err := record.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON returned error: %v", err)
	}
	for _, forbidden := range []string{record.Header(), string(canonical), "[attestation]"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("%s contains forbidden report text %q:\n%s", surface, forbidden, text)
		}
	}
}

func assertPathExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("%s does not exist: %v", path, err)
	}
}

func assertPathMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s exists or returned unexpected error: %v", path, err)
	}
}

func attachReleasedWorkerOwnership(t *testing.T, ctx context.Context, dispatch *dispatchContext) (storage.Store, storage.AgentOwnershipLease) {
	t.Helper()
	store, err := storage.Open(ctx, storage.Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedNow})
	if err != nil {
		t.Fatalf("Open ownership store: %v", err)
	}
	if err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO projects(id, local_path, local_path_canonical, git_root, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)`,
			"project-worker", dispatch.repoPath, dispatch.repoPath, dispatch.repoPath, "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z")
		return err
	}); err != nil {
		store.Close()
		t.Fatalf("seed worker ownership project: %v", err)
	}
	lease, err := storage.AcquireAgentOwnershipLease(ctx, store, storage.AgentOwnershipLeaseRequest{
		ProjectID:     "project-worker",
		DeliveryRunID: dispatch.opts.RunID,
		RunID:         dispatch.opts.RunID,
		OwnerID:       workerOwnershipOwnerID(dispatch),
		Now:           fixedNow(),
		LeaseUntil:    fixedNow().Add(time.Hour),
		Resources: []storage.AgentOwnershipResource{
			{ResourceKind: "repo-path", ResourceKey: "."},
		},
	})
	if err != nil {
		store.Close()
		t.Fatalf("AcquireAgentOwnershipLease: %v", err)
	}
	dispatch.ownershipStore = store
	dispatch.ownershipLease = &lease
	if err := storage.ReleaseAgentOwnershipLease(ctx, store, lease, fixedNow().Add(time.Minute)); err != nil {
		store.Close()
		t.Fatalf("ReleaseAgentOwnershipLease: %v", err)
	}
	return store, lease
}

type workerFakeGit struct {
	status              string
	err                 error
	worktreeSetup       func(worktreePath string) error
	worktreeRemoveErr   error
	branchDeleteErr     error
	remoteBranchExists  bool
	fetchCalls          int
	fetchBase           string
	worktreeAddCalls    int
	worktreeBranch      string
	worktreeBase        string
	worktreePath        string
	removeCalls         int
	removePath          string
	branchDeleteCalls   int
	deletedBranch       string
	addAllCalls         int
	commitCalls         int
	pushCalls           int
	forcePushCalls      int
	lastCommitMessage   string
	lastForcePushBranch string
}

func (f *workerFakeGit) FetchOriginBase(_ context.Context, _, baseBranch string) error {
	f.fetchCalls++
	f.fetchBase = baseBranch
	return f.err
}

func (f *workerFakeGit) WorktreeAdd(_ context.Context, _, branch, worktreePath, baseBranch string) error {
	f.worktreeAddCalls++
	f.worktreeBranch = branch
	f.worktreeBase = baseBranch
	f.worktreePath = worktreePath
	if f.err != nil {
		return f.err
	}
	if err := os.MkdirAll(worktreePath, 0o755); err != nil {
		return err
	}
	if f.worktreeSetup != nil {
		return f.worktreeSetup(worktreePath)
	}
	return nil
}

func (f *workerFakeGit) WorktreeRemove(_ context.Context, _, worktreePath string) error {
	f.removeCalls++
	f.removePath = worktreePath
	return f.worktreeRemoveErr
}

func (f *workerFakeGit) StatusPorcelain(context.Context, string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.status, nil
}

func (f *workerFakeGit) AddAll(context.Context, string) error {
	f.addAllCalls++
	return f.err
}

func (f *workerFakeGit) Commit(_ context.Context, _, message string) error {
	f.commitCalls++
	f.lastCommitMessage = message
	return f.err
}

func (f *workerFakeGit) PushUpstream(context.Context, string, string) error {
	f.pushCalls++
	if f.remoteBranchExists {
		return os.ErrExist
	}
	return f.err
}

func (f *workerFakeGit) PushUpstreamForceWithLease(_ context.Context, _, branch string) error {
	f.forcePushCalls++
	f.lastForcePushBranch = branch
	return f.err
}

func (f *workerFakeGit) BranchDelete(_ context.Context, _, branch string) error {
	f.branchDeleteCalls++
	f.deletedBranch = branch
	return f.branchDeleteErr
}

type workerFakeGitHub struct {
	prURL         string
	prs           []gh.PullRequestReference
	prsByCall     map[int][]gh.PullRequestReference
	openPRs       []gh.PullRequest
	err           error
	repoErr       error
	createErr     error
	listHeadErr   error
	listOpenErr   error
	createPRCalls int
	listHeadCalls int
	lastPRHead    string
	lastPRBase    string
	lastPRTitle   string
	lastPRBody    string
}

func (f *workerFakeGitHub) RepoName(context.Context) (string, error) {
	if f.repoErr != nil {
		return "", f.repoErr
	}
	if f.err != nil {
		return "", f.err
	}
	return "owner/repo", nil
}

func (f *workerFakeGitHub) CreatePR(_ context.Context, head, base, title, body string) (string, error) {
	f.createPRCalls++
	if f.createErr != nil {
		return "", f.createErr
	}
	if f.err != nil {
		return "", f.err
	}
	f.lastPRHead = head
	f.lastPRBase = base
	f.lastPRTitle = title
	f.lastPRBody = body
	if f.prURL == "" {
		return "https://github.com/owner/repo/pull/1", nil
	}
	return f.prURL, nil
}

func (f *workerFakeGitHub) ListHeadPRs(context.Context, string) ([]gh.PullRequestReference, error) {
	f.listHeadCalls++
	if f.listHeadErr != nil {
		return nil, f.listHeadErr
	}
	if f.err != nil {
		return nil, f.err
	}
	if f.prsByCall != nil {
		if prs, ok := f.prsByCall[f.listHeadCalls]; ok {
			return prs, nil
		}
	}
	return f.prs, nil
}

func (f *workerFakeGitHub) ListOpenPRs(context.Context) ([]gh.PullRequest, error) {
	if f.listOpenErr != nil {
		return nil, f.listOpenErr
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.openPRs, nil
}

type workerFakeAgent struct {
	invocation agent.Invocation
	result     agent.Result
	resultSet  bool
	summary    string
	log        string
	exitCode   int
	err        error
	runCalls   int
}

func (f *workerFakeAgent) Run(_ context.Context, invocation agent.Invocation) (agent.Result, error) {
	f.runCalls++
	f.invocation = invocation
	if err := os.WriteFile(invocation.LogPath, []byte(f.log), 0o644); err != nil {
		return agent.Result{ExitCode: -1}, err
	}
	if f.err != nil {
		return agent.Result{ExitCode: -1}, f.err
	}
	if f.resultSet {
		return f.result, nil
	}
	return agent.Result{ExitCode: f.exitCode, Summary: f.summary}, nil
}

type workerContextErrAgent struct{}

func (workerContextErrAgent) Run(ctx context.Context, invocation agent.Invocation) (agent.Result, error) {
	_ = os.WriteFile(invocation.LogPath, []byte("parent context stopped\n"), 0o644)
	if ctx.Err() != nil {
		return agent.Result{ExitCode: -1}, ctx.Err()
	}
	return agent.Result{ExitCode: -1}, context.Canceled
}

type workerFakeLock struct {
	err error
}

func (l *workerFakeLock) Release() error {
	if l.err != nil {
		return l.err
	}
	return nil
}
