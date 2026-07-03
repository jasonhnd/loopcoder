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
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jasonhnd/loopcoder/internal/agent"
	"github.com/jasonhnd/loopcoder/internal/attestation"
	"github.com/jasonhnd/loopcoder/internal/config"
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

	DefaultVerifierTimeout = 600 * time.Second
	VerifierStallTimeout   = 120 * time.Second

	reviewPacketChangedFilesBudgetBytes = 8 * 1024
	reviewPacketDiffBudgetBytes         = 80 * 1024
	reviewPacketDiffFileBudgetBytes     = 24 * 1024
	reviewPacketIssueBudgetBytes        = 12 * 1024
	reviewPacketSpecBudgetBytes         = 40 * 1024
	reviewPacketTotalPromptBudgetBytes  = 160 * 1024
	providerFailureLogBudgetBytes       = 4 * 1024
)

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
}

type Result struct {
	Verdict  Verdict
	ExitCode int
}

type Verdict struct {
	Verdict         string                         `json:"verdict"`
	Findings        []Finding                      `json:"findings"`
	Evidence        string                         `json:"evidence"`
	SpecConformance string                         `json:"spec_conformance"`
	Attestation     *attestation.AttestationRecord `json:"attestation,omitempty"`
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
	Git                GitClient
	GitHub             func(repoPath string) GitHubClient
	AgentLookup        func(provider string) (agent.Runner, error)
	AcquireLock        func(repoPath string, timeout time.Duration) (Lock, error)
	MkdirTemp          func(dir, pattern string) (string, error)
	RemoveAll          func(path string) error
	ReviewPacketLimits ReviewPacketLimits
}

type ReviewPacketLimits struct {
	ChangedFilesBytes int
	DiffBytes         int
	DiffFileBytes     int
	IssueBytes        int
	SpecBytes         int
	TotalPromptBytes  int
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
	Path           string
	Content        string
	Available      bool
	Reason         string
	ExpectedAbsent bool
	ExpectedReason string
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
	resilience, err := config.ResilienceForRepo(ctx, repoPath, config.LoadOptions{
		BaseBranch:     opts.BaseBranch,
		ConfigFromBase: opts.ConfigFromBase,
	})
	if err != nil {
		return Result{}, err
	}
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
	prompt, packet := buildPromptWithLimits(opts, inputs, deps.ReviewPacketLimits)
	if packet.Insufficient {
		note := "review packet insufficient: " + packet.InsufficientReason
		verdict := needsHumanVerdict("warning", "", note)
		return Result{Verdict: verdict, ExitCode: ExitCodeForVerdict(verdict.Verdict)}, nil
	}
	if err := checkoutPRWorktree(ctx, deps, repoPath, worktreePath, opts.PRNumber); err != nil {
		return Result{}, err
	}

	agentResult, agentErr := runner.Run(ctx, agent.Invocation{
		WorktreePath: worktreePath,
		Prompt:       prompt,
		Model:        opts.Model,
		Effort:       opts.Effort,
		ReadOnly:     true,
		OutputSchema: VerdictJSONSchema,
		LogPath:      logPath,
		HardCap:      opts.Timeout,
		StallTimeout: config.DurationSeconds(resilience.Verifier.StallTimeoutSeconds, VerifierStallTimeout),
		RunID:        fmt.Sprintf("loopreview-%d", opts.PRNumber),
		Role:         "verifier",
	})
	if agentResult.Hung {
		verdict := verifierHungVerdict(opts.Provider, logPath, opts.Timeout, agentResult.HungReason)
		return Result{Verdict: verdict, ExitCode: ExitCodeForVerdict(verdict.Verdict)}, nil
	}
	record := verifierAttestation(opts, agentResult)
	fmt.Fprintln(warnings, record.Header())
	if agentErr != nil {
		verdict := needsHumanVerdict("error", "", providerFailureNote(logPath, fmt.Sprintf("%s verifier failed: %v", opts.Provider, agentErr)))
		return resultWithAttestation(verdict, record), nil
	}
	if agentResult.ExitCode != 0 {
		verdict := needsHumanVerdict("error", "", providerFailureNote(logPath, fmt.Sprintf("%s verifier exited with code %d; see %s", opts.Provider, agentResult.ExitCode, logPath)))
		return resultWithAttestation(verdict, record), nil
	}

	verdict, err := ParseVerdict(agentResult.Summary)
	if err != nil {
		verdict = needsHumanVerdict("error", "", fmt.Sprintf("structured verdict parse failed: %v", err))
		return resultWithAttestation(verdict, record), nil
	}
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
	verdict.Findings = nonNilFindings(verdict.Findings)
	return resultWithAttestation(verdict, record), nil
}

