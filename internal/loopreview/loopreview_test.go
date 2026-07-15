package loopreview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/agent"
	"github.com/jasonhnd/loopcoder/internal/reporter"
	gh "github.com/jasonhnd/loopcoder/internal/vcs/github"
)

func TestEvidenceProducerHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_LOOPREVIEW_PRODUCER_HELPER") != "1" {
		return
	}
	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 || separator+1 >= len(os.Args) {
		fmt.Fprintln(os.Stderr, "missing helper separator")
		os.Exit(2)
	}
	mode := os.Args[separator+1]
	args := os.Args[separator+2:]
	switch mode {
	case "write-output":
		if len(args) != 2 {
			fmt.Fprintf(os.Stderr, "write-output got %d args\n", len(args))
			os.Exit(2)
		}
		if err := os.MkdirAll(filepath.Dir(args[0]), 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "mkdir output: %v\n", err)
			os.Exit(2)
		}
		if err := os.WriteFile(args[0], []byte("producer arg: "+args[1]+"\n"), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "write output: %v\n", err)
			os.Exit(2)
		}
		os.Exit(0)
	case "sleep":
		if len(args) != 1 {
			fmt.Fprintf(os.Stderr, "sleep got %d args\n", len(args))
			os.Exit(2)
		}
		duration, err := time.ParseDuration(args[0])
		if err != nil {
			fmt.Fprintf(os.Stderr, "parse sleep duration: %v\n", err)
			os.Exit(2)
		}
		time.Sleep(duration)
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "unknown helper mode %q\n", mode)
		os.Exit(2)
	}
}

func TestVerifierWatchdogDefaultsAreRaised(t *testing.T) {
	if DefaultVerifierTimeout != 15*time.Minute || VerifierStallTimeout != 5*time.Minute {
		t.Fatalf("verifier watchdog defaults = hard cap %s stall %s, want 15m0s/5m0s", DefaultVerifierTimeout, VerifierStallTimeout)
	}
}

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

func TestVerdictJSONSchemaMatchesAcceptedWireContractAndStrictRequired(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal([]byte(VerdictJSONSchema), &schema); err != nil {
		t.Fatalf("unmarshal VerdictJSONSchema: %v", err)
	}

	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema properties = %#v, want object", schema["properties"])
	}
	got := mapKeys(properties)
	want := []string{"evidence", "findings", "spec_conformance", "verdict"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("top-level schema properties = %#v, want %#v", got, want)
	}

	assertObjectSchemasRequireAllProperties(t, "#", schema)
}

func assertObjectSchemasRequireAllProperties(t *testing.T, path string, node any) {
	t.Helper()

	switch typed := node.(type) {
	case map[string]any:
		if schemaType, _ := typed["type"].(string); schemaType == "object" {
			properties, _ := typed["properties"].(map[string]any)
			if len(properties) > 0 {
				got := mapKeys(properties)
				required := stringArray(typed["required"])
				if !reflect.DeepEqual(required, got) {
					t.Fatalf("%s required = %#v, want exactly all properties %#v", path, required, got)
				}
			}
		}
		for key, child := range typed {
			assertObjectSchemasRequireAllProperties(t, path+"/"+key, child)
		}
	case []any:
		for i, child := range typed {
			assertObjectSchemasRequireAllProperties(t, fmt.Sprintf("%s/%d", path, i), child)
		}
	}
}

func mapKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func stringArray(value any) []string {
	values, _ := value.([]any)
	out := make([]string, 0, len(values))
	for _, item := range values {
		if text, ok := item.(string); ok {
			out = append(out, text)
		}
	}
	sort.Strings(out)
	return out
}

