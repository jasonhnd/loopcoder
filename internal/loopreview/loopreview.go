// Package loopreview runs an independent read-only verifier for a pull request.
package loopreview

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jasonhnd/loopcoder/internal/agent"
	"github.com/jasonhnd/loopcoder/internal/config"
	lcdefaults "github.com/jasonhnd/loopcoder/internal/defaults"
	"github.com/jasonhnd/loopcoder/internal/gitutil"
	"github.com/jasonhnd/loopcoder/internal/lockfile"
	"github.com/jasonhnd/loopcoder/internal/mcp"
	"github.com/jasonhnd/loopcoder/internal/reporter"
	"github.com/jasonhnd/loopcoder/internal/supervisedexec"
	gh "github.com/jasonhnd/loopcoder/internal/vcs/github"
)

const (
	VerdictPass       = "pass"
	VerdictFail       = "fail"
	VerdictNeedsHuman = "needs-human"

	SpecConformancePass          = "pass"
	SpecConformanceFail          = "fail"
	SpecConformanceNotApplicable = "not-applicable"

	DefaultVerifierTimeout = lcdefaults.VerifierTimeout
	VerifierStallTimeout   = lcdefaults.VerifierStallTimeout

	reviewPacketChangedFilesBudgetBytes = lcdefaults.ReviewPacketChangedFilesBudgetBytes
	reviewPacketDiffBudgetBytes         = lcdefaults.ReviewPacketDiffBudgetBytes
	reviewPacketDiffFileBudgetBytes     = lcdefaults.ReviewPacketDiffFileBudgetBytes
	reviewPacketDocBodyFileBytes        = lcdefaults.ReviewPacketDocumentationBodyFileBytes
	reviewPacketDocBodyTotalBytes       = lcdefaults.ReviewPacketDocumentationBodyTotalBytes
	reviewPacketDocBodyMaxFiles         = lcdefaults.ReviewPacketDocumentationBodyMaxFiles
	reviewPacketGeneratedDiffFileBytes  = lcdefaults.ReviewPacketGeneratedDiffFileBytes
	reviewPacketGeneratedSizeBytes      = lcdefaults.ReviewPacketGeneratedSizeBytes
	reviewPacketIssueBudgetBytes        = lcdefaults.ReviewPacketIssueBudgetBytes
	reviewPacketRenderedArtifactBytes   = lcdefaults.ReviewPacketRenderedArtifactBudgetBytes
	reviewPacketRubricBudgetBytes       = lcdefaults.ReviewPacketRubricBudgetBytes
	reviewPacketSpecBudgetBytes         = lcdefaults.ReviewPacketSpecBudgetBytes
	reviewPacketTotalPromptBudgetBytes  = lcdefaults.ReviewPacketTotalPromptBudgetBytes
	renderedArtifactFileBudgetBytes     = lcdefaults.RenderedArtifactFileBudgetBytes
	renderedArtifactMaxDirectoryFiles   = lcdefaults.RenderedArtifactMaxDirectoryFiles
	renderedArtifactProducerTimeout     = lcdefaults.RenderedArtifactProducerTimeout
	producerFailureLogBudgetBytes       = lcdefaults.ProducerFailureLogBudgetBytes
	providerFailureLogBudgetBytes       = lcdefaults.ProviderFailureLogBudgetBytes
)

var defaultGeneratedPatterns = lcdefaults.ReviewPacketGeneratedPatterns()

type Options struct {
	RepoPath       string
	PRNumber       int
	Provider       string
	Model          string
	Effort         string
	BaseBranch     string
	ConfigFromBase bool
	Timeout        time.Duration
	Stderr         io.Writer
	// BeforeProviderCall runs after all provider-free preflight work and just
	// before the verifier runner. Returning an error prevents provider launch.
	BeforeProviderCall func() error
}

type Result struct {
	Verdict         Verdict
	ExitCode        int
	ProviderInvoked bool
}

type Verdict struct {
	Verdict           string             `json:"verdict"`
	Findings          []Finding          `json:"findings"`
	Evidence          string             `json:"evidence"`
	SpecConformance   string             `json:"spec_conformance"`
	Reason            string             `json:"reason,omitempty"`
	NextAction        string             `json:"next_action,omitempty"`
	RenderedArtifacts []RenderedArtifact `json:"rendered_artifacts,omitempty"`
	Report            *reporter.Report   `json:"report,omitempty"`
}

type RenderedArtifact struct {
	Source         string `json:"source"`
	Status         string `json:"status"`
	DeclaredOutput string `json:"declared_output,omitempty"`
	Path           string `json:"path,omitempty"`
	Kind           string `json:"kind,omitempty"`
	MediaType      string `json:"media_type,omitempty"`
	Bytes          int64  `json:"bytes,omitempty"`
	SHA256         string `json:"sha256,omitempty"`
	Files          int    `json:"files,omitempty"`
	Truncated      bool   `json:"truncated,omitempty"`
	Summary        string `json:"summary,omitempty"`
	Error          string `json:"error,omitempty"`
}

type Finding struct {
	Severity string `json:"severity"`
	File     string `json:"file"`
	Note     string `json:"note"`
}

type GitClient interface {
	FetchOriginBase(ctx context.Context, repoPath, baseBranch string) error
	FetchPRHead(ctx context.Context, repoPath string, prNumber int) error
	FetchPRHeadRef(ctx context.Context, repoPath string, prNumber int, destRef string) error
	WorktreeAddDetachedAt(ctx context.Context, repoPath, worktreePath, rev string) error
	WorktreeRemove(ctx context.Context, repoPath, worktreePath string) error
	RevParse(ctx context.Context, repoPath, rev string) (string, error)
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
	Git                 GitClient
	GitHub              func(repoPath string) GitHubClient
	AgentLookup         func(provider string) (agent.Runner, error)
	AcquireLock         func(repoPath string, timeout time.Duration) (Lock, error)
	MkdirTemp           func(dir, pattern string) (string, error)
	RemoveAll           func(path string) error
	RunEvidenceProducer EvidenceProducerRunner
	ReviewPacketLimits  ReviewPacketLimits
}

type EvidenceProducerInvocation struct {
	Command      string
	Argv         []string
	WorktreePath string
	Timeout      time.Duration
}

type EvidenceProducerResult struct {
	ExitCode int
	Output   string
	TimedOut bool
	Err      error
}

type EvidenceProducerRunner func(ctx context.Context, invocation EvidenceProducerInvocation) EvidenceProducerResult

type ReviewPacketLimits struct {
	ChangedFilesBytes          int
	DiffBytes                  int
	DiffFileBytes              int
	DocumentationBodyFileBytes int
	DocumentationBodyMaxFiles  int
	DocumentationBodyBytes     int
	GeneratedDiffFileBytes     int
	GeneratedSizeBytes         int
	GeneratedPatterns          []string
	IssueBytes                 int
	RenderedArtifactBytes      int
	RubricBytes                int
	SpecBytes                  int
	TotalPromptBytes           int
}

type reviewInputs struct {
	PR                      gh.PullRequest
	Refs                    reviewRefs
	Issue                   gh.Issue
	IssuePresent            bool
	Diff                    string
	ChangedFiles            []string
	GeneratedAttributeRules []generatedAttributeRule
	PRHeadFileBodies        []prHeadFileBodyInput
	Spec                    specInput
	Rubric                  rubricInput
	RenderedArtifacts       []renderedArtifactInput
	ReviewPacketOrder       []string
}

type reviewRefs struct {
	PRNumber           int
	BaseBranch         string
	BaseSHA            string
	HeadBranch         string
	HeadSHA            string
	PRHeadFileSource   prHeadFileSource
	VerificationReason string
}

type prHeadFileSource struct {
	Ref      string
	Verified bool
	Reason   string
}

type prHeadFileContent struct {
	Path      string
	SourceRef string
	Content   string
}

type prHeadFileBodyInput struct {
	Path      string
	SourceRef string
	Content   string
	Available bool
	Reason    string
}

type specInput struct {
	Path           string
	Content        string
	Available      bool
	Reason         string
	ExpectedAbsent bool
	ExpectedReason string
}

type rubricInput struct {
	Configured bool
	Checklist  []string
	Files      []rubricFileInput
}

type rubricFileInput struct {
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
		MkdirTemp:           os.MkdirTemp,
		RemoveAll:           os.RemoveAll,
		RunEvidenceProducer: runEvidenceProducerCommand,
	}
}

func Run(ctx context.Context, opts Options, deps Deps) (Result, error) {
	deps = withDefaults(deps)
	warnings := opts.Stderr
	if warnings == nil {
		warnings = io.Discard
	}
	opts.Stderr = warnings

	if opts.PRNumber <= 0 {
		return Result{}, errors.New("pull request number is required")
	}
	if strings.TrimSpace(opts.Provider) == "" {
		return Result{}, errors.New("provider is required")
	}
	if strings.TrimSpace(opts.BaseBranch) == "" {
		opts.BaseBranch = lcdefaults.BaseBranch
	}
	repoPath, err := resolveRepo(opts.RepoPath)
	if err != nil {
		return Result{}, err
	}
	github := deps.GitHub(repoPath)
	if github == nil {
		return Result{}, errors.New("github client is not configured")
	}
	pr, err := github.ViewPR(ctx, opts.PRNumber)
	if err != nil {
		return Result{}, fmt.Errorf("gh pr view %d: %w", opts.PRNumber, err)
	}
	refs := reviewRefsFromPR(opts.PRNumber, opts.BaseBranch, pr)
	opts.BaseBranch = refs.BaseBranch
	refs, err = prepareReviewRefs(ctx, deps, repoPath, refs)
	if err != nil {
		verdict := needsHumanVerdict("warning", "", "review refs unavailable: "+err.Error())
		return Result{Verdict: verdict, ExitCode: ExitCodeForVerdict(verdict.Verdict)}, nil
	}
	cfg, err := config.LoadForRepo(ctx, repoPath, config.LoadOptions{
		BaseBranch:     opts.BaseBranch,
		ConfigFromBase: opts.ConfigFromBase,
		Warnings:       warnings,
	})
	if err != nil {
		return Result{}, err
	}
	resilience := cfg.Resilience
	if opts.Timeout <= 0 {
		opts.Timeout = config.DurationSeconds(resilience.Verifier.HardCapSeconds, DefaultVerifierTimeout)
	}
	runner, err := deps.AgentLookup(opts.Provider)
	if err != nil {
		return Result{}, err
	}
	if runner == nil {
		return Result{}, fmt.Errorf("provider %q resolved to nil runner", opts.Provider)
	}

	scratchPath, err := deps.MkdirTemp("", "loopcoder-loopreview-*")
	if err != nil {
		return Result{}, fmt.Errorf("create scratch directory: %w", err)
	}
	worktreePath := filepath.Join(scratchPath, "wt")
	logPath := filepath.Join(scratchPath, "loopreview.log")
	defer cleanup(deps, warnings, repoPath, worktreePath, scratchPath)

	inputs, err := gatherInputs(ctx, deps, github, repoPath, opts, pr, refs)
	if err != nil {
		return Result{}, err
	}
	inputs.Rubric = loadRubric(ctx, deps.Git, repoPath, opts.BaseBranch, cfg.Domain.Verification.Rubric)
	inputs.RenderedArtifacts = configuredRenderedArtifacts(cfg)
	inputs.ReviewPacketOrder = cfg.Domain.Verification.ReviewPacketOrder
	producer := cfg.Domain.Evidence.Producer
	if evidenceProducerConfigured(producer) {
		if err := checkoutPRWorktree(ctx, deps, repoPath, worktreePath, refs.PRHeadFileSource); err != nil {
			return Result{}, err
		}
		produced, producerErr := runConfiguredEvidenceProducer(ctx, deps, worktreePath, producer, opts.Timeout)
		inputs.RenderedArtifacts = append(inputs.RenderedArtifacts, produced...)
		if producerErr != nil {
			verdict := needsHumanVerdict("warning", "", producerErr.Error())
			verdict.RenderedArtifacts = publicRenderedArtifacts(inputs.RenderedArtifacts)
			return Result{Verdict: verdict, ExitCode: ExitCodeForVerdict(verdict.Verdict)}, nil
		}
	}
	prompt, packet := buildPromptWithLimits(opts, inputs, deps.ReviewPacketLimits)
	if packet.Insufficient {
		note := "review packet insufficient: " + packet.InsufficientReason
		verdict := needsHumanVerdict("warning", "", note)
		verdict.RenderedArtifacts = publicRenderedArtifacts(inputs.RenderedArtifacts)
		return Result{Verdict: verdict, ExitCode: ExitCodeForVerdict(verdict.Verdict)}, nil
	}
	if !evidenceProducerConfigured(producer) {
		if err := checkoutPRWorktree(ctx, deps, repoPath, worktreePath, refs.PRHeadFileSource); err != nil {
			return Result{}, err
		}
	}
	mcpServers, err := mcp.ServersForInvocation(cfg.MCP, mcp.RoleVerifier, true)
	if err != nil {
		return Result{}, err
	}

	workID := fmt.Sprintf("loopreview-%d", opts.PRNumber)
	providerInvoked := false
	if opts.BeforeProviderCall != nil {
		if err := opts.BeforeProviderCall(); err != nil {
			return Result{}, fmt.Errorf("before verifier provider call: %w", agent.ProviderCallRefusedError{Err: err})
		}
	}
	agentResult, agentErr := runner.Run(ctx, agent.Invocation{
		WorktreePath: worktreePath,
		Prompt:       prompt,
		Model:        opts.Model,
		Effort:       opts.Effort,
		ReadOnly:     true,
		OutputSchema: VerdictJSONSchema,
		LogPath:      logPath,
		Stderr:       warnings,
		HardCap:      opts.Timeout,
		StallTimeout: config.DurationSeconds(resilience.Verifier.StallTimeoutSeconds, VerifierStallTimeout),
		RunID:        workID,
		Role:         "verifier",
		MCPServers:   mcpServers,
		OnProviderLaunch: func(int) {
			providerInvoked = true
		},
		OnProviderStart: func(agent.ProviderProcess) error {
			providerInvoked = true
			return nil
		},
	})
	if agentResult.Hung {
		verdict := verifierHungVerdict(opts.Provider, logPath, opts.Timeout, agentResult.HungReason)
		return Result{Verdict: verdict, ExitCode: ExitCodeForVerdict(verdict.Verdict), ProviderInvoked: providerInvoked}, nil
	}
	record := verifierReport(opts, agentResult, inputs, refs, worktreePath, workID)
	fmt.Fprintln(warnings, record.Header())
	if agentErr != nil {
		verdict := needsHumanVerdict("error", "", providerFailureNote(logPath, fmt.Sprintf("%s verifier failed: %v", opts.Provider, agentErr)))
		return resultWithProviderEvidence(verdict, record, providerInvoked), nil
	}
	if agentResult.ExitCode != 0 {
		verdict := needsHumanVerdict("error", "", providerFailureNote(logPath, fmt.Sprintf("%s verifier exited with code %d; see %s", opts.Provider, agentResult.ExitCode, logPath)))
		return resultWithProviderEvidence(verdict, record, providerInvoked), nil
	}

	verdict, err := ParseVerdict(agentResult.Summary)
	if err != nil {
		verdict = needsHumanVerdict("error", "", fmt.Sprintf("structured verdict parse failed: %v", err))
		verdict.RenderedArtifacts = publicRenderedArtifacts(inputs.RenderedArtifacts)
		return resultWithProviderEvidence(verdict, record, providerInvoked), nil
	}
	verdict.RenderedArtifacts = publicRenderedArtifacts(inputs.RenderedArtifacts)
	if inputs.Spec.ExpectedAbsent {
		verdict.SpecConformance = SpecConformanceNotApplicable
	} else if !inputs.Spec.Available {
		verdict.Verdict = VerdictNeedsHuman
		verdict.SpecConformance = SpecConformanceNotApplicable
		verdict.Findings = append(verdict.Findings, Finding{
			Severity: "warning",
			File:     inputs.Spec.Path,
			Note:     "merged design/spec unavailable: " + inputs.Spec.Reason,
		})
	}
	if note := missingRubricEvidenceNote(packet.Rubric); note != "" {
		verdict.Verdict = VerdictNeedsHuman
		verdict.Findings = append(nonNilFindings(verdict.Findings), Finding{
			Severity: "warning",
			File:     firstMissingRubricPath(packet.Rubric),
			Note:     note,
		})
		appendVerdictEvidence(&verdict, note)
	}
	verdict.Findings = nonNilFindings(verdict.Findings)
	return resultWithProviderEvidence(verdict, record, providerInvoked), nil
}