func verifierAttestation(opts Options, result agent.Result) attestation.AttestationRecord {
	return attestation.AttestationRecord{
		Role:        attestation.RoleVerifier,
		Provider:    opts.Provider,
		Model:       result.Model,
		ModelSource: attestation.ModelSourceParsed,
		Effort:      result.Effort,
		Permission:  attestation.PermissionReadOnly,
		Action:      fmt.Sprintf("review PR #%d", opts.PRNumber),
		ExitCode:    result.ExitCode,
		StartedAt:   result.StartedAt,
		EndedAt:     result.EndedAt,
		DurationMS:  result.DurationMS,
		Usage:       result.Usage,
		Verified:    true,
	}
}

func resultWithAttestation(verdict Verdict, record attestation.AttestationRecord) Result {
	verdict.Attestation = &record
	if err := record.Validate(); err != nil {
		note := "incomplete verifier attestation: " + err.Error()
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
	prompt, _ := buildPromptWithLimits(opts, inputs, ReviewPacketLimits{})
	return prompt
}

type reviewPacket struct {
	PRNumber                 int
	PRTitle                  string
	HeadRef                  string
	BaseBranch               string
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
	ChangedFiles             changedFilesSection
	Diff                     packetSection
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
	baseBranch := opts.BaseBranch
	if strings.TrimSpace(baseBranch) == "" {
		baseBranch = "main"
	}

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
		PRNumber:           opts.PRNumber,
		PRTitle:            inputs.PR.Title,
		HeadRef:            inputs.PR.HeadRefName,
		BaseBranch:         baseBranch,
		IssueNumber:        issueNumber,
		IssueTitle:         issueTitle,
		IssueBody:          truncatePacketSection(issueBody, limits.IssueBytes),
		SpecPath:           specPath,
		SpecAvailable:      inputs.Spec.Available,
		SpecReason:         specReason,
		SpecExpectedAbsent: inputs.Spec.ExpectedAbsent,
		SpecExpectedReason: inputs.Spec.ExpectedReason,
		DocumentationOnly:  documentationOnly,
		SpecPathChanged:    specPathChanged,
		SpecPathAdded:      specPathAdded,
		SpecContent:        truncatePacketSection(inputs.Spec.Content, limits.SpecBytes),
		ChangedFiles:       buildChangedFilesSection(inputs.ChangedFiles, limits.ChangedFilesBytes),
		Diff:               buildDiffSection(inputs.Diff, limits.DiffBytes, limits.DiffFileBytes),
		Limits:             limits,
	}
}