func TestNormalizeVerdictNeedsHumanUsesFindingReasonBeforePositiveEvidence(t *testing.T) {
	verdict := NormalizeVerdict(Verdict{
		Verdict: VerdictNeedsHuman,
		Findings: []Finding{
			{
				Severity: "warning",
				File:     "docs/specs/merged-design.md",
				Note:     "merged design/spec unavailable: origin/main does not contain the referenced file",
			},
		},
		Evidence:        "All five acceptance criteria satisfied and no regressions were found.",
		SpecConformance: SpecConformanceNotApplicable,
	})

	wantReason := "docs/specs/merged-design.md: merged design/spec unavailable: origin/main does not contain the referenced file"
	if verdict.Reason != wantReason {
		t.Fatalf("Reason = %q, want %q", verdict.Reason, wantReason)
	}
	if strings.Contains(verdict.Reason, "All five acceptance criteria") {
		t.Fatalf("Reason used positive evidence: %q", verdict.Reason)
	}
	if verdict.NextAction == "" || verdict.NextAction == verdict.Reason {
		t.Fatalf("NextAction = %q, want separate action", verdict.NextAction)
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

func TestBuildPromptRecordsPRRefMetadata(t *testing.T) {
	inputs := loopreviewPromptTestInputs()
	inputs.PR.BaseRefName = "release"
	inputs.PR.BaseRefOID = "base-sha"
	inputs.PR.HeadRefName = "loop/issue-199"
	inputs.PR.HeadRefOID = "head-sha"
	inputs.Refs = reviewRefs{
		PRNumber:   199,
		BaseBranch: "release",
		BaseSHA:    "base-sha",
		HeadBranch: "loop/issue-199",
		HeadSHA:    "head-sha",
		PRHeadFileSource: prHeadFileSource{
			Ref:      prHeadLocalRef(199),
			Verified: true,
			Reason:   "fetched from GitHub PR head ref",
		},
	}

	prompt, _ := buildPromptWithLimits(loopreviewPromptTestOptions(), inputs, ReviewPacketLimits{})
	for _, want := range []string{
		"Number: #199",
		"Head: loop/issue-199",
		"Head SHA: head-sha",
		"Base: release",
		"Base SHA: base-sha",
		"PR-head file source ref: refs/loopcoder/loopreview/pr-199-head",
		"PR-head file source verified: yes",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestReadPRHeadFileReadsOnlyVerifiedPRHeadRef(t *testing.T) {
	repo := t.TempDir()
	currentPath := filepath.Join(repo, "docs", "specs", "fallback.md")
	if err := os.MkdirAll(filepath.Dir(currentPath), 0o755); err != nil {
		t.Fatalf("mkdir current worktree file: %v", err)
	}
	if err := os.WriteFile(currentPath, []byte("current worktree content\n"), 0o644); err != nil {
		t.Fatalf("write current worktree file: %v", err)
	}
	sourceRef := prHeadLocalRef(77)
	fakeGit := &loopreviewFakeGit{
		show: map[string]string{
			"origin/main:docs/specs/fallback.md":  "stale base content\n",
			sourceRef + ":docs/specs/fallback.md": "PR head content\n",
		},
	}

	got, err := readPRHeadFile(context.Background(), fakeGit, repo, prHeadFileSource{
		Ref:      sourceRef,
		Verified: true,
	}, "docs/specs/fallback.md")
	if err != nil {
		t.Fatalf("readPRHeadFile returned error: %v", err)
	}
	if got.Content != "PR head content\n" {
		t.Fatalf("content = %q, want PR head content", got.Content)
	}
	if got.SourceRef != sourceRef || got.Path != "docs/specs/fallback.md" {
		t.Fatalf("metadata = %#v, want source ref/path", got)
	}
	if !reflect.DeepEqual(fakeGit.showCalls, []string{sourceRef + ":docs/specs/fallback.md"}) {
		t.Fatalf("show calls = %#v, want only PR-head ref", fakeGit.showCalls)
	}
}

func TestReadPRHeadFileRejectsUnverifiedInferredBranch(t *testing.T) {
	repo := t.TempDir()
	fakeGit := &loopreviewFakeGit{
		show: map[string]string{
			"origin/loop/issue-77:docs/specs/fallback.md": "unverified branch content\n",
		},
	}

	_, err := readPRHeadFile(context.Background(), fakeGit, repo, prHeadFileSource{
		Ref:      "origin/loop/issue-77",
		Verified: false,
	}, "docs/specs/fallback.md")
	if err == nil {
		t.Fatal("readPRHeadFile returned nil error for unverified source")
	}
	if !strings.Contains(err.Error(), "not verified") {
		t.Fatalf("error = %q, want not verified", err.Error())
	}
	if len(fakeGit.showCalls) != 0 {
		t.Fatalf("show calls = %#v, want no read from unverified branch", fakeGit.showCalls)
	}
}

func TestBuildPromptDefaultOrderRemainsSourceFirstWithoutDomainVerification(t *testing.T) {
	prompt, _ := buildPromptWithLimits(loopreviewPromptTestOptions(), loopreviewPromptTestInputs(), ReviewPacketLimits{})

	assertPromptOrder(t, prompt,
		"# PR",
		"# Changed files",
		"# Diff excerpts",
		"# Issue",
		"# Merged design/spec",
	)
	if strings.Contains(prompt, "# Rubric") {
		t.Fatalf("prompt unexpectedly contains rubric section:\n%s", prompt)
	}
}

func TestBuildPromptInjectsRubricAndHonorsConfiguredSectionOrder(t *testing.T) {
	inputs := loopreviewPromptTestInputs()
	inputs.Rubric = rubricInput{
		Configured: true,
		Checklist:  []string{"Rendered artifact matches the approved governance spec."},
		Files: []rubricFileInput{{
			Path:      "governance/qa-checklist.md",
			Content:   "Confirm disclosure approval is documented.\n",
			Available: true,
		}},
	}
	inputs.ReviewPacketOrder = []string{"rubric", "issue", "spec", "changed_files", "diff"}

	prompt, packet := buildPromptWithLimits(loopreviewPromptTestOptions(), inputs, ReviewPacketLimits{})
	if !packet.Rubric.Configured {
		t.Fatal("rubric was not marked configured")
	}
	assertPromptOrder(t, prompt,
		"# PR",
		"# Rubric",
		"# Issue",
		"# Merged design/spec",
		"# Changed files",
		"# Diff excerpts",
	)
	for _, want := range []string{
		"When a Rubric section is configured, apply it as required review criteria.",
		"Missing configured rubric files are missing evidence",
		"Status: available",
		"## Inline checklist",
		"- Rendered artifact matches the approved governance spec.",
		"## governance/qa-checklist.md",
		"Confirm disclosure approval is documented.",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestBuildPromptInjectsRenderedArtifactsAndHonorsConfiguredSectionOrder(t *testing.T) {
	inputs := loopreviewPromptTestInputs()
	inputs.RenderedArtifacts = []renderedArtifactInput{{
		Artifact: RenderedArtifact{
			Source:         "domain.evidence.producer",
			Status:         "available",
			DeclaredOutput: "out/report.md",
			Path:           "out/report.md",
			Kind:           "markdown",
			MediaType:      "text/markdown",
			Bytes:          31,
			Summary:        "markdown file content included inline with bounded excerpt",
		},
		Content:             truncatePacketSection("# Rendered report\nApproved.\n", 4096),
		IncludeInLoopreview: true,
	}}
	inputs.ReviewPacketOrder = []string{"rendered_artifact", "issue", "spec", "changed_files", "diff"}

	prompt, packet := buildPromptWithLimits(loopreviewPromptTestOptions(), inputs, ReviewPacketLimits{})
	if !packet.RenderedArtifacts.Configured {
		t.Fatal("rendered artifacts were not marked configured")
	}
	assertPromptOrder(t, prompt,
		"# PR",
		"# Rendered artifacts",
		"# Issue",
		"# Merged design/spec",
		"# Changed files",
		"# Diff excerpts",
	)
	for _, want := range []string{
		"When a Rendered artifacts section is configured, treat it as required product evidence",
		"Source: domain.evidence.producer",
		"Declared output: out/report.md",
		"```markdown",
		"# Rendered report",
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

func TestBuildReviewPacketIncludesCompletePRHeadBodyForLargeDocSpec(t *testing.T) {
	specPath := "docs/specs/0535-loopreview-packet-truncation-reliability.md"
	sourceRef := prHeadLocalRef(547)
	body := loopreviewLargeSpecBody()
	inputs := loopreviewPromptTestInputs()
	inputs.ChangedFiles = []string{specPath}
	inputs.Diff = loopreviewNewFileDiff(specPath, loopreviewAddedDiffBody(body))
	inputs.Refs = reviewRefs{
		PRNumber: 547,
		PRHeadFileSource: prHeadFileSource{
			Ref:      sourceRef,
			Verified: true,
		},
	}
	inputs.PRHeadFileBodies = []prHeadFileBodyInput{{
		Path:      specPath,
		SourceRef: sourceRef,
		Content:   body,
		Available: true,
	}}

	prompt, packet := buildPromptWithLimits(loopreviewPromptTestOptions(), inputs, ReviewPacketLimits{
		DiffFileBytes:              1200,
		DocumentationBodyFileBytes: len(body) + 256,
		DocumentationBodyBytes:     len(body) + 2048,
		DocumentationBodyMaxFiles:  1,
	})
	if !packet.Diff.Truncated {
		t.Fatal("diff was not truncated")
	}
	if packet.PRHeadFileBodies.IncludedCount != 1 {
		t.Fatalf("included PR-head bodies = %d, want 1; skipped=%#v", packet.PRHeadFileBodies.IncludedCount, packet.PRHeadFileBodies.Skipped)
	}
	bodySection := strings.Index(prompt, "# PR-head file content")
	if bodySection < 0 {
		t.Fatalf("prompt missing PR-head file content section:\n%s", prompt)
	}
	if strings.Contains(prompt[:bodySection], "Relationship to existing specs") || strings.Contains(prompt[:bodySection], "Non-goals") {
		t.Fatalf("generic diff excerpt unexpectedly contains tail sections before PR-head body:\n%s", prompt[:bodySection])
	}
	for _, want := range []string{
		"Source: " + sourceRef + ":" + specPath,
		"Completeness: complete",
		"## Relationship to existing specs",
		"## Non-goals",
		"Line 600: final acceptance-criteria-relevant documentation detail.",
	} {
		if !strings.Contains(prompt[bodySection:], want) {
			t.Fatalf("PR-head body section missing %q:\n%s", want, prompt[bodySection:])
		}
	}
}

func TestBuildReviewPacketSkipsPRHeadBodyWhenBodyBudgetExceeded(t *testing.T) {
	specPath := "docs/specs/large.md"
	body := "kept heading\n" + strings.Repeat("required body line\n", 50)
	inputs := loopreviewPromptTestInputs()
	inputs.ChangedFiles = []string{specPath}
	inputs.Diff = loopreviewNewFileDiff(specPath, "+kept heading\n")
	inputs.PRHeadFileBodies = []prHeadFileBodyInput{{
		Path:      specPath,
		SourceRef: prHeadLocalRef(548),
		Content:   body,
		Available: true,
	}}

	prompt, packet := buildPromptWithLimits(loopreviewPromptTestOptions(), inputs, ReviewPacketLimits{
		DocumentationBodyFileBytes: 64,
		DocumentationBodyBytes:     4096,
		DocumentationBodyMaxFiles:  1,
	})
	if packet.PRHeadFileBodies.IncludedCount != 0 {
		t.Fatalf("included PR-head bodies = %d, want 0", packet.PRHeadFileBodies.IncludedCount)
	}
	if strings.Contains(prompt, "required body line") {
		t.Fatalf("oversized PR-head body was included:\n%s", prompt)
	}
	if !strings.Contains(prompt, "exceeding per-file body budget 64 bytes") {
		t.Fatalf("prompt missing budget skip reason:\n%s", prompt)
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

func TestBuildReviewPacketTruncatesRubricBudget(t *testing.T) {
	inputs := loopreviewPromptTestInputs()
	inputs.Rubric = rubricInput{
		Configured: true,
		Checklist:  []string{"Keep this checklist item."},
		Files: []rubricFileInput{{
			Path:      "governance/qa-checklist.md",
			Content:   "keep rubric context\n" + strings.Repeat("omitted rubric context\n", 40) + "TAIL_RUBRIC_SHOULD_NOT_APPEAR\n",
			Available: true,
		}},
	}

	prompt, packet := buildPromptWithLimits(loopreviewPromptTestOptions(), inputs, ReviewPacketLimits{
		RubricBytes: len("## Inline checklist\n- Keep this checklist item.\n") + len("\n## governance/qa-checklist.md\nkeep rubric context\n") + 1,
	})
	if !packet.Rubric.Content.Truncated {
		t.Fatal("rubric was not truncated")
	}
	for _, want := range []string{
		"# Rubric",
		"keep rubric context",
		"[TRUNCATED rubric: omitted",
		"bytes",
		"lines",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "TAIL_RUBRIC_SHOULD_NOT_APPEAR") {
		t.Fatalf("prompt contains omitted rubric tail:\n%s", prompt)
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

func TestRunLoadsDomainRubricAndPreservesPassWhenEvidenceAvailable(t *testing.T) {
	repo := t.TempDir()
	scratchRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".delivery.yml"), []byte(`domain:
  verification:
    rubric:
      paths:
        - governance/qa-checklist.md
      checklist:
        - Inline rubric item.
    review_packet_order:
      - rubric
      - changed_files
      - diff
      - issue
      - spec
`), 0o644); err != nil {
		t.Fatalf("write delivery config: %v", err)
	}
	fakeGit := &loopreviewFakeGit{
		show: map[string]string{
			"origin/main:docs/specs/design.md":       "# Design\n",
			"origin/main:governance/qa-checklist.md": "Rubric file criterion.\n",
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
		diff:  loopreviewDiffPatch("internal/foo.go", "+ package foo\n"),
		files: []string{"internal/foo.go"},
	}
	fakeAgent := &loopreviewFakeAgent{
		summary: `{"verdict":"pass","findings":[],"evidence":"diff satisfies issue, spec, and rubric","spec_conformance":"pass"}`,
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
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Verdict.Verdict != VerdictPass || result.ExitCode != 0 {
		t.Fatalf("result = %#v, want pass exit 0", result)
	}
	for _, want := range []string{
		"# Rubric",
		"Inline rubric item.",
		"Rubric file criterion.",
	} {
		if !strings.Contains(fakeAgent.invocation.Prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, fakeAgent.invocation.Prompt)
		}
	}
	assertPromptOrder(t, fakeAgent.invocation.Prompt, "# PR", "# Rubric", "# Changed files", "# Diff excerpts", "# Issue", "# Merged design/spec")
}

func TestRunExecutesDomainEvidenceProducerAndFeedsRenderedArtifact(t *testing.T) {
	repo := t.TempDir()
	scratchRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".delivery.yml"), []byte(`domain:
  verification:
    review_packet_order:
      - rendered_artifact
      - changed_files
      - diff
      - issue
      - spec
  evidence:
    producer:
      command: make render
      outputs:
        - out/report.md
`), 0o644); err != nil {
		t.Fatalf("write delivery config: %v", err)
	}
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
		diff:  loopreviewDiffPatch("content/source.md", "+ source\n"),
		files: []string{"content/source.md"},
	}
	fakeAgent := &loopreviewFakeAgent{
		summary: `{"verdict":"pass","findings":[],"evidence":"rendered artifact satisfies issue and spec","spec_conformance":"pass"}`,
	}
	var producerInvocation EvidenceProducerInvocation

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
		RunEvidenceProducer: func(_ context.Context, invocation EvidenceProducerInvocation) EvidenceProducerResult {
			producerInvocation = invocation
			outDir := filepath.Join(invocation.WorktreePath, "out")
			if err := os.MkdirAll(outDir, 0o755); err != nil {
				return EvidenceProducerResult{ExitCode: 127, Err: err}
			}
			if err := os.WriteFile(filepath.Join(outDir, "report.md"), []byte("# Rendered report\nApproved.\n"), 0o644); err != nil {
				return EvidenceProducerResult{ExitCode: 127, Err: err}
			}
			return EvidenceProducerResult{ExitCode: 0}
		},
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Verdict.Verdict != VerdictPass || result.ExitCode != 0 {
		t.Fatalf("result = %#v, want pass exit 0", result)
	}
	if producerInvocation.Command != "make render" || producerInvocation.WorktreePath == "" {
		t.Fatalf("producer invocation = %#v, want make render in PR worktree", producerInvocation)
	}
	for _, want := range []string{
		"# Rendered artifacts",
		"Source: domain.evidence.producer",
		"Declared output: out/report.md",
		"# Rendered report",
	} {
		if !strings.Contains(fakeAgent.invocation.Prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, fakeAgent.invocation.Prompt)
		}
	}
	if len(result.Verdict.RenderedArtifacts) != 1 {
		t.Fatalf("rendered artifacts = %#v, want one", result.Verdict.RenderedArtifacts)
	}
	artifact := result.Verdict.RenderedArtifacts[0]
	if artifact.Source != "domain.evidence.producer" || artifact.Path != "out/report.md" || artifact.Kind != "markdown" || artifact.Status != "available" || artifact.SHA256 == "" {
		t.Fatalf("artifact = %#v, want available markdown report with sha", artifact)
	}
}

func TestRunExecutesDomainEvidenceProducerArgvAndFeedsRenderedArtifact(t *testing.T) {
	repo := t.TempDir()
	scratchRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".delivery.yml"), []byte(`domain:
  verification:
    review_packet_order:
      - rendered_artifact
      - changed_files
      - diff
      - issue
      - spec
  evidence:
    producer:
      argv: ["producer-bin", "--literal", "value && exit 9"]
      outputs:
        - out/report.md
`), 0o644); err != nil {
		t.Fatalf("write delivery config: %v", err)
	}
	fakeGit := &loopreviewFakeGit{
		show: map[string]string{
			"origin/main:docs/specs/design.md": "# Design\n",
		},
	}
	fakeGitHub := loopreviewStandardFakeGitHub()
	fakeAgent := &loopreviewFakeAgent{
		summary: `{"verdict":"pass","findings":[],"evidence":"rendered artifact satisfies issue and spec","spec_conformance":"pass"}`,
	}
	var producerInvocation EvidenceProducerInvocation

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
		RunEvidenceProducer: func(_ context.Context, invocation EvidenceProducerInvocation) EvidenceProducerResult {
			producerInvocation = invocation
			if !reflect.DeepEqual(invocation.Argv, []string{"producer-bin", "--literal", "value && exit 9"}) {
				return EvidenceProducerResult{ExitCode: 7, Output: fmt.Sprintf("argv = %#v", invocation.Argv)}
			}
			outDir := filepath.Join(invocation.WorktreePath, "out")
			if err := os.MkdirAll(outDir, 0o755); err != nil {
				return EvidenceProducerResult{ExitCode: 127, Err: err}
			}
			if err := os.WriteFile(filepath.Join(outDir, "report.md"), []byte("# Rendered report\nApproved.\n"), 0o644); err != nil {
				return EvidenceProducerResult{ExitCode: 127, Err: err}
			}
			return EvidenceProducerResult{ExitCode: 0}
		},
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Verdict.Verdict != VerdictPass || result.ExitCode != 0 {
		t.Fatalf("result = %#v, want pass exit 0", result)
	}
	if producerInvocation.Command != "" || !reflect.DeepEqual(producerInvocation.Argv, []string{"producer-bin", "--literal", "value && exit 9"}) {
		t.Fatalf("producer invocation = %#v, want argv-only invocation", producerInvocation)
	}
	if !strings.Contains(fakeAgent.invocation.Prompt, "# Rendered artifacts") {
		t.Fatalf("prompt missing rendered artifact:\n%s", fakeAgent.invocation.Prompt)
	}
}

func TestRunEvidenceProducerCommandArgvDoesNotUseShell(t *testing.T) {
	t.Setenv("GO_WANT_LOOPREVIEW_PRODUCER_HELPER", "1")
	worktreePath := t.TempDir()
	literal := "value && exit 9"

	result := runEvidenceProducerCommand(context.Background(), EvidenceProducerInvocation{
		Argv: []string{
			os.Args[0],
			"-test.run=TestEvidenceProducerHelperProcess",
			"--",
			"write-output",
			"out/report.md",
			literal,
		},
		WorktreePath: worktreePath,
		Timeout:      5 * time.Second,
	})
	if result.Err != nil {
		t.Fatalf("runEvidenceProducerCommand Err = %v, output:\n%s", result.Err, result.Output)
	}
	if result.ExitCode != 0 || result.TimedOut {
		t.Fatalf("result = %#v, want exit 0 without timeout", result)
	}
	data, err := os.ReadFile(filepath.Join(worktreePath, "out", "report.md"))
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	if !strings.Contains(string(data), literal) {
		t.Fatalf("report = %q, want literal shell metacharacters preserved", string(data))
	}
}

func TestRunEvidenceProducerCommandHardTimeout(t *testing.T) {
	t.Setenv("GO_WANT_LOOPREVIEW_PRODUCER_HELPER", "1")
	result := runEvidenceProducerCommand(context.Background(), EvidenceProducerInvocation{
		Argv: []string{
			os.Args[0],
			"-test.run=TestEvidenceProducerHelperProcess",
			"--",
			"sleep",
			"10s",
		},
		WorktreePath: t.TempDir(),
		Timeout:      100 * time.Millisecond,
	})
	if !result.TimedOut {
		t.Fatalf("TimedOut = false, want true; result = %#v", result)
	}
	if result.Err != nil {
		t.Fatalf("Err = %v, want nil timeout kill result", result.Err)
	}
}

func TestRunProducerFailureReturnsNeedsHumanWithoutVerifier(t *testing.T) {
	repo := t.TempDir()
	scratchRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".delivery.yml"), []byte(`domain:
  evidence:
    producer:
      command: make render
      outputs:
        - out/report.md
`), 0o644); err != nil {
		t.Fatalf("write delivery config: %v", err)
	}
	fakeGit := &loopreviewFakeGit{
		show: map[string]string{
			"origin/main:docs/specs/design.md": "# Design\n",
		},
	}
	fakeGitHub := loopreviewStandardFakeGitHub()
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
		RunEvidenceProducer: func(context.Context, EvidenceProducerInvocation) EvidenceProducerResult {
			return EvidenceProducerResult{ExitCode: 7, Output: "render failed"}
		},
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Verdict.Verdict != VerdictNeedsHuman || result.ExitCode != 2 {
		t.Fatalf("result = %#v, want needs-human exit 2", result)
	}
	if fakeAgent.calls != 0 {
		t.Fatalf("agent calls = %d, want 0", fakeAgent.calls)
	}
	for _, want := range []string{"domain evidence producer exited with code 7", "render failed"} {
		if !strings.Contains(result.Verdict.Evidence, want) {
			t.Fatalf("evidence missing %q:\n%s", want, result.Verdict.Evidence)
		}
	}
	if len(result.Verdict.RenderedArtifacts) != 1 || result.Verdict.RenderedArtifacts[0].Status != "error" {
		t.Fatalf("rendered artifacts = %#v, want producer error artifact", result.Verdict.RenderedArtifacts)
	}
}

func TestRunProducerMissingOutputReturnsNeedsHumanWithoutVerifier(t *testing.T) {
	repo := t.TempDir()
	scratchRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".delivery.yml"), []byte(`domain:
  evidence:
    producer:
      command: make render
      outputs:
        - out/missing.pdf
`), 0o644); err != nil {
		t.Fatalf("write delivery config: %v", err)
	}
	fakeGit := &loopreviewFakeGit{
		show: map[string]string{
			"origin/main:docs/specs/design.md": "# Design\n",
		},
	}
	fakeGitHub := loopreviewStandardFakeGitHub()
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
		RunEvidenceProducer: func(context.Context, EvidenceProducerInvocation) EvidenceProducerResult {
			return EvidenceProducerResult{ExitCode: 0}
		},
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Verdict.Verdict != VerdictNeedsHuman || fakeAgent.calls != 0 {
		t.Fatalf("result=%#v agent calls=%d, want needs-human without verifier", result, fakeAgent.calls)
	}
	if !strings.Contains(result.Verdict.Evidence, "out/missing.pdf (missing)") {
		t.Fatalf("evidence = %q, want missing output", result.Verdict.Evidence)
	}
	if len(result.Verdict.RenderedArtifacts) != 1 || result.Verdict.RenderedArtifacts[0].DeclaredOutput != "out/missing.pdf" || result.Verdict.RenderedArtifacts[0].Status != "missing" {
		t.Fatalf("rendered artifacts = %#v, want missing output artifact", result.Verdict.RenderedArtifacts)
	}
}

func TestRunProducerIncludeFalseSurfacesArtifactWithoutPacketSection(t *testing.T) {
	repo := t.TempDir()
	scratchRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".delivery.yml"), []byte(`domain:
  evidence:
    producer:
      command: make render
      outputs:
        - out/report.md
      include_in_loopreview: false
`), 0o644); err != nil {
		t.Fatalf("write delivery config: %v", err)
	}
	fakeGit := &loopreviewFakeGit{
		show: map[string]string{
			"origin/main:docs/specs/design.md": "# Design\n",
		},
	}
	fakeGitHub := loopreviewStandardFakeGitHub()
	fakeAgent := &loopreviewFakeAgent{
		summary: `{"verdict":"pass","findings":[],"evidence":"diff satisfies issue and spec","spec_conformance":"pass"}`,
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
		RunEvidenceProducer: func(_ context.Context, invocation EvidenceProducerInvocation) EvidenceProducerResult {
			outDir := filepath.Join(invocation.WorktreePath, "out")
			if err := os.MkdirAll(outDir, 0o755); err != nil {
				return EvidenceProducerResult{ExitCode: 127, Err: err}
			}
			if err := os.WriteFile(filepath.Join(outDir, "report.md"), []byte("# Rendered report\n"), 0o644); err != nil {
				return EvidenceProducerResult{ExitCode: 127, Err: err}
			}
			return EvidenceProducerResult{ExitCode: 0}
		},
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Verdict.Verdict != VerdictPass {
		t.Fatalf("result = %#v, want pass", result)
	}
	if strings.Contains(fakeAgent.invocation.Prompt, "# Rendered artifacts") {
		t.Fatalf("prompt included rendered artifacts despite include_in_loopreview=false:\n%s", fakeAgent.invocation.Prompt)
	}
	if len(result.Verdict.RenderedArtifacts) != 1 || result.Verdict.RenderedArtifacts[0].Path != "out/report.md" {
		t.Fatalf("rendered artifacts = %#v, want reported artifact", result.Verdict.RenderedArtifacts)
	}
}