func resultWithProviderEvidence(verdict Verdict, record reporter.Report, invoked bool) Result {
	result := resultWithReport(verdict, record)
	result.ProviderInvoked = invoked
	return result
}

func verifierReport(opts Options, result agent.Result, inputs reviewInputs, refs reviewRefs, worktreePath, workID string) reporter.Report {
	issueNumber := 0
	if inputs.IssuePresent {
		issueNumber = inputs.Issue.Number
	}
	return reporter.Report{
		WorkID:      workID,
		Issue:       issueNumber,
		Branch:      refs.HeadBranch,
		Worktree:    worktreePath,
		Role:        reporter.RoleVerifier,
		Provider:    opts.Provider,
		Model:       firstNonEmpty(opts.Model, result.Model),
		ModelSource: reporter.ModelSourceForProvider(opts.Provider),
		Effort:      firstNonEmpty(opts.Effort, result.Effort),
		Permission:  reporter.PermissionReadOnly,
		Action:      providerAttributedReviewAction(fmt.Sprintf("review PR #%d", opts.PRNumber), workID, result),
		ExitCode:    result.ExitCode,
		StartedAt:   result.StartedAt,
		EndedAt:     result.EndedAt,
		DurationMS:  result.DurationMS,
		Usage:       result.Usage,
		Verified:    true,
	}
}

func providerAttributedReviewAction(action, attempt string, result agent.Result) string {
	var parts []string
	if strings.TrimSpace(result.AdapterVersion) != "" {
		parts = append(parts, "adapter="+strings.TrimSpace(result.AdapterVersion))
	}
	if strings.TrimSpace(result.ExternalSessionRef) == "" && len(parts) == 0 {
		return action
	}
	if strings.TrimSpace(attempt) != "" {
		parts = append(parts, "attempt="+strings.TrimSpace(attempt))
	}
	if strings.TrimSpace(result.ExternalSessionRef) != "" {
		parts = append(parts, "session="+strings.TrimSpace(result.ExternalSessionRef))
	}
	if len(parts) == 0 {
		return action
	}
	return action + " [" + strings.Join(parts, " ") + "]"
}

func resultWithReport(verdict Verdict, record reporter.Report) Result {
	verdict.Report = &record
	if err := record.Validate(); err != nil {
		note := "incomplete verifier report: " + err.Error()
		verdict.Verdict = VerdictNeedsHuman
		verdict.SpecConformance = SpecConformanceNotApplicable
		verdict.Findings = append(nonNilFindings(verdict.Findings), Finding{
			Severity: "error",
			File:     "",
			Note:     note,
		})
		if strings.TrimSpace(verdict.Evidence) == "" {
			verdict.Evidence = note
		} else if !strings.Contains(verdict.Evidence, note) {
			verdict.Evidence = strings.TrimSpace(verdict.Evidence) + "\n" + note
		}
	}
	verdict.Findings = nonNilFindings(verdict.Findings)
	verdict = NormalizeVerdict(verdict)
	return Result{Verdict: verdict, ExitCode: ExitCodeForVerdict(verdict.Verdict)}
}

func verifierHungVerdict(provider, logPath string, timeout time.Duration, reason string) Verdict {
	var note string
	switch reason {
	case agent.HungReasonDeadline:
		note = fmt.Sprintf("%s verifier timed out after %s", provider, formatTimeout(timeout))
	case agent.HungReasonStall:
		note = fmt.Sprintf("%s verifier hung (reason=hung hung_reason=stall; silent for %s)", provider, formatTimeout(VerifierStallTimeout))
	default:
		note = fmt.Sprintf("%s verifier hung (reason=hung hung_reason=%s)", provider, firstNonEmpty(reason, "unknown"))
	}
	return needsHumanVerdict("error", "", providerFailureNote(logPath, note))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func reviewRefsFromPR(prNumber int, fallbackBase string, pr gh.PullRequest) reviewRefs {
	baseBranch := strings.TrimSpace(pr.BaseRefName)
	if baseBranch == "" {
		baseBranch = strings.TrimSpace(fallbackBase)
	}
	if baseBranch == "" {
		baseBranch = lcdefaults.BaseBranch
	}
	headBranch := strings.TrimSpace(pr.HeadRefName)
	return reviewRefs{
		PRNumber:   prNumber,
		BaseBranch: baseBranch,
		BaseSHA:    strings.TrimSpace(pr.BaseRefOID),
		HeadBranch: headBranch,
		HeadSHA:    strings.TrimSpace(pr.HeadRefOID),
		PRHeadFileSource: prHeadFileSource{
			Ref: prHeadLocalRef(prNumber),
		},
	}
}

func prepareReviewRefs(ctx context.Context, deps Deps, repoPath string, refs reviewRefs) (reviewRefs, error) {
	if strings.TrimSpace(refs.BaseBranch) == "" {
		refs.BaseBranch = lcdefaults.BaseBranch
	}
	if strings.TrimSpace(refs.PRHeadFileSource.Ref) == "" {
		refs.PRHeadFileSource.Ref = prHeadLocalRef(refs.PRNumber)
	}
	lock, err := deps.AcquireLock(repoPath, 60*time.Second)
	if err != nil {
		return refs, err
	}
	if lock == nil {
		return refs, errors.New("lock acquisition returned nil lock")
	}
	release := true
	defer func() {
		if release {
			_ = lock.Release()
		}
	}()

	if err := deps.Git.FetchOriginBase(ctx, repoPath, refs.BaseBranch); err != nil {
		return refs, fmt.Errorf("git fetch PR base %s: %w", refs.BaseBranch, err)
	}
	fetchedBase, err := deps.Git.RevParse(ctx, repoPath, "origin/"+refs.BaseBranch+"^{commit}")
	if err != nil {
		return refs, fmt.Errorf("verify PR base %s: %w", refs.BaseBranch, err)
	}
	if refs.BaseSHA != "" {
		if !sameCommit(fetchedBase, refs.BaseSHA) {
			return refs, fmt.Errorf("verify PR base %s: fetched origin/%s at %s, GitHub reports %s", refs.BaseBranch, refs.BaseBranch, fetchedBase, refs.BaseSHA)
		}
	} else {
		refs.BaseSHA = fetchedBase
	}
	if err := deps.Git.FetchPRHeadRef(ctx, repoPath, refs.PRNumber, refs.PRHeadFileSource.Ref); err != nil {
		return refs, fmt.Errorf("git fetch PR #%d head into %s: %w", refs.PRNumber, refs.PRHeadFileSource.Ref, err)
	}
	fetchedHead, err := deps.Git.RevParse(ctx, repoPath, refs.PRHeadFileSource.Ref+"^{commit}")
	if err != nil {
		return refs, fmt.Errorf("verify PR #%d head ref %s: %w", refs.PRNumber, refs.PRHeadFileSource.Ref, err)
	}
	if refs.HeadSHA != "" && !sameCommit(fetchedHead, refs.HeadSHA) {
		return refs, fmt.Errorf("verify PR #%d head: fetched %s at %s, GitHub reports %s", refs.PRNumber, refs.PRHeadFileSource.Ref, fetchedHead, refs.HeadSHA)
	}
	refs.PRHeadFileSource.Verified = true
	refs.PRHeadFileSource.Reason = "fetched from GitHub PR head ref"
	if refs.HeadSHA == "" {
		refs.HeadSHA = fetchedHead
	}
	refs.VerificationReason = "base and PR head refs fetched before packet construction"

	releaseErr := lock.Release()
	release = false
	if releaseErr != nil {
		return refs, releaseErr
	}
	return refs, nil
}

func sameCommit(left, right string) bool {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	if left == "" || right == "" {
		return false
	}
	return strings.EqualFold(left, right)
}

func prHeadLocalRef(prNumber int) string {
	return fmt.Sprintf("refs/loopcoder/loopreview/pr-%d-head", prNumber)
}

func readPRHeadFile(ctx context.Context, git GitClient, repoPath string, source prHeadFileSource, rawPath string) (prHeadFileContent, error) {
	if !source.Verified {
		return prHeadFileContent{}, errors.New("PR-head file source ref is not verified")
	}
	sourceRef := strings.TrimSpace(source.Ref)
	if sourceRef == "" {
		return prHeadFileContent{}, errors.New("PR-head file source ref is unavailable")
	}
	cleanPath, err := cleanRepoRelativePath(rawPath)
	if err != nil {
		return prHeadFileContent{}, err
	}
	content, err := git.Show(ctx, repoPath, sourceRef+":"+cleanPath)
	if err != nil {
		return prHeadFileContent{}, err
	}
	return prHeadFileContent{
		Path:      cleanPath,
		SourceRef: sourceRef,
		Content:   content,
	}, nil
}

func loadPRHeadFileBodies(ctx context.Context, git GitClient, repoPath string, refs reviewRefs, changedFiles []string, diff string, limits ReviewPacketLimits) []prHeadFileBodyInput {
	candidates := prHeadFileBodyCandidatePaths(changedFiles, diff)
	if len(candidates) == 0 {
		return nil
	}
	out := make([]prHeadFileBodyInput, 0, len(candidates))
	maxFiles := limits.DocumentationBodyMaxFiles
	for index, candidate := range candidates {
		if maxFiles > 0 && index >= maxFiles {
			out = append(out, prHeadFileBodyInput{
				Path:      candidate,
				Available: false,
				Reason:    fmt.Sprintf("maximum PR-head file body count exceeded before read (limit %d)", maxFiles),
			})
			continue
		}
		content, err := readPRHeadFile(ctx, git, repoPath, refs.PRHeadFileSource, candidate)
		if err != nil {
			out = append(out, prHeadFileBodyInput{
				Path:      candidate,
				Available: false,
				Reason:    err.Error(),
			})
			continue
		}
		if !textualPRHeadBody(content.Content) {
			out = append(out, prHeadFileBodyInput{
				Path:      content.Path,
				SourceRef: content.SourceRef,
				Available: false,
				Reason:    "PR-head file content is not textual UTF-8",
			})
			continue
		}
		out = append(out, prHeadFileBodyInput{
			Path:      content.Path,
			SourceRef: content.SourceRef,
			Content:   content.Content,
			Available: true,
		})
	}
	return out
}

func prHeadFileBodyCandidatePaths(changedFiles []string, diff string) []string {
	documentationOnly := docsOnlyChangedFiles(changedFiles)
	seen := map[string]bool{}
	paths := []string{}
	for _, rawPath := range changedFiles {
		cleanPath, err := cleanRepoRelativePath(rawPath)
		if err != nil {
			continue
		}
		if seen[cleanPath] || !prHeadFileBodyCandidate(cleanPath, diff, documentationOnly) {
			continue
		}
		seen[cleanPath] = true
		paths = append(paths, cleanPath)
	}
	return paths
}

func prHeadFileBodyCandidate(repoPath string, diff string, documentationOnly bool) bool {
	if documentationOnly {
		return documentationBodyTextPath(repoPath)
	}
	return diffAddsPath(diff, repoPath) && documentationDeliverablePath(repoPath)
}

func documentationBodyTextPath(repoPath string) bool {
	repoPath = normalizeRepoPath(repoPath)
	if strings.HasPrefix(repoPath, "docs/") {
		return knownTextBodyExtension(repoPath)
	}
	return documentationDeliverablePath(repoPath)
}

func documentationDeliverablePath(repoPath string) bool {
	switch strings.ToLower(path.Ext(repoPath)) {
	case ".md", ".markdown", ".mdx", ".txt", ".text", ".rst", ".adoc", ".asciidoc":
		return true
	}
	switch strings.ToLower(repoPathBase(repoPath)) {
	case "readme", "changelog", "license", "notice", "security", "contributing", "code_of_conduct":
		return true
	default:
		return false
	}
}

func knownTextBodyExtension(repoPath string) bool {
	switch strings.ToLower(path.Ext(repoPath)) {
	case ".md", ".markdown", ".mdx", ".txt", ".text", ".rst", ".adoc", ".asciidoc", ".json", ".jsonl", ".yaml", ".yml", ".toml", ".xml", ".html", ".htm", ".csv", ".tsv":
		return true
	default:
		return false
	}
}

func textualPRHeadBody(content string) bool {
	return utf8.ValidString(content) && !strings.ContainsRune(content, 0)
}

func Render(w io.Writer, result Result) error {
	result.Verdict = NormalizeVerdict(result.Verdict)
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
		Reason          *string    `json:"reason"`
		NextAction      *string    `json:"next_action"`
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
		Reason:          stringValue(payload.Reason),
		NextAction:      stringValue(payload.NextAction),
	}, nil
}

