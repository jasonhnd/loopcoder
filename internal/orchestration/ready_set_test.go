package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	compiler "github.com/jasonhnd/loopcoder/internal/compile"
	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/guardrails"
	"github.com/jasonhnd/loopcoder/internal/state"
	gh "github.com/jasonhnd/loopcoder/internal/vcs/github"
)

type fakeReader struct {
	repo            string
	issues          []gh.Issue
	views           map[int]gh.Issue
	viewErrs        map[int]error
	prs             []gh.PullRequest
	checks          map[int][]gh.Check
	checkErrs       map[int]error
	branchChecks    map[string]gh.BranchChecksResult
	branchCheckErrs map[string]error
	branchHeads     map[string]string
	branchHeadErrs  map[string]error
	diffFiles       map[int][]string
	diffs           map[int]string
	diffErrs        map[int]error
}

func (f fakeReader) RepoName(context.Context) (string, error) {
	return f.repo, nil
}

func (f fakeReader) ListIssues(context.Context, string) ([]gh.Issue, error) {
	return append([]gh.Issue(nil), f.issues...), nil
}

func (f fakeReader) ViewIssue(_ context.Context, number int) (gh.Issue, error) {
	if err := f.viewErrs[number]; err != nil {
		return gh.Issue{}, err
	}
	return f.views[number], nil
}

func (f fakeReader) ListOpenPRs(context.Context) ([]gh.PullRequest, error) {
	return append([]gh.PullRequest(nil), f.prs...), nil
}

func (f fakeReader) PRChecks(_ context.Context, number int) ([]gh.Check, error) {
	if err := f.checkErrs[number]; err != nil {
		return nil, err
	}
	return append([]gh.Check(nil), f.checks[number]...), nil
}

func (f fakeReader) BranchChecks(_ context.Context, branch string) (gh.BranchChecksResult, error) {
	if err := f.branchCheckErrs[branch]; err != nil {
		return gh.BranchChecksResult{}, err
	}
	if result, ok := f.branchChecks[branch]; ok {
		result.Checks = append([]gh.Check(nil), result.Checks...)
		return result, nil
	}
	return gh.BranchChecksResult{Branch: branch, HeadSHA: "abc123", Checks: passChecks()}, nil
}

func (f fakeReader) BranchHeadSHA(_ context.Context, branch string) (string, error) {
	if err := f.branchHeadErrs[branch]; err != nil {
		return "", err
	}
	if sha, ok := f.branchHeads[branch]; ok {
		return sha, nil
	}
	return "abc123", nil
}

func (f fakeReader) PRDiff(_ context.Context, number int) (string, error) {
	if err := f.diffErrs[number]; err != nil {
		return "", err
	}
	return f.diffs[number], nil
}

func (f fakeReader) PRDiffNameOnly(_ context.Context, number int) ([]string, error) {
	if err := f.diffErrs[number]; err != nil {
		return nil, err
	}
	return append([]string(nil), f.diffFiles[number]...), nil
}

