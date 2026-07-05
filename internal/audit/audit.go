// Package audit runs loopcoder's built-in deterministic security audit.
package audit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/config"
	lcdefaults "github.com/jasonhnd/loopcoder/internal/defaults"
	"github.com/jasonhnd/loopcoder/internal/gitutil"
	"github.com/jasonhnd/loopcoder/internal/supervisedexec"
)

const (
	LayerSAST = "sast"
	LayerLLM  = "llm"

	VerdictClean      = "clean"
	VerdictFindings   = "findings"
	VerdictNeedsHuman = "needs-human"

	SeverityCritical = "critical"
	SeverityHigh     = "high"
	SeverityMedium   = "medium"
	SeverityLow      = "low"
	SeverityInfo     = "info"

	ParseStatusParsed = "parsed"
	ParseStatusFailed = "failed"

	DefaultThreshold = SeverityMedium

	defaultSASTTimeoutSeconds = 300
	maxCommandOutputBytes     = 4 << 20
)

type Options struct {
	RepoPath       string
	Layers         []string
	Threshold      string
	BaseBranch     string
	ConfigFromBase bool
}

type Result struct {
	SchemaVersion   int          `json:"schema_version"`
	Repo            string       `json:"repo"`
	Layers          []string     `json:"layers"`
	Threshold       string       `json:"threshold"`
	Verdict         string       `json:"verdict"`
	Findings        []Finding    `json:"findings"`
	ToolResults     []ToolResult `json:"tool_results"`
	NeedsHuman      []NeedHuman  `json:"needs_human"`
	RuntimeFailures []string     `json:"runtime_failures,omitempty"`
}