// NormalizeVerdict fills additive human-decision fields from the stable verdict,
// finding, and evidence fields. It keeps negative/escalated reasons tied to the
// finding that caused the decision instead of positive evidence prose.
func NormalizeVerdict(verdict Verdict) Verdict {
	verdict.Findings = nonNilFindings(verdict.Findings)
	verdict.Reason = DecisionReason(verdict)
	verdict.NextAction = DecisionNextAction(verdict)
	return verdict
}

func DecisionReason(verdict Verdict) string {
	switch verdict.Verdict {
	case VerdictNeedsHuman:
		return reporter.NormalizeDecision(reporter.DecisionInput{
			Status:         verdict.Verdict,
			ExplicitReason: verdict.Reason,
			Findings:       reporterDecisionFindings(verdict.Findings, true),
			FallbackReason: "human judgment is required before continuing",
		}).Reason
	case VerdictFail:
		fallback := firstNonEmpty(verdict.Evidence, "verifier reported a failing verdict")
		if verdict.SpecConformance == SpecConformanceFail {
			fallback = "spec conformance failed"
		}
		return reporter.NormalizeDecision(reporter.DecisionInput{
			Status:         verdict.Verdict,
			ExplicitReason: verdict.Reason,
			Findings:       reporterDecisionFindings(verdict.Findings, true),
			FallbackReason: fallback,
		}).Reason
	case VerdictPass:
		return reporter.NormalizeDecision(reporter.DecisionInput{
			Status:         verdict.Verdict,
			ExplicitReason: verdict.Reason,
			FallbackReason: firstNonEmpty(verdict.Evidence, "acceptance criteria satisfied"),
		}).Reason
	default:
		return reporter.NormalizeDecision(reporter.DecisionInput{
			Status:         verdict.Verdict,
			ExplicitReason: verdict.Reason,
			Findings:       reporterDecisionFindings(verdict.Findings, true),
			FallbackReason: firstNonEmpty(verdict.Evidence, "verifier returned an unrecognized verdict"),
		}).Reason
	}
}

func DecisionNextAction(verdict Verdict) string {
	switch verdict.Verdict {
	case VerdictNeedsHuman:
		return reporter.NormalizeDecision(reporter.DecisionInput{
			Status:             verdict.Verdict,
			ExplicitNextAction: verdict.NextAction,
			FallbackNextAction: "human should decide whether the reported uncertainty is acceptable for this PR",
		}).NextAction
	case VerdictFail:
		return reporter.NormalizeDecision(reporter.DecisionInput{
			Status:             verdict.Verdict,
			ExplicitNextAction: verdict.NextAction,
			FallbackNextAction: "fix the failed gate or regression before continuing",
		}).NextAction
	case VerdictPass:
		return reporter.NormalizeDecision(reporter.DecisionInput{
			Status:             verdict.Verdict,
			ExplicitNextAction: verdict.NextAction,
			FallbackNextAction: "continue with the configured merge or promotion gate",
		}).NextAction
	default:
		return reporter.NormalizeDecision(reporter.DecisionInput{
			Status:             verdict.Verdict,
			ExplicitNextAction: verdict.NextAction,
			FallbackNextAction: "inspect the verifier result before continuing",
		}).NextAction
	}
}

