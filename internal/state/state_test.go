package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/reporter"
)

func TestFormatTimestampUsesUTCRFC3339(t *testing.T) {
	jst := time.FixedZone("JST", 9*60*60)
	input := time.Date(2026, 6, 26, 12, 34, 56, 987654321, jst)

	got := FormatTimestamp(input)
	want := "2026-06-26T03:34:56Z"
	if got != want {
		t.Fatalf("FormatTimestamp() = %q, want %q", got, want)
	}
}

func TestParseTimestampAcceptsPowerShellRoundTripFormat(t *testing.T) {
	got, err := ParseTimestamp("2026-06-26T12:34:56.1234567+09:00")
	if err != nil {
		t.Fatalf("ParseTimestamp returned error: %v", err)
	}

	want := time.Date(2026, 6, 26, 3, 34, 56, 123456700, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("ParseTimestamp() = %s, want %s", got, want)
	}
}

func TestTimestampFormatParseRoundTrip(t *testing.T) {
	input := time.Date(2026, 6, 26, 12, 34, 56, 0, time.FixedZone("offset", -7*60*60))

	formatted := FormatTimestamp(input)
	parsed, err := ParseTimestamp(formatted)
	if err != nil {
		t.Fatalf("ParseTimestamp(%q) returned error: %v", formatted, err)
	}

	if !parsed.Equal(input.UTC()) {
		t.Fatalf("parsed timestamp = %s, want %s", parsed, input.UTC())
	}
}

func TestRunIDForIssueUsesDocumentedShape(t *testing.T) {
	got := RunIDForIssue(91, time.Date(2026, 6, 26, 12, 0, 0, 0, time.FixedZone("JST", 9*60*60)))
	want := "run-20260626T030000Z-issue-91"
	if got != want {
		t.Fatalf("RunIDForIssue() = %q, want %q", got, want)
	}
	if !IsRunID(got) {
		t.Fatalf("IsRunID(%q) = false, want true", got)
	}
}