type Finding struct {
	ID          string `json:"id"`
	Layer       string `json:"layer"`
	Tool        string `json:"tool,omitempty"`
	Severity    string `json:"severity"`
	File        string `json:"file"`
	Line        int    `json:"line,omitempty"`
	Column      int    `json:"column,omitempty"`
	Rule        string `json:"rule"`
	Category    string `json:"category"`
	Message     string `json:"message"`
	Evidence    string `json:"evidence"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Waived      bool   `json:"waived,omitempty"`
	WaiverID    string `json:"waiver_id,omitempty"`
}

type ToolResult struct {
	ID              string   `json:"id"`
	Argv            []string `json:"argv"`
	Parser          string   `json:"parser"`
	DurationMS      int64    `json:"duration_ms"`
	ExitStatus      int      `json:"exit_status"`
	OutputTruncated bool     `json:"output_truncated,omitempty"`
	ParseStatus     string   `json:"parse_status"`
	FindingCount    int      `json:"finding_count"`
	Error           string   `json:"error,omitempty"`
}

type NeedHuman struct {
	Layer   string `json:"layer"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

type Plan struct {
	Threshold string
	Commands  []SASTCommand
	Native    NativePlan
}

type SASTCommand struct {
	ID             string
	Argv           []string
	Parser         string
	TimeoutSeconds int
}

type NativePlan struct {
	Secrets         bool
	FilePermissions bool
	Include         []string
	Exclude         []string
}

type GitStatus interface {
	StatusPorcelain(ctx context.Context, repoPath string) (string, error)
}

type CommandRunner interface {
	Run(ctx context.Context, invocation CommandInvocation) CommandRunResult
}

type CommandInvocation struct {
	ID               string
	Argv             []string
	WorkingDirectory string
	Timeout          time.Duration
	MaxOutputBytes   int
}

type CommandRunResult struct {
	Stdout          []byte
	Stderr          []byte
	ExitCode        int
	DurationMS      int64
	TimedOut        bool
	OutputTruncated bool
	Err             error
}

type Deps struct {
	LoadConfig func(ctx context.Context, repoPath string, opts config.LoadOptions) (config.Config, error)
	Git        GitStatus
	Runner     CommandRunner
}

type ExecRunner struct{}

func DefaultDeps() Deps {
	return Deps{
		LoadConfig: config.LoadForRepo,
		Git:        gitutil.New(),
		Runner:     ExecRunner{},
	}
}

func Run(ctx context.Context, opts Options, deps Deps) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	deps = withDefaults(deps)
	displayRepo := strings.TrimSpace(opts.RepoPath)
	if displayRepo == "" {
		displayRepo = "."
	}
	layers, layerErr := NormalizeLayers(opts.Layers)
	result := Result{
		SchemaVersion: 1,
		Repo:          displayRepo,
		Layers:        nonNilStrings(layers),
		Threshold:     DefaultThreshold,
		Verdict:       VerdictNeedsHuman,
		Findings:      []Finding{},
		ToolResults:   []ToolResult{},
		NeedsHuman:    []NeedHuman{},
	}
	if layerErr != nil {
		result.Layers = nonNilStrings(opts.Layers)
		appendRuntimeFailure(&result, LayerSAST, "invalid-layer", layerErr.Error())
		finalize(&result)
		return result, nil
	}

	repoPath, err := resolveRepo(opts.RepoPath)
	if err != nil {
		appendRuntimeFailure(&result, LayerSAST, "repo", err.Error())
		finalize(&result)
		return result, nil
	}

	baseBranch := strings.TrimSpace(opts.BaseBranch)
	if baseBranch == "" {
		baseBranch = lcdefaults.BaseBranch
	}
	cfg, err := deps.LoadConfig(ctx, repoPath, config.LoadOptions{
		BaseBranch:     baseBranch,
		ConfigFromBase: opts.ConfigFromBase,
		Warnings:       io.Discard,
	})
	if err != nil {
		appendRuntimeFailure(&result, LayerSAST, "config", err.Error())
		finalize(&result)
		return result, nil
	}

	plan, err := ResolvePlan(repoPath, cfg, opts.Threshold)
	if err != nil {
		appendRuntimeFailure(&result, LayerSAST, "config", err.Error())
		finalize(&result)
		return result, nil
	}
	result.Threshold = plan.Threshold

	beforeStatus, statusKnown, statusErr := statusPorcelain(ctx, deps.Git, repoPath)
	if statusErr != nil {
		appendRuntimeFailure(&result, LayerSAST, "git-status", statusErr.Error())
	}

	if containsLayer(layers, LayerSAST) {
		findings, toolResults, failures := runSAST(ctx, repoPath, plan, deps.Runner)
		result.Findings = append(result.Findings, findings...)
		result.ToolResults = append(result.ToolResults, toolResults...)
		for _, failure := range failures {
			appendRuntimeFailure(&result, LayerSAST, "sast-command", failure)
		}
	}
	if containsLayer(layers, LayerLLM) {
		result.NeedsHuman = append(result.NeedsHuman, NeedHuman{
			Layer:   LayerLLM,
			Reason:  "not-implemented",
			Message: "LLM audit layer is reserved for the C2 implementation slice",
		})
	}

	if statusKnown {
		afterStatus, err := deps.Git.StatusPorcelain(ctx, repoPath)
		if err != nil {
			appendRuntimeFailure(&result, LayerSAST, "git-status", fmt.Sprintf("read post-audit worktree status: %v", err))
		} else if beforeStatus != afterStatus {
			appendRuntimeFailure(&result, LayerSAST, "worktree-mutated", "audit changed the worktree: "+changedStatusPaths(beforeStatus, afterStatus))
		}
	}

	SortFindings(result.Findings)
	finalize(&result)
	return result, nil
}

