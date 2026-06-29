package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/agent"
	"github.com/jasonhnd/loopcoder/internal/attestation"
	"github.com/jasonhnd/loopcoder/internal/state"
	gh "github.com/jasonhnd/loopcoder/internal/vcs/github"
)

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
	if result.Attestation == nil {
		t.Fatal("result missing attestation")
	}
	if err := result.Attestation.Validate(); err != nil {
		t.Fatalf("result attestation does not validate: %v", err)
	}
	canonicalAttestation, err := result.Attestation.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON returned error: %v", err)
	}
	for _, want := range []string{
		result.Attestation.Header(),
		string(canonicalAttestation),
		"```json\n",
		`"role":"worker"`,
		`"provider":"codex"`,
		`"model_source":"parsed"`,
		`"permission":"write"`,
		`"action":"implement issue #101"`,
		`"usage":{"input_tokens":120,"output_tokens":34,"total_tokens":154}`,
	} {
		if !strings.Contains(fakeGitHub.lastPRBody, want) {
			t.Fatalf("PR body missing %q:\n%s", want, fakeGitHub.lastPRBody)
		}
	}
	if strings.Contains(fakeGitHub.lastPRBody, "worker: codex") {
		t.Fatalf("PR body still contains bare worker line:\n%s", fakeGitHub.lastPRBody)
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
	attestationField, ok := jsonFields["attestation"]
	if !ok {
		t.Fatalf("success JSON missing attestation field: %s", string(data))
	}
	attestationBytes, err := json.Marshal(attestationField)
	if err != nil {
		t.Fatalf("marshal attestation field: %v", err)
	}
	var renderedAttestation attestation.AttestationRecord
	if err := json.Unmarshal(attestationBytes, &renderedAttestation); err != nil {
		t.Fatalf("attestation field invalid: %v", err)
	}
	if err := renderedAttestation.Validate(); err != nil {
		t.Fatalf("attestation field does not validate: %v", err)
	}
	if renderedAttestation.Header() != result.Attestation.Header() {
		t.Fatalf("attestation JSON header = %q, want %q", renderedAttestation.Header(), result.Attestation.Header())
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
	eventCount, err := state.CountEvents(repo, "run-test")
	if err != nil {
		t.Fatalf("CountEvents returned error: %v", err)
	}
	if eventCount != 9 {
		t.Fatalf("event count = %d, want 9", eventCount)
	}
	if fakeAgent.invocation.WorktreePath == "" || fakeAgent.invocation.Prompt == "" || fakeAgent.invocation.LogPath == "" {
		t.Fatalf("agent invocation missing required fields: %#v", fakeAgent.invocation)
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
}

func TestDispatchHardFailsInvalidAttestationBeforeDelivery(t *testing.T) {
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
		t.Fatal("Dispatch returned nil error, want attestation failure")
	}
	for _, want := range []string{"validate worker attestation", "model is required", "usage requires total_tokens or both input_tokens and output_tokens"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error missing %q: %v", want, err)
		}
	}
	if fakeGit.addAllCalls != 0 || fakeGit.commitCalls != 0 || fakeGit.pushCalls != 0 {
		t.Fatalf("delivery git calls after attestation failure: add=%d commit=%d push=%d", fakeGit.addAllCalls, fakeGit.commitCalls, fakeGit.pushCalls)
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
		Usage: attestation.Usage{
			InputTokens:  &inputTokens,
			OutputTokens: &outputTokens,
			TotalTokens:  &totalTokens,
		},
		StartedAt:  "2026-06-28T00:00:00Z",
		EndedAt:    "2026-06-28T00:00:42Z",
		DurationMS: 42000,
	}
}

type workerFakeGit struct {
	status      string
	err         error
	addAllCalls int
	commitCalls int
	pushCalls   int
}

func (f *workerFakeGit) FetchOriginBase(context.Context, string, string) error {
	return f.err
}

func (f *workerFakeGit) WorktreeAdd(_ context.Context, _ string, _ string, worktreePath string, _ string) error {
	if f.err != nil {
		return f.err
	}
	return os.MkdirAll(worktreePath, 0o755)
}

func (f *workerFakeGit) WorktreeRemove(context.Context, string, string) error {
	return nil
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

func (f *workerFakeGit) Commit(context.Context, string, string) error {
	f.commitCalls++
	return f.err
}

func (f *workerFakeGit) PushUpstream(context.Context, string, string) error {
	f.pushCalls++
	return f.err
}

func (f *workerFakeGit) BranchDelete(context.Context, string, string) error {
	return nil
}

type workerFakeGitHub struct {
	prURL         string
	prs           []gh.PullRequestReference
	err           error
	createPRCalls int
	lastPRBody    string
}

func (f *workerFakeGitHub) RepoName(context.Context) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return "owner/repo", nil
}

func (f *workerFakeGitHub) CreatePR(_ context.Context, _, _, _, body string) (string, error) {
	f.createPRCalls++
	if f.err != nil {
		return "", f.err
	}
	f.lastPRBody = body
	if f.prURL == "" {
		return "https://github.com/owner/repo/pull/1", nil
	}
	return f.prURL, nil
}

func (f *workerFakeGitHub) ListHeadPRs(context.Context, string) ([]gh.PullRequestReference, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.prs, nil
}

type workerFakeAgent struct {
	invocation agent.Invocation
	result     agent.Result
	resultSet  bool
	summary    string
	log        string
	exitCode   int
	err        error
}

func (f *workerFakeAgent) Run(_ context.Context, invocation agent.Invocation) (agent.Result, error) {
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

type workerFakeLock struct {
	err error
}

func (l *workerFakeLock) Release() error {
	if l.err != nil {
		return l.err
	}
	return nil
}