func reporterDecisionFindings(findings []Finding, blocking bool) []reporter.DecisionFinding {
	out := make([]reporter.DecisionFinding, 0, len(findings))
	for _, finding := range findings {
		out = append(out, reporter.DecisionFinding{
			Severity: finding.Severity,
			File:     finding.File,
			Message:  finding.Note,
			Blocking: blocking,
		})
	}
	return out
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

func BuildPrompt(opts Options, inputs reviewInputs) string {
	prompt, _ := buildPromptWithLimits(opts, inputs, ReviewPacketLimits{})
	return prompt
}

type reviewPacket struct {
	PRNumber                 int
	PRTitle                  string
	HeadRef                  string
	HeadSHA                  string
	BaseBranch               string
	BaseSHA                  string
	PRHeadFileSourceRef      string
	PRHeadFileSourceVerified bool
	PRHeadFileSourceReason   string
	IssueNumber              string
	IssueTitle               string
	IssueBody                packetSection
	SpecPath                 string
	SpecAvailable            bool
	SpecReason               string
	SpecExpectedAbsent       bool
	SpecExpectedReason       string
	DocumentationOnly        bool
	SpecPathChanged          bool
	SpecPathAdded            bool
	SpecContent              packetSection
	Rubric                   rubricSection
	RenderedArtifacts        renderedArtifactsSection
	PRHeadFileBodies         prHeadFileBodiesSection
	ChangedFiles             changedFilesSection
	Diff                     packetSection
	ReviewPacketOrder        []string
	Limits                   ReviewPacketLimits
	TotalPromptBudgetApplied bool
	Insufficient             bool
	InsufficientReason       string
}

type packetSection struct {
	Text          string
	OriginalBytes int
	OriginalLines int
	OmittedBytes  int
	OmittedLines  int
	OmittedFiles  []string
	Truncated     bool
}

type changedFilesSection struct {
	Text         string
	TotalFiles   int
	OmittedFiles int
	OmittedBytes int
	OmittedLines int
	Truncated    bool
}

type rubricSection struct {
	Configured     bool
	ChecklistCount int
	FileCount      int
	MissingFiles   []rubricMissingFile
	Content        packetSection
}

type rubricMissingFile struct {
	Path   string
	Reason string
}

type renderedArtifactInput struct {
	Artifact            RenderedArtifact
	Content             packetSection
	IncludeInLoopreview bool
}

type renderedArtifactsSection struct {
	Configured bool
	Artifacts  []renderedArtifactInput
	Content    packetSection
}

type prHeadFileBodiesSection struct {
	CandidateCount int
	IncludedCount  int
	Skipped        []prHeadFileBodySkip
	Content        packetSection
}

type prHeadFileBodySkip struct {
	Path   string
	Reason string
}

func buildPromptWithLimits(opts Options, inputs reviewInputs, limits ReviewPacketLimits) (string, reviewPacket) {
	limits = limits.withDefaults()
	packet := buildReviewPacket(opts, inputs, limits)
	prompt := formatReviewPrompt(opts, packet)

	if limits.TotalPromptBytes <= 0 || len(prompt) <= limits.TotalPromptBytes {
		return prompt, packet
	}

	adjusted := limits
	for i := 0; i < 12 && len(prompt) > limits.TotalPromptBytes; i++ {
		overflow := len(prompt) - limits.TotalPromptBytes
		if !reduceReviewPacketBudgets(&adjusted, overflow) {
			break
		}
		packet = buildReviewPacket(opts, inputs, adjusted)
		packet.TotalPromptBudgetApplied = true
		prompt = formatReviewPrompt(opts, packet)
	}
	if len(prompt) > limits.TotalPromptBytes {
		packet.Insufficient = true
		packet.InsufficientReason = fmt.Sprintf("minimum review packet is %d bytes, exceeding total prompt budget %d bytes", len(prompt), limits.TotalPromptBytes)
	}
	return prompt, packet
}

func buildReviewPacket(opts Options, inputs reviewInputs, limits ReviewPacketLimits) reviewPacket {
	refs := inputs.Refs
	if refs.PRNumber == 0 {
		refs = reviewRefsFromPR(opts.PRNumber, opts.BaseBranch, inputs.PR)
	}
	baseBranch := refs.BaseBranch

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

	specPath := inputs.Spec.Path
	if strings.TrimSpace(specPath) == "" {
		specPath = "(not discovered)"
	}
	specReason := inputs.Spec.Reason
	if inputs.Spec.Available {
		specReason = ""
	}
	documentationOnly := docsOnlyChangedFiles(inputs.ChangedFiles)
	specPathChanged := changedFilesContainPath(inputs.ChangedFiles, inputs.Spec.Path)
	specPathAdded := diffAddsPath(inputs.Diff, inputs.Spec.Path)

	return reviewPacket{
		PRNumber:                 opts.PRNumber,
		PRTitle:                  inputs.PR.Title,
		HeadRef:                  refs.HeadBranch,
		HeadSHA:                  refs.HeadSHA,
		BaseBranch:               baseBranch,
		BaseSHA:                  refs.BaseSHA,
		PRHeadFileSourceRef:      refs.PRHeadFileSource.Ref,
		PRHeadFileSourceVerified: refs.PRHeadFileSource.Verified,
		PRHeadFileSourceReason:   refs.PRHeadFileSource.Reason,
		IssueNumber:              issueNumber,
		IssueTitle:               issueTitle,
		IssueBody:                truncatePacketSection(issueBody, limits.IssueBytes),
		SpecPath:                 specPath,
		SpecAvailable:            inputs.Spec.Available,
		SpecReason:               specReason,
		SpecExpectedAbsent:       inputs.Spec.ExpectedAbsent,
		SpecExpectedReason:       inputs.Spec.ExpectedReason,
		DocumentationOnly:        documentationOnly,
		SpecPathChanged:          specPathChanged,
		SpecPathAdded:            specPathAdded,
		SpecContent:              truncatePacketSection(inputs.Spec.Content, limits.SpecBytes),
		Rubric:                   buildRubricSection(inputs.Rubric, limits.RubricBytes),
		RenderedArtifacts:        buildRenderedArtifactsSection(inputs.RenderedArtifacts, limits.RenderedArtifactBytes),
		PRHeadFileBodies:         buildPRHeadFileBodiesSection(inputs.PRHeadFileBodies, limits),
		ChangedFiles:             buildChangedFilesSection(inputs.ChangedFiles, limits.ChangedFilesBytes),
		Diff:                     buildDiffSection(inputs.Diff, limits, inputs.GeneratedAttributeRules),
		ReviewPacketOrder:        append([]string(nil), inputs.ReviewPacketOrder...),
		Limits:                   limits,
	}
}

func formatReviewPrompt(opts Options, packet reviewPacket) string {
	return fmt.Sprintf(`You are the independent loopcoder Verifier for pull request #%d.

Review adversarially. You are not the implementation worker. Do not modify files, commit, push, write review comments, or run shell commands.

Return only JSON matching this schema:

%s

# Review contract
- Use the bounded review packet below as the primary evidence.
- Compare the bounded diff excerpts and PR-head file content against the GitHub issue, acceptance criteria, merged design/spec, and any configured domain rubric.
- Treat complete PR-head file content as authoritative fallback evidence for that changed file; a TRUNCATED diff marker for the same path is not missing evidence by itself when the complete PR-head body answers the criterion.
- When a Rubric section is configured, apply it as required review criteria.
- When a Rendered artifacts section is configured, treat it as required product evidence for the changed output.
- For complete packets with no relevant TRUNCATED markers, decide from the packet instead of exploring the repository.
- Use "pass" only when the PR satisfies the issue and spec and you found no blocking concerns.
- Use "fail" for concrete implementation defects, missing acceptance criteria, regressions, or test gaps that should be fixed by a worker.
- Use "needs-human" when evidence is incomplete, ambiguous, unavailable, or unsafe to decide automatically.
- Missing configured rubric files are missing evidence; return "needs-human" and cite the missing rubric file paths.
- Missing, failed, or truncated rendered artifacts are missing evidence when they matter to the domain output; return "needs-human" unless the remaining packet is still sufficient to decide safely.
- If the packet classifies a missing merged spec as expected/non-blocking because a documentation-only PR introduces that referenced spec, do not return "needs-human" solely for that expected absence; keep spec_conformance "not-applicable" and decide from the issue and bounded packet.
- For code PRs or mixed code/documentation PRs, an unavailable merged design/spec remains missing evidence and must be treated as "needs-human" when spec conformance cannot be checked safely.
- Return "needs-human" if a TRUNCATED marker could hide a relevant acceptance criterion, risky changed file, or code needed for a safe decision. Cite the marker in evidence.
- Prefer the packet over broad repository exploration. Inspect only changed files, files cited by the issue/spec, or files necessary to confirm a concrete finding.
- Stop and return "needs-human" if deciding safely would require broad repository exploration or work beyond the bounded input/tool budget.
- Return the final JSON in this turn. Do not keep searching for extra confidence once the packet is sufficient.
- Include concise findings with severity, file when applicable, and note.

# Bounded review packet
%s
`, opts.PRNumber, VerdictJSONSchema, formatReviewPacket(packet))
}

const VerdictJSONSchema = `{"type":"object","additionalProperties":false,"required":["verdict","findings","evidence","spec_conformance"],"properties":{"verdict":{"type":"string","enum":["pass","fail","needs-human"]},"findings":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["severity","file","note"],"properties":{"severity":{"type":"string"},"file":{"type":"string"},"note":{"type":"string"}}}},"evidence":{"type":"string"},"spec_conformance":{"type":"string","enum":["pass","fail","not-applicable"]}}}`

func (limits ReviewPacketLimits) withDefaults() ReviewPacketLimits {
	defaults := ReviewPacketLimits{
		ChangedFilesBytes:          reviewPacketChangedFilesBudgetBytes,
		DiffBytes:                  reviewPacketDiffBudgetBytes,
		DiffFileBytes:              reviewPacketDiffFileBudgetBytes,
		DocumentationBodyFileBytes: reviewPacketDocBodyFileBytes,
		DocumentationBodyMaxFiles:  reviewPacketDocBodyMaxFiles,
		DocumentationBodyBytes:     reviewPacketDocBodyTotalBytes,
		GeneratedDiffFileBytes:     reviewPacketGeneratedDiffFileBytes,
		GeneratedSizeBytes:         reviewPacketGeneratedSizeBytes,
		GeneratedPatterns:          defaultGeneratedPatterns,
		IssueBytes:                 reviewPacketIssueBudgetBytes,
		RenderedArtifactBytes:      reviewPacketRenderedArtifactBytes,
		RubricBytes:                reviewPacketRubricBudgetBytes,
		SpecBytes:                  reviewPacketSpecBudgetBytes,
		TotalPromptBytes:           reviewPacketTotalPromptBudgetBytes,
	}
	if limits.ChangedFilesBytes <= 0 {
		limits.ChangedFilesBytes = defaults.ChangedFilesBytes
	}
	if limits.DiffBytes <= 0 {
		limits.DiffBytes = defaults.DiffBytes
	}
	if limits.DiffFileBytes <= 0 {
		limits.DiffFileBytes = defaults.DiffFileBytes
	}
	if limits.DocumentationBodyFileBytes <= 0 {
		limits.DocumentationBodyFileBytes = defaults.DocumentationBodyFileBytes
	}
	if limits.DocumentationBodyMaxFiles <= 0 {
		limits.DocumentationBodyMaxFiles = defaults.DocumentationBodyMaxFiles
	}
	if limits.DocumentationBodyBytes <= 0 {
		limits.DocumentationBodyBytes = defaults.DocumentationBodyBytes
	}
	if limits.GeneratedDiffFileBytes <= 0 {
		limits.GeneratedDiffFileBytes = defaults.GeneratedDiffFileBytes
	}
	if limits.GeneratedSizeBytes <= 0 {
		limits.GeneratedSizeBytes = defaults.GeneratedSizeBytes
	}
	if len(limits.GeneratedPatterns) == 0 {
		limits.GeneratedPatterns = append([]string(nil), defaults.GeneratedPatterns...)
	}
	if limits.IssueBytes <= 0 {
		limits.IssueBytes = defaults.IssueBytes
	}
	if limits.RenderedArtifactBytes <= 0 {
		limits.RenderedArtifactBytes = defaults.RenderedArtifactBytes
	}
	if limits.RubricBytes <= 0 {
		limits.RubricBytes = defaults.RubricBytes
	}
	if limits.SpecBytes <= 0 {
		limits.SpecBytes = defaults.SpecBytes
	}
	if limits.TotalPromptBytes <= 0 {
		limits.TotalPromptBytes = defaults.TotalPromptBytes
	}
	return limits
}

func reduceReviewPacketBudgets(limits *ReviewPacketLimits, bytesToRemove int) bool {
	if bytesToRemove <= 0 {
		bytesToRemove = 1
	}
	reduced := false
	for _, budget := range []*int{
		&limits.DiffBytes,
		&limits.DiffFileBytes,
		&limits.GeneratedDiffFileBytes,
		&limits.SpecBytes,
		&limits.RenderedArtifactBytes,
		&limits.RubricBytes,
		&limits.IssueBytes,
		&limits.ChangedFilesBytes,
		&limits.DocumentationBodyBytes,
		&limits.DocumentationBodyFileBytes,
	} {
		if bytesToRemove <= 0 {
			break
		}
		if *budget <= 0 {
			continue
		}
		reduction := bytesToRemove
		if reduction > *budget {
			reduction = *budget
		}
		*budget -= reduction
		bytesToRemove -= reduction
		reduced = true
	}
	return reduced
}

func formatReviewPacket(packet reviewPacket) string {
	var out strings.Builder
	if packet.TotalPromptBudgetApplied {
		fmt.Fprintf(&out, "[TOTAL PROMPT BUDGET APPLIED: prompt budget %d bytes; excerpts were reduced before provider invocation]\n\n", packet.Limits.TotalPromptBytes)
	}
	fmt.Fprintf(&out, "# PR\n")
	fmt.Fprintf(&out, "Number: #%d\n", packet.PRNumber)
	fmt.Fprintf(&out, "Title: %s\n", packet.PRTitle)
	fmt.Fprintf(&out, "Head: %s\n", packet.HeadRef)
	fmt.Fprintf(&out, "Head SHA: %s\n", firstNonEmpty(packet.HeadSHA, "(unavailable)"))
	fmt.Fprintf(&out, "Base: %s\n", packet.BaseBranch)
	fmt.Fprintf(&out, "Base SHA: %s\n", firstNonEmpty(packet.BaseSHA, "(unavailable)"))
	fmt.Fprintf(&out, "PR-head file source ref: %s\n", firstNonEmpty(packet.PRHeadFileSourceRef, "(unavailable)"))
	fmt.Fprintf(&out, "PR-head file source verified: %s\n", yesNo(packet.PRHeadFileSourceVerified))
	if strings.TrimSpace(packet.PRHeadFileSourceReason) != "" {
		fmt.Fprintf(&out, "PR-head file source note: %s\n", packet.PRHeadFileSourceReason)
	}
	fmt.Fprintf(&out, "\n")

	for _, section := range reviewPacketSections(packet.ReviewPacketOrder, packet.Rubric.Configured, packet.RenderedArtifacts.Configured, packet.PRHeadFileBodies.CandidateCount > 0) {
		switch section {
		case reviewPacketSectionChangedFiles:
			formatChangedFilesPacketSection(&out, packet)
		case reviewPacketSectionDiff:
			formatDiffPacketSection(&out, packet)
		case reviewPacketSectionPRHeadFileContent:
			formatPRHeadFileBodiesPacketSection(&out, packet)
		case reviewPacketSectionIssue:
			formatIssuePacketSection(&out, packet)
		case reviewPacketSectionSpec:
			formatSpecPacketSection(&out, packet)
		case reviewPacketSectionRubric:
			formatRubricPacketSection(&out, packet)
		case reviewPacketSectionRenderedArtifact:
			formatRenderedArtifactsPacketSection(&out, packet)
		}
	}
	return out.String()
}

const (
	reviewPacketSectionChangedFiles      = "changed_files"
	reviewPacketSectionDiff              = "diff"
	reviewPacketSectionPRHeadFileContent = "pr_head_file_content"
	reviewPacketSectionIssue             = "issue"
	reviewPacketSectionSpec              = "spec"
	reviewPacketSectionRubric            = "rubric"
	reviewPacketSectionRenderedArtifact  = "rendered_artifact"
)

var defaultReviewPacketSections = []string{
	reviewPacketSectionChangedFiles,
	reviewPacketSectionDiff,
	reviewPacketSectionPRHeadFileContent,
	reviewPacketSectionIssue,
	reviewPacketSectionSpec,
}

func reviewPacketSections(configured []string, includeRubric bool, includeRenderedArtifacts bool, includePRHeadFileContent bool) []string {
	known := map[string]bool{
		reviewPacketSectionChangedFiles:      true,
		reviewPacketSectionDiff:              true,
		reviewPacketSectionPRHeadFileContent: includePRHeadFileContent,
		reviewPacketSectionIssue:             true,
		reviewPacketSectionSpec:              true,
		reviewPacketSectionRubric:            includeRubric,
		reviewPacketSectionRenderedArtifact:  includeRenderedArtifacts,
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(defaultReviewPacketSections)+1)
	for _, raw := range configured {
		section := normalizeReviewPacketSection(raw)
		if !known[section] || seen[section] {
			continue
		}
		seen[section] = true
		out = append(out, section)
	}
	for _, section := range defaultReviewPacketSections {
		if !known[section] || seen[section] {
			continue
		}
		seen[section] = true
		out = append(out, section)
	}
	if includeRubric && !seen[reviewPacketSectionRubric] {
		out = append(out, reviewPacketSectionRubric)
	}
	if includeRenderedArtifacts && !seen[reviewPacketSectionRenderedArtifact] {
		out = append(out, reviewPacketSectionRenderedArtifact)
	}
	return out
}

func normalizeReviewPacketSection(section string) string {
	return strings.ToLower(strings.TrimSpace(strings.ReplaceAll(section, "-", "_")))
}

func formatChangedFilesPacketSection(out *strings.Builder, packet reviewPacket) {
	fmt.Fprintf(out, "# Changed files\n")
	fmt.Fprintf(out, "Total changed files: %d\n", packet.ChangedFiles.TotalFiles)
	fmt.Fprintf(out, "Documentation-only: %s\n", yesNo(packet.DocumentationOnly))
	fmt.Fprintf(out, "Referenced spec changed in PR: %s\n", yesNo(packet.SpecPathChanged))
	fmt.Fprintf(out, "Referenced spec added in PR: %s\n", yesNo(packet.SpecPathAdded))
	fmt.Fprintf(out, "Budget: %d bytes\n", packet.Limits.ChangedFilesBytes)
	fmt.Fprintf(out, "%s\n\n", formatChangedFilesSection(packet.ChangedFiles))
}

func formatDiffPacketSection(out *strings.Builder, packet reviewPacket) {
	fmt.Fprintf(out, "# Diff excerpts\n")
	fmt.Fprintf(out, "Total diff budget: %d bytes\n", packet.Limits.DiffBytes)
	fmt.Fprintf(out, "Per-file diff budget: %d bytes\n", packet.Limits.DiffFileBytes)
	fmt.Fprintf(out, "%s\n\n", formatPacketSection("diff", packet.Diff))
}

func formatPRHeadFileBodiesPacketSection(out *strings.Builder, packet reviewPacket) {
	if packet.PRHeadFileBodies.CandidateCount == 0 {
		return
	}
	fmt.Fprintf(out, "# PR-head file content\n")
	fmt.Fprintf(out, "Use: complete PR-head file bodies are authoritative fallback evidence for their paths.\n")
	fmt.Fprintf(out, "Candidate bodies: %d\n", packet.PRHeadFileBodies.CandidateCount)
	fmt.Fprintf(out, "Included complete bodies: %d\n", packet.PRHeadFileBodies.IncludedCount)
	fmt.Fprintf(out, "Maximum complete bodies: %d\n", packet.Limits.DocumentationBodyMaxFiles)
	fmt.Fprintf(out, "Per-file body budget: %d bytes\n", packet.Limits.DocumentationBodyFileBytes)
	fmt.Fprintf(out, "Aggregate body budget: %d bytes\n", packet.Limits.DocumentationBodyBytes)
	text := strings.TrimRight(packet.PRHeadFileBodies.Content.Text, "\n")
	if strings.TrimSpace(text) != "" {
		fmt.Fprintf(out, "%s\n", text)
	}
	if len(packet.PRHeadFileBodies.Skipped) > 0 {
		fmt.Fprintf(out, "Skipped PR-head file bodies:\n")
		for _, skipped := range packet.PRHeadFileBodies.Skipped {
			fmt.Fprintf(out, "- %s: %s\n", skipped.Path, skipped.Reason)
		}
	}
	fmt.Fprintf(out, "\n")
}

func formatIssuePacketSection(out *strings.Builder, packet reviewPacket) {
	fmt.Fprintf(out, "# Issue\n")
	fmt.Fprintf(out, "Number: %s\n", packet.IssueNumber)
	fmt.Fprintf(out, "Title: %s\n", packet.IssueTitle)
	fmt.Fprintf(out, "Issue-body budget: %d bytes\n", packet.Limits.IssueBytes)
	fmt.Fprintf(out, "%s\n\n", formatPacketSection("issue body", packet.IssueBody))
}

func formatSpecPacketSection(out *strings.Builder, packet reviewPacket) {
	fmt.Fprintf(out, "# Merged design/spec from origin/%s\n", packet.BaseBranch)
	fmt.Fprintf(out, "Path: %s\n", packet.SpecPath)
	if packet.SpecAvailable {
		fmt.Fprintf(out, "Status: available\n")
		fmt.Fprintf(out, "Spec budget: %d bytes\n", packet.Limits.SpecBytes)
		fmt.Fprintf(out, "%s\n", formatPacketSection("merged spec", packet.SpecContent))
	} else if packet.SpecExpectedAbsent {
		fmt.Fprintf(out, "Status: expected absent from origin/%s\n", packet.BaseBranch)
		fmt.Fprintf(out, "Classification: expected/non-blocking\n")
		fmt.Fprintf(out, "Reason: %s\n", packet.SpecExpectedReason)
		fmt.Fprintf(out, "Spec conformance: not-applicable\n")
	} else {
		fmt.Fprintf(out, "Status: unavailable\n")
		fmt.Fprintf(out, "Classification: missing evidence\n")
		fmt.Fprintf(out, "Reason: %s\n", packet.SpecReason)
	}
	fmt.Fprintf(out, "\n")
}

func formatRubricPacketSection(out *strings.Builder, packet reviewPacket) {
	if !packet.Rubric.Configured {
		return
	}
	fmt.Fprintf(out, "# Rubric\n")
	fmt.Fprintf(out, "Status: %s\n", rubricStatus(packet.Rubric))
	fmt.Fprintf(out, "Checklist items: %d\n", packet.Rubric.ChecklistCount)
	fmt.Fprintf(out, "Configured files: %d\n", packet.Rubric.FileCount)
	fmt.Fprintf(out, "Rubric budget: %d bytes\n", packet.Limits.RubricBytes)
	if len(packet.Rubric.MissingFiles) > 0 {
		fmt.Fprintf(out, "Missing configured rubric files:\n")
		for _, missing := range packet.Rubric.MissingFiles {
			fmt.Fprintf(out, "- %s: %s\n", missing.Path, missing.Reason)
		}
	}
	fmt.Fprintf(out, "%s\n\n", formatPacketSection("rubric", packet.Rubric.Content))
}

func formatRenderedArtifactsPacketSection(out *strings.Builder, packet reviewPacket) {
	if !packet.RenderedArtifacts.Configured {
		return
	}
	fmt.Fprintf(out, "# Rendered artifacts\n")
	fmt.Fprintf(out, "Status: available\n")
	fmt.Fprintf(out, "Configured artifacts: %d\n", len(packet.RenderedArtifacts.Artifacts))
	fmt.Fprintf(out, "Rendered artifact budget: %d bytes\n", packet.Limits.RenderedArtifactBytes)
	fmt.Fprintf(out, "%s\n\n", formatPacketSection("rendered artifacts", packet.RenderedArtifacts.Content))
}

func rubricStatus(rubric rubricSection) string {
	if len(rubric.MissingFiles) > 0 {
		return "missing evidence"
	}
	return "available"
}

func buildRubricSection(input rubricInput, byteBudget int) rubricSection {
	section := rubricSection{
		Configured:     input.Configured,
		ChecklistCount: len(input.Checklist),
		FileCount:      len(input.Files),
	}
	if !input.Configured {
		return section
	}

	var out strings.Builder
	if len(input.Checklist) > 0 {
		out.WriteString("## Inline checklist\n")
		for _, item := range input.Checklist {
			fmt.Fprintf(&out, "- %s\n", item)
		}
	}
	for _, file := range input.Files {
		if !file.Available {
			section.MissingFiles = append(section.MissingFiles, rubricMissingFile{
				Path:   file.Path,
				Reason: firstNonEmpty(file.Reason, "unavailable"),
			})
			continue
		}
		if out.Len() > 0 {
			out.WriteString("\n")
		}
		fmt.Fprintf(&out, "## %s\n", file.Path)
		content := strings.TrimRight(file.Content, "\n")
		if strings.TrimSpace(content) == "" {
			out.WriteString("(empty)\n")
			continue
		}
		out.WriteString(content)
		out.WriteString("\n")
	}
	section.Content = truncatePacketSection(out.String(), byteBudget)
	return section
}

func buildRenderedArtifactsSection(inputs []renderedArtifactInput, byteBudget int) renderedArtifactsSection {
	packetInputs := make([]renderedArtifactInput, 0, len(inputs))
	for _, input := range inputs {
		if input.IncludeInLoopreview {
			packetInputs = append(packetInputs, input)
		}
	}
	section := renderedArtifactsSection{
		Configured: len(packetInputs) > 0,
		Artifacts:  append([]renderedArtifactInput(nil), packetInputs...),
	}
	if len(packetInputs) == 0 {
		return section
	}

	var out strings.Builder
	for _, input := range packetInputs {
		artifact := input.Artifact
		if out.Len() > 0 {
			out.WriteString("\n")
		}
		label := firstNonEmpty(artifact.Path, artifact.DeclaredOutput, artifact.Source)
		fmt.Fprintf(&out, "## %s\n", label)
		fmt.Fprintf(&out, "Source: %s\n", firstNonEmpty(artifact.Source, "unknown"))
		fmt.Fprintf(&out, "Status: %s\n", firstNonEmpty(artifact.Status, "available"))
		if strings.TrimSpace(artifact.DeclaredOutput) != "" {
			fmt.Fprintf(&out, "Declared output: %s\n", artifact.DeclaredOutput)
		}
		if strings.TrimSpace(artifact.Kind) != "" {
			fmt.Fprintf(&out, "Kind: %s\n", artifact.Kind)
		}
		if strings.TrimSpace(artifact.MediaType) != "" {
			fmt.Fprintf(&out, "Media type: %s\n", artifact.MediaType)
		}
		if artifact.Bytes > 0 {
			fmt.Fprintf(&out, "Bytes: %d\n", artifact.Bytes)
		}
		if strings.TrimSpace(artifact.SHA256) != "" {
			fmt.Fprintf(&out, "SHA-256: %s\n", artifact.SHA256)
		}
		if artifact.Files > 0 {
			fmt.Fprintf(&out, "Files: %d\n", artifact.Files)
		}
		if strings.TrimSpace(artifact.Summary) != "" {
			fmt.Fprintf(&out, "Summary: %s\n", artifact.Summary)
		}
		if strings.TrimSpace(artifact.Error) != "" {
			fmt.Fprintf(&out, "Error: %s\n", artifact.Error)
		}
		if strings.TrimSpace(input.Content.Text) != "" {
			fmt.Fprintf(&out, "\n```%s\n%s\n```\n", artifactFenceLanguage(artifact), strings.TrimRight(input.Content.Text, "\n"))
			if input.Content.Truncated {
				fmt.Fprintf(&out, "[TRUNCATED rendered artifact %s: omitted %d bytes, %d lines]\n", label, input.Content.OmittedBytes, input.Content.OmittedLines)
			}
		}
	}
	section.Content = truncatePacketSection(out.String(), byteBudget)
	return section
}

func buildPRHeadFileBodiesSection(inputs []prHeadFileBodyInput, limits ReviewPacketLimits) prHeadFileBodiesSection {
	section := prHeadFileBodiesSection{CandidateCount: len(inputs)}
	if len(inputs) == 0 {
		return section
	}
	maxFiles := limits.DocumentationBodyMaxFiles
	perFileBudget := limits.DocumentationBodyFileBytes
	aggregateBudget := limits.DocumentationBodyBytes
	var out strings.Builder
	usedBytes := 0
	originalBytes := 0
	originalLines := 0
	omittedBytes := 0
	omittedLines := 0
	for _, input := range inputs {
		label := firstNonEmpty(input.Path, "(unknown)")
		if !input.Available {
			section.Skipped = append(section.Skipped, prHeadFileBodySkip{
				Path:   label,
				Reason: firstNonEmpty(input.Reason, "unavailable"),
			})
			continue
		}
		originalBytes += len(input.Content)
		originalLines += countLines(input.Content)
		if section.IncludedCount >= maxFiles {
			omittedBytes += len(input.Content)
			omittedLines += countLines(input.Content)
			section.Skipped = append(section.Skipped, prHeadFileBodySkip{
				Path:   label,
				Reason: fmt.Sprintf("maximum complete body count exceeded (limit %d)", maxFiles),
			})
			continue
		}
		if len(input.Content) > perFileBudget {
			omittedBytes += len(input.Content)
			omittedLines += countLines(input.Content)
			section.Skipped = append(section.Skipped, prHeadFileBodySkip{
				Path:   label,
				Reason: fmt.Sprintf("body is %d bytes, exceeding per-file body budget %d bytes", len(input.Content), perFileBudget),
			})
			continue
		}
		block := formatPRHeadFileBodyBlock(input)
		if usedBytes+len(block) > aggregateBudget {
			omittedBytes += len(input.Content)
			omittedLines += countLines(input.Content)
			section.Skipped = append(section.Skipped, prHeadFileBodySkip{
				Path:   label,
				Reason: fmt.Sprintf("body would exceed aggregate body budget %d bytes", aggregateBudget),
			})
			continue
		}
		if out.Len() > 0 {
			out.WriteString("\n")
		}
		out.WriteString(block)
		usedBytes += len(block)
		section.IncludedCount++
	}
	section.Content = packetSection{
		Text:          out.String(),
		OriginalBytes: originalBytes,
		OriginalLines: originalLines,
		OmittedBytes:  omittedBytes,
		OmittedLines:  omittedLines,
		Truncated:     omittedBytes > 0 || omittedLines > 0,
	}
	return section
}

func formatPRHeadFileBodyBlock(input prHeadFileBodyInput) string {
	var out strings.Builder
	path := firstNonEmpty(input.Path, "(unknown)")
	fmt.Fprintf(&out, "## %s\n", path)
	fmt.Fprintf(&out, "Source: %s:%s\n", firstNonEmpty(input.SourceRef, "(unavailable)"), path)
	fmt.Fprintf(&out, "Completeness: complete\n")
	fmt.Fprintf(&out, "Bytes: %d\n", len(input.Content))
	fmt.Fprintf(&out, "Lines: %d\n", countLines(input.Content))
	fmt.Fprintf(&out, "\n```%s\n%s\n```\n", prHeadFileBodyFenceLanguage(path), strings.TrimRight(input.Content, "\n"))
	return out.String()
}

func prHeadFileBodyFenceLanguage(repoPath string) string {
	switch strings.ToLower(path.Ext(repoPath)) {
	case ".md", ".markdown", ".mdx":
		return "markdown"
	default:
		return "text"
	}
}

func artifactFenceLanguage(artifact RenderedArtifact) string {
	switch artifact.Kind {
	case "markdown":
		return "markdown"
	case "json":
		return "json"
	case "csv":
		return "csv"
	case "html":
		return "html"
	default:
		return "text"
	}
}

func configuredRenderedArtifacts(cfg config.Config) []renderedArtifactInput {
	if strings.EqualFold(strings.TrimSpace(cfg.Verification.Browser.Enabled), "never") {
		return nil
	}
	out := []renderedArtifactInput{}
	for _, artifact := range cfg.Evidence.Artifacts() {
		if artifact.ProjectType != "website" {
			continue
		}
		parts := []string{}
		if artifact.PreviewURL != "" {
			parts = append(parts, "preview_url="+artifact.PreviewURL)
		}
		if artifact.PreviewBuild != "" {
			parts = append(parts, "preview_build="+artifact.PreviewBuild)
		}
		if len(parts) == 0 {
			continue
		}
		summary := strings.Join(parts, " ")
		out = append(out, renderedArtifactInput{
			Artifact: RenderedArtifact{
				Source:  "verification.browser",
				Status:  "available",
				Kind:    "browser-preview",
				Summary: summary,
			},
			Content: packetSection{
				Text:          summary + "\n",
				OriginalBytes: len(summary) + 1,
				OriginalLines: 1,
			},
			IncludeInLoopreview: true,
		})
	}
	return out
}

func evidenceProducerConfigured(producer config.DomainEvidenceProducer) bool {
	return strings.TrimSpace(producer.Command) != "" || len(producer.Argv) > 0
}

func producerIncludeInLoopreview(producer config.DomainEvidenceProducer) bool {
	if producer.IncludeInLoopreview == nil {
		return true
	}
	return *producer.IncludeInLoopreview
}

func runConfiguredEvidenceProducer(ctx context.Context, deps Deps, worktreePath string, producer config.DomainEvidenceProducer, verifierTimeout time.Duration) ([]renderedArtifactInput, error) {
	command := strings.TrimSpace(producer.Command)
	source := "domain.evidence.producer"
	if command == "" && len(producer.Argv) == 0 {
		return nil, nil
	}
	outputs := normalizeRenderedArtifactOutputs(producer.Outputs)
	if len(outputs) == 0 {
		artifact := renderedArtifactFailure(source, "", "producer declares no outputs to collect")
		return []renderedArtifactInput{artifact}, errors.New("domain evidence producer declares no outputs to collect")
	}

	timeout := evidenceProducerTimeout(producer.TimeoutSeconds, verifierTimeout)
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result := deps.RunEvidenceProducer(runCtx, EvidenceProducerInvocation{
		Command:      command,
		Argv:         append([]string(nil), producer.Argv...),
		WorktreePath: worktreePath,
		Timeout:      timeout,
	})
	if result.TimedOut || errors.Is(result.Err, context.DeadlineExceeded) || runCtx.Err() == context.DeadlineExceeded {
		note := fmt.Sprintf("domain evidence producer timed out after %s", formatTimeout(timeout))
		if tail := boundedProducerOutput(result.Output); tail != "" {
			note += "\nproducer log tail:\n" + tail
		}
		return []renderedArtifactInput{renderedArtifactFailure(source, "", note)}, errors.New(note)
	}
	if result.Err != nil {
		note := "domain evidence producer failed: " + result.Err.Error()
		if tail := boundedProducerOutput(result.Output); tail != "" {
			note += "\nproducer log tail:\n" + tail
		}
		return []renderedArtifactInput{renderedArtifactFailure(source, "", note)}, errors.New(note)
	}
	if result.ExitCode != 0 {
		note := fmt.Sprintf("domain evidence producer exited with code %d", result.ExitCode)
		if tail := boundedProducerOutput(result.Output); tail != "" {
			note += "\nproducer log tail:\n" + tail
		}
		return []renderedArtifactInput{renderedArtifactFailure(source, "", note)}, errors.New(note)
	}

	artifacts, err := collectDeclaredRenderedArtifacts(worktreePath, outputs, producerIncludeInLoopreview(producer))
	if err != nil {
		return artifacts, err
	}
	return artifacts, nil
}

func normalizeRenderedArtifactOutputs(outputs []string) []string {
	out := make([]string, 0, len(outputs))
	seen := map[string]bool{}
	for _, raw := range outputs {
		cleaned, err := cleanRepoRelativePath(raw)
		if err != nil {
			value := strings.TrimSpace(raw)
			if value == "" {
				value = "(empty output path)"
			}
			if !seen[value] {
				seen[value] = true
				out = append(out, value)
			}
			continue
		}
		if !seen[cleaned] {
			seen[cleaned] = true
			out = append(out, cleaned)
		}
	}
	return out
}

func evidenceProducerTimeout(seconds int, verifierTimeout time.Duration) time.Duration {
	timeout := config.DurationSeconds(seconds, renderedArtifactProducerTimeout)
	if verifierTimeout > 0 && timeout > verifierTimeout {
		return verifierTimeout
	}
	return timeout
}

func collectDeclaredRenderedArtifacts(worktreePath string, outputs []string, includeInLoopreview bool) ([]renderedArtifactInput, error) {
	artifacts := []renderedArtifactInput{}
	problems := []string{}
	for _, rawOutput := range outputs {
		output, err := cleanRepoRelativePath(rawOutput)
		if err != nil {
			label := strings.TrimSpace(rawOutput)
			if label == "" {
				label = "(empty output path)"
			}
			artifacts = append(artifacts, renderedArtifactFailure("domain.evidence.producer", label, err.Error()))
			problems = append(problems, fmt.Sprintf("%s (%s)", label, err.Error()))
			continue
		}
		fullPath, err := safeWorktreePath(worktreePath, output)
		if err != nil {
			artifacts = append(artifacts, renderedArtifactFailure("domain.evidence.producer", output, err.Error()))
			problems = append(problems, fmt.Sprintf("%s (%s)", output, err.Error()))
			continue
		}
		info, err := os.Lstat(fullPath)
		if err != nil {
			reason := "missing"
			if !errors.Is(err, os.ErrNotExist) {
				reason = err.Error()
			}
			artifacts = append(artifacts, renderedArtifactFailure("domain.evidence.producer", output, reason))
			problems = append(problems, fmt.Sprintf("%s (%s)", output, reason))
			continue
		}
		if info.IsDir() {
			collected, err := collectRenderedArtifactDirectory(worktreePath, output, fullPath, includeInLoopreview)
			artifacts = append(artifacts, collected...)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s (%s)", output, err.Error()))
			}
			continue
		}
		artifact, err := collectRenderedArtifactFile(worktreePath, output, output, fullPath, info, includeInLoopreview)
		artifacts = append(artifacts, artifact)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s (%s)", output, err.Error()))
		}
	}
	if len(problems) > 0 {
		return artifacts, fmt.Errorf("domain evidence producer output unavailable: %s", strings.Join(problems, "; "))
	}
	return artifacts, nil
}