func ResolvePlan(repoPath string, cfg config.Config, thresholdOverride string) (Plan, error) {
	threshold := strings.ToLower(strings.TrimSpace(thresholdOverride))
	if threshold == "" {
		threshold = strings.ToLower(strings.TrimSpace(cfg.Audit.SeverityThreshold))
	}
	if threshold == "" {
		threshold = DefaultThreshold
	}
	if !ValidSeverity(threshold) {
		return Plan{}, fmt.Errorf("audit threshold %q is not one of critical, high, medium, low, info", threshold)
	}

	commands := make([]SASTCommand, 0, len(cfg.Audit.SAST.Commands))
	if cfg.Audit.SAST.Commands != nil {
		for _, command := range cfg.Audit.SAST.Commands {
			commands = append(commands, SASTCommand{
				ID:             commandID(command.ID, command.Argv),
				Argv:           append([]string(nil), command.Argv...),
				Parser:         strings.ToLower(strings.TrimSpace(command.Parser)),
				TimeoutSeconds: command.TimeoutSeconds,
			})
		}
	} else if fileExists(filepath.Join(repoPath, "go.mod")) {
		commands = defaultGoCommands()
	}
	for index := range commands {
		if commands[index].TimeoutSeconds <= 0 {
			commands[index].TimeoutSeconds = defaultSASTTimeoutSeconds
		}
		if len(commands[index].Argv) == 0 {
			return Plan{}, fmt.Errorf("audit SAST command %q has empty argv", commands[index].ID)
		}
		if strings.TrimSpace(commands[index].Parser) == "" {
			return Plan{}, fmt.Errorf("audit SAST command %q has empty parser", commands[index].ID)
		}
		if !ValidParser(commands[index].Parser) {
			return Plan{}, fmt.Errorf("audit SAST command %q parser %q is not recognized", commands[index].ID, commands[index].Parser)
		}
	}

	native := NativePlan{
		Secrets:         true,
		FilePermissions: true,
		Include:         []string{"**/*"},
		Exclude:         []string{".git/**", "vendor/**", "dist/**"},
	}
	if cfg.Audit.SAST.Native.Secrets != nil {
		native.Secrets = *cfg.Audit.SAST.Native.Secrets
	}
	if cfg.Audit.SAST.Native.FilePermissions != nil {
		native.FilePermissions = *cfg.Audit.SAST.Native.FilePermissions
	}
	if len(cfg.Audit.SAST.Native.Include) > 0 {
		native.Include = cleanPatterns(cfg.Audit.SAST.Native.Include)
	}
	if len(cfg.Audit.SAST.Native.Exclude) > 0 {
		native.Exclude = cleanPatterns(cfg.Audit.SAST.Native.Exclude)
	}

	return Plan{Threshold: threshold, Commands: commands, Native: native}, nil
}

func (ExecRunner) Run(ctx context.Context, invocation CommandInvocation) CommandRunResult {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(invocation.Argv) == 0 {
		return CommandRunResult{ExitCode: -1, Err: errors.New("empty argv")}
	}
	timeout := invocation.Timeout
	if timeout <= 0 {
		timeout = defaultSASTTimeoutSeconds * time.Second
	}
	limit := invocation.MaxOutputBytes
	if limit <= 0 {
		limit = maxCommandOutputBytes
	}

	cmd := exec.CommandContext(ctx, invocation.Argv[0], invocation.Argv[1:]...)
	cmd.Dir = invocation.WorkingDirectory
	stdout := &limitedBuffer{limit: limit}
	stderr := &limitedBuffer{limit: limit}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	started := time.Now()
	runResult, err := supervisedexec.Run(ctx, cmd, supervisedexec.Options{HardCap: timeout})
	durationMS := time.Since(started).Milliseconds()
	truncated := stdout.truncated || stderr.truncated
	if truncated {
		return CommandRunResult{
			Stdout:          stdout.bytes(),
			Stderr:          stderr.bytes(),
			ExitCode:        -1,
			DurationMS:      durationMS,
			OutputTruncated: true,
			Err:             fmt.Errorf("%s output exceeded %d bytes", invocation.ID, limit),
		}
	}
	if err != nil {
		return CommandRunResult{
			Stdout:     stdout.bytes(),
			Stderr:     stderr.bytes(),
			ExitCode:   -1,
			DurationMS: durationMS,
			Err:        err,
		}
	}
	if runResult.Outcome == supervisedexec.OutcomeDeadline {
		return CommandRunResult{
			Stdout:     stdout.bytes(),
			Stderr:     stderr.bytes(),
			ExitCode:   -1,
			DurationMS: durationMS,
			TimedOut:   true,
			Err:        fmt.Errorf("%s timed out after %s", invocation.ID, timeout),
		}
	}
	return CommandRunResult{
		Stdout:     stdout.bytes(),
		Stderr:     stderr.bytes(),
		ExitCode:   runResult.ExitCode,
		DurationMS: durationMS,
	}
}

