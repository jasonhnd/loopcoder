package loopreview

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/agent"
	"github.com/jasonhnd/loopcoder/internal/attestation"
	gh "github.com/jasonhnd/loopcoder/internal/vcs/github"
)

func TestParseVerdictAcceptsStructuredVerdicts(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		verdict  string
		exitCode int
	}{
		{
			name:     "pass",
			raw:      `{"verdict":"pass","findings":[],"evidence":"diff and spec match","spec_conformance":"pass"}`,
			verdict:  VerdictPass,
			exitCode: 0,
		},
		{
			name:     "fail",
			raw:      `{"verdict":"fail","findings":[{"severity":"error","file":"main.go","note":"missing required behavior"}],"evidence":"acceptance criterion not met","spec_conformance":"fail"}`,
			verdict:  VerdictFail,
			exitCode: 1,
		},
		{
			name:     "needs human",
			raw:      `{"verdict":"needs-human","findings":[{"severity":"warning","file":"","note":"ambiguous acceptance criteria"}],"evidence":"cannot decide safely","spec_conformance":"not-applicable"}`,
			verdict:  VerdictNeedsHuman,
			exitCode: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseVerdict(tt.raw)
			if err != nil {
				t.Fatalf("ParseVerdict returned error: %v", err)
			}
			if got.Verdict != tt.verdict {
				t.Fatalf("Verdict = %q, want %q", got.Verdict, tt.verdict)
			}
			if ExitCodeForVerdict(got.Verdict) != tt.exitCode {
				t.Fatalf("ExitCodeForVerdict = %d, want %d", ExitCodeForVerdict(got.Verdict), tt.exitCode)
			}
		})
	}
}

func TestParseVerdictRejectsInvalidJSONOrSchema(t *testing.T) {
	tests := []string{
		`not json`,
		`{"verdict":"ok","findings":[],"evidence":"x","spec_conformance":"pass"}`,
		`{"verdict":"pass","findings":[],"evidence":"","spec_conformance":"pass"}`,
		`{"verdict":"pass","evidence":"x","spec_conformance":"pass"}`,
		`{"verdict":"pass","findings":[{"severity":"","file":"","note":"x"}],"evidence":"x","spec_conformance":"pass"}`,
	}
	for _, raw := range tests {
		t.Run(raw, func(t *testing.T) {
			if _, err := ParseVerdict(raw); err == nil {
				t.Fatalf("ParseVerdict(%q) returned nil error", raw)
			}
		})
	}
}