func TestRunFeedsBrowserPreviewAsRenderedArtifact(t *testing.T) {
	repo := t.TempDir()
	scratchRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".delivery.yml"), []byte(`verification:
  browser:
    enabled: auto
evidence:
  website:
    preview_url: https://preview.example.test/pr-152
`), 0o644); err != nil {
		t.Fatalf("write delivery config: %v", err)
	}
	fakeGit := &loopreviewFakeGit{
		show: map[string]string{
			"origin/main:docs/specs/design.md": "# Design\n",
		},
	}
	fakeGitHub := loopreviewStandardFakeGitHub()
	fakeAgent := &loopreviewFakeAgent{
		summary: `{"verdict":"pass","findings":[],"evidence":"browser preview evidence and diff satisfy issue","spec_conformance":"pass"}`,
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
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Verdict.Verdict != VerdictPass {
		t.Fatalf("result = %#v, want pass", result)
	}
	for _, want := range []string{"# Rendered artifacts", "Source: verification.browser", "preview_url=https://preview.example.test/pr-152"} {
		if !strings.Contains(fakeAgent.invocation.Prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, fakeAgent.invocation.Prompt)
		}
	}
	if len(result.Verdict.RenderedArtifacts) != 1 || result.Verdict.RenderedArtifacts[0].Source != "verification.browser" {
		t.Fatalf("rendered artifacts = %#v, want browser preview artifact", result.Verdict.RenderedArtifacts)
	}
}