func collectRenderedArtifactDirectory(worktreePath, declaredOutput, fullPath string, includeInLoopreview bool) ([]renderedArtifactInput, error) {
	type fileEntry struct {
		rel  string
		full string
		info fs.FileInfo
	}
	files := []fileEntry{}
	truncated := false
	walkErr := filepath.WalkDir(fullPath, func(current string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if current == fullPath {
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(worktreePath, current)
		if err != nil {
			return err
		}
		if len(files) >= renderedArtifactMaxDirectoryFiles {
			truncated = true
			return filepath.SkipAll
		}
		files = append(files, fileEntry{
			rel:  normalizeRepoPath(filepath.ToSlash(rel)),
			full: current,
			info: info,
		})
		return nil
	})
	sort.Slice(files, func(i, j int) bool {
		return files[i].rel < files[j].rel
	})

	var manifest strings.Builder
	fmt.Fprintf(&manifest, "Directory manifest for %s\n", declaredOutput)
	for _, file := range files {
		kind, mediaType := classifyRenderedArtifactFile(file.rel, nil)
		fmt.Fprintf(&manifest, "- %s kind=%s media_type=%s bytes=%d\n", file.rel, kind, mediaType, file.info.Size())
	}
	if truncated {
		fmt.Fprintf(&manifest, "[TRUNCATED directory manifest: showing first %d files]\n", renderedArtifactMaxDirectoryFiles)
	}
	artifacts := []renderedArtifactInput{{
		Artifact: RenderedArtifact{
			Source:         "domain.evidence.producer",
			Status:         "available",
			DeclaredOutput: declaredOutput,
			Path:           declaredOutput,
			Kind:           "directory",
			Files:          len(files),
			Truncated:      truncated,
			Summary:        fmt.Sprintf("directory output with %d collected file(s)", len(files)),
		},
		Content: packetSection{
			Text:          manifest.String(),
			OriginalBytes: manifest.Len(),
			OriginalLines: countLines(manifest.String()),
		},
		IncludeInLoopreview: includeInLoopreview,
	}}
	for _, file := range files {
		artifact, err := collectRenderedArtifactFile(worktreePath, declaredOutput, file.rel, file.full, file.info, includeInLoopreview)
		artifacts = append(artifacts, artifact)
		if err != nil {
			return artifacts, err
		}
	}
	return artifacts, walkErr
}

func collectRenderedArtifactFile(_ string, declaredOutput, repoPath, fullPath string, info fs.FileInfo, includeInLoopreview bool) (renderedArtifactInput, error) {
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(fullPath)
		summary := "symlink"
		if err == nil {
			summary = "symlink -> " + target
		}
		return renderedArtifactInput{
			Artifact: RenderedArtifact{
				Source:         "domain.evidence.producer",
				Status:         "available",
				DeclaredOutput: declaredOutput,
				Path:           repoPath,
				Kind:           "symlink",
				Summary:        summary,
			},
			Content: packetSection{
				Text:          summary + "\n",
				OriginalBytes: len(summary) + 1,
				OriginalLines: 1,
			},
			IncludeInLoopreview: includeInLoopreview,
		}, nil
	}

	prefix, readErr := readFilePrefix(fullPath, renderedArtifactFileBudgetBytes)
	kind, mediaType := classifyRenderedArtifactFile(repoPath, prefix)
	sum, hashErr := sha256File(fullPath)
	summary := renderedArtifactSummary(repoPath, kind, info.Size(), sum, prefix)
	artifact := renderedArtifactInput{
		Artifact: RenderedArtifact{
			Source:         "domain.evidence.producer",
			Status:         "available",
			DeclaredOutput: declaredOutput,
			Path:           repoPath,
			Kind:           kind,
			MediaType:      mediaType,
			Bytes:          info.Size(),
			SHA256:         sum,
			Summary:        summary,
		},
		IncludeInLoopreview: includeInLoopreview,
	}
	if readErr == nil && inlineRenderedArtifact(kind, prefix) {
		text := string(prefix)
		omitted := int(info.Size()) - len(prefix)
		if omitted < 0 {
			omitted = 0
		}
		artifact.Content = packetSection{
			Text:          text,
			OriginalBytes: int(info.Size()),
			OriginalLines: countLines(text),
			OmittedBytes:  omitted,
			Truncated:     omitted > 0,
		}
	}
	if readErr != nil {
		artifact.Artifact.Status = "error"
		artifact.Artifact.Error = readErr.Error()
		return artifact, readErr
	}
	if hashErr != nil {
		artifact.Artifact.Status = "error"
		artifact.Artifact.Error = hashErr.Error()
		return artifact, hashErr
	}
	return artifact, nil
}