func TestDiscoverSpecPathPrefersSpecsInNewLayout(t *testing.T) {
	tests := []struct {
		name  string
		texts []string
		want  string
	}{
		{
			name:  "spec only",
			texts: []string{"Implement per docs/specs/0165-documentation-layout.md."},
			want:  "docs/specs/0165-documentation-layout.md",
		},
		{
			name: "reference before spec",
			texts: []string{
				"Read docs/reference/architecture.md for current behavior.",
				"Implement per docs/specs/0165-documentation-layout.md.",
			},
			want: "docs/specs/0165-documentation-layout.md",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := discoverSpecPath(tt.texts...); got != tt.want {
				t.Fatalf("discoverSpecPath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRunInvokesReadOnlyVerifierAndReturnsPass(t *testing.T) {
	repo := t.TempDir()
	scratchRoot := t.TempDir()
	fakeGit := &loopreviewFakeGit{
		show: map[string]string{
			"origin/main:docs/specs/design.md": "# Design\n\nAcceptance criteria here.\n",
		},
	}
	fakeGitHub := &loopreviewFakeGitHub{
		pr: gh.PullRequest{
			Number:      152,
			Title:       "Add loopreview",
			HeadRefName: "loop/issue-152",
			ClosingIssuesReferences: []gh.IssueReference{{
				Number: 152,
			}},
		},
		issue: gh.Issue{
			Number: 152,
			Title:  "Add loopreview command",
			Body:   "Implement per docs/specs/design.md with acceptance criteria.",
		},
		diff:  "diff --git a/internal/loopreview/loopreview.go b/internal/loopreview/loopreview.go\n",
		files: []string{"internal/loopreview/loopreview.go"},
	}
	fakeAgent := &loopreviewFakeAgent{
		summary: `{"verdict":"pass","findings":[],"evidence":"diff satisfies issue and spec","spec_conformance":"pass"}`,
	}
	fakeLock := &loopreviewFakeLock{}

	result, err := Run(context.Background(), Options{
		RepoPath:   repo,
		PRNumber:   152,
		Provider:   "claude",
		BaseBranch: "main",
	}, Deps{
		Git: fakeGit,
		GitHub: func(path string) GitHubClient {
			if path != repo {
				t.Fatalf("GitHub repo path = %q, want %q", path, repo)
			}
			return fakeGitHub
		},
		AgentLookup: func(provider string) (agent.Runner, error) {
			if provider != "claude" {
				t.Fatalf("provider = %q, want claude", provider)
			}
			return fakeAgent, nil
		},
		AcquireLock: func(path string, timeout time.Duration) (Lock, error) {
			if path != repo {
				t.Fatalf("lock repo path = %q, want %q", path, repo)
			}
			if timeout != 60*time.Second {
				t.Fatalf("lock timeout = %s, want 60s", timeout)
			}
			return fakeLock, nil
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
		RemoveAll: os.RemoveAll,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Verdict.Verdict != VerdictPass || result.ExitCode != 0 {
		t.Fatalf("result = %#v, want pass exit 0", result)
	}
	if result.Verdict.Attestation == nil {
		t.Fatal("verdict missing attestation")
	}
	if fakeGit.fetchBase != "main" || fakeGit.fetchPR != 152 || fakeGit.addRev != "FETCH_HEAD" {
		t.Fatalf("git checkout calls not recorded correctly: %#v", fakeGit)
	}
	if !fakeGit.removed {
		t.Fatal("worktree was not removed")
	}
	if !fakeLock.released {
		t.Fatal("worktree-add lock was not released")
	}
	inv := fakeAgent.invocation
	if !inv.ReadOnly {
		t.Fatal("agent invocation ReadOnly = false, want true")
	}
	if inv.OutputSchema != VerdictJSONSchema {
		t.Fatal("agent invocation did not receive verdict schema")
	}
	if inv.WorktreePath == "" || inv.LogPath == "" {
		t.Fatalf("agent invocation missing paths: %#v", inv)
	}
	for _, want := range []string{
		"independent loopcoder Verifier",
		"internal/loopreview/loopreview.go",
		"diff --git",
		"Add loopreview command",
		"Acceptance criteria here",
	} {
		if !strings.Contains(inv.Prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, inv.Prompt)
		}
	}
	if _, err := os.Stat(filepath.Dir(inv.WorktreePath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("scratch directory still exists or stat failed unexpectedly: %v", err)
	}
}

func TestRunVerifierAttestation(t *testing.T) {
	validSummary := `{"verdict":"pass","findings":[],"evidence":"diff satisfies issue and spec","spec_conformance":"pass"}`
	tests := []struct {
		name        string
		agent       agent.Result
		wantVerdict string
		wantNote    string
	}{
		{
			name:        "valid attestation stays pass",
			agent:       validLoopreviewAgentResult(validSummary, 0),
			wantVerdict: VerdictPass,
		},
		{
			name: "incomplete attestation forces needs human",
			agent: agent.Result{
				ExitCode:   0,
				Summary:    validSummary,
				Effort:     "high",
				StartedAt:  "2026-06-28T00:00:00Z",
				EndedAt:    "2026-06-28T00:00:02Z",
				DurationMS: 2000,
				Usage: attestation.Usage{
					TotalTokens: int64Ptr(123),
				},
			},
			wantVerdict: VerdictNeedsHuman,
			wantNote:    "incomplete verifier attestation",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr strings.Builder
			result := runWithAgentResult(t, tt.agent, nil, &stderr)
			if result.Verdict.Verdict != tt.wantVerdict {
				t.Fatalf("verdict = %q, want %q", result.Verdict.Verdict, tt.wantVerdict)
			}
			if result.ExitCode != ExitCodeForVerdict(tt.wantVerdict) {
				t.Fatalf("exit code = %d, want %d", result.ExitCode, ExitCodeForVerdict(tt.wantVerdict))
			}
			if result.Verdict.Attestation == nil {
				t.Fatal("verdict missing attestation")
			}

			record := result.Verdict.Attestation
			if record.Role != attestation.RoleVerifier || record.Provider != "codex" || record.ModelSource != attestation.ModelSourceParsed || record.Permission != attestation.PermissionReadOnly || !record.Verified {
				t.Fatalf("attestation identity fields = %#v", record)
			}
			if record.Action != "review PR #152" || record.ExitCode != tt.agent.ExitCode {
				t.Fatalf("attestation action/exit = (%q, %d), want review PR #152/%d", record.Action, record.ExitCode, tt.agent.ExitCode)
			}
			if !strings.Contains(stderr.String(), record.Header()) {
				t.Fatalf("stderr missing attestation header %q:\n%s", record.Header(), stderr.String())
			}

			var rendered strings.Builder
			if err := Render(&rendered, result); err != nil {
				t.Fatalf("Render returned error: %v", err)
			}
			var payload map[string]any
			if err := json.Unmarshal([]byte(rendered.String()), &payload); err != nil {
				t.Fatalf("rendered verdict is not JSON: %v\n%s", err, rendered.String())
			}
			if _, ok := payload["attestation"]; !ok {
				t.Fatalf("rendered verdict missing attestation: %s", rendered.String())
			}

			if tt.wantNote != "" {
				found := false
				for _, finding := range result.Verdict.Findings {
					if strings.Contains(finding.Note, tt.wantNote) && strings.Contains(finding.Note, "model is required") {
						found = true
					}
				}
				if !found {
					t.Fatalf("findings missing incomplete-attestation note: %#v", result.Verdict.Findings)
				}
			} else if err := record.Validate(); err != nil {
				t.Fatalf("valid attestation did not validate: %v", err)
			}
		})
	}
}

func TestRunSurfacesFailVerdict(t *testing.T) {
	result := runWithAgentSummary(t, `{"verdict":"fail","findings":[{"severity":"error","file":"file.go","note":"bug"}],"evidence":"bug in diff","spec_conformance":"fail"}`, nil)
	if result.Verdict.Verdict != VerdictFail || result.ExitCode != 1 {
		t.Fatalf("result = %#v, want fail exit 1", result)
	}
}

func TestRunParseFailureReturnsNeedsHuman(t *testing.T) {
	result := runWithAgentSummary(t, "not-json", nil)
	if result.Verdict.Verdict != VerdictNeedsHuman || result.ExitCode != 2 {
		t.Fatalf("result = %#v, want needs-human exit 2", result)
	}
	if !strings.Contains(result.Verdict.Findings[0].Note, "structured verdict parse failed") {
		t.Fatalf("finding note = %q", result.Verdict.Findings[0].Note)
	}
}

func TestRunUnreadableSpecForcesNeedsHuman(t *testing.T) {
	result := runWithAgentSummary(t, `{"verdict":"pass","findings":[],"evidence":"looks good","spec_conformance":"pass"}`, errors.New("missing spec"))
	if result.Verdict.Verdict != VerdictNeedsHuman || result.ExitCode != 2 {
		t.Fatalf("result = %#v, want needs-human exit 2", result)
	}
	if result.Verdict.SpecConformance != SpecConformanceNotApplicable {
		t.Fatalf("SpecConformance = %q, want not-applicable", result.Verdict.SpecConformance)
	}
	if len(result.Verdict.Findings) == 0 || !strings.Contains(result.Verdict.Findings[len(result.Verdict.Findings)-1].Note, "merged design/spec unavailable") {
		t.Fatalf("findings missing spec-unavailable note: %#v", result.Verdict.Findings)
	}
}

func TestRunVerifierTimeoutReturnsNeedsHuman(t *testing.T) {
	repo := t.TempDir()
	scratchRoot := t.TempDir()
	fakeGit := &loopreviewFakeGit{
		show: map[string]string{
			"origin/main:docs/specs/design.md": "# Design\n",
		},
	}
	fakeGitHub := &loopreviewFakeGitHub{
		pr: gh.PullRequest{
			Number:      152,
			Title:       "PR",
			HeadRefName: "loop/issue-152",
			ClosingIssuesReferences: []gh.IssueReference{{
				Number: 152,
			}},
		},
		issue: gh.Issue{
			Number: 152,
			Title:  "Issue",
			Body:   "See docs/specs/design.md.",
		},
		diff:  "diff",
		files: []string{"file.go"},
	}
	fakeAgent := &loopreviewFakeAgent{blockUntilCanceled: true}

	result, err := Run(context.Background(), Options{
		RepoPath:   repo,
		PRNumber:   152,
		Provider:   "claude",
		BaseBranch: "main",
		Timeout:    10 * time.Millisecond,
	}, Deps{
		Git: fakeGit,
		GitHub: func(string) GitHubClient {
			return fakeGitHub
		},
		AgentLookup: func(string) (agent.Runner, error) {
			return fakeAgent, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &loopreviewFakeLock{}, nil
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
		RemoveAll: os.RemoveAll,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Verdict.Verdict != VerdictNeedsHuman || result.ExitCode != 2 {
		t.Fatalf("result = %#v, want needs-human exit 2", result)
	}
	if !strings.Contains(result.Verdict.Evidence, "claude verifier timed out after 10ms") {
		t.Fatalf("evidence = %q", result.Verdict.Evidence)
	}
	if fakeAgent.ctxErr != context.DeadlineExceeded {
		t.Fatalf("agent ctxErr = %v, want context deadline exceeded", fakeAgent.ctxErr)
	}
}

func runWithAgentSummary(t *testing.T, summary string, showErr error) Result {
	t.Helper()
	return runWithAgentResult(t, validLoopreviewAgentResult(summary, 0), showErr, nil)
}

func runWithAgentResult(t *testing.T, agentResult agent.Result, showErr error, stderr *strings.Builder) Result {
	t.Helper()
	repo := t.TempDir()
	scratchRoot := t.TempDir()
	fakeGit := &loopreviewFakeGit{
		show: map[string]string{
			"origin/main:docs/specs/design.md": "# Design\n",
		},
		showErr: showErr,
	}
	fakeGitHub := &loopreviewFakeGitHub{
		pr: gh.PullRequest{
			Number:      152,
			Title:       "PR",
			HeadRefName: "loop/issue-152",
			ClosingIssuesReferences: []gh.IssueReference{{
				Number: 152,
			}},
		},
		issue: gh.Issue{
			Number: 152,
			Title:  "Issue",
			Body:   "See docs/specs/design.md.",
		},
		diff:  "diff",
		files: []string{"file.go"},
	}
	fakeAgent := &loopreviewFakeAgent{result: &agentResult}

	var warnings strings.Builder
	if stderr == nil {
		stderr = &warnings
	}

	result, err := Run(context.Background(), Options{
		RepoPath:   repo,
		PRNumber:   152,
		Provider:   "codex",
		BaseBranch: "main",
		Stderr:     stderr,
	}, Deps{
		Git: fakeGit,
		GitHub: func(string) GitHubClient {
			return fakeGitHub
		},
		AgentLookup: func(string) (agent.Runner, error) {
			return fakeAgent, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &loopreviewFakeLock{}, nil
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
		RemoveAll: os.RemoveAll,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	return result
}

func validLoopreviewAgentResult(summary string, exitCode int) agent.Result {
	return agent.Result{
		ExitCode:   exitCode,
		Summary:    summary,
		Model:      "gpt-5",
		Effort:     "high",
		StartedAt:  "2026-06-28T00:00:00Z",
		EndedAt:    "2026-06-28T00:00:02Z",
		DurationMS: 2000,
		Usage: attestation.Usage{
			InputTokens:  int64Ptr(12),
			OutputTokens: int64Ptr(34),
			TotalTokens:  int64Ptr(46),
		},
	}
}

func int64Ptr(value int64) *int64 {
	return &value
}

type loopreviewFakeGit struct {
	fetchBase string
	fetchPR   int
	addRev    string
	removed   bool
	show      map[string]string
	showErr   error
}

func (f *loopreviewFakeGit) FetchOriginBase(_ context.Context, _ string, baseBranch string) error {
	f.fetchBase = baseBranch
	return nil
}

func (f *loopreviewFakeGit) FetchPRHead(_ context.Context, _ string, prNumber int) error {
	f.fetchPR = prNumber
	return nil
}

func (f *loopreviewFakeGit) WorktreeAddDetachedAt(_ context.Context, _ string, worktreePath, rev string) error {
	f.addRev = rev
	return os.MkdirAll(worktreePath, 0o755)
}

func (f *loopreviewFakeGit) WorktreeRemove(context.Context, string, string) error {
	f.removed = true
	return nil
}

func (f *loopreviewFakeGit) Show(_ context.Context, _ string, revPath string) (string, error) {
	if f.showErr != nil {
		return "", f.showErr
	}
	return f.show[revPath], nil
}

type loopreviewFakeGitHub struct {
	pr    gh.PullRequest
	issue gh.Issue
	diff  string
	files []string
}

func (f *loopreviewFakeGitHub) ViewPR(context.Context, int) (gh.PullRequest, error) {
	return f.pr, nil
}

func (f *loopreviewFakeGitHub) ViewIssue(context.Context, int) (gh.Issue, error) {
	return f.issue, nil
}

func (f *loopreviewFakeGitHub) PRDiff(context.Context, int) (string, error) {
	return f.diff, nil
}

func (f *loopreviewFakeGitHub) PRDiffNameOnly(context.Context, int) ([]string, error) {
	return f.files, nil
}

type loopreviewFakeAgent struct {
	invocation         agent.Invocation
	result             *agent.Result
	summary            string
	exitCode           int
	err                error
	blockUntilCanceled bool
	ctxErr             error
}

func (f *loopreviewFakeAgent) Run(ctx context.Context, invocation agent.Invocation) (agent.Result, error) {
	f.invocation = invocation
	if err := os.WriteFile(invocation.LogPath, []byte("verifier log\n"), 0o644); err != nil {
		return agent.Result{ExitCode: -1}, err
	}
	if f.blockUntilCanceled {
		<-ctx.Done()
		f.ctxErr = ctx.Err()
		return agent.Result{ExitCode: -1}, f.ctxErr
	}
	if f.err != nil {
		return agent.Result{ExitCode: -1}, f.err
	}
	if f.result != nil {
		return *f.result, nil
	}
	return validLoopreviewAgentResult(f.summary, f.exitCode), nil
}

type loopreviewFakeLock struct {
	released bool
}

func (l *loopreviewFakeLock) Release() error {
	l.released = true
	return nil
}
