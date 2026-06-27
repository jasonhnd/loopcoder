// Package loopreview runs an independent read-only verifier for a pull request.
package loopreview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/agent"
	"github.com/jasonhnd/loopcoder/internal/gitutil"
	"github.com/jasonhnd/loopcoder/internal/lockfile"
	gh "github.com/jasonhnd/loopcoder/internal/vcs/github"
)

const (
	VerdictPass       = "pass"
	VerdictFail       = "fail"
	VerdictNeedsHuman = "needs-human"

	SpecConformancePass          = "pass"
	SpecConformanceFail          = "fail"
	SpecConformanceNotApplicable = "not-applicable"
)

type Options struct {
	RepoPath   string
	PRNumber   int
	Provider   string
	BaseBranch string
	Stderr     io.Writer
}

type Result struct {
	Verdict  Verdict
	ExitCode int
}

type Verdict struct {
	Verdict         string    `json:"verdict"`
	Findings        []Finding `json:"findings"`
	Evidence        string    `json:"evidence"`
	SpecConformance string    `json:"spec_conformance"`
}

type Finding struct {
	Severity string `json:"severity"`
	File     string `json:"file"`
	Note     string `json:"note"`
}

type GitClient interface {
	FetchOriginBase(ctx context.Context, repoPath, baseBranch string) error
	FetchPRHead(ctx context.Context, repoPath string, prNumber int) error
	WorktreeAddDetachedAt(ctx context.Context, repoPath, worktreePath, rev string) error
	WorktreeRemove(ctx context.Context, repoPath, worktreePath string) error
	Show(ctx context.Context, repoPath, revPath string) (string, error)
}

type GitHubClient interface {
	ViewPR(ctx context.Context, number int) (gh.PullRequest, error)
	ViewIssue(ctx context.Context, number int) (gh.Issue, error)
	PRDiff(ctx context.Context, number int) (string, error)
	PRDiffNameOnly(ctx context.Context, number int) ([]string, error)
}

type Lock interface {
	Release() error
}

type Deps struct {
	Git         GitClient
	GitHub      func(repoPath string) GitHubClient
	AgentLookup func(provider string) (agent.Runner, error)
	AcquireLock func(repoPath string, timeout time.Duration) (Lock, error)
	MkdirTemp   func(dir, pattern string) (string, error)
	RemoveAll   func(path string) error
}

type reviewInputs struct {
	PR           gh.PullRequest
	Issue        gh.Issue
	IssuePresent bool
	Diff         string
	ChangedFiles []string
	Spec         specInput
}

type specInput struct {
	Path      string
	Content   string
	Available bool
	Reason    string
}

func DefaultDeps() Deps {
	return Deps{
		Git: gitutil.New(),
		GitHub: func(repoPath string) GitHubClient {
			return gh.New(repoPath)
		},
		AgentLookup: agent.Lookup,
		AcquireLock: func(repoPath string, timeout time.Duration) (Lock, error) {
			return lockfile.Acquire(repoPath, timeout)
		},
		MkdirTemp: os.MkdirTemp,
		RemoveAll: os.RemoveAll,
	}
}