func runSAST(ctx context.Context, repoPath string, plan Plan, runner CommandRunner) ([]Finding, []ToolResult, []string) {
	findings := []Finding{}
	toolResults := []ToolResult{}
	failures := []string{}

	for _, command := range plan.Commands {
		tool := ToolResult{
			ID:          command.ID,
			Argv:        append([]string(nil), command.Argv...),
			Parser:      command.Parser,
			ParseStatus: ParseStatusFailed,
		}
		run := runner.Run(ctx, CommandInvocation{
			ID:               command.ID,
			Argv:             command.Argv,
			WorkingDirectory: repoPath,
			Timeout:          time.Duration(command.TimeoutSeconds) * time.Second,
			MaxOutputBytes:   maxCommandOutputBytes,
		})
		tool.DurationMS = run.DurationMS
		tool.ExitStatus = run.ExitCode
		tool.OutputTruncated = run.OutputTruncated
		if run.TimedOut {
			tool.Error = "timeout"
			toolResults = append(toolResults, tool)
			failures = append(failures, fmt.Sprintf("%s timed out", command.ID))
			continue
		}
		if run.OutputTruncated {
			tool.Error = "output exceeded bound"
			toolResults = append(toolResults, tool)
			failures = append(failures, fmt.Sprintf("%s output exceeded bound", command.ID))
			continue
		}
		if run.Err != nil {
			tool.Error = run.Err.Error()
			toolResults = append(toolResults, tool)
			failures = append(failures, fmt.Sprintf("%s failed to run: %v", command.ID, run.Err))
			continue
		}

		parsed, err := ParseToolOutput(command.Parser, command.ID, firstNonEmptyBytes(run.Stdout, run.Stderr))
		if err != nil {
			tool.Error = err.Error()
			toolResults = append(toolResults, tool)
			failures = append(failures, fmt.Sprintf("%s output could not be parsed: %v", command.ID, err))
			continue
		}
		if run.ExitCode != 0 && len(parsed) == 0 {
			tool.Error = fmt.Sprintf("exit status %d with no parseable findings", run.ExitCode)
			toolResults = append(toolResults, tool)
			failures = append(failures, fmt.Sprintf("%s exited %d with no parseable findings", command.ID, run.ExitCode))
			continue
		}

		tool.ParseStatus = ParseStatusParsed
		tool.FindingCount = len(parsed)
		toolResults = append(toolResults, tool)
		findings = append(findings, parsed...)
	}

	nativeFindings := ScanNative(repoPath, plan.Native)
	findings = append(findings, nativeFindings...)

	return findings, toolResults, failures
}

func NormalizeLayers(values []string) ([]string, error) {
	if len(values) == 0 {
		return []string{LayerSAST}, nil
	}
	seen := map[string]bool{}
	layers := []string{}
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			layer := strings.ToLower(strings.TrimSpace(part))
			if layer == "" {
				continue
			}
			if layer == "all" {
				for _, expanded := range []string{LayerSAST, LayerLLM} {
					if !seen[expanded] {
						seen[expanded] = true
						layers = append(layers, expanded)
					}
				}
				continue
			}
			if !ValidLayer(layer) {
				return nil, fmt.Errorf("unsupported audit layer %q", layer)
			}
			if !seen[layer] {
				seen[layer] = true
				layers = append(layers, layer)
			}
		}
	}
	if len(layers) == 0 {
		return nil, errors.New("at least one audit layer is required")
	}
	return layers, nil
}

func ExitCode(result Result) int {
	if len(result.RuntimeFailures) > 0 {
		return 3
	}
	switch result.Verdict {
	case VerdictClean:
		return 0
	case VerdictFindings:
		return 1
	case VerdictNeedsHuman:
		return 2
	default:
		return 3
	}
}

func SortFindings(findings []Finding) {
	sort.SliceStable(findings, func(i, j int) bool {
		left := findings[i]
		right := findings[j]
		if severityRank(left.Severity) != severityRank(right.Severity) {
			return severityRank(left.Severity) < severityRank(right.Severity)
		}
		if left.File != right.File {
			return left.File < right.File
		}
		if left.Line != right.Line {
			return left.Line < right.Line
		}
		if left.Rule != right.Rule {
			return left.Rule < right.Rule
		}
		return left.Fingerprint < right.Fingerprint
	})
}