func formatReviewPrompt(opts Options, packet reviewPacket) string {
	return fmt.Sprintf(`You are the independent loopcoder Verifier for pull request #%d.

Review adversarially. You are not the implementation worker. Do not modify files, commit, push, write review comments, or run shell commands.

Return only JSON matching this schema:

%s

# Review contract
- Use the bounded review packet below as the primary evidence.
- Compare the bounded diff excerpts against the GitHub issue, acceptance criteria, and merged design/spec.
- For complete packets with no relevant TRUNCATED markers, decide from the packet instead of exploring the repository.
- Use "pass" only when the PR satisfies the issue and spec and you found no blocking concerns.
- Use "fail" for concrete implementation defects, missing acceptance criteria, regressions, or test gaps that should be fixed by a worker.
- Use "needs-human" when evidence is incomplete, ambiguous, unavailable, or unsafe to decide automatically.
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
		ChangedFilesBytes: reviewPacketChangedFilesBudgetBytes,
		DiffBytes:         reviewPacketDiffBudgetBytes,
		DiffFileBytes:     reviewPacketDiffFileBudgetBytes,
		IssueBytes:        reviewPacketIssueBudgetBytes,
		SpecBytes:         reviewPacketSpecBudgetBytes,
		TotalPromptBytes:  reviewPacketTotalPromptBudgetBytes,
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
	if limits.IssueBytes <= 0 {
		limits.IssueBytes = defaults.IssueBytes
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
		&limits.SpecBytes,
		&limits.IssueBytes,
		&limits.ChangedFilesBytes,
		&limits.DiffFileBytes,
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
	fmt.Fprintf(&out, "Base: %s\n\n", packet.BaseBranch)

	fmt.Fprintf(&out, "# Changed files\n")
	fmt.Fprintf(&out, "Total changed files: %d\n", packet.ChangedFiles.TotalFiles)
	fmt.Fprintf(&out, "Documentation-only: %s\n", yesNo(packet.DocumentationOnly))
	fmt.Fprintf(&out, "Referenced spec changed in PR: %s\n", yesNo(packet.SpecPathChanged))
	fmt.Fprintf(&out, "Referenced spec added in PR: %s\n", yesNo(packet.SpecPathAdded))
	fmt.Fprintf(&out, "Budget: %d bytes\n", packet.Limits.ChangedFilesBytes)
	fmt.Fprintf(&out, "%s\n\n", formatChangedFilesSection(packet.ChangedFiles))

	fmt.Fprintf(&out, "# Diff excerpts\n")
	fmt.Fprintf(&out, "Total diff budget: %d bytes\n", packet.Limits.DiffBytes)
	fmt.Fprintf(&out, "Per-file diff budget: %d bytes\n", packet.Limits.DiffFileBytes)
	fmt.Fprintf(&out, "%s\n\n", formatPacketSection("diff", packet.Diff))

	fmt.Fprintf(&out, "# Issue\n")
	fmt.Fprintf(&out, "Number: %s\n", packet.IssueNumber)
	fmt.Fprintf(&out, "Title: %s\n", packet.IssueTitle)
	fmt.Fprintf(&out, "Issue-body budget: %d bytes\n", packet.Limits.IssueBytes)
	fmt.Fprintf(&out, "%s\n\n", formatPacketSection("issue body", packet.IssueBody))

	fmt.Fprintf(&out, "# Merged design/spec from origin/%s\n", packet.BaseBranch)
	fmt.Fprintf(&out, "Path: %s\n", packet.SpecPath)
	if packet.SpecAvailable {
		fmt.Fprintf(&out, "Status: available\n")
		fmt.Fprintf(&out, "Spec budget: %d bytes\n", packet.Limits.SpecBytes)
		fmt.Fprintf(&out, "%s\n", formatPacketSection("merged spec", packet.SpecContent))
	} else if packet.SpecExpectedAbsent {
		fmt.Fprintf(&out, "Status: expected absent from origin/%s\n", packet.BaseBranch)
		fmt.Fprintf(&out, "Classification: expected/non-blocking\n")
		fmt.Fprintf(&out, "Reason: %s\n", packet.SpecExpectedReason)
		fmt.Fprintf(&out, "Spec conformance: not-applicable\n")
	} else {
		fmt.Fprintf(&out, "Status: unavailable\n")
		fmt.Fprintf(&out, "Classification: missing evidence\n")
		fmt.Fprintf(&out, "Reason: %s\n", packet.SpecReason)
	}
	return out.String()
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

func buildDiffSection(diff string, diffBudget, perFileBudget int) packetSection {
	diff = strings.TrimRight(diff, "\n")
	if strings.TrimSpace(diff) == "" {
		return packetSection{}
	}
	patches := splitDiffPatches(diff)
	var out strings.Builder
	omittedBytes := 0
	omittedLines := 0
	omittedFiles := []string{}
	for i, patch := range patches {
		patchSection := truncatePacketSection(patch.Text, perFileBudget)
		block := formatDiffPatchBlock(patch.File, patchSection)
		if out.Len()+len(block) > diffBudget {
			for _, omitted := range patches[i:] {
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
	inputs.Spec = classifySpecAbsence(inputs.Spec, opts.BaseBranch, changedFiles, diff)
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
		baseBranch = "main"
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
