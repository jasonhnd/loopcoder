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

func TestVerdictJSONSchemaIsValidJSON(t *testing.T) {
	if !json.Valid([]byte(VerdictJSONSchema)) {
		t.Fatalf("VerdictJSONSchema is not valid JSON: %s", VerdictJSONSchema)
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

func TestLoadIssueResolvesFallbackFromLoopIssueBranch(t *testing.T) {
	fakeGitHub := &loopreviewFakeGitHub{
		issues: map[int]gh.Issue{
			101: {Number: 101, Title: "Issue 101", Body: "Implement per docs/specs/design.md."},
		},
	}

	issue, present := loadIssue(context.Background(), fakeGitHub, gh.PullRequest{
		Title:       "Worker PR",
		Body:        "Worker-authored PR body without a spec path.",
		HeadRefName: "loop/issue-101",
	})

	if !present {
		t.Fatal("loadIssue did not resolve an issue")
	}
	if issue.Number != 101 {
		t.Fatalf("issue number = %d, want 101", issue.Number)
	}
	assertViewedIssues(t, fakeGitHub, 101)
}

func TestLoadIssueResolvesFallbackFromPRBodyReference(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "closing keyword", body: "Worker-authored PR body.\n\nCloses #101"},
		{name: "bare reference", body: "Worker-authored PR body for #101."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeGitHub := &loopreviewFakeGitHub{
				issues: map[int]gh.Issue{
					101: {Number: 101, Title: "Issue 101", Body: "Implement per docs/specs/design.md."},
				},
			}

			issue, present := loadIssue(context.Background(), fakeGitHub, gh.PullRequest{
				Title:       "Worker PR",
				Body:        tt.body,
				HeadRefName: "worker/feature",
			})

			if !present {
				t.Fatal("loadIssue did not resolve an issue")
			}
			if issue.Number != 101 {
				t.Fatalf("issue number = %d, want 101", issue.Number)
			}
			assertViewedIssues(t, fakeGitHub, 101)
		})
	}
}

func TestLoadIssueKeepsClosingIssueReferencesPreferred(t *testing.T) {
	fakeGitHub := &loopreviewFakeGitHub{
		issues: map[int]gh.Issue{
			101: {Number: 101, Title: "Branch issue", Body: "Branch body."},
			202: {Number: 202, Title: "Closing issue", Body: "Implement per docs/specs/design.md."},
			303: {Number: 303, Title: "Body issue", Body: "Body reference."},
		},
	}

	issue, present := loadIssue(context.Background(), fakeGitHub, gh.PullRequest{
		Title:       "Worker PR",
		Body:        "Closes #303",
		HeadRefName: "loop/issue-101",
		ClosingIssuesReferences: []gh.IssueReference{{
			Number: 202,
		}},
	})

	if !present {
		t.Fatal("loadIssue did not resolve an issue")
	}
	if issue.Number != 202 {
		t.Fatalf("issue number = %d, want 202", issue.Number)
	}
	assertViewedIssues(t, fakeGitHub, 202)
}

func TestLoadIssueKeepsNoReferenceFallback(t *testing.T) {
	fakeGitHub := &loopreviewFakeGitHub{}
	pr := gh.PullRequest{
		Title:       "Worker PR",
		Body:        "Worker-authored PR body without a spec path.",
		HeadRefName: "worker/feature",
	}

	issue, present := loadIssue(context.Background(), fakeGitHub, pr)

	if present {
		t.Fatal("loadIssue unexpectedly resolved an issue")
	}
	if issue.Title != pr.Title || issue.Body != pr.Body {
		t.Fatalf("fallback issue = %#v, want PR title/body", issue)
	}
	assertViewedIssues(t, fakeGitHub)
}