func renderedArtifactFailure(source, declaredOutput, reason string) renderedArtifactInput {
	reason = strings.TrimSpace(reason)
	status := "missing"
	if strings.TrimSpace(declaredOutput) == "" {
		status = "error"
	}
	return renderedArtifactInput{
		Artifact: RenderedArtifact{
			Source:         source,
			Status:         status,
			DeclaredOutput: strings.TrimSpace(declaredOutput),
			Error:          reason,
			Summary:        reason,
		},
		Content: packetSection{
			Text:          reason + "\n",
			OriginalBytes: len(reason) + 1,
			OriginalLines: 1,
		},
		IncludeInLoopreview: true,
	}
}

func safeWorktreePath(worktreePath, repoPath string) (string, error) {
	absRoot, err := filepath.Abs(worktreePath)
	if err != nil {
		return "", err
	}
	fullPath := filepath.Join(absRoot, filepath.FromSlash(repoPath))
	absPath, err := filepath.Abs(fullPath)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(absRoot, absPath)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("must stay within PR worktree")
	}
	return absPath, nil
}

func readFilePrefix(filePath string, limit int) ([]byte, error) {
	if limit <= 0 {
		return nil, nil
	}
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	buf := make([]byte, limit)
	n, err := io.ReadFull(file, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	return buf[:n], nil
}

func sha256File(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func classifyRenderedArtifactFile(repoPath string, prefix []byte) (string, string) {
	switch strings.ToLower(path.Ext(repoPath)) {
	case ".md", ".markdown":
		return "markdown", "text/markdown"
	case ".json", ".jsonl":
		return "json", "application/json"
	case ".csv", ".tsv":
		return "csv", "text/csv"
	case ".html", ".htm":
		return "html", "text/html"
	case ".txt", ".text", ".log", ".xml", ".yaml", ".yml":
		return "text", "text/plain"
	case ".pdf":
		return "pdf", "application/pdf"
	}
	if len(prefix) > 0 && utf8.Valid(prefix) && !bytes.Contains(prefix, []byte{0}) {
		return "text", "text/plain"
	}
	return "binary", "application/octet-stream"
}

func inlineRenderedArtifact(kind string, prefix []byte) bool {
	if len(prefix) == 0 || !utf8.Valid(prefix) || bytes.Contains(prefix, []byte{0}) {
		return false
	}
	switch kind {
	case "text", "markdown", "json", "csv", "html":
		return true
	default:
		return false
	}
}

func renderedArtifactSummary(repoPath, kind string, size int64, sha string, prefix []byte) string {
	switch kind {
	case "text", "markdown", "json", "csv", "html":
		return fmt.Sprintf("%s file content included inline with bounded excerpt", kind)
	case "pdf":
		version := "unknown"
		if bytes.HasPrefix(prefix, []byte("%PDF-")) {
			line := strings.SplitN(string(prefix), "\n", 2)[0]
			version = strings.TrimSpace(strings.TrimPrefix(line, "%PDF-"))
		}
		return fmt.Sprintf("PDF binary summary: version=%s bytes=%d sha256=%s", version, size, sha)
	default:
		return fmt.Sprintf("%s artifact manifest: path=%s bytes=%d sha256=%s", kind, repoPath, size, sha)
	}
}

func publicRenderedArtifacts(inputs []renderedArtifactInput) []RenderedArtifact {
	out := make([]RenderedArtifact, 0, len(inputs))
	for _, input := range inputs {
		if strings.TrimSpace(input.Artifact.Source) == "" && strings.TrimSpace(input.Artifact.Path) == "" && strings.TrimSpace(input.Artifact.Summary) == "" {
			continue
		}
		out = append(out, input.Artifact)
	}
	return out
}

func boundedProducerOutput(output string) string {
	output = strings.TrimSpace(output)
	if output == "" {
		return ""
	}
	if len(output) > producerFailureLogBudgetBytes {
		output = output[len(output)-producerFailureLogBudgetBytes:]
	}
	return strings.TrimSpace(output)
}

func runEvidenceProducerCommand(ctx context.Context, invocation EvidenceProducerInvocation) EvidenceProducerResult {
	if ctx == nil {
		ctx = context.Background()
	}
	cmd, err := evidenceProducerCommand(invocation)
	if err != nil {
		return EvidenceProducerResult{
			ExitCode: 127,
			Err:      err,
		}
	}
	cmd.Dir = invocation.WorktreePath

	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	result, err := supervisedexec.Run(ctx, cmd, supervisedexec.Options{
		HardCap: invocation.Timeout,
		Role:    "evidence-producer",
	})
	timedOut := result.Outcome == supervisedexec.OutcomeDeadline || ctx.Err() == context.DeadlineExceeded || errors.Is(err, context.DeadlineExceeded)
	exitCode := result.ExitCode
	if err != nil && result.Outcome == supervisedexec.OutcomeCompleted {
		exitCode = 127
	}
	return EvidenceProducerResult{
		ExitCode: exitCode,
		Output:   output.String(),
		TimedOut: timedOut,
		Err:      err,
	}
}

func evidenceProducerCommand(invocation EvidenceProducerInvocation) (*exec.Cmd, error) {
	if len(invocation.Argv) > 0 {
		for index, arg := range invocation.Argv {
			if strings.TrimSpace(arg) == "" {
				return nil, fmt.Errorf("domain evidence producer argv[%d] is empty", index)
			}
		}
		return exec.Command(invocation.Argv[0], invocation.Argv[1:]...), nil
	}
	command := strings.TrimSpace(invocation.Command)
	if command == "" {
		return nil, errors.New("domain evidence producer command is empty")
	}
	file, args := shellCommand(command)
	return exec.Command(file, args...), nil
}

func shellCommand(command string) (string, []string) {
	if runtime.GOOS == "windows" {
		return "cmd", []string{"/c", command}
	}
	return "sh", []string{"-c", command}
}

func loadRubric(ctx context.Context, git GitClient, repoPath, baseBranch string, rubric config.DomainRubric) rubricInput {
	input := rubricInput{
		Checklist: normalizeRubricChecklist(rubric.Checklist),
	}
	if len(rubric.Paths) == 0 && len(input.Checklist) == 0 {
		return input
	}
	input.Configured = true
	if strings.TrimSpace(baseBranch) == "" {
		baseBranch = lcdefaults.BaseBranch
	}
	for _, rawPath := range rubric.Paths {
		pathLabel := strings.TrimSpace(rawPath)
		cleanPath, err := cleanRubricPath(rawPath)
		if err != nil {
			input.Files = append(input.Files, rubricFileInput{
				Path:      firstNonEmpty(pathLabel, "(empty rubric path)"),
				Available: false,
				Reason:    err.Error(),
			})
			continue
		}
		content, err := git.Show(ctx, repoPath, "origin/"+baseBranch+":"+cleanPath)
		if err != nil {
			input.Files = append(input.Files, rubricFileInput{
				Path:      cleanPath,
				Available: false,
				Reason:    err.Error(),
			})
			continue
		}
		input.Files = append(input.Files, rubricFileInput{
			Path:      cleanPath,
			Content:   content,
			Available: true,
		})
	}
	return input
}

func normalizeRubricChecklist(items []string) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}

func cleanRubricPath(rawPath string) (string, error) {
	return cleanRepoRelativePath(rawPath)
}

func cleanRepoRelativePath(rawPath string) (string, error) {
	normalized := strings.TrimSpace(strings.ReplaceAll(rawPath, `\`, `/`))
	normalized = strings.TrimPrefix(normalized, "./")
	if normalized == "" {
		return "", fmt.Errorf("must be a non-empty repo-relative path")
	}
	if strings.HasPrefix(normalized, "/") {
		return "", fmt.Errorf("must be repo-relative")
	}
	if strings.Contains(normalized, ":") {
		return "", fmt.Errorf("must be repo-relative")
	}
	if strings.ContainsRune(normalized, 0) {
		return "", fmt.Errorf("must not contain NUL bytes")
	}
	cleaned := path.Clean(normalized)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("must be repo-relative")
	}
	return cleaned, nil
}

func missingRubricEvidenceNote(rubric rubricSection) string {
	if !rubric.Configured || len(rubric.MissingFiles) == 0 {
		return ""
	}
	parts := make([]string, 0, len(rubric.MissingFiles))
	for _, missing := range rubric.MissingFiles {
		parts = append(parts, fmt.Sprintf("%s (%s)", missing.Path, missing.Reason))
	}
	return "configured rubric evidence unavailable: " + strings.Join(parts, "; ")
}

func firstMissingRubricPath(rubric rubricSection) string {
	if len(rubric.MissingFiles) == 0 {
		return ""
	}
	return rubric.MissingFiles[0].Path
}

func appendVerdictEvidence(verdict *Verdict, note string) {
	note = strings.TrimSpace(note)
	if note == "" {
		return
	}
	if strings.TrimSpace(verdict.Evidence) == "" {
		verdict.Evidence = note
		return
	}
	if strings.Contains(verdict.Evidence, note) {
		return
	}
	verdict.Evidence = strings.TrimSpace(verdict.Evidence) + "\n" + note
}

func formatPacketSection(label string, section packetSection) string {
	text := strings.TrimRight(section.Text, "\n")
	if strings.TrimSpace(text) == "" {
		text = "(empty)"
	}
	if section.Truncated {
		text += fmt.Sprintf("\n[TRUNCATED %s: omitted %d bytes, %d lines%s]", label, section.OmittedBytes, section.OmittedLines, formatOmittedFiles(section.OmittedFiles))
	}
	return text
}

func formatOmittedFiles(files []string) string {
	if len(files) == 0 {
		return ""
	}
	return "; omitted files: " + strings.Join(files, ", ")
}

func formatChangedFilesSection(section changedFilesSection) string {
	text := strings.TrimRight(section.Text, "\n")
	if strings.TrimSpace(text) == "" {
		text = "(none)"
	}
	if section.Truncated {
		text += fmt.Sprintf("\n[TRUNCATED changed files: omitted %d files, %d bytes, %d lines]", section.OmittedFiles, section.OmittedBytes, section.OmittedLines)
	}
	return text
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

func buildChangedFilesSection(files []string, byteBudget int) changedFilesSection {
	section := changedFilesSection{TotalFiles: len(files)}
	if len(files) == 0 {
		return section
	}
	var out strings.Builder
	for i, file := range files {
		file = strings.TrimSpace(file)
		if file == "" {
			continue
		}
		line := "- " + file + "\n"
		if out.Len()+len(line) > byteBudget {
			section.Truncated = true
			section.OmittedFiles = countNonEmptyStrings(files[i:])
			section.OmittedBytes = byteLenChangedFiles(files[i:])
			section.OmittedLines = section.OmittedFiles
			break
		}
		out.WriteString(line)
	}
	section.Text = out.String()
	return section
}

func byteLenChangedFiles(files []string) int {
	total := 0
	for _, file := range files {
		file = strings.TrimSpace(file)
		if file == "" {
			continue
		}
		total += len("- " + file + "\n")
	}
	return total
}

func countNonEmptyStrings(values []string) int {
	count := 0
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			count++
		}
	}
	return count
}

func buildDiffSection(diff string, limits ReviewPacketLimits, generatedRules []generatedAttributeRule) packetSection {
	diff = strings.TrimRight(diff, "\n")
	if strings.TrimSpace(diff) == "" {
		return packetSection{}
	}
	patches := splitDiffPatches(diff)
	orderedPatches := sourceFirstDiffPatches(patches, limits, generatedRules)
	var out strings.Builder
	omittedBytes := 0
	omittedLines := 0
	omittedFiles := []string{}
	for i, patch := range orderedPatches {
		perFileBudget := limits.DiffFileBytes
		if patch.Generated {
			perFileBudget = limits.GeneratedDiffFileBytes
		}
		patchSection := truncatePacketSection(patch.Text, perFileBudget)
		block := formatDiffPatchBlock(patch.File, patchSection)
		if out.Len()+len(block) > limits.DiffBytes {
			for _, omitted := range orderedPatches[i:] {
				omittedBytes += len(omitted.Text)
				omittedLines += countLines(omitted.Text)
				omittedFiles = append(omittedFiles, omitted.File)
			}
			break
		}
		out.WriteString(block)
		if patchSection.Truncated {
			omittedBytes += patchSection.OmittedBytes
			omittedLines += patchSection.OmittedLines
		}
	}
	return packetSection{
		Text:          out.String(),
		OriginalBytes: len(diff),
		OriginalLines: countLines(diff),
		OmittedBytes:  omittedBytes,
		OmittedLines:  omittedLines,
		OmittedFiles:  omittedFiles,
		Truncated:     omittedBytes > 0 || omittedLines > 0,
	}
}

type classifiedDiffPatch struct {
	diffPatch
	Generated bool
}

func sourceFirstDiffPatches(patches []diffPatch, limits ReviewPacketLimits, generatedRules []generatedAttributeRule) []classifiedDiffPatch {
	source := []classifiedDiffPatch{}
	generated := []classifiedDiffPatch{}
	for _, patch := range patches {
		classified := classifiedDiffPatch{
			diffPatch: patch,
			Generated: isGeneratedPatch(patch, limits, generatedRules),
		}
		if classified.Generated {
			generated = append(generated, classified)
			continue
		}
		source = append(source, classified)
	}
	return append(source, generated...)
}

func isGeneratedPatch(patch diffPatch, limits ReviewPacketLimits, generatedRules []generatedAttributeRule) bool {
	file := normalizeRepoPath(patch.File)
	if file == "" {
		return false
	}
	if matchesAnyRepoGlob(file, limits.GeneratedPatterns) {
		return true
	}
	if generatedByAttributes(file, generatedRules) {
		return true
	}
	return generatedBySizeThreshold(file, len(patch.Text), limits.GeneratedSizeBytes)
}

func matchesAnyRepoGlob(path string, patterns []string) bool {
	for _, pattern := range patterns {
		if matchRepoGlob(pattern, path) {
			return true
		}
	}
	return false
}

func generatedByAttributes(path string, rules []generatedAttributeRule) bool {
	generatedSet := false
	generated := false
	diffSet := false
	diffEnabled := true
	for _, rule := range rules {
		if !matchRepoGlob(rule.Pattern, path) {
			continue
		}
		if rule.Generated != nil {
			generatedSet = true
			generated = *rule.Generated
		}
		if rule.Diff != nil {
			diffSet = true
			diffEnabled = *rule.Diff
		}
	}
	if generatedSet && generated {
		return true
	}
	return diffSet && !diffEnabled
}

func generatedBySizeThreshold(path string, size, threshold int) bool {
	if threshold <= 0 || size < threshold {
		return false
	}
	lower := strings.ToLower(normalizeRepoPath(path))
	base := repoPathBase(lower)
	for _, signal := range []string{
		"/generated/",
		"/baseline/",
		"/baselines/",
		"/snapshot/",
		"/snapshots/",
		"/vendor/",
		"/dist/",
	} {
		if strings.Contains("/"+lower+"/", signal) {
			return true
		}
	}
	for _, signal := range []string{
		".generated.",
		".gen.",
		".min.",
	} {
		if strings.Contains(base, signal) {
			return true
		}
	}
	for _, suffix := range []string{
		"_generated.go",
		".pb.go",
		".lock",
	} {
		if strings.HasSuffix(base, suffix) {
			return true
		}
	}
	return false
}

type diffPatch struct {
	File string
	Text string
}

func splitDiffPatches(diff string) []diffPatch {
	lines := strings.SplitAfter(diff, "\n")
	patches := []diffPatch{}
	var current strings.Builder
	currentFile := "(unattributed diff)"
	flush := func() {
		if current.Len() == 0 {
			return
		}
		patches = append(patches, diffPatch{File: currentFile, Text: current.String()})
		current.Reset()
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "diff --git ") {
			flush()
			currentFile = parseDiffHeaderFile(line)
		}
		current.WriteString(line)
	}
	flush()
	if len(patches) == 0 {
		patches = append(patches, diffPatch{File: "(combined diff)", Text: diff})
	}
	return patches
}

func parseDiffHeaderFile(header string) string {
	fields := strings.Fields(header)
	if len(fields) >= 4 {
		return strings.TrimPrefix(fields[3], "b/")
	}
	return "(unattributed diff)"
}

func formatDiffPatchBlock(file string, section packetSection) string {
	var out strings.Builder
	fmt.Fprintf(&out, "## %s\n", file)
	out.WriteString(strings.TrimRight(section.Text, "\n"))
	if section.Truncated {
		fmt.Fprintf(&out, "\n[TRUNCATED diff for %s: omitted %d bytes, %d lines]", file, section.OmittedBytes, section.OmittedLines)
	}
	out.WriteString("\n")
	return out.String()
}

func truncatePacketSection(text string, byteBudget int) packetSection {
	if byteBudget < 0 {
		byteBudget = 0
	}
	prefix, omittedBytes, omittedLines := truncateUTF8(text, byteBudget)
	return packetSection{
		Text:          prefix,
		OriginalBytes: len(text),
		OriginalLines: countLines(text),
		OmittedBytes:  omittedBytes,
		OmittedLines:  omittedLines,
		Truncated:     omittedBytes > 0,
	}
}

func truncateUTF8(text string, byteBudget int) (string, int, int) {
	if len(text) <= byteBudget {
		return text, 0, 0
	}
	if byteBudget < 0 {
		byteBudget = 0
	}
	end := byteBudget
	if end > len(text) {
		end = len(text)
	}
	for end > 0 && !utf8.ValidString(text[:end]) {
		end--
	}
	omitted := text[end:]
	return text[:end], len(text) - end, countOmittedLines(omitted)
}

func countLines(text string) int {
	if text == "" {
		return 0
	}
	return strings.Count(text, "\n") + 1
}

func countOmittedLines(text string) int {
	text = strings.TrimPrefix(text, "\r\n")
	text = strings.TrimPrefix(text, "\n")
	return countLines(text)
}

func gatherInputs(ctx context.Context, deps Deps, github GitHubClient, repoPath string, opts Options, pr gh.PullRequest, refs reviewRefs) (reviewInputs, error) {
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
		Refs:         refs,
		Diff:         diff,
		ChangedFiles: changedFiles,
	}
	issue, present := loadIssue(ctx, github, pr)
	inputs.Issue = issue
	inputs.IssuePresent = present
	inputs.GeneratedAttributeRules = loadGeneratedAttributeRules(ctx, deps.Git, repoPath, refs.BaseBranch, opts.Stderr)
	inputs.Spec = loadSpec(ctx, deps.Git, repoPath, refs.BaseBranch, specSearchTexts(issue, present, pr))
	inputs.Spec = classifySpecAbsence(inputs.Spec, refs.BaseBranch, changedFiles, diff)
	inputs.PRHeadFileBodies = loadPRHeadFileBodies(ctx, deps.Git, repoPath, refs, changedFiles, diff, deps.ReviewPacketLimits.withDefaults())
	return inputs, nil
}

type generatedAttributeRule struct {
	Pattern   string
	Generated *bool
	Diff      *bool
}

func loadGeneratedAttributeRules(ctx context.Context, git GitClient, repoPath, baseBranch string, warnings io.Writer) []generatedAttributeRule {
	if strings.TrimSpace(baseBranch) == "" {
		baseBranch = lcdefaults.BaseBranch
	}
	revPath := "origin/" + baseBranch + ":.gitattributes"
	content, err := git.Show(ctx, repoPath, revPath)
	if err != nil {
		if gitutil.IsPathAbsentOnRef(err, ".gitattributes") {
			return nil
		}
		if warnings == nil {
			warnings = io.Discard
		}
		fmt.Fprintf(warnings, "[loopcoder] warning: generated-file classification via .gitattributes is unavailable from %s: %v; falling back to glob and size heuristics\n", revPath, err)
		return nil
	}
	if strings.TrimSpace(content) == "" {
		return nil
	}
	return parseGeneratedAttributeRules(content)
}

func parseGeneratedAttributeRules(content string) []generatedAttributeRule {
	rules := []generatedAttributeRule{}
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || strings.HasPrefix(fields[0], "[attr]") {
			continue
		}
		pattern := normalizeGitattributesPattern(fields[0])
		if pattern == "" {
			continue
		}
		rule := generatedAttributeRule{Pattern: pattern}
		for _, attr := range fields[1:] {
			if value, ok := parseAttributeBool(attr, "linguist-generated"); ok {
				rule.Generated = boolPtr(value)
				continue
			}
			if value, ok := parseAttributeBool(attr, "linguist-diff"); ok {
				rule.Diff = boolPtr(value)
			}
		}
		if rule.Generated != nil || rule.Diff != nil {
			rules = append(rules, rule)
		}
	}
	return rules
}

func normalizeGitattributesPattern(pattern string) string {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" || strings.HasPrefix(pattern, "!") {
		return ""
	}
	if unquoted, err := strconv.Unquote(pattern); err == nil {
		pattern = unquoted
	}
	pattern = normalizeRepoPath(pattern)
	if strings.HasSuffix(pattern, "/") {
		pattern += "**"
	}
	return pattern
}

func parseAttributeBool(attr, name string) (bool, bool) {
	attr = strings.TrimSpace(attr)
	switch attr {
	case name:
		return true, true
	case "-" + name, "!" + name:
		return false, true
	}
	prefix := name + "="
	if !strings.HasPrefix(attr, prefix) {
		return false, false
	}
	switch strings.ToLower(strings.TrimSpace(strings.TrimPrefix(attr, prefix))) {
	case "true", "1", "yes", "on":
		return true, true
	case "false", "0", "no", "off":
		return false, true
	default:
		return false, false
	}
}

func boolPtr(value bool) *bool {
	return &value
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
	for _, number := range fallbackIssueNumbers(pr) {
		issue, err := github.ViewIssue(ctx, number)
		if err == nil {
			return issue, true
		}
	}
	return gh.Issue{
		Title: pr.Title,
		Body:  pr.Body,
	}, false
}

func fallbackIssueNumbers(pr gh.PullRequest) []int {
	numbers := []int{}
	seen := map[int]bool{}
	add := func(number int) {
		if number <= 0 || seen[number] {
			return
		}
		seen[number] = true
		numbers = append(numbers, number)
	}
	if number := loopIssueBranchNumber(pr.HeadRefName); number > 0 {
		add(number)
	}
	for _, number := range bodyIssueReferenceNumbers(pr.Body) {
		add(number)
	}
	return numbers
}

func loopIssueBranchNumber(branch string) int {
	match := loopIssueBranchPattern.FindStringSubmatch(strings.TrimSpace(branch))
	if len(match) != 2 {
		return 0
	}
	return atoiPositive(match[1])
}

func bodyIssueReferenceNumbers(body string) []int {
	numbers := []int{}
	for _, match := range closingIssueBodyPattern.FindAllStringSubmatch(body, -1) {
		if len(match) == 2 {
			numbers = append(numbers, atoiPositive(match[1]))
		}
	}
	for _, match := range bareIssueBodyPattern.FindAllStringSubmatch(body, -1) {
		if len(match) == 2 {
			numbers = append(numbers, atoiPositive(match[1]))
		}
	}
	return numbers
}

func atoiPositive(value string) int {
	number, err := strconv.Atoi(value)
	if err != nil || number <= 0 {
		return 0
	}
	return number
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

func classifySpecAbsence(spec specInput, baseBranch string, changedFiles []string, diff string) specInput {
	if spec.Available || strings.TrimSpace(spec.Path) == "" {
		return spec
	}
	if strings.TrimSpace(baseBranch) == "" {
		baseBranch = lcdefaults.BaseBranch
	}
	if !isDocsSpecPath(spec.Path) || !docsOnlyChangedFiles(changedFiles) {
		return spec
	}
	if !changedFilesContainPath(changedFiles, spec.Path) || !diffAddsPath(diff, spec.Path) {
		return spec
	}
	spec.ExpectedAbsent = true
	spec.ExpectedReason = fmt.Sprintf("expected: this PR introduces the spec, so it is absent from origin/%s", baseBranch)
	return spec
}

func isDocsSpecPath(path string) bool {
	return strings.HasPrefix(normalizeRepoPath(path), "docs/specs/")
}

func docsOnlyChangedFiles(files []string) bool {
	seen := false
	for _, file := range files {
		normalized := normalizeRepoPath(file)
		if normalized == "" {
			continue
		}
		seen = true
		if !strings.HasPrefix(normalized, "docs/") {
			return false
		}
	}
	return seen
}

func changedFilesContainPath(files []string, path string) bool {
	target := normalizeRepoPath(path)
	if target == "" {
		return false
	}
	for _, file := range files {
		if normalizeRepoPath(file) == target {
			return true
		}
	}
	return false
}

func diffAddsPath(diff string, path string) bool {
	target := normalizeRepoPath(path)
	if target == "" || strings.TrimSpace(diff) == "" {
		return false
	}
	for _, patch := range splitDiffPatches(diff) {
		if normalizeRepoPath(patch.File) != target {
			continue
		}
		if strings.Contains(patch.Text, "\nnew file mode ") || strings.HasPrefix(patch.Text, "new file mode ") {
			return true
		}
		if diffPatchAddsTarget(patch.Text, target) {
			return true
		}
	}
	return false
}

func diffPatchAddsTarget(patchText string, target string) bool {
	fromDevNull := false
	toTarget := false
	for _, line := range strings.Split(patchText, "\n") {
		line = strings.TrimSpace(line)
		if line == "--- /dev/null" {
			fromDevNull = true
			continue
		}
		if strings.HasPrefix(line, "+++ ") && normalizeDiffPath(strings.TrimPrefix(line, "+++ ")) == target {
			toTarget = true
		}
	}
	return fromDevNull && toTarget
}

func normalizeDiffPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "/dev/null" {
		return path
	}
	path = strings.TrimPrefix(path, "a/")
	path = strings.TrimPrefix(path, "b/")
	return normalizeRepoPath(path)
}

func normalizeRepoPath(path string) string {
	path = strings.TrimSpace(strings.ReplaceAll(path, `\`, `/`))
	path = strings.TrimPrefix(path, "./")
	return path
}

func matchRepoGlob(pattern, path string) bool {
	pattern = normalizeRepoPath(pattern)
	path = normalizeRepoPath(path)
	if pattern == "" || path == "" {
		return false
	}
	anchored := strings.HasPrefix(pattern, "/")
	pattern = strings.TrimPrefix(pattern, "/")
	if strings.HasSuffix(pattern, "/") {
		pattern += "**"
	}
	if !anchored && !strings.Contains(pattern, "/") {
		return matchGlobPattern(pattern, repoPathBase(path))
	}
	return matchGlobPattern(pattern, path)
}

func matchGlobPattern(pattern, value string) bool {
	re, err := regexp.Compile(globPatternRegexp(pattern))
	if err != nil {
		return false
	}
	return re.MatchString(value)
}

func globPatternRegexp(pattern string) string {
	var out strings.Builder
	out.WriteString("^")
	for i := 0; i < len(pattern); {
		switch {
		case strings.HasPrefix(pattern[i:], "**/"):
			out.WriteString("(?:.*/)?")
			i += 3
		case strings.HasPrefix(pattern[i:], "**"):
			out.WriteString(".*")
			i += 2
		default:
			ch := pattern[i]
			switch ch {
			case '*':
				out.WriteString("[^/]*")
			case '?':
				out.WriteString("[^/]")
			default:
				out.WriteString(regexp.QuoteMeta(string(ch)))
			}
			i++
		}
	}
	out.WriteString("$")
	return out.String()
}

func repoPathBase(path string) string {
	path = normalizeRepoPath(path)
	if path == "" {
		return ""
	}
	index := strings.LastIndex(path, "/")
	if index < 0 {
		return path
	}
	return path[index+1:]
}

func checkoutPRWorktree(ctx context.Context, deps Deps, repoPath, worktreePath string, source prHeadFileSource) error {
	if !source.Verified {
		return errors.New("PR-head worktree source ref is not verified")
	}
	sourceRef := strings.TrimSpace(source.Ref)
	if sourceRef == "" {
		return errors.New("PR-head worktree source ref is unavailable")
	}
	lock, err := deps.AcquireLock(repoPath, 60*time.Second)
	if err != nil {
		return err
	}
	if lock == nil {
		return errors.New("lock acquisition returned nil lock")
	}
	addErr := deps.Git.WorktreeAddDetachedAt(ctx, repoPath, worktreePath, sourceRef)
	releaseErr := lock.Release()
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
	if deps.RunEvidenceProducer == nil {
		deps.RunEvidenceProducer = defaults.RunEvidenceProducer
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

func providerFailureNote(logPath, note string) string {
	logBytes, err := os.ReadFile(logPath)
	if err != nil || len(logBytes) == 0 {
		return note
	}
	logText := string(logBytes)
	if len(logText) > providerFailureLogBudgetBytes {
		logText = logText[len(logText)-providerFailureLogBudgetBytes:]
	}
	logText = strings.TrimSpace(logText)
	if logText == "" {
		return note
	}
	return note + "\nprovider log tail:\n" + logText
}

func formatTimeout(timeout time.Duration) string {
	if timeout%time.Second == 0 {
		return fmt.Sprintf("%ds", int(timeout/time.Second))
	}
	return timeout.String()
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

var (
	specPathPattern         = regexp.MustCompile(`docs/[A-Za-z0-9._/-]+\.md`)
	loopIssueBranchPattern  = regexp.MustCompile(`^loop/issue-(\d+)(?:$|[-_/].*)`)
	closingIssueBodyPattern = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s+#(\d+)\b`)
	bareIssueBodyPattern    = regexp.MustCompile(`#(\d+)\b`)
)

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