func TestLatestRunIDSelectsNewestRunDirectory(t *testing.T) {
	repo := t.TempDir()
	runsRoot := filepath.Join(repo, ".loopcoder", "runs")
	if err := os.MkdirAll(filepath.Join(runsRoot, "run-old"), 0o755); err != nil {
		t.Fatalf("MkdirAll old run: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(runsRoot, "run-new"), 0o755); err != nil {
		t.Fatalf("MkdirAll new run: %v", err)
	}

	oldTime := time.Date(2026, 6, 26, 1, 0, 0, 0, time.UTC)
	newTime := time.Date(2026, 6, 26, 2, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(runsRoot, "run-old"), oldTime, oldTime); err != nil {
		t.Fatalf("Chtimes old run: %v", err)
	}
	if err := os.Chtimes(filepath.Join(runsRoot, "run-new"), newTime, newTime); err != nil {
		t.Fatalf("Chtimes new run: %v", err)
	}

	got, err := LatestRunID(repo)
	if err != nil {
		t.Fatalf("LatestRunID returned error: %v", err)
	}
	if got != "run-new" {
		t.Fatalf("LatestRunID() = %q, want run-new", got)
	}
}

func TestLatestRunIDReturnsEmptyWhenRunsRootMissing(t *testing.T) {
	got, err := LatestRunID(t.TempDir())
	if err != nil {
		t.Fatalf("LatestRunID returned error: %v", err)
	}
	if got != "" {
		t.Fatalf("LatestRunID() = %q, want empty", got)
	}
}

func TestLoadAttemptsInfersOptionalFields(t *testing.T) {
	repo := t.TempDir()
	workers := filepath.Join(repo, ".loopcoder", "runs", "run-test", "workers")
	if err := os.MkdirAll(workers, 0o755); err != nil {
		t.Fatalf("MkdirAll workers: %v", err)
	}
	attemptPath := filepath.Join(workers, "job-42-1234.attempt.json")
	data := []byte(`{
  "version": 1,
  "issue": "42",
  "attempt": 2,
  "provider": "codex",
  "pid": "1234",
  "phase": "codex_started",
  "status": "running",
  "recovery_context_path": ".loopcoder/runs/run-test/recovery/job-42-1234-context.md",
  "heartbeat_at": "2026-06-26T12:01:00Z",
  "last_progress_at": "2026-06-26T12:01:00Z",
  "usage": {"input_tokens": 10, "output_tokens": 5},
  "attestation": {"role":"worker","provider":"codex","model":"gpt-5.5","model_source":"parsed","effort":"xhigh","permission":"write","action":"implement issue #42","exit_code":0,"started_at":"2026-06-28T00:00:00Z","ended_at":"2026-06-28T00:00:42Z","duration_ms":42000,"usage":{"input_tokens":10,"output_tokens":5},"verified":true},
  "cost_usd": "1.25",
  "exit_code": null
}`)
	if err := os.WriteFile(attemptPath, data, 0o644); err != nil {
		t.Fatalf("WriteFile attempt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workers, "bad.attempt.json"), []byte(`{bad`), 0o644); err != nil {
		t.Fatalf("WriteFile bad attempt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workers, "missing-issue.attempt.json"), []byte(`{"attempt":1}`), 0o644); err != nil {
		t.Fatalf("WriteFile missing issue attempt: %v", err)
	}

	attempts, err := LoadAttempts(repo, "run-test")
	if err != nil {
		t.Fatalf("LoadAttempts returned error: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("LoadAttempts returned %d attempts, want 1: %#v", len(attempts), attempts)
	}
	got := attempts[0]
	if got.JobID != "job-42-1234" {
		t.Fatalf("JobID = %q, want job-42-1234", got.JobID)
	}
	if got.Issue != 42 || got.Attempt != 2 {
		t.Fatalf("Issue/Attempt = %d/%d, want 42/2", got.Issue, got.Attempt)
	}
	if got.Branch != "loop/issue-42-retry-2" {
		t.Fatalf("Branch = %q, want loop/issue-42-retry-2", got.Branch)
	}
	if got.RecoveryContextPath != ".loopcoder/runs/run-test/recovery/job-42-1234-context.md" {
		t.Fatalf("RecoveryContextPath = %q", got.RecoveryContextPath)
	}
	if got.PID == nil || *got.PID != 1234 {
		t.Fatalf("PID = %#v, want 1234", got.PID)
	}
	if got.ExitCode != nil {
		t.Fatalf("ExitCode = %#v, want nil", got.ExitCode)
	}
	if got.Usage == nil || got.Usage.InputTokens == nil || *got.Usage.InputTokens != 10 ||
		got.Usage.OutputTokens == nil || *got.Usage.OutputTokens != 5 {
		t.Fatalf("Usage = %#v, want input=10 output=5", got.Usage)
	}
	if got.Report == nil {
		t.Fatal("Report = nil, want persisted record")
	}
	if err := got.Report.Validate(); err != nil {
		t.Fatalf("Report does not validate: %v", err)
	}
	if got.Report.Action != "implement issue #42" {
		t.Fatalf("Report.Action = %q", got.Report.Action)
	}
	if got.CostUSD == nil || *got.CostUSD != 1.25 {
		t.Fatalf("CostUSD = %#v, want 1.25", got.CostUSD)
	}
	if got.Path != attemptPath {
		t.Fatalf("Path = %q, want %q", got.Path, attemptPath)
	}
}

func TestCountEventsCountsNonBlankLines(t *testing.T) {
	repo := t.TempDir()
	runPath := filepath.Join(repo, ".loopcoder", "runs", "run-test")
	if err := os.MkdirAll(runPath, 0o755); err != nil {
		t.Fatalf("MkdirAll run path: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runPath, "events.jsonl"), []byte("{\"event\":\"one\"}\n\n{\"event\":\"two\"}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile events: %v", err)
	}

	got, err := CountEvents(repo, "run-test")
	if err != nil {
		t.Fatalf("CountEvents returned error: %v", err)
	}
	if got != 2 {
		t.Fatalf("CountEvents() = %d, want 2", got)
	}
}

func TestCountEventsReturnsZeroWhenMissing(t *testing.T) {
	got, err := CountEvents(t.TempDir(), "run-missing")
	if err != nil {
		t.Fatalf("CountEvents returned error: %v", err)
	}
	if got != 0 {
		t.Fatalf("CountEvents() = %d, want 0", got)
	}
}

func TestWriteAttemptWritesCompactSidecar(t *testing.T) {
	repo := t.TempDir()
	exitCode := 0
	errText := "failed password=hunter2"
	totalTokens := int64(154)
	costUSD := 0.42
	reportRecord := validAttemptReport(101, totalTokens)

	path, err := WriteAttempt(repo, "run-test", AttemptRecord{
		Version:        1,
		JobID:          "job-101-1234",
		Issue:          101,
		Attempt:        2,
		Provider:       "codex",
		PID:            1234,
		Phase:          "codex_exited",
		Status:         "failed",
		Branch:         "loop/issue-101-retry-2",
		StartedAt:      "2026-06-26T12:00:00Z",
		HeartbeatAt:    "2026-06-26T12:01:00Z",
		LastProgressAt: "2026-06-26T12:01:00Z",
		LogBytes:       123,
		ExitCode:       &exitCode,
		Error:          &errText,
		Usage:          &reporter.Usage{TotalTokens: &totalTokens},
		Report:         &reportRecord,
		CostUSD:        &costUSD,
	})
	if err != nil {
		t.Fatalf("WriteAttempt returned error: %v", err)
	}
	if path != filepath.Join(repo, ".loopcoder", "runs", "run-test", "workers", "job-101-1234.attempt.json") {
		t.Fatalf("path = %q", path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile attempt: %v", err)
	}
	if strings.Contains(string(data), "\n") {
		t.Fatalf("attempt JSON is not compact: %q", string(data))
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("attempt JSON invalid: %v", err)
	}
	for _, key := range []string{"version", "job_id", "issue", "attempt", "provider", "pid", "phase", "status", "branch", "started_at", "heartbeat_at", "last_progress_at", "log_bytes", "exit_code", "error", "usage", "report", "cost_usd"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("attempt JSON missing key %q: %s", key, string(data))
		}
	}
	if got["branch"] != "loop/issue-101-retry-2" {
		t.Fatalf("branch = %#v", got["branch"])
	}
	reportField, ok := got["report"].(map[string]any)
	if !ok {
		t.Fatalf("report = %#v", got["report"])
	}
	if reportField["action"] != "implement issue #101" || reportField["role"] != "worker" {
		t.Fatalf("report field = %#v", reportField)
	}
}

func TestAppendEventWritesCompactJSONLine(t *testing.T) {
	repo := t.TempDir()
	exitCode := 0

	err := AppendEvent(repo, "run-test", Event{
		Timestamp: "2026-06-26T12:00:00Z",
		RunID:     "run-test",
		JobID:     "job-101-1234",
		Issue:     101,
		Phase:     "cleanup",
		Status:    "succeeded",
		LogBytes:  99,
		ExitCode:  &exitCode,
		Error:     nil,
	})
	if err != nil {
		t.Fatalf("AppendEvent returned error: %v", err)
	}

	data, err := os.ReadFile(EventsPath(repo, "run-test"))
	if err != nil {
		t.Fatalf("ReadFile events: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("event lines = %d, want 1: %q", len(lines), string(data))
	}
	if strings.Contains(lines[0], " ") {
		t.Fatalf("event JSON line is not compact: %q", lines[0])
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("event JSON invalid: %v", err)
	}
	for _, key := range []string{"ts", "run_id", "job_id", "issue", "phase", "status", "log_bytes", "exit_code", "error"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("event JSON missing key %q: %s", key, lines[0])
		}
	}
	if got["error"] != nil {
		t.Fatalf("error = %#v, want nil", got["error"])
	}
}

func TestEventPromotionCommitFieldsJSONCompatibility(t *testing.T) {
	tests := []struct {
		name              string
		event             Event
		inputJSON         string
		wantMergeCommit   string
		wantPriorStable   string
		wantAbsentOnWrite bool
	}{
		{
			name: "omitempty when empty",
			event: Event{
				Timestamp: "2026-07-03T00:00:00Z",
				RunID:     "run-test",
				JobID:     "promote",
				Phase:     "promote",
				Status:    "promoted",
				LogBytes:  0,
			},
			wantAbsentOnWrite: true,
		},
		{
			name: "populated round trip",
			event: Event{
				Timestamp:         "2026-07-03T00:00:00Z",
				RunID:             "run-test",
				JobID:             "promote",
				Phase:             "promote",
				Status:            "promoted",
				LogBytes:          0,
				MergeCommit:       "merge-sha",
				PriorStableCommit: "prior-sha",
			},
			wantMergeCommit: "merge-sha",
			wantPriorStable: "prior-sha",
		},
		{
			name:      "legacy event without fields",
			inputJSON: `{"ts":"2026-07-03T00:00:00Z","run_id":"run-test","job_id":"promote","issue":0,"phase":"promote","status":"promoted","log_bytes":0}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := []byte(tt.inputJSON)
			if tt.inputJSON == "" {
				var err error
				data, err = json.Marshal(tt.event)
				if err != nil {
					t.Fatalf("Marshal Event returned error: %v", err)
				}
			}
			if tt.wantAbsentOnWrite {
				text := string(data)
				if strings.Contains(text, "merge_commit") || strings.Contains(text, "prior_stable_commit") {
					t.Fatalf("empty promotion commit fields were not omitted: %s", text)
				}
			}

			var got Event
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("Unmarshal Event returned error: %v", err)
			}
			if got.MergeCommit != tt.wantMergeCommit {
				t.Fatalf("MergeCommit = %q, want %q", got.MergeCommit, tt.wantMergeCommit)
			}
			if got.PriorStableCommit != tt.wantPriorStable {
				t.Fatalf("PriorStableCommit = %q, want %q", got.PriorStableCommit, tt.wantPriorStable)
			}
		})
	}
}

func TestLifecycleTransitionValidation(t *testing.T) {
	if !ValidLifecycleTransition(LifecyclePlanned, LifecycleQueued) {
		t.Fatal("planned -> queued should be valid")
	}
	if !ValidLifecycleTransition(LifecycleRunning, LifecycleSucceeded) {
		t.Fatal("running -> succeeded should be valid")
	}
	if ValidLifecycleTransition(LifecycleSucceeded, LifecycleRunning) {
		t.Fatal("succeeded -> running should be invalid")
	}
	if ValidLifecycleTransition(LifecycleQueued, LifecycleQueued) {
		t.Fatal("same-state transition should be invalid")
	}
}

func TestAppendLifecycleTransitionPersistsAndRejectsInvalidTransitions(t *testing.T) {
	repo := t.TempDir()
	runID := "run-test"

	err := AppendLifecycleTransition(repo, runID, LifecycleTransition{
		Timestamp: "2026-07-09T00:00:00Z",
		To:        LifecycleQueued,
		Reason:    "ready set selected",
	})
	if err != nil {
		t.Fatalf("AppendLifecycleTransition planned->queued returned error: %v", err)
	}
	err = AppendLifecycleTransition(repo, runID, LifecycleTransition{
		Timestamp: "2026-07-09T00:00:01Z",
		To:        LifecyclePlanned,
	})
	if err == nil || !strings.Contains(err.Error(), "invalid lifecycle transition") {
		t.Fatalf("AppendLifecycleTransition queued->planned error = %v, want invalid transition", err)
	}

	lifecycle, err := LoadLifecycle(repo, runID)
	if err != nil {
		t.Fatalf("LoadLifecycle returned error: %v", err)
	}
	if lifecycle.State != LifecycleQueued {
		t.Fatalf("lifecycle state = %s, want queued", lifecycle.State)
	}
	if len(lifecycle.History) != 1 {
		t.Fatalf("history length = %d, want 1", len(lifecycle.History))
	}
	got := lifecycle.History[0]
	if got.From != LifecyclePlanned || got.To != LifecycleQueued || got.Event != LifecycleTransitionEvent || got.Source != "explicit" {
		t.Fatalf("transition = %#v, want explicit planned->queued", got)
	}
}

func TestLoadLifecycleDerivesCurrentStateAfterRestart(t *testing.T) {
	repo := t.TempDir()
	runID := "run-restart"
	for _, transition := range []LifecycleTransition{
		{Timestamp: "2026-07-09T00:00:00Z", To: LifecycleQueued},
		{Timestamp: "2026-07-09T00:00:01Z", To: LifecycleRunning},
		{Timestamp: "2026-07-09T00:00:02Z", To: LifecycleSucceeded},
	} {
		if err := AppendLifecycleTransition(repo, runID, transition); err != nil {
			t.Fatalf("AppendLifecycleTransition: %v", err)
		}
	}

	reloaded, err := LoadLifecycle(repo, runID)
	if err != nil {
		t.Fatalf("LoadLifecycle returned error: %v", err)
	}
	if reloaded.State != LifecycleSucceeded {
		t.Fatalf("state = %s, want succeeded", reloaded.State)
	}
	if len(reloaded.History) != 3 {
		t.Fatalf("history length = %d, want 3", len(reloaded.History))
	}
}

func TestLoadLifecycleMapsLegacyEventsConservatively(t *testing.T) {
	repo := t.TempDir()
	runID := "run-legacy"
	lines := []string{
		`{"ts":"2026-07-09T00:00:00Z","run_id":"run-legacy","job_id":"job-1","issue":647,"phase":"worktree_created","status":"running"}`,
		`{"ts":"2026-07-09T00:00:01Z","run_id":"run-legacy","job_id":"job-1","issue":647,"phase":"cleanup","status":"succeeded"}`,
		`{"ts":"2026-07-09T00:00:02Z","run_id":"run-legacy","job_id":"promote","status":"promoted"}`,
	}
	if err := os.MkdirAll(filepath.Dir(EventsPath(repo, runID)), 0o755); err != nil {
		t.Fatalf("MkdirAll events: %v", err)
	}
	if err := os.WriteFile(EventsPath(repo, runID), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile events: %v", err)
	}

	lifecycle, err := LoadLifecycle(repo, runID)
	if err != nil {
		t.Fatalf("LoadLifecycle returned error: %v", err)
	}
	if lifecycle.State != LifecycleSucceeded {
		t.Fatalf("state = %s, want succeeded", lifecycle.State)
	}
	if len(lifecycle.History) != 2 {
		t.Fatalf("history length = %d, want 2: %#v", len(lifecycle.History), lifecycle.History)
	}
	if lifecycle.History[0].From != LifecyclePlanned || lifecycle.History[0].To != LifecycleRunning || lifecycle.History[0].Source != "legacy" {
		t.Fatalf("first legacy transition = %#v, want planned->running", lifecycle.History[0])
	}
	if lifecycle.History[1].From != LifecycleRunning || lifecycle.History[1].To != LifecycleSucceeded {
		t.Fatalf("second legacy transition = %#v, want running->succeeded", lifecycle.History[1])
	}
}

func TestLifecycleRecordsParentChildRunMetadata(t *testing.T) {
	repo := t.TempDir()
	runID := "run-parent"
	if err := AppendLifecycleTransition(repo, runID, LifecycleTransition{
		Timestamp:  "2026-07-09T00:00:00Z",
		To:         LifecycleWaiting,
		ChildRunID: "run-child",
		Reason:     "child dispatched",
	}); err != nil {
		t.Fatalf("AppendLifecycleTransition: %v", err)
	}

	lifecycle, err := LoadLifecycle(repo, runID)
	if err != nil {
		t.Fatalf("LoadLifecycle returned error: %v", err)
	}
	if len(lifecycle.ChildRunIDs) != 1 || lifecycle.ChildRunIDs[0] != "run-child" {
		t.Fatalf("child run ids = %#v, want run-child", lifecycle.ChildRunIDs)
	}
}

func validAttemptReport(issue int, totalTokens int64) reporter.Report {
	return reporter.Report{
		Role:        reporter.RoleWorker,
		Provider:    "codex",
		Model:       "gpt-5.5",
		ModelSource: reporter.ModelSourceParsed,
		Effort:      "xhigh",
		Permission:  reporter.PermissionWrite,
		Action:      "implement issue #" + strconv.Itoa(issue),
		ExitCode:    0,
		StartedAt:   "2026-06-28T00:00:00Z",
		EndedAt:     "2026-06-28T00:00:42Z",
		DurationMS:  42000,
		Usage: reporter.Usage{
			TotalTokens: &totalTokens,
		},
		Verified: true,
	}
}