func TestComputeReadySetClassifications(t *testing.T) {
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	pid := 1234
	reader := fakeReader{
		repo: "owner/repo",
		issues: []gh.Issue{
			{Number: 1, Title: "Ready", State: "OPEN"},
			{Number: 2, Title: "Blocked by open dep", State: "OPEN", Labels: []gh.Label{{Name: "blocked-by:#10"}}},
			{Number: 3, Title: "Has PR", State: "OPEN"},
			{Number: 4, Title: "Running", State: "OPEN"},
			{Number: 5, Title: "Failed", State: "OPEN"},
			{Number: 6, Title: "Closing ref dep", State: "OPEN", Labels: []gh.Label{{Name: "blocked-by:#11"}}},
		},
		views: map[int]gh.Issue{
			10: {Number: 10, Title: "Dependency", State: "OPEN"},
			11: {
				Number:                         11,
				Title:                          "Dependency with closing PR",
				State:                          "OPEN",
				ClosedByPullRequestsReferences: []gh.PullRequestReference{{Number: 99}},
			},
		},
		prs: []gh.PullRequest{
			{Number: 30, Title: "Implementation", URL: "https://github.com/owner/repo/pull/30", HeadRefName: "loop/issue-3"},
		},
		checks: map[int][]gh.Check{
			30: {{Name: "go", Bucket: "fail"}},
		},
	}

	result, err := ComputeReadySet(context.Background(), Options{
		Reader:     reader,
		RepoPath:   "C:/repo",
		BaseBranch: "main",
		RunID:      "run-test",
		Attempts: []state.Attempt{
			{
				JobID:          "job-4",
				Issue:          4,
				Attempt:        1,
				Status:         "running",
				PID:            &pid,
				HeartbeatAt:    "2026-06-26T11:59:50Z",
				LastProgressAt: "2026-06-26T11:59:50Z",
				Branch:         "loop/issue-4",
			},
			{
				JobID:   "job-5",
				Issue:   5,
				Attempt: 1,
				Status:  "failed",
				Branch:  "loop/issue-5",
			},
		},
		Thresholds:   config.Default().Resilience.Worker,
		ProcessAlive: func(int) bool { return false },
		Now:          now,
	})
	if err != nil {
		t.Fatalf("ComputeReadySet returned error: %v", err)
	}

	if result.Repo != "owner/repo" || result.GeneratedAt != "2026-06-26T12:00:00Z" {
		t.Fatalf("metadata mismatch: %#v", result)
	}
	if result.Summary.ReadyCount != 2 {
		t.Fatalf("ReadyCount = %d, want 2; ready=%#v", result.Summary.ReadyCount, result.Ready)
	}
	if result.Summary.BlockedByUnmergedDepCount != 1 ||
		result.Summary.HasOpenPRCount != 1 ||
		result.Summary.HasLiveAttemptCount != 1 ||
		result.Summary.RecoveryNeededCount != 1 {
		t.Fatalf("summary counts incorrect: %#v", result.Summary)
	}

	classes := map[int]string{}
	reasons := map[int]string{}
	for _, blocked := range result.Blocked {
		classes[blocked.Issue] = blocked.Classification
		reasons[blocked.Issue] = blocked.Reason
		if blocked.Issue == 3 {
			if len(blocked.OpenPRs) != 1 || blocked.OpenPRs[0].SubState != "fixing" {
				t.Fatalf("issue #3 PR summary = %#v, want fixing", blocked.OpenPRs)
			}
		}
	}
	if classes[2] != "blocked-by-unmerged-dep" {
		t.Fatalf("issue #2 classification = %q", classes[2])
	}
	if !strings.Contains(reasons[2], "#10 is still open") {
		t.Fatalf("issue #2 reason = %q", reasons[2])
	}
	if classes[3] != "has-open-PR" {
		t.Fatalf("issue #3 classification = %q", classes[3])
	}
	if classes[4] != "has-live-attempt" {
		t.Fatalf("issue #4 classification = %q", classes[4])
	}
	if classes[5] != "recovery-needed" {
		t.Fatalf("issue #5 classification = %q", classes[5])
	}
}