func NewFinding(layer, tool, severity, file string, line, column int, rule, category, message, evidence string) Finding {
	file = normalizeRepoPath(file)
	evidence = boundEvidence(RedactSecrets(strings.TrimSpace(evidence)))
	finding := Finding{
		Layer:    strings.ToLower(strings.TrimSpace(layer)),
		Tool:     strings.TrimSpace(tool),
		Severity: normalizeSeverity(severity),
		File:     file,
		Line:     line,
		Column:   column,
		Rule:     strings.TrimSpace(rule),
		Category: strings.TrimSpace(category),
		Message:  strings.TrimSpace(message),
		Evidence: evidence,
	}
	if finding.Severity == "" {
		finding.Severity = SeverityMedium
	}
	finding.Fingerprint = findingFingerprint(finding)
	finding.ID = findingID(finding)
	return finding
}

func ValidLayer(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case LayerSAST, LayerLLM:
		return true
	default:
		return false
	}
}

func ValidSeverity(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case SeverityCritical, SeverityHigh, SeverityMedium, SeverityLow, SeverityInfo:
		return true
	default:
		return false
	}
}

func ValidParser(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "govulncheck-json", "staticcheck-json", "gosec-json", "generic-line":
		return true
	default:
		return false
	}
}

func withDefaults(deps Deps) Deps {
	defaults := DefaultDeps()
	if deps.LoadConfig == nil {
		deps.LoadConfig = defaults.LoadConfig
	}
	if deps.Git == nil {
		deps.Git = defaults.Git
	}
	if deps.Runner == nil {
		deps.Runner = defaults.Runner
	}
	return deps
}

func defaultGoCommands() []SASTCommand {
	return []SASTCommand{
		{ID: "govulncheck", Argv: []string{"govulncheck", "-json", "./..."}, Parser: "govulncheck-json", TimeoutSeconds: defaultSASTTimeoutSeconds},
		{ID: "staticcheck", Argv: []string{"staticcheck", "-f", "json", "./..."}, Parser: "staticcheck-json", TimeoutSeconds: defaultSASTTimeoutSeconds},
		{ID: "gosec", Argv: []string{"gosec", "-fmt", "json", "-quiet", "./..."}, Parser: "gosec-json", TimeoutSeconds: defaultSASTTimeoutSeconds},
	}
}

func commandID(id string, argv []string) string {
	id = strings.TrimSpace(id)
	if id != "" {
		return id
	}
	if len(argv) == 0 {
		return "command"
	}
	base := filepath.Base(strings.TrimSpace(argv[0]))
	if base == "" || base == "." || base == string(filepath.Separator) {
		return "command"
	}
	return base
}

func cleanPatterns(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			out = append(out, normalizeRepoPath(trimmed))
		}
	}
	return out
}

func finalize(result *Result) {
	if result.Findings == nil {
		result.Findings = []Finding{}
	}
	if result.ToolResults == nil {
		result.ToolResults = []ToolResult{}
	}
	if result.NeedsHuman == nil {
		result.NeedsHuman = []NeedHuman{}
	}
	if len(result.RuntimeFailures) > 0 || len(result.NeedsHuman) > 0 {
		result.Verdict = VerdictNeedsHuman
		return
	}
	for _, finding := range result.Findings {
		if finding.Waived {
			continue
		}
		if severityAtOrAbove(finding.Severity, result.Threshold) {
			result.Verdict = VerdictFindings
			return
		}
	}
	result.Verdict = VerdictClean
}

func appendRuntimeFailure(result *Result, layer, reason, message string) {
	message = strings.TrimSpace(message)
	if message == "" {
		message = reason
	}
	result.RuntimeFailures = append(result.RuntimeFailures, message)
	result.NeedsHuman = append(result.NeedsHuman, NeedHuman{Layer: layer, Reason: reason, Message: message})
}

func statusPorcelain(ctx context.Context, git GitStatus, repoPath string) (string, bool, error) {
	if !hasGitMetadata(repoPath) {
		return "", false, nil
	}
	status, err := git.StatusPorcelain(ctx, repoPath)
	if err != nil {
		return "", false, fmt.Errorf("read pre-audit worktree status: %w", err)
	}
	return status, true, nil
}