func TestCollectDeclaredRenderedArtifactSummarizesPDFWithoutInlineContent(t *testing.T) {
	worktree := t.TempDir()
	outDir := filepath.Join(worktree, "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir output dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "report.pdf"), []byte("%PDF-1.7\n%binary\n\x00\x01"), 0o644); err != nil {
		t.Fatalf("write pdf: %v", err)
	}

	artifacts, err := collectDeclaredRenderedArtifacts(worktree, []string{"out/report.pdf"}, true)
	if err != nil {
		t.Fatalf("collectDeclaredRenderedArtifacts returned error: %v", err)
	}
	if len(artifacts) != 1 {
		t.Fatalf("artifacts = %#v, want one", artifacts)
	}
	artifact := artifacts[0]
	if artifact.Artifact.Kind != "pdf" || artifact.Artifact.MediaType != "application/pdf" || artifact.Artifact.SHA256 == "" {
		t.Fatalf("artifact = %#v, want PDF manifest with hash", artifact.Artifact)
	}
	if !strings.Contains(artifact.Artifact.Summary, "PDF binary summary: version=1.7") {
		t.Fatalf("summary = %q, want deterministic PDF summary", artifact.Artifact.Summary)
	}
	if strings.TrimSpace(artifact.Content.Text) != "" {
		t.Fatalf("PDF content was inlined unexpectedly: %#v", artifact.Content)
	}
}