func TestComputeReadySetUnknownDependencyFailsClosed(t *testing.T) {
	result, err := ComputeReadySet(context.Background(), Options{
		Reader: fakeReader{
			repo:   "owner/repo",
			issues: []gh.Issue{{Number: 7, Title: "Unknown dep", State: "OPEN", Labels: []gh.Label{{Name: "blocked-by:#99"}}}},
			views:  map[int]gh.Issue{},
			viewErrs: map[int]error{
				99: errors.New("not found"),
			},
		},
		RepoPath:     "C:/repo",
		BaseBranch:   "main",
		Thresholds:   config.Default().Resilience.Worker,
		ProcessAlive: func(int) bool { return false },
		Now:          time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("ComputeReadySet returned error: %v", err)
	}
	if len(result.Blocked) != 1 {
		t.Fatalf("blocked count = %d, want 1", len(result.Blocked))
	}
	if result.Blocked[0].Classification != "blocked-by-unmerged-dep" {
		t.Fatalf("classification = %q", result.Blocked[0].Classification)
	}
	if !strings.Contains(result.Blocked[0].Reason, "#99 state is unknown") {
		t.Fatalf("reason = %q", result.Blocked[0].Reason)
	}
}

func TestComputeReadySetNeedsHumanLabelIsNonReady(t *testing.T) {
	result, err := ComputeReadySet(context.Background(), Options{
		Reader: fakeReader{
			repo: "owner/repo",
			issues: []gh.Issue{
				{Number: 35, Title: "Kicked back", State: "OPEN", Labels: []gh.Label{{Name: "needs-human"}}},
			},
		},
		RepoPath:     "C:/repo",
		BaseBranch:   "main",
		Thresholds:   config.Default().Resilience.Worker,
		ProcessAlive: func(int) bool { return false },
		Now:          time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("ComputeReadySet returned error: %v", err)
	}
	if len(result.Ready) != 0 {
		t.Fatalf("ready = %#v, want none", result.Ready)
	}
	if len(result.Blocked) != 1 || result.Blocked[0].Classification != "needs-human" {
		t.Fatalf("blocked = %#v, want needs-human", result.Blocked)
	}
	if !strings.Contains(result.Blocked[0].Reason, "needs-human") {
		t.Fatalf("reason = %q, want needs-human label", result.Blocked[0].Reason)
	}
}

func TestComputeReadySetHungAttemptWithLivePidBlocksAsLive(t *testing.T) {
	pid := 2222
	result, err := ComputeReadySet(context.Background(), Options{
		Reader: fakeReader{
			repo:   "owner/repo",
			issues: []gh.Issue{{Number: 8, Title: "Hung", State: "OPEN"}},
		},
		RepoPath:   "C:/repo",
		BaseBranch: "main",
		Attempts: []state.Attempt{{
			JobID:          "job-8",
			Issue:          8,
			Attempt:        1,
			Status:         "running",
			PID:            &pid,
			LastProgressAt: "2026-06-26T11:49:00Z",
			Branch:         "loop/issue-8",
		}},
		Thresholds:   config.Default().Resilience.Worker,
		ProcessAlive: func(int) bool { return true },
		Now:          time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("ComputeReadySet returned error: %v", err)
	}
	if len(result.Blocked) != 1 {
		t.Fatalf("blocked count = %d, want 1", len(result.Blocked))
	}
	if result.Blocked[0].Classification != "has-live-attempt" {
		t.Fatalf("classification = %q", result.Blocked[0].Classification)
	}
	if !strings.Contains(result.Blocked[0].Reason, "hung but pid is still alive") {
		t.Fatalf("reason = %q", result.Blocked[0].Reason)
	}
}

func TestComputeReadySetPRCheckFailureIsBestEffortGated(t *testing.T) {
	result, err := ComputeReadySet(context.Background(), Options{
		Reader: fakeReader{
			repo:   "owner/repo",
			issues: []gh.Issue{{Number: 9, Title: "PR checks unavailable", State: "OPEN"}},
			prs: []gh.PullRequest{{
				Number:      90,
				Title:       "Fix #9",
				URL:         "https://github.com/owner/repo/pull/90",
				HeadRefName: "feature/checks-unavailable",
			}},
			checkErrs: map[int]error{
				90: errors.New("checks unavailable"),
			},
		},
		RepoPath:     "C:/repo",
		BaseBranch:   "main",
		Thresholds:   config.Default().Resilience.Worker,
		ProcessAlive: func(int) bool { return false },
		Now:          time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("ComputeReadySet returned error: %v", err)
	}
	if len(result.Blocked) != 1 {
		t.Fatalf("blocked count = %d, want 1", len(result.Blocked))
	}
	if result.Blocked[0].Classification != "has-open-PR" {
		t.Fatalf("classification = %q", result.Blocked[0].Classification)
	}
	if len(result.Blocked[0].OpenPRs) != 1 || result.Blocked[0].OpenPRs[0].SubState != "gated" {
		t.Fatalf("open PR summaries = %#v, want gated", result.Blocked[0].OpenPRs)
	}
}

func TestComputeReadySetMarksGuardrailFrozenIssueNonReady(t *testing.T) {
	repo := t.TempDir()
	if _, err := guardrails.RecordDecision(repo, guardrails.Decision{
		Guardrail:       guardrails.GuardrailCircuitBreaker,
		Enabled:         true,
		Allowed:         false,
		Status:          guardrails.StatusNeedsHuman,
		Reason:          "guardrails.circuit_breaker.max_no_progress_waves",
		DeliveryScopeID: "main:1,2",
		BaseBranch:      "main",
		RunID:           "run-test-wave",
		Issue:           1,
		Issues:          []int{1, 2},
		Observed: guardrails.Observed{
			NoProgressWaves: 1,
		},
		DecisionAt: time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("RecordDecision frozen ledger: %v", err)
	}

	result, err := ComputeReadySet(context.Background(), Options{
		Reader: fakeReader{
			repo: "owner/repo",
			issues: []gh.Issue{
				{Number: 1, Title: "Frozen", State: "OPEN"},
				{Number: 2, Title: "Ready sibling", State: "OPEN"},
			},
		},
		RepoPath:     repo,
		BaseBranch:   "main",
		Thresholds:   config.Default().Resilience.Worker,
		ProcessAlive: func(int) bool { return false },
		Now:          time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("ComputeReadySet returned error: %v", err)
	}

	if len(result.Ready) != 1 || result.Ready[0].Issue != 2 {
		t.Fatalf("ready = %#v, want only issue #2", result.Ready)
	}
	if len(result.Blocked) != 1 || result.Blocked[0].Issue != 1 {
		t.Fatalf("blocked = %#v, want only issue #1", result.Blocked)
	}
	if result.Blocked[0].Classification != "guardrail-frozen" {
		t.Fatalf("classification = %q, want guardrail-frozen", result.Blocked[0].Classification)
	}
	for _, want := range []string{"guardrails.circuit_breaker.max_no_progress_waves", "no_progress_waves=1", "human_decision=clarify the issue"} {
		if !strings.Contains(result.Blocked[0].Reason, want) {
			t.Fatalf("reason missing %q:\n%s", want, result.Blocked[0].Reason)
		}
	}
}

func TestComputeReadySetOrdersEpicReadyLayerFromSliceDAG(t *testing.T) {
	repo := t.TempDir()
	writeEpicOrderingArtifact(t, repo, compiler.EpicSliceDAGArtifact{
		Version:   compiler.EpicDAGVersion,
		EpicID:    "epic-1",
		EpicTitle: "Migration",
		Nodes: []compiler.EpicSliceNode{
			{ID: "a", Ref: "migration/a", Issue: 10},
			{ID: "b", Ref: "migration/b", Issue: 20},
			{ID: "c", Ref: "migration/c", Issue: 30, DependsOn: []string{"b"}},
		},
		Ordering: &compiler.EpicDAGOrdering{
			Ready: []compiler.EpicDAGOrderNode{
				{ID: "b", Ref: "migration/b", Issue: 20, UnblockCount: 1, OnCriticalPath: true},
				{ID: "a", Ref: "migration/a", Issue: 10, UnblockCount: 0},
			},
			Layers: []compiler.EpicDAGLayer{
				{Index: 0, Nodes: []compiler.EpicDAGOrderNode{
					{ID: "b", Ref: "migration/b", Issue: 20, UnblockCount: 1, OnCriticalPath: true},
					{ID: "a", Ref: "migration/a", Issue: 10, UnblockCount: 0},
				}},
				{Index: 1, Nodes: []compiler.EpicDAGOrderNode{
					{ID: "c", Ref: "migration/c", Issue: 30, DependsOn: []string{"b"}},
				}},
			},
			CriticalPath:    []string{"migration/b", "migration/c"},
			CriticalPathETA: 2,
		},
	})

	result, err := ComputeReadySet(context.Background(), Options{
		Reader: fakeReader{
			repo: "owner/repo",
			issues: []gh.Issue{
				{Number: 10, Title: "A", State: "OPEN"},
				{Number: 20, Title: "B", State: "OPEN"},
			},
		},
		RepoPath:     repo,
		BaseBranch:   "main",
		Thresholds:   config.Default().Resilience.Worker,
		ProcessAlive: func(int) bool { return false },
		Now:          time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("ComputeReadySet returned error: %v", err)
	}

	if len(result.Ready) != 2 || result.Ready[0].Issue != 20 || result.Ready[1].Issue != 10 {
		t.Fatalf("ready = %#v, want issue #20 before #10 from epic ordering", result.Ready)
	}
	if !result.Ready[0].OnCriticalPath || result.Ready[0].SliceRef != "migration/b" {
		t.Fatalf("first ready issue missing epic hint: %#v", result.Ready[0])
	}
	if len(result.EpicOrdering) != 1 || result.EpicOrdering[0].CriticalPathETA != 2 {
		t.Fatalf("epic ordering summary = %#v, want ETA 2", result.EpicOrdering)
	}
}

func TestComputeReadySetIsolatesBadEpicArtifacts(t *testing.T) {
	repo := t.TempDir()
	writeEpicOrderingArtifactNamed(t, repo, "valid.slice_dag.json", compiler.EpicSliceDAGArtifact{
		Version:   compiler.EpicDAGVersion,
		EpicID:    "valid-epic",
		EpicTitle: "Valid migration",
		Nodes: []compiler.EpicSliceNode{
			{ID: "a", Ref: "valid/a", Issue: 10},
			{ID: "b", Ref: "valid/b", Issue: 20},
		},
		Ordering: &compiler.EpicDAGOrdering{
			Ready: []compiler.EpicDAGOrderNode{
				{ID: "b", Ref: "valid/b", Issue: 20, UnblockCount: 1, OnCriticalPath: true},
				{ID: "a", Ref: "valid/a", Issue: 10},
			},
			Layers: []compiler.EpicDAGLayer{
				{Index: 0, Nodes: []compiler.EpicDAGOrderNode{
					{ID: "b", Ref: "valid/b", Issue: 20, UnblockCount: 1, OnCriticalPath: true},
					{ID: "a", Ref: "valid/a", Issue: 10},
				}},
			},
			CriticalPath:    []string{"valid/b"},
			CriticalPathETA: 1,
		},
	})
	writeEpicOrderingArtifactNamed(t, repo, "cycle.slice_dag.json", compiler.EpicSliceDAGArtifact{
		Version:   compiler.EpicDAGVersion,
		EpicID:    "bad-epic",
		EpicTitle: "Bad migration",
		Nodes: []compiler.EpicSliceNode{
			{ID: "bad-a", Ref: "bad/a", Issue: 40, DependsOn: []string{"bad-b"}},
			{ID: "bad-b", Ref: "bad/b", DependsOn: []string{"bad-a"}},
		},
	})
	root := filepath.Join(repo, ".loopcoder", "epics")
	if err := os.WriteFile(filepath.Join(root, "broken.slice_dag.json"), []byte("{not-json\n"), 0o644); err != nil {
		t.Fatalf("WriteFile corrupt epic artifact: %v", err)
	}

	result, err := ComputeReadySet(context.Background(), Options{
		Reader: fakeReader{
			repo: "owner/repo",
			issues: []gh.Issue{
				{Number: 10, Title: "A", State: "OPEN"},
				{Number: 20, Title: "B", State: "OPEN"},
				{Number: 40, Title: "Bad A", State: "OPEN"},
			},
		},
		RepoPath:     repo,
		BaseBranch:   "main",
		Thresholds:   config.Default().Resilience.Worker,
		ProcessAlive: func(int) bool { return false },
		Now:          time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("ComputeReadySet returned error: %v", err)
	}

	if len(result.Ready) != 2 || result.Ready[0].Issue != 20 || result.Ready[1].Issue != 10 {
		t.Fatalf("ready = %#v, want valid epic issues ordered before corrupt artifact fallout", result.Ready)
	}
	if len(result.EpicOrdering) != 1 || result.EpicOrdering[0].EpicID != "valid-epic" {
		t.Fatalf("epic ordering = %#v, want only valid epic summary", result.EpicOrdering)
	}
	if len(result.Blocked) != 1 || result.Blocked[0].Issue != 40 || result.Blocked[0].Classification != "needs-human" {
		t.Fatalf("blocked = %#v, want bad epic issue marked needs-human", result.Blocked)
	}
	if !strings.Contains(result.Blocked[0].Reason, "cycle.slice_dag.json") ||
		!strings.Contains(result.Blocked[0].Reason, "epic slice DAG contains a cycle") {
		t.Fatalf("bad artifact reason = %q", result.Blocked[0].Reason)
	}
}

func writeEpicOrderingArtifact(t *testing.T, repo string, artifact compiler.EpicSliceDAGArtifact) {
	t.Helper()
	writeEpicOrderingArtifactNamed(t, repo, "epic-1.slice_dag.json", artifact)
}

func writeEpicOrderingArtifactNamed(t *testing.T, repo, name string, artifact compiler.EpicSliceDAGArtifact) {
	t.Helper()
	root := filepath.Join(repo, ".loopcoder", "epics")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll epic dir: %v", err)
	}
	data, err := json.MarshalIndent(artifact, "", "  ")
	if err != nil {
		t.Fatalf("Marshal epic artifact: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, name), append(data, '\n'), 0o644); err != nil {
		t.Fatalf("WriteFile epic artifact: %v", err)
	}
}