func Run(ctx context.Context, opts Options, deps Deps) (Result, error) {
	deps = withDefaults(deps)
	warnings := opts.Stderr
	if warnings == nil {
		warnings = io.Discard
	}

	if opts.PRNumber <= 0 {
		return Result{}, errors.New("pull request number is required")
	}
	if strings.TrimSpace(opts.Provider) == "" {
		return Result{}, errors.New("provider is required")
	}
	if strings.TrimSpace(opts.BaseBranch) == "" {
		opts.BaseBranch = "main"
	}

	repoPath, err := resolveRepo(opts.RepoPath)
	if err != nil {
		return Result{}, err
	}
	runner, err := deps.AgentLookup(opts.Provider)
	if err != nil {
		return Result{}, err
	}
	if runner == nil {
		return Result{}, fmt.Errorf("provider %q resolved to nil runner", opts.Provider)
	}
	github := deps.GitHub(repoPath)
	if github == nil {
		return Result{}, errors.New("github client is not configured")
	}

	scratchPath, err := deps.MkdirTemp("", "loopcoder-loopreview-*")
	if err != nil {
		return Result{}, fmt.Errorf("create scratch directory: %w", err)
	}
	worktreePath := filepath.Join(scratchPath, "wt")
	logPath := filepath.Join(scratchPath, "loopreview.log")
	defer cleanup(deps, warnings, repoPath, worktreePath, scratchPath)

	if err := deps.Git.FetchOriginBase(ctx, repoPath, opts.BaseBranch); err != nil {
		return Result{}, fmt.Errorf("git fetch origin %s: %w", opts.BaseBranch, err)
	}
	inputs, err := gatherInputs(ctx, deps, github, repoPath, opts)
	if err != nil {
		return Result{}, err
	}
	if err := checkoutPRWorktree(ctx, deps, repoPath, worktreePath, opts.PRNumber); err != nil {
		return Result{}, err
	}

	agentResult, agentErr := runner.Run(ctx, agent.Invocation{
		WorktreePath: worktreePath,
		Prompt:       BuildPrompt(opts, inputs),
		ReadOnly:     true,
		OutputSchema: VerdictJSONSchema,
		LogPath:      logPath,
	})
	if agentErr != nil {
		verdict := needsHumanVerdict("error", "", fmt.Sprintf("%s verifier failed: %v", opts.Provider, agentErr))
		return Result{Verdict: verdict, ExitCode: ExitCodeForVerdict(verdict.Verdict)}, nil
	}
	if agentResult.ExitCode != 0 {
		verdict := needsHumanVerdict("error", "", fmt.Sprintf("%s verifier exited with code %d; see %s", opts.Provider, agentResult.ExitCode, logPath))
		return Result{Verdict: verdict, ExitCode: ExitCodeForVerdict(verdict.Verdict)}, nil
	}

	verdict, err := ParseVerdict(agentResult.Summary)
	if err != nil {
		verdict = needsHumanVerdict("error", "", fmt.Sprintf("structured verdict parse failed: %v", err))
		return Result{Verdict: verdict, ExitCode: ExitCodeForVerdict(verdict.Verdict)}, nil
	}
	if !inputs.Spec.Available {
		verdict.Verdict = VerdictNeedsHuman
		verdict.SpecConformance = SpecConformanceNotApplicable
		verdict.Findings = append(verdict.Findings, Finding{
			Severity: "warning",
			File:     inputs.Spec.Path,
			Note:     "merged design/spec unavailable: " + inputs.Spec.Reason,
		})
	}
	verdict.Findings = nonNilFindings(verdict.Findings)
	return Result{Verdict: verdict, ExitCode: ExitCodeForVerdict(verdict.Verdict)}, nil
}

func Render(w io.Writer, result Result) error {
	data, err := json.Marshal(result.Verdict)
	if err != nil {
		return err
	}
	if _, err := w.Write(append(data, '\n')); err != nil {
		return err
	}
	return nil
}