func TestRunForcesNeedsHumanWhenConfiguredRubricFileIsMissing(t *testing.T) {
	repo := t.TempDir()
	scratchRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".delivery.yml"), []byte(`domain:
  verification:
    rubric:
      paths:
        - governance/missing-checklist.md
      checklist:
        - Inline rubric item still loads.
`), 0o644); err != nil {
		t.Fatalf("write delivery config: %v", err)
	}
	fakeGit := &loopreviewFakeGit{
		show: map[string]string{
			"origin/main:docs/specs/design.md": "# Design\n",
		},
		showErrs: map[string]error{
			"origin/main:governance/missing-checklist.md": errors.New("path does not exist in origin/main"),
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
		diff:  loopreviewDiffPatch("internal/foo.go", "+ package foo\n"),
		files: []string{"internal/foo.go"},
	}
	fakeAgent := &loopreviewFakeAgent{
		summary: `{"verdict":"pass","findings":[],"evidence":"would pass without the missing rubric file","spec_conformance":"pass"}`,
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
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Verdict.Verdict != VerdictNeedsHuman || result.ExitCode != 2 {
		t.Fatalf("result = %#v, want needs-human exit 2", result)
	}
	for _, want := range []string{
		"configured rubric evidence unavailable",
		"governance/missing-checklist.md",
		"path does not exist in origin/main",
	} {
		if !strings.Contains(result.Verdict.Evidence, want) {
			t.Fatalf("evidence missing %q:\n%s", want, result.Verdict.Evidence)
		}
	}
	if fakeAgent.calls != 1 {
		t.Fatalf("agent calls = %d, want 1", fakeAgent.calls)
	}
	for _, want := range []string{
		"# Rubric",
		"Status: missing evidence",
		"Missing configured rubric files:",
		"Inline rubric item still loads.",
	} {
		if !strings.Contains(fakeAgent.invocation.Prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, fakeAgent.invocation.Prompt)
		}
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

	opts := Options{
		PRNumber:   152,
		BaseBranch: "main",
	}
	refs := reviewRefsFromPR(opts.PRNumber, opts.BaseBranch, fakeGitHub.pr)
	inputs, err := gatherInputs(context.Background(), Deps{Git: fakeGit}, fakeGitHub, t.TempDir(), opts, fakeGitHub.pr, refs)
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

func TestGatherInputsWarnsWhenGeneratedAttributesGitShowFails(t *testing.T) {
	fakeGit := &loopreviewFakeGit{
		show: map[string]string{
			"origin/main:docs/specs/design.md": "# Design\n",
		},
		showErrs: map[string]error{
			"origin/main:.gitattributes": errors.New("git show failed"),
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
		diff:  loopreviewDiffPatch("internal/foo.go", "+ package foo\n"),
		files: []string{"internal/foo.go"},
	}
	var stderr strings.Builder

	opts := Options{
		PRNumber:   152,
		BaseBranch: "main",
		Stderr:     &stderr,
	}
	refs := reviewRefsFromPR(opts.PRNumber, opts.BaseBranch, fakeGitHub.pr)
	inputs, err := gatherInputs(context.Background(), Deps{Git: fakeGit}, fakeGitHub, t.TempDir(), opts, fakeGitHub.pr, refs)
	if err != nil {
		t.Fatalf("gatherInputs returned error: %v", err)
	}
	if len(inputs.GeneratedAttributeRules) != 0 {
		t.Fatalf("GeneratedAttributeRules = %#v, want fallback without attribute rules", inputs.GeneratedAttributeRules)
	}
	for _, want := range []string{"warning", "generated-file classification via .gitattributes is unavailable", "falling back to glob and size heuristics"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr missing %q:\n%s", want, stderr.String())
		}
	}
}

func TestLoadGeneratedAttributeRulesDiscriminatesBaseRefFailures(t *testing.T) {
	tests := []struct {
		name     string
		showErr  error
		wantWarn bool
	}{
		{
			name:    "path absent from valid base is silent",
			showErr: errors.New("git show origin/main:.gitattributes: exit status 128: fatal: Path '.gitattributes' does not exist in 'main'"),
		},
		{
			name:    "path exists on disk but absent from valid base is silent",
			showErr: errors.New("git show origin/main:.gitattributes: exit status 128: fatal: .gitattributes exists on disk, but not in 'main'"),
		},
		{
			name:     "bad base pathspec warns and falls back",
			showErr:  errors.New("git show origin/main:.gitattributes: exit status 128: error: pathspec 'main' did not match any file(s) known to git"),
			wantWarn: true,
		},
		{
			name:     "invalid object name warns and falls back",
			showErr:  errors.New("git show origin/main:.gitattributes: exit status 128: fatal: invalid object name 'main'"),
			wantWarn: true,
		},
		{
			name:     "ambiguous argument warns and falls back",
			showErr:  errors.New("git show origin/main:.gitattributes: exit status 128: fatal: ambiguous argument 'main:.gitattributes': unknown revision or path not in the working tree"),
			wantWarn: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeGit := &loopreviewFakeGit{
				showErrs: map[string]error{
					"origin/main:.gitattributes": tt.showErr,
				},
			}
			var warnings strings.Builder

			rules := loadGeneratedAttributeRules(context.Background(), fakeGit, t.TempDir(), "main", &warnings)
			if len(rules) != 0 {
				t.Fatalf("rules = %#v, want fallback without attribute rules", rules)
			}
			if tt.wantWarn {
				for _, want := range []string{"warning", "generated-file classification via .gitattributes is unavailable", "falling back to glob and size heuristics"} {
					if !strings.Contains(warnings.String(), want) {
						t.Fatalf("warning missing %q:\n%s", want, warnings.String())
					}
				}
				return
			}
			if warnings.Len() != 0 {
				t.Fatalf("warnings = %q, want none", warnings.String())
			}
		})
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
	if result.Verdict.Report == nil {
		t.Fatal("verdict missing report")
	}
	sourceRef := prHeadLocalRef(152)
	if fakeGit.fetchBase != "main" || fakeGit.fetchPRRef != 152 || fakeGit.fetchPRRefDest != sourceRef || fakeGit.addRev != sourceRef {
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

func TestRunPassesConfiguredReadOnlyVerifierMCPServers(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".delivery.yml"), []byte(`
mcp:
  servers:
    - name: worker-only
      transport: stdio
      command: ./tools/worker-only
      roles: [worker]
      read_only: true
    - name: verifier-write
      transport: stdio
      command: ./tools/verifier-write
      roles: [verifier]
      read_only: false
    - name: verifier-read
      transport: http
      url: https://mcp.example.com/verifier
      auth:
        header: Authorization
        env: VERIFIER_MCP_TOKEN
      roles: [worker, verifier]
      read_only: true
`), 0o644); err != nil {
		t.Fatalf("write delivery config: %v", err)
	}
	scratchRoot := t.TempDir()
	fakeGit := &loopreviewFakeGit{
		show: map[string]string{
			"origin/main:docs/specs/design.md": "# Design\n\nAcceptance criteria here.\n",
		},
	}
	fakeGitHub := &loopreviewFakeGitHub{
		pr: gh.PullRequest{
			Number:      465,
			Title:       "MCP invocation contract",
			HeadRefName: "loop/issue-465",
			ClosingIssuesReferences: []gh.IssueReference{{
				Number: 465,
			}},
		},
		issue: gh.Issue{
			Number: 465,
			Title:  "MCP invocation contract",
			Body:   "Implement per docs/specs/design.md.",
		},
		diff:  "diff --git a/internal/agent/agent.go b/internal/agent/agent.go\n",
		files: []string{"internal/agent/agent.go"},
	}
	fakeAgent := &loopreviewFakeAgent{
		summary: `{"verdict":"pass","findings":[],"evidence":"diff satisfies issue and spec","spec_conformance":"pass"}`,
	}

	result, err := Run(context.Background(), Options{
		RepoPath:   repo,
		PRNumber:   465,
		Provider:   "claude",
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
	if result.Verdict.Verdict != VerdictPass {
		t.Fatalf("verdict = %q, want pass", result.Verdict.Verdict)
	}
	servers := fakeAgent.invocation.MCPServers
	if len(servers) != 1 {
		t.Fatalf("MCPServers = %#v, want only verifier-read", servers)
	}
	if servers[0].Name != "verifier-read" || servers[0].URL != "https://mcp.example.com/verifier" || !servers[0].ReadOnly {
		t.Fatalf("verifier MCP server = %#v, want read-only verifier-read", servers[0])
	}
	if servers[0].Auth.Header != "Authorization" || servers[0].Auth.Env != "VERIFIER_MCP_TOKEN" {
		t.Fatalf("verifier MCP auth = %#v, want env-backed Authorization", servers[0].Auth)
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

func TestRunIncludesCompletePRHeadBodyForLargeAddedDocSpec(t *testing.T) {
	repo := t.TempDir()
	scratchRoot := t.TempDir()
	specPath := "docs/specs/0535-loopreview-packet-truncation-reliability.md"
	sourceRef := prHeadLocalRef(547)
	body := loopreviewLargeSpecBody()
	fakeGit := &loopreviewFakeGit{
		show: map[string]string{
			sourceRef + ":" + specPath: body,
		},
		showErrs: map[string]error{
			"origin/main:" + specPath: errors.New("path does not exist in origin/main"),
		},
	}
	fakeGitHub := &loopreviewFakeGitHub{
		pr: gh.PullRequest{
			Number:      547,
			Title:       "Add large spec",
			HeadRefName: "loop/issue-547",
			ClosingIssuesReferences: []gh.IssueReference{{
				Number: 547,
			}},
		},
		issue: gh.Issue{
			Number: 547,
			Title:  "Add large spec",
			Body:   "Add the doc-first design in " + specPath + ".",
		},
		diff:  loopreviewNewFileDiff(specPath, loopreviewAddedDiffBody(body)),
		files: []string{specPath},
	}
	fakeAgent := &loopreviewFakeAgent{
		summary: `{"verdict":"pass","findings":[],"evidence":"complete PR-head body includes required tail sections","spec_conformance":"pass"}`,
	}

	result, err := Run(context.Background(), Options{
		RepoPath:   repo,
		PRNumber:   547,
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
			DiffFileBytes:              1200,
			DocumentationBodyFileBytes: len(body) + 256,
			DocumentationBodyBytes:     len(body) + 2048,
			DocumentationBodyMaxFiles:  1,
		},
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Verdict.Verdict != VerdictPass {
		t.Fatalf("verdict = %q, want pass", result.Verdict.Verdict)
	}
	if !containsString(fakeGit.showCalls, sourceRef+":"+specPath) {
		t.Fatalf("show calls = %#v, want PR-head file read", fakeGit.showCalls)
	}
	for _, want := range []string{
		"# PR-head file content",
		"Source: " + sourceRef + ":" + specPath,
		"Completeness: complete",
		"## Relationship to existing specs",
		"## Non-goals",
	} {
		if !strings.Contains(fakeAgent.invocation.Prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, fakeAgent.invocation.Prompt)
		}
	}
}

func TestRunUsesGitHubPRBaseAndVerifiedHeadRef(t *testing.T) {
	repo := t.TempDir()
	scratchRoot := t.TempDir()
	sourceRef := prHeadLocalRef(538)
	fakeGit := &loopreviewFakeGit{
		show: map[string]string{
			"origin/release:docs/specs/design.md": "# Design\n\nAcceptance criteria.\n",
		},
		revParse: map[string]string{
			"origin/release^{commit}": sourceRef + "-base-sha",
			sourceRef + "^{commit}":   sourceRef + "-head-sha",
		},
	}
	fakeGitHub := &loopreviewFakeGitHub{
		pr: gh.PullRequest{
			Number:      538,
			Title:       "Fresh refs",
			Body:        "Closes #538",
			BaseRefName: "release",
			BaseRefOID:  sourceRef + "-base-sha",
			HeadRefName: "loop/issue-538",
			HeadRefOID:  sourceRef + "-head-sha",
			ClosingIssuesReferences: []gh.IssueReference{{
				Number: 538,
			}},
		},
		issue: gh.Issue{
			Number: 538,
			Title:  "Issue 538",
			Body:   "Implement per docs/specs/design.md.",
		},
		diff:  loopreviewDiffPatch("internal/loopreview/loopreview.go", "+ fresh refs\n"),
		files: []string{"internal/loopreview/loopreview.go"},
	}
	fakeAgent := &loopreviewFakeAgent{
		summary: `{"verdict":"pass","findings":[],"evidence":"packet uses true base and verified head","spec_conformance":"pass"}`,
	}

	result, err := Run(context.Background(), Options{
		RepoPath:   repo,
		PRNumber:   538,
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
	if result.Verdict.Verdict != VerdictPass {
		t.Fatalf("verdict = %q, want pass", result.Verdict.Verdict)
	}
	if fakeGit.fetchBase != "release" {
		t.Fatalf("fetched base = %q, want release", fakeGit.fetchBase)
	}
	if fakeGit.fetchPRRef != 538 || fakeGit.fetchPRRefDest != sourceRef {
		t.Fatalf("PR head fetch = #%d %q, want #538 %q", fakeGit.fetchPRRef, fakeGit.fetchPRRefDest, sourceRef)
	}
	if fakeGit.addRev != sourceRef {
		t.Fatalf("worktree rev = %q, want verified source ref %q", fakeGit.addRev, sourceRef)
	}
	if !reflect.DeepEqual(fakeGit.revParseCalls, []string{"origin/release^{commit}", sourceRef + "^{commit}"}) {
		t.Fatalf("rev-parse calls = %#v", fakeGit.revParseCalls)
	}
	if !reflect.DeepEqual(fakeGit.showCalls, []string{"origin/release:.gitattributes", "origin/release:docs/specs/design.md"}) {
		t.Fatalf("show calls = %#v, want true base only", fakeGit.showCalls)
	}
	for _, want := range []string{
		"Head: loop/issue-538",
		"Head SHA: " + sourceRef + "-head-sha",
		"Base: release",
		"Base SHA: " + sourceRef + "-base-sha",
		"PR-head file source ref: " + sourceRef,
		"PR-head file source verified: yes",
		"# Merged design/spec from origin/release",
	} {
		if !strings.Contains(fakeAgent.invocation.Prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, fakeAgent.invocation.Prompt)
		}
	}
}

func TestRunReturnsNeedsHumanWhenPRHeadRefSHADoesNotMatch(t *testing.T) {
	repo := t.TempDir()
	sourceRef := prHeadLocalRef(538)
	fakeGit := &loopreviewFakeGit{
		revParse: map[string]string{
			"origin/main^{commit}":  "base-sha",
			sourceRef + "^{commit}": "stale-head-sha",
		},
	}
	fakeGitHub := &loopreviewFakeGitHub{
		pr: gh.PullRequest{
			Number:      538,
			Title:       "Fresh refs",
			BaseRefName: "main",
			BaseRefOID:  "base-sha",
			HeadRefName: "loop/issue-538",
			HeadRefOID:  "head-sha",
		},
	}
	fakeAgent := &loopreviewFakeAgent{}

	result, err := Run(context.Background(), Options{
		RepoPath:   repo,
		PRNumber:   538,
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
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Verdict.Verdict != VerdictNeedsHuman || result.ExitCode != 2 {
		t.Fatalf("result = %#v, want needs-human", result)
	}
	if !strings.Contains(result.Verdict.Evidence, "review refs unavailable") || !strings.Contains(result.Verdict.Evidence, "GitHub reports head-sha") {
		t.Fatalf("evidence = %q, want SHA mismatch", result.Verdict.Evidence)
	}
	if fakeAgent.calls != 0 {
		t.Fatalf("agent calls = %d, want verifier not invoked", fakeAgent.calls)
	}
	if fakeGit.addRev != "" {
		t.Fatalf("worktree was checked out at %q despite unverified head", fakeGit.addRev)
	}
}

func TestRunVerifierReport(t *testing.T) {
	validSummary := `{"verdict":"pass","findings":[],"evidence":"diff satisfies issue and spec","spec_conformance":"pass"}`
	tests := []struct {
		name        string
		agent       agent.Result
		wantVerdict string
		wantNote    string
	}{
		{
			name:        "valid report stays pass",
			agent:       validLoopreviewAgentResult(validSummary, 0),
			wantVerdict: VerdictPass,
		},
		{
			name: "incomplete report forces needs human",
			agent: agent.Result{
				ExitCode:   0,
				Summary:    validSummary,
				Effort:     "high",
				StartedAt:  "2026-06-28T00:00:00Z",
				EndedAt:    "2026-06-28T00:00:02Z",
				DurationMS: 2000,
				Usage: reporter.Usage{
					TotalTokens: int64Ptr(123),
				},
			},
			wantVerdict: VerdictNeedsHuman,
			wantNote:    "incomplete verifier report",
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
			if result.Verdict.Report == nil {
				t.Fatal("verdict missing report")
			}

			record := result.Verdict.Report
			if record.Role != reporter.RoleVerifier || record.Provider != "codex" || record.ModelSource != reporter.ModelSourceParsed || record.Permission != reporter.PermissionReadOnly || !record.Verified {
				t.Fatalf("report identity fields = %#v", record)
			}
			if record.Action != "review PR #152" || record.ExitCode != tt.agent.ExitCode {
				t.Fatalf("report action/exit = (%q, %d), want review PR #152/%d", record.Action, record.ExitCode, tt.agent.ExitCode)
			}
			if !strings.Contains(stderr.String(), record.Header()) {
				t.Fatalf("stderr missing report header %q:\n%s", record.Header(), stderr.String())
			}

			var rendered strings.Builder
			if err := Render(&rendered, result); err != nil {
				t.Fatalf("Render returned error: %v", err)
			}
			var payload map[string]any
			if err := json.Unmarshal([]byte(rendered.String()), &payload); err != nil {
				t.Fatalf("rendered verdict is not JSON: %v\n%s", err, rendered.String())
			}
			if _, ok := payload["report"]; !ok {
				t.Fatalf("rendered verdict missing report: %s", rendered.String())
			}

			if tt.wantNote != "" {
				found := false
				for _, finding := range result.Verdict.Findings {
					if strings.Contains(finding.Note, tt.wantNote) && strings.Contains(finding.Note, "model is required") {
						found = true
					}
				}
				if !found {
					t.Fatalf("findings missing incomplete-report note: %#v", result.Verdict.Findings)
				}
			} else if err := record.Validate(); err != nil {
				t.Fatalf("valid report did not validate: %v", err)
			}
		})
	}
}

func TestVerifierReportAllowsAntigravitySelfReportedNoUsage(t *testing.T) {
	record := verifierReport(Options{
		PRNumber: 559,
		Provider: "antigravity",
	}, agent.Result{
		ExitCode:   0,
		Summary:    `{"verdict":"needs-human","findings":[],"evidence":"read-only unsupported","spec_conformance":"not-applicable"}`,
		Model:      "Gemini 3.1 Pro (High)",
		Effort:     "High",
		StartedAt:  "2026-06-28T00:00:00Z",
		EndedAt:    "2026-06-28T00:00:02Z",
		DurationMS: 2000,
	}, reviewInputs{}, reviewRefs{}, "", "loopreview-559")

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

func TestVerifierReportIncludesGrokAdapterAttribution(t *testing.T) {
	record := verifierReport(Options{
		PRNumber: 834,
		Provider: "grok",
	}, agent.Result{
		ExitCode:           0,
		Model:              "grok-4.5",
		AdapterVersion:     "0.1.211",
		ExternalSessionRef: "session-abc",
		StartedAt:          "2026-07-13T00:00:00Z",
		EndedAt:            "2026-07-13T00:00:01Z",
		DurationMS:         1000,
	}, reviewInputs{}, reviewRefs{}, "", "loopreview-834")

	want := "review PR #834 [adapter=0.1.211 attempt=loopreview-834 session=session-abc]"
	if record.Action != want {
		t.Fatalf("Action = %q, want %q", record.Action, want)
	}
	if err := record.Validate(); err != nil {
		t.Fatalf("Validate returned error: %v", err)
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
	if result.Verdict.Report == nil || result.Verdict.Report.ExitCode != 7 {
		t.Fatalf("report = %#v, want exit code 7", result.Verdict.Report)
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
	if result.Verdict.Report != nil {
		t.Fatalf("hung verifier result had report: %#v", result.Verdict.Report)
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

func loopreviewAddedDiffBody(body string) string {
	var out strings.Builder
	for _, line := range strings.SplitAfter(body, "\n") {
		if line == "" {
			continue
		}
		out.WriteString("+")
		out.WriteString(line)
	}
	return out.String()
}

func loopreviewLargeSpecBody() string {
	var out strings.Builder
	out.WriteString("# Packet truncation reliability\n\n")
	for i := 1; i <= 600; i++ {
		switch i {
		case 560:
			out.WriteString("## Relationship to existing specs\n")
			out.WriteString("This section is intentionally near the tail so the generic per-file diff cap omits it.\n\n")
		case 590:
			out.WriteString("## Non-goals\n")
			out.WriteString("This section is also intentionally near the tail and must remain reviewable from PR-head content.\n\n")
		case 600:
			out.WriteString("Line 600: final acceptance-criteria-relevant documentation detail.\n")
		default:
			fmt.Fprintf(&out, "Line %03d: bounded review packet documentation filler.\n", i)
		}
	}
	return out.String()
}

func assertPromptOrder(t *testing.T, prompt string, labels ...string) {
	t.Helper()
	previous := -1
	for _, label := range labels {
		index := strings.Index(prompt, label)
		if index < 0 {
			t.Fatalf("prompt missing %q:\n%s", label, prompt)
		}
		if index <= previous {
			t.Fatalf("prompt section %q appeared out of order:\n%s", label, prompt)
		}
		previous = index
	}
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
		Usage: reporter.Usage{
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

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func loopreviewStandardFakeGitHub() *loopreviewFakeGitHub {
	return &loopreviewFakeGitHub{
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
		diff:  loopreviewDiffPatch("content/source.md", "+ source\n"),
		files: []string{"content/source.md"},
	}
}

type loopreviewFakeGit struct {
	fetchBase      string
	fetchPR        int
	fetchPRRef     int
	fetchPRRefDest string
	addRev         string
	removed        bool
	show           map[string]string
	showErr        error
	showErrs       map[string]error
	showCalls      []string
	revParse       map[string]string
	revParseErr    error
	revParseErrs   map[string]error
	revParseCalls  []string
}

func (f *loopreviewFakeGit) FetchOriginBase(_ context.Context, _ string, baseBranch string) error {
	f.fetchBase = baseBranch
	return nil
}

func (f *loopreviewFakeGit) FetchPRHead(_ context.Context, _ string, prNumber int) error {
	f.fetchPR = prNumber
	return nil
}

func (f *loopreviewFakeGit) FetchPRHeadRef(_ context.Context, _ string, prNumber int, destRef string) error {
	f.fetchPRRef = prNumber
	f.fetchPRRefDest = destRef
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

func (f *loopreviewFakeGit) RevParse(_ context.Context, _ string, rev string) (string, error) {
	f.revParseCalls = append(f.revParseCalls, rev)
	if err := f.revParseErrs[rev]; err != nil {
		return "", err
	}
	if f.revParseErr != nil {
		return "", f.revParseErr
	}
	if value, ok := f.revParse[rev]; ok {
		return value, nil
	}
	return "fake-sha", nil
}

func (f *loopreviewFakeGit) Show(_ context.Context, _ string, revPath string) (string, error) {
	f.showCalls = append(f.showCalls, revPath)
	if err := f.showErrs[revPath]; err != nil {
		return "", err
	}
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