func TestBuildPromptUsesBoundedReviewPacketContract(t *testing.T) {
	prompt, _ := buildPromptWithLimits(loopreviewPromptTestOptions(), loopreviewPromptTestInputs(), ReviewPacketLimits{})
	for _, want := range []string{
		"# Bounded review packet",
		"Use the bounded review packet below as the primary evidence.",
		"Return \"needs-human\" if a TRUNCATED marker could hide",
		"bounded input/tool budget",
		"Total changed files: 1",
		"Per-file diff budget:",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestBuildPromptMarksDocFirstSpecAbsenceExpected(t *testing.T) {
	specPath := "docs/specs/0220-loopreview-new-spec-not-a-blocker.md"
	inputs := loopreviewPromptTestInputs()
	inputs.Issue.Body = "Add the design in " + specPath + "."
	inputs.ChangedFiles = []string{specPath}
	inputs.Diff = loopreviewNewFileDiff(specPath, "+# Spec\n+\n+Acceptance criteria.\n")
	inputs.Spec = classifySpecAbsence(specInput{
		Path:      specPath,
		Available: false,
		Reason:    "path does not exist in origin/main",
	}, "main", inputs.ChangedFiles, inputs.Diff)

	prompt, packet := buildPromptWithLimits(loopreviewPromptTestOptions(), inputs, ReviewPacketLimits{})
	if !packet.SpecExpectedAbsent {
		t.Fatal("packet did not mark expected spec absence")
	}
	for _, want := range []string{
		"Documentation-only: yes",
		"Referenced spec changed in PR: yes",
		"Referenced spec added in PR: yes",
		"Status: expected absent from origin/main",
		"Classification: expected/non-blocking",
		"expected: this PR introduces the spec, so it is absent from origin/main",
		"do not return \"needs-human\" solely for that expected absence",
		"Spec conformance: not-applicable",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestBuildReviewPacketTruncatesChangedFilesBudget(t *testing.T) {
	inputs := loopreviewPromptTestInputs()
	inputs.ChangedFiles = []string{
		"internal/loopreview/a.go",
		"internal/loopreview/b.go",
		"internal/loopreview/c.go",
		"internal/loopreview/d.go",
	}

	prompt, packet := buildPromptWithLimits(loopreviewPromptTestOptions(), inputs, ReviewPacketLimits{
		ChangedFilesBytes: len("- internal/loopreview/a.go\n") + 1,
	})
	if !packet.ChangedFiles.Truncated {
		t.Fatal("changed files were not truncated")
	}
	for _, want := range []string{
		"Total changed files: 4",
		"[TRUNCATED changed files: omitted 3 files",
		"omitted",
		"bytes",
		"lines",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "internal/loopreview/d.go") {
		t.Fatalf("prompt contains omitted changed file:\n%s", prompt)
	}
}

func TestBuildReviewPacketTruncatesPerFileDiffBudget(t *testing.T) {
	inputs := loopreviewPromptTestInputs()
	inputs.Diff = loopreviewDiffPatch("internal/loopreview/big.go", "kept-line\n"+strings.Repeat("+ omitted body\n", 40)+"TAIL_DIFF_SHOULD_NOT_APPEAR\n")

	prompt, packet := buildPromptWithLimits(loopreviewPromptTestOptions(), inputs, ReviewPacketLimits{
		DiffFileBytes: 90,
	})
	if !packet.Diff.Truncated {
		t.Fatal("diff was not truncated")
	}
	for _, want := range []string{
		"[TRUNCATED diff for internal/loopreview/big.go: omitted",
		"[TRUNCATED diff: omitted",
		"bytes",
		"lines",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "TAIL_DIFF_SHOULD_NOT_APPEAR") {
		t.Fatalf("prompt contains omitted diff tail:\n%s", prompt)
	}
}

func TestBuildReviewPacketTruncatesTotalDiffBudget(t *testing.T) {
	inputs := loopreviewPromptTestInputs()
	inputs.Diff = loopreviewDiffPatch("internal/loopreview/a.go", "+ kept\n") +
		loopreviewDiffPatch("internal/loopreview/b.go", strings.Repeat("+ omitted b body\n", 40)+"TAIL_TOTAL_DIFF_B_SHOULD_NOT_APPEAR\n") +
		loopreviewDiffPatch("internal/loopreview/c.go", strings.Repeat("+ omitted c body\n", 40)+"TAIL_TOTAL_DIFF_C_SHOULD_NOT_APPEAR\n")

	prompt, packet := buildPromptWithLimits(loopreviewPromptTestOptions(), inputs, ReviewPacketLimits{
		DiffBytes:     180,
		DiffFileBytes: 4096,
	})
	if !packet.Diff.Truncated {
		t.Fatal("diff was not truncated")
	}
	if !strings.Contains(prompt, "internal/loopreview/a.go") {
		t.Fatalf("prompt missing first diff patch:\n%s", prompt)
	}
	for _, want := range []string{
		"[TRUNCATED diff: omitted",
		"omitted files: internal/loopreview/b.go, internal/loopreview/c.go",
		"bytes",
		"lines",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	for _, omittedPatchHeader := range []string{
		"## internal/loopreview/b.go",
		"## internal/loopreview/c.go",
	} {
		if strings.Contains(prompt, omittedPatchHeader) {
			t.Fatalf("prompt contains omitted diff patch header %q:\n%s", omittedPatchHeader, prompt)
		}
	}
	if strings.Contains(prompt, "TAIL_TOTAL_DIFF_B_SHOULD_NOT_APPEAR") || strings.Contains(prompt, "TAIL_TOTAL_DIFF_C_SHOULD_NOT_APPEAR") {
		t.Fatalf("prompt contains omitted total diff tail:\n%s", prompt)
	}
}

func TestBuildReviewPacketOrdersSourceBeforeGeneratedDiffs(t *testing.T) {
	tests := []struct {
		name                    string
		generatedPath           string
		generatedAttributeRules []generatedAttributeRule
	}{
		{
			name:          "default generated glob",
			generatedPath: "tests/baseline/large.jsonl",
		},
		{
			name:                    "gitattributes generated marker",
			generatedPath:           "snapshots/large.txt",
			generatedAttributeRules: parseGeneratedAttributeRules("snapshots/** linguist-generated=true\n"),
		},
		{
			name:                    "gitattributes diff disabled marker",
			generatedPath:           "golden/large.txt",
			generatedAttributeRules: parseGeneratedAttributeRules("golden/** linguist-diff=false\n"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inputs := loopreviewPromptTestInputs()
			inputs.GeneratedAttributeRules = tt.generatedAttributeRules
			inputs.ChangedFiles = []string{tt.generatedPath, "internal/foo.go"}
			inputs.Diff = loopreviewDiffPatch(tt.generatedPath, "+ generated header\n"+strings.Repeat("+ generated data row\n", 80)+"+ GENERATED_TAIL_SHOULD_NOT_APPEAR\n") +
				loopreviewDiffPatch("internal/foo.go", "+ package foo\n+ const SourceLastLine = \"SOURCE_LAST_LINE\"\n")

			prompt, packet := buildPromptWithLimits(loopreviewPromptTestOptions(), inputs, ReviewPacketLimits{
				DiffBytes:              700,
				DiffFileBytes:          4096,
				GeneratedDiffFileBytes: 120,
			})

			if !packet.Diff.Truncated {
				t.Fatal("diff was not truncated")
			}
			sourceIndex := strings.Index(prompt, "## internal/foo.go")
			generatedIndex := strings.Index(prompt, "## "+tt.generatedPath)
			if sourceIndex < 0 {
				t.Fatalf("prompt missing source diff:\n%s", prompt)
			}
			if generatedIndex < 0 {
				t.Fatalf("prompt missing generated diff note:\n%s", prompt)
			}
			if generatedIndex < sourceIndex {
				t.Fatalf("generated diff appeared before source diff:\n%s", prompt)
			}
			for _, want := range []string{
				"SOURCE_LAST_LINE",
				"[TRUNCATED diff for " + tt.generatedPath + ": omitted",
				"[TRUNCATED diff: omitted",
			} {
				if !strings.Contains(prompt, want) {
					t.Fatalf("prompt missing %q:\n%s", want, prompt)
				}
			}
			for _, unwanted := range []string{
				"[TRUNCATED diff for internal/foo.go",
				"GENERATED_TAIL_SHOULD_NOT_APPEAR",
				"omitted files: internal/foo.go",
			} {
				if strings.Contains(prompt, unwanted) {
					t.Fatalf("prompt contains %q:\n%s", unwanted, prompt)
				}
			}
		})
	}
}

func TestBuildReviewPacketDoesNotClassifyLargeSourceBySizeOnly(t *testing.T) {
	inputs := loopreviewPromptTestInputs()
	inputs.ChangedFiles = []string{"internal/large.go", "internal/foo.go"}
	inputs.Diff = loopreviewDiffPatch("internal/large.go", "+ "+strings.Repeat("source ", 60)+"LEGIT_SOURCE_DEEP_LINE\n") +
		loopreviewDiffPatch("internal/foo.go", "+ package foo\n")

	prompt, packet := buildPromptWithLimits(loopreviewPromptTestOptions(), inputs, ReviewPacketLimits{
		DiffBytes:              1200,
		DiffFileBytes:          900,
		GeneratedDiffFileBytes: 80,
		GeneratedSizeBytes:     100,
	})

	if packet.Diff.Truncated {
		t.Fatalf("large source diff was truncated as generated:\n%s", prompt)
	}
	if !strings.Contains(prompt, "LEGIT_SOURCE_DEEP_LINE") {
		t.Fatalf("prompt missing large source tail:\n%s", prompt)
	}
	if strings.Contains(prompt, "[TRUNCATED diff for internal/large.go") {
		t.Fatalf("large source diff was classified as generated:\n%s", prompt)
	}
}

func TestGatherInputsParsesGeneratedAttributes(t *testing.T) {
	fakeGit := &loopreviewFakeGit{
		show: map[string]string{
			"origin/main:.gitattributes":       "snapshots/** linguist-generated=true\ngolden/** linguist-diff=false\nmanual/** -linguist-generated\n",
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
		diff:  loopreviewDiffPatch("snapshots/out.txt", "+ snapshot\n"),
		files: []string{"snapshots/out.txt"},
	}

	inputs, err := gatherInputs(context.Background(), Deps{Git: fakeGit}, fakeGitHub, t.TempDir(), Options{
		PRNumber:   152,
		BaseBranch: "main",
	})
	if err != nil {
		t.Fatalf("gatherInputs returned error: %v", err)
	}
	if !generatedByAttributes("snapshots/out.txt", inputs.GeneratedAttributeRules) {
		t.Fatalf("snapshots/out.txt was not classified from .gitattributes: %#v", inputs.GeneratedAttributeRules)
	}
	if !generatedByAttributes("golden/out.txt", inputs.GeneratedAttributeRules) {
		t.Fatalf("golden/out.txt was not classified from linguist-diff=false: %#v", inputs.GeneratedAttributeRules)
	}
	if generatedByAttributes("manual/out.txt", inputs.GeneratedAttributeRules) {
		t.Fatalf("manual/out.txt was classified despite an explicit unset: %#v", inputs.GeneratedAttributeRules)
	}
}

func TestBuildReviewPacketTruncatesIssueBudget(t *testing.T) {
	inputs := loopreviewPromptTestInputs()
	inputs.Issue.Body = "keep issue context\n" + strings.Repeat("omitted issue context\n", 40) + "TAIL_ISSUE_SHOULD_NOT_APPEAR\n"

	prompt, packet := buildPromptWithLimits(loopreviewPromptTestOptions(), inputs, ReviewPacketLimits{
		IssueBytes: len("keep issue context\n") + 1,
	})
	if !packet.IssueBody.Truncated {
		t.Fatal("issue body was not truncated")
	}
	for _, want := range []string{
		"keep issue context",
		"[TRUNCATED issue body: omitted",
		"bytes",
		"lines",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "TAIL_ISSUE_SHOULD_NOT_APPEAR") {
		t.Fatalf("prompt contains omitted issue tail:\n%s", prompt)
	}
}

func TestBuildReviewPacketTruncatesSpecBudget(t *testing.T) {
	inputs := loopreviewPromptTestInputs()
	inputs.Spec.Content = "# Spec\n\nkeep spec context\n" + strings.Repeat("omitted spec context\n", 40) + "TAIL_SPEC_SHOULD_NOT_APPEAR\n"

	prompt, packet := buildPromptWithLimits(loopreviewPromptTestOptions(), inputs, ReviewPacketLimits{
		SpecBytes: len("# Spec\n\nkeep spec context\n") + 1,
	})
	if !packet.SpecContent.Truncated {
		t.Fatal("spec body was not truncated")
	}
	for _, want := range []string{
		"keep spec context",
		"[TRUNCATED merged spec: omitted",
		"bytes",
		"lines",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "TAIL_SPEC_SHOULD_NOT_APPEAR") {
		t.Fatalf("prompt contains omitted spec tail:\n%s", prompt)
	}
}

func TestBuildPromptAppliesTotalByteBudget(t *testing.T) {
	basePrompt, _ := buildPromptWithLimits(loopreviewPromptTestOptions(), loopreviewPromptTestInputs(), ReviewPacketLimits{})
	totalBudget := len(basePrompt) + 1200

	inputs := loopreviewPromptTestInputs()
	inputs.ChangedFiles = []string{}
	for i := 0; i < 200; i++ {
		inputs.ChangedFiles = append(inputs.ChangedFiles, "internal/loopreview/very-long-file-name-for-budget-test.go")
	}
	inputs.Diff = loopreviewDiffPatch("internal/loopreview/big.go", strings.Repeat("+ big diff line for total prompt budget\n", 400))
	inputs.Issue.Body = strings.Repeat("issue body line for total prompt budget\n", 400)
	inputs.Spec.Content = strings.Repeat("spec body line for total prompt budget\n", 400)

	prompt, packet := buildPromptWithLimits(loopreviewPromptTestOptions(), inputs, ReviewPacketLimits{
		TotalPromptBytes: totalBudget,
	})
	if packet.Insufficient {
		t.Fatalf("packet unexpectedly insufficient: %s", packet.InsufficientReason)
	}
	if len(prompt) > totalBudget {
		t.Fatalf("prompt length = %d, want <= %d", len(prompt), totalBudget)
	}
	if !packet.TotalPromptBudgetApplied {
		t.Fatal("total prompt budget was not applied")
	}
	for _, want := range []string{
		"TOTAL PROMPT BUDGET APPLIED",
		"TRUNCATED",
		"omitted",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestRunInsufficientReviewPacketReturnsNeedsHumanWithoutAgent(t *testing.T) {
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
		diff:  "diff --git a/file.go b/file.go\n+change\n",
		files: []string{"file.go"},
	}
	fakeAgent := &loopreviewFakeAgent{
		summary: `{"verdict":"pass","findings":[],"evidence":"would pass","spec_conformance":"pass"}`,
	}

	result, err := Run(context.Background(), Options{
		RepoPath:   repo,
		PRNumber:   152,
		Provider:   "codex",
		BaseBranch: "main",
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
		ReviewPacketLimits: ReviewPacketLimits{
			TotalPromptBytes: 64,
		},
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Verdict.Verdict != VerdictNeedsHuman || result.ExitCode != 2 {
		t.Fatalf("result = %#v, want needs-human exit 2", result)
	}
	if !strings.Contains(result.Verdict.Evidence, "review packet insufficient") {
		t.Fatalf("evidence = %q", result.Verdict.Evidence)
	}
	if fakeAgent.calls != 0 {
		t.Fatalf("agent calls = %d, want 0", fakeAgent.calls)
	}
	if fakeGit.fetchPR != 0 || fakeGit.addRev != "" {
		t.Fatalf("worktree checkout should not run for insufficient packet: %#v", fakeGit)
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
		Model:      "claude-opus",
		Effort:     "max",
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
	if inv.Model != "claude-opus" || inv.Effort != "max" {
		t.Fatalf("agent invocation model/effort = %#v", inv)
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

func TestRunDocFirstNewSpecCanPassWithoutMergedSpec(t *testing.T) {
	repo := t.TempDir()
	scratchRoot := t.TempDir()
	specPath := "docs/specs/0220-loopreview-new-spec-not-a-blocker.md"
	fakeGit := &loopreviewFakeGit{
		showErr: errors.New("path does not exist in origin/main"),
	}
	fakeGitHub := &loopreviewFakeGitHub{
		pr: gh.PullRequest{
			Number:      228,
			Title:       "Add loopreview new-spec classification fix spec",
			HeadRefName: "loop/issue-228-doc",
			ClosingIssuesReferences: []gh.IssueReference{{
				Number: 228,
			}},
		},
		issue: gh.Issue{
			Number: 228,
			Title:  "Add design spec",
			Body:   "Add the doc-first design in " + specPath + ".",
		},
		diff:  loopreviewNewFileDiff(specPath, "+# Spec\n+\n+## Acceptance Criteria\n+\n+- Define the behavior.\n"),
		files: []string{specPath},
	}
	fakeAgent := &loopreviewFakeAgent{
		summary: `{"verdict":"pass","findings":[],"evidence":"documentation-only PR introduces the referenced spec; base absence is expected and non-blocking","spec_conformance":"pass"}`,
	}

	result, err := Run(context.Background(), Options{
		RepoPath:   repo,
		PRNumber:   228,
		Provider:   "codex",
		BaseBranch: "main",
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
	if result.Verdict.Verdict != VerdictPass || result.ExitCode != 0 {
		t.Fatalf("result = %#v, want pass exit 0", result)
	}
	if result.Verdict.SpecConformance != SpecConformanceNotApplicable {
		t.Fatalf("SpecConformance = %q, want not-applicable", result.Verdict.SpecConformance)
	}
	if len(result.Verdict.Findings) != 0 {
		t.Fatalf("findings = %#v, want none", result.Verdict.Findings)
	}
	for _, want := range []string{
		"Documentation-only: yes",
		"Classification: expected/non-blocking",
		"expected: this PR introduces the spec, so it is absent from origin/main",
	} {
		if !strings.Contains(fakeAgent.invocation.Prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, fakeAgent.invocation.Prompt)
		}
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

func TestRunVerifierNonZeroExitReturnsNeedsHuman(t *testing.T) {
	result := runWithAgentResult(t, validLoopreviewAgentResult("not-json", 7), nil, nil)
	if result.Verdict.Verdict != VerdictNeedsHuman || result.ExitCode != 2 {
		t.Fatalf("result = %#v, want needs-human exit 2", result)
	}
	if !strings.Contains(result.Verdict.Evidence, "codex verifier exited with code 7") {
		t.Fatalf("evidence = %q", result.Verdict.Evidence)
	}
	if result.Verdict.Attestation == nil || result.Verdict.Attestation.ExitCode != 7 {
		t.Fatalf("attestation = %#v, want exit code 7", result.Verdict.Attestation)
	}
}

func TestRunCodePRMissingMergedSpecStillNeedsHuman(t *testing.T) {
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
	fakeAgent := &loopreviewFakeAgent{result: &agent.Result{
		ExitCode:   -1,
		Hung:       true,
		HungReason: agent.HungReasonDeadline,
	}}

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
	if result.Verdict.Attestation != nil {
		t.Fatalf("hung verifier result had attestation: %#v", result.Verdict.Attestation)
	}
	if fakeAgent.calls != 1 {
		t.Fatalf("agent calls = %d, want 1", fakeAgent.calls)
	}
	if fakeAgent.invocation.HardCap != 10*time.Millisecond || fakeAgent.invocation.StallTimeout != VerifierStallTimeout {
		t.Fatalf("agent supervision = hard cap %s stall %s, want 10ms/%s", fakeAgent.invocation.HardCap, fakeAgent.invocation.StallTimeout, VerifierStallTimeout)
	}
}

func runWithAgentSummary(t *testing.T, summary string, showErr error) Result {
	t.Helper()
	return runWithAgentResult(t, validLoopreviewAgentResult(summary, 0), showErr, nil)
}

func loopreviewPromptTestOptions() Options {
	return Options{
		PRNumber:   199,
		BaseBranch: "main",
	}
}

func loopreviewPromptTestInputs() reviewInputs {
	return reviewInputs{
		PR: gh.PullRequest{
			Number:      199,
			Title:       "Bounded packet",
			HeadRefName: "loop/issue-199",
		},
		Issue: gh.Issue{
			Number: 199,
			Title:  "Implement bounded review packet",
			Body:   "Implement per docs/specs/0194-reliable-loopreview-verifier.md.",
		},
		IssuePresent: true,
		Diff:         loopreviewDiffPatch("internal/loopreview/loopreview.go", "+ bounded packet\n"),
		ChangedFiles: []string{"internal/loopreview/loopreview.go"},
		Spec: specInput{
			Path:      "docs/specs/0194-reliable-loopreview-verifier.md",
			Content:   "# Reliable loopreview Verifier\n\nBounded packet acceptance criteria.\n",
			Available: true,
		},
	}
}

func loopreviewDiffPatch(path, body string) string {
	return "diff --git a/" + path + " b/" + path + "\n" +
		"--- a/" + path + "\n" +
		"+++ b/" + path + "\n" +
		"@@ -1 +1 @@\n" +
		body
}

func loopreviewNewFileDiff(path, body string) string {
	return "diff --git a/" + path + " b/" + path + "\n" +
		"new file mode 100644\n" +
		"index 0000000..1111111\n" +
		"--- /dev/null\n" +
		"+++ b/" + path + "\n" +
		"@@ -0,0 +1 @@\n" +
		body
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

func assertViewedIssues(t *testing.T, fakeGitHub *loopreviewFakeGitHub, want ...int) {
	t.Helper()
	if len(fakeGitHub.viewedIssues) != len(want) {
		t.Fatalf("viewed issues = %#v, want %#v", fakeGitHub.viewedIssues, want)
	}
	for i := range want {
		if fakeGitHub.viewedIssues[i] != want[i] {
			t.Fatalf("viewed issues = %#v, want %#v", fakeGitHub.viewedIssues, want)
		}
	}
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
	pr           gh.PullRequest
	issue        gh.Issue
	issues       map[int]gh.Issue
	issueErrors  map[int]error
	viewedIssues []int
	diff         string
	files        []string
}

func (f *loopreviewFakeGitHub) ViewPR(context.Context, int) (gh.PullRequest, error) {
	return f.pr, nil
}

func (f *loopreviewFakeGitHub) ViewIssue(_ context.Context, number int) (gh.Issue, error) {
	f.viewedIssues = append(f.viewedIssues, number)
	if err := f.issueErrors[number]; err != nil {
		return gh.Issue{}, err
	}
	if f.issues != nil {
		issue, ok := f.issues[number]
		if !ok {
			return gh.Issue{}, errors.New("issue not found")
		}
		return issue, nil
	}
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
	calls              int
}

func (f *loopreviewFakeAgent) Run(ctx context.Context, invocation agent.Invocation) (agent.Result, error) {
	f.calls++
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