func ParseVerdict(raw string) (Verdict, error) {
	var payload struct {
		Verdict         *string    `json:"verdict"`
		Findings        *[]Finding `json:"findings"`
		Evidence        *string    `json:"evidence"`
		SpecConformance *string    `json:"spec_conformance"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &payload); err != nil {
		return Verdict{}, fmt.Errorf("parse verdict JSON: %w", err)
	}
	if payload.Verdict == nil {
		return Verdict{}, errors.New("missing verdict")
	}
	if payload.Findings == nil {
		return Verdict{}, errors.New("missing findings")
	}
	if payload.Evidence == nil {
		return Verdict{}, errors.New("missing evidence")
	}
	if payload.SpecConformance == nil {
		return Verdict{}, errors.New("missing spec_conformance")
	}

	verdict := strings.TrimSpace(*payload.Verdict)
	if !validVerdict(verdict) {
		return Verdict{}, fmt.Errorf("invalid verdict %q", verdict)
	}
	specConformance := strings.TrimSpace(*payload.SpecConformance)
	if !validSpecConformance(specConformance) {
		return Verdict{}, fmt.Errorf("invalid spec_conformance %q", specConformance)
	}
	evidence := strings.TrimSpace(*payload.Evidence)
	if evidence == "" {
		return Verdict{}, errors.New("empty evidence")
	}

	findings := nonNilFindings(*payload.Findings)
	for i, finding := range findings {
		if strings.TrimSpace(finding.Severity) == "" {
			return Verdict{}, fmt.Errorf("finding %d missing severity", i)
		}
		if strings.TrimSpace(finding.Note) == "" {
			return Verdict{}, fmt.Errorf("finding %d missing note", i)
		}
		findings[i].Severity = strings.TrimSpace(finding.Severity)
		findings[i].File = strings.TrimSpace(finding.File)
		findings[i].Note = strings.TrimSpace(finding.Note)
	}

	return Verdict{
		Verdict:         verdict,
		Findings:        findings,
		Evidence:        evidence,
		SpecConformance: specConformance,
	}, nil
}

func BuildPrompt(opts Options, inputs reviewInputs) string {
	issueTitle := "(issue unavailable)"
	issueBody := "(issue body unavailable)"
	issueNumber := "(unknown)"
	if inputs.IssuePresent {
		issueNumber = fmt.Sprintf("#%d", inputs.Issue.Number)
		issueTitle = inputs.Issue.Title
		issueBody = inputs.Issue.Body
	} else if strings.TrimSpace(inputs.Issue.Title) != "" || strings.TrimSpace(inputs.Issue.Body) != "" {
		issueTitle = inputs.Issue.Title
		issueBody = inputs.Issue.Body
	}

	specText := fmt.Sprintf("Unavailable: %s", inputs.Spec.Reason)
	if inputs.Spec.Available {
		specText = fmt.Sprintf("Path: %s\n\n%s", inputs.Spec.Path, inputs.Spec.Content)
	}

	return fmt.Sprintf(`You are the independent loopcoder Verifier for pull request #%d.

Review adversarially. You are not the implementation worker. Run only read-only inspections or checks. Do not modify files, commit, push, or write review comments.

Return only JSON matching this schema:

%s

# Review contract
- Compare the diff against the GitHub issue, acceptance criteria, and merged design/spec.
- Use "pass" only when the PR satisfies the issue and spec and you found no blocking concerns.
- Use "fail" for concrete implementation defects, missing acceptance criteria, regressions, or test gaps that should be fixed by a worker.
- Use "needs-human" when evidence is incomplete, ambiguous, unavailable, or unsafe to decide automatically.
- Include concise findings with severity, file when applicable, and note.

# PR
Number: #%d
Title: %s
Head: %s

# Changed files
%s

# Diff
%s

# Issue
Number: %s
Title: %s

%s

# Merged design/spec from origin/%s
%s
`, opts.PRNumber, VerdictJSONSchema, opts.PRNumber, inputs.PR.Title, inputs.PR.HeadRefName, formatChangedFiles(inputs.ChangedFiles), inputs.Diff, issueNumber, issueTitle, issueBody, opts.BaseBranch, specText)
}

const VerdictJSONSchema = `{"type":"object","additionalProperties":false,"required":["verdict","findings","evidence","spec_conformance"],"properties":{"verdict":{"type":"string","enum":["pass","fail","needs-human"]},"findings":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["severity","file","note"],"properties":{"severity":{"type":"string"},"file":{"type":"string"},"note":{"type":"string"}}},"evidence":{"type":"string"},"spec_conformance":{"type":"string","enum":["pass","fail","not-applicable"]}}}`

func gatherInputs(ctx context.Context, deps Deps, github GitHubClient, repoPath string, opts Options) (reviewInputs, error) {
	pr, err := github.ViewPR(ctx, opts.PRNumber)
	if err != nil {
		return reviewInputs{}, fmt.Errorf("gh pr view %d: %w", opts.PRNumber, err)
	}
	diff, err := github.PRDiff(ctx, opts.PRNumber)
	if err != nil {
		return reviewInputs{}, fmt.Errorf("gh pr diff %d: %w", opts.PRNumber, err)
	}
	changedFiles, err := github.PRDiffNameOnly(ctx, opts.PRNumber)
	if err != nil {
		return reviewInputs{}, fmt.Errorf("gh pr diff %d --name-only: %w", opts.PRNumber, err)
	}

	inputs := reviewInputs{
		PR:           pr,
		Diff:         diff,
		ChangedFiles: changedFiles,
	}
	issue, present := loadIssue(ctx, github, pr)
	inputs.Issue = issue
	inputs.IssuePresent = present
	inputs.Spec = loadSpec(ctx, deps.Git, repoPath, opts.BaseBranch, specSearchTexts(issue, present, pr))
	return inputs, nil
}

func loadIssue(ctx context.Context, github GitHubClient, pr gh.PullRequest) (gh.Issue, bool) {
	for _, ref := range pr.ClosingIssuesReferences {
		if ref.Number <= 0 {
			continue
		}
		issue, err := github.ViewIssue(ctx, ref.Number)
		if err == nil {
			return issue, true
		}
	}
	return gh.Issue{
		Title: pr.Title,
		Body:  pr.Body,
	}, false
}

func specSearchTexts(issue gh.Issue, issuePresent bool, pr gh.PullRequest) []string {
	texts := []string{}
	if issuePresent {
		texts = append(texts, issue.Body, issue.Title)
	}
	texts = append(texts, pr.Body, pr.Title)
	return texts
}

func loadSpec(ctx context.Context, git GitClient, repoPath, baseBranch string, texts []string) specInput {
	path := discoverSpecPath(texts...)
	if path == "" {
		return specInput{Available: false, Reason: "no docs/*.md reference discovered"}
	}
	content, err := git.Show(ctx, repoPath, "origin/"+baseBranch+":"+path)
	if err != nil {
		return specInput{Path: path, Available: false, Reason: err.Error()}
	}
	if strings.TrimSpace(content) == "" {
		return specInput{Path: path, Available: false, Reason: "spec file is empty"}
	}
	return specInput{Path: path, Content: content, Available: true}
}

func checkoutPRWorktree(ctx context.Context, deps Deps, repoPath, worktreePath string, prNumber int) error {
	lock, err := deps.AcquireLock(repoPath, 60*time.Second)
	if err != nil {
		return err
	}
	fetchErr := deps.Git.FetchPRHead(ctx, repoPath, prNumber)
	var addErr error
	if fetchErr == nil {
		addErr = deps.Git.WorktreeAddDetachedAt(ctx, repoPath, worktreePath, "FETCH_HEAD")
	}
	releaseErr := lock.Release()
	if fetchErr != nil {
		return fmt.Errorf("git fetch PR head: %w", fetchErr)
	}
	if addErr != nil {
		return fmt.Errorf("git worktree add: %w", addErr)
	}
	if releaseErr != nil {
		return releaseErr
	}
	return nil
}

func cleanup(deps Deps, warnings io.Writer, repoPath, worktreePath, scratchPath string) {
	if strings.TrimSpace(worktreePath) != "" {
		if info, err := os.Stat(worktreePath); err == nil && info.IsDir() {
			if err := deps.Git.WorktreeRemove(context.Background(), repoPath, worktreePath); err != nil {
				fmt.Fprintf(warnings, "[loopcoder] warning: failed to remove verifier worktree %s: %v\n", worktreePath, err)
			}
		}
	}
	if strings.TrimSpace(scratchPath) != "" {
		if err := deps.RemoveAll(scratchPath); err != nil {
			fmt.Fprintf(warnings, "[loopcoder] warning: failed to remove scratch directory %s: %v\n", scratchPath, err)
		}
	}
}

func withDefaults(deps Deps) Deps {
	defaults := DefaultDeps()
	if deps.Git == nil {
		deps.Git = defaults.Git
	}
	if deps.GitHub == nil {
		deps.GitHub = defaults.GitHub
	}
	if deps.AgentLookup == nil {
		deps.AgentLookup = defaults.AgentLookup
	}
	if deps.AcquireLock == nil {
		deps.AcquireLock = defaults.AcquireLock
	}
	if deps.MkdirTemp == nil {
		deps.MkdirTemp = defaults.MkdirTemp
	}
	if deps.RemoveAll == nil {
		deps.RemoveAll = defaults.RemoveAll
	}
	return deps
}

func resolveRepo(repoPath string) (string, error) {
	if strings.TrimSpace(repoPath) == "" {
		return "", errors.New("repo path is required")
	}
	absolute, err := filepath.Abs(repoPath)
	if err != nil {
		return "", fmt.Errorf("resolve repo path: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", fmt.Errorf("resolve repo path: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("repo path is not a directory: %s", absolute)
	}
	return absolute, nil
}

func needsHumanVerdict(severity, file, note string) Verdict {
	return Verdict{
		Verdict: VerdictNeedsHuman,
		Findings: []Finding{{
			Severity: severity,
			File:     file,
			Note:     note,
		}},
		Evidence:        note,
		SpecConformance: SpecConformanceNotApplicable,
	}
}

func ExitCodeForVerdict(verdict string) int {
	switch verdict {
	case VerdictPass:
		return 0
	case VerdictFail:
		return 1
	default:
		return 2
	}
}

func validVerdict(verdict string) bool {
	switch verdict {
	case VerdictPass, VerdictFail, VerdictNeedsHuman:
		return true
	default:
		return false
	}
}

func validSpecConformance(value string) bool {
	switch value {
	case SpecConformancePass, SpecConformanceFail, SpecConformanceNotApplicable:
		return true
	default:
		return false
	}
}

func nonNilFindings(findings []Finding) []Finding {
	if findings == nil {
		return []Finding{}
	}
	return findings
}

func formatChangedFiles(files []string) string {
	if len(files) == 0 {
		return "(none)"
	}
	lines := make([]string, 0, len(files))
	for _, file := range files {
		if strings.TrimSpace(file) != "" {
			lines = append(lines, "- "+strings.TrimSpace(file))
		}
	}
	if len(lines) == 0 {
		return "(none)"
	}
	return strings.Join(lines, "\n")
}

var specPathPattern = regexp.MustCompile(`docs/[A-Za-z0-9._/-]+\.md`)

func discoverSpecPath(texts ...string) string {
	seen := map[string]bool{}
	candidates := []string{}
	for _, text := range texts {
		for _, match := range specPathPattern.FindAllString(strings.ReplaceAll(text, `\`, `/`), -1) {
			cleaned := strings.Trim(match, ".,;:)]}")
			if cleaned == "" || seen[cleaned] {
				continue
			}
			seen[cleaned] = true
			candidates = append(candidates, cleaned)
		}
	}
	for _, candidate := range candidates {
		if strings.HasPrefix(candidate, "docs/specs/") {
			return candidate
		}
	}
	if len(candidates) > 0 {
		return candidates[0]
	}
	return ""
}