func hasGitMetadata(repoPath string) bool {
	_, err := os.Stat(filepath.Join(repoPath, ".git"))
	return err == nil
}

func changedStatusPaths(before, after string) string {
	beforeSet := statusPathSet(before)
	afterSet := statusPathSet(after)
	changed := []string{}
	for path := range afterSet {
		if !beforeSet[path] {
			changed = append(changed, path)
		}
	}
	if len(changed) == 0 {
		for path := range afterSet {
			changed = append(changed, path)
		}
	}
	sort.Strings(changed)
	if len(changed) == 0 {
		return "(status changed with no paths parsed)"
	}
	return strings.Join(changed, ", ")
}

func statusPathSet(status string) map[string]bool {
	out := map[string]bool{}
	for _, line := range strings.Split(strings.ReplaceAll(status, "\r\n", "\n"), "\n") {
		if len(line) < 4 {
			continue
		}
		path := strings.TrimSpace(line[3:])
		if index := strings.Index(path, " -> "); index >= 0 {
			path = strings.TrimSpace(path[index+4:])
		}
		if path != "" {
			out[normalizeRepoPath(path)] = true
		}
	}
	return out
}

func containsLayer(layers []string, layer string) bool {
	for _, candidate := range layers {
		if candidate == layer {
			return true
		}
	}
	return false
}

func firstNonEmptyBytes(values ...[]byte) []byte {
	for _, value := range values {
		if len(bytes.TrimSpace(value)) > 0 {
			return value
		}
	}
	return nil
}

func normalizeRepoPath(path string) string {
	path = strings.TrimSpace(strings.ReplaceAll(path, `\`, `/`))
	path = strings.TrimPrefix(path, "./")
	return path
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func resolveRepo(repoPath string) (string, error) {
	if strings.TrimSpace(repoPath) == "" {
		repoPath = "."
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

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return append([]string(nil), values...)
}

func normalizeSeverity(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "error":
		return SeverityHigh
	case "warning", "warn":
		return SeverityMedium
	case SeverityCritical, SeverityHigh, SeverityMedium, SeverityLow, SeverityInfo:
		return value
	default:
		return SeverityMedium
	}
}

func severityRank(severity string) int {
	switch normalizeSeverity(severity) {
	case SeverityCritical:
		return 0
	case SeverityHigh:
		return 1
	case SeverityMedium:
		return 2
	case SeverityLow:
		return 3
	default:
		return 4
	}
}

func severityAtOrAbove(severity, threshold string) bool {
	return severityRank(severity) <= severityRank(threshold)
}

func findingFingerprint(f Finding) string {
	parts := []string{
		f.Layer,
		f.Tool,
		normalizeSeverity(f.Severity),
		normalizeRepoPath(f.File),
		fmt.Sprintf("%d", f.Line),
		fmt.Sprintf("%d", f.Column),
		f.Rule,
		strings.ToLower(strings.TrimSpace(f.Evidence)),
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func findingID(f Finding) string {
	tool := strings.TrimSpace(f.Tool)
	if tool == "" {
		tool = f.Layer
	}
	file := f.File
	if file == "" {
		file = "global"
	}
	location := file
	if f.Line > 0 {
		location = fmt.Sprintf("%s:%d", file, f.Line)
	}
	return strings.ReplaceAll(fmt.Sprintf("audit-%s:%s:%s", tool, f.Rule, location), " ", "-")
}

func boundEvidence(value string) string {
	value = strings.TrimSpace(value)
	const maxEvidence = 500
	if len(value) <= maxEvidence {
		return value
	}
	return value[:maxEvidence] + "..."
}

type limitedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.limit <= 0 {
		return b.buf.Write(p)
	}
	remaining := b.limit - b.buf.Len()
	if remaining <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if len(p) > remaining {
		b.truncated = true
		_, _ = b.buf.Write(p[:remaining])
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *limitedBuffer) bytes() []byte {
	return append([]byte(nil), b.buf.Bytes()...)
}
