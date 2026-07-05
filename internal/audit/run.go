package audit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/agent"
	"github.com/jasonhnd/loopcoder/internal/config"
	lcdefaults "github.com/jasonhnd/loopcoder/internal/defaults"
	"github.com/jasonhnd/loopcoder/internal/supervisedexec"
)

type Deps struct {
	Runner      CommandRunner
	AgentLookup func(provider string) (agent.Runner, error)
}

type ExecRunner struct{}

func DefaultDeps() Deps {
	return Deps{
		Runner:      ExecRunner{},
		AgentLookup: agent.Lookup,
	}
}

func Run(ctx context.Context, opts Options, deps Deps) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	deps = withDefaults(deps)
	repoPath, err := resolveRepo(opts.RepoPath)
	if err != nil {
		return Result{}, err
	}
	cfg, err := loadAuditConfig(ctx, repoPath, opts)
	if err != nil {
		result := NewResult(repoPath, []string{LayerSAST}, SeverityMedium)
		result.RuntimeFailures = append(result.RuntimeFailures, err.Error())
		return Finalize(result), nil
	}
	plan, err := BuildPlan(repoPath, cfg.Audit, opts)
	if err != nil {
		result := NewResult(repoPath, []string{LayerSAST}, SeverityMedium)
		result.RuntimeFailures = append(result.RuntimeFailures, err.Error())
		return Finalize(result), nil
	}
	result := NewResult(repoPath, plan.Layers, plan.Threshold)

	if containsLayer(plan.Layers, LayerSAST) {
		runSASTLayer(ctx, repoPath, plan, deps, &result)
	}
	if containsLayer(plan.Layers, LayerLLM) {
		runLLMLayer(ctx, repoPath, cfg, plan, opts, deps, &result)
	}

	return Finalize(result), nil
}

func withDefaults(deps Deps) Deps {
	defaults := DefaultDeps()
	if deps.Runner == nil {
		deps.Runner = defaults.Runner
	}
	if deps.AgentLookup == nil {
		deps.AgentLookup = defaults.AgentLookup
	}
	return deps
}

func reviewProviderOptions(cfg config.Config, opts Options) (provider, model, effort string, timeout time.Duration) {
	provider = strings.TrimSpace(opts.Provider)
	if provider == "" {
		provider = strings.TrimSpace(cfg.Adapters.Verifier)
	}
	model = strings.TrimSpace(opts.Model)
	if model == "" {
		model = strings.TrimSpace(cfg.Verifier.Model)
	}
	effort = strings.TrimSpace(opts.Effort)
	if effort == "" {
		effort = strings.TrimSpace(cfg.Verifier.ReasoningEffort)
	}
	timeout = opts.Timeout
	if timeout <= 0 {
		timeout = config.DurationSeconds(cfg.Resilience.Verifier.HardCapSeconds, lcdefaults.VerifierTimeout)
	}
	return provider, model, effort, timeout
}

func runSASTLayer(ctx context.Context, repoPath string, plan Plan, deps Deps, result *Result) {
	before, tracked, err := trackedStatus(repoPath)
	if err != nil {
		result.RuntimeFailures = append(result.RuntimeFailures, fmt.Sprintf("read worktree status before audit: %v", err))
	}

	for _, command := range plan.Commands {
		runSASTCommand(ctx, repoPath, command, deps.Runner, result)
	}
	if plan.Native.Secrets || plan.Native.FilePermissions {
		nativeFindings, nativeErr := RunNativeScans(repoPath, plan.Native)
		if nativeErr != nil {
			result.RuntimeFailures = append(result.RuntimeFailures, nativeErr.Error())
		}
		result.Findings = append(result.Findings, nativeFindings...)
	}

	if tracked {
		after, _, statusErr := trackedStatus(repoPath)
		if statusErr != nil {
			result.RuntimeFailures = append(result.RuntimeFailures, fmt.Sprintf("read worktree status after audit: %v", statusErr))
		} else if before != after {
			result.RuntimeFailures = append(result.RuntimeFailures, "audit command changed tracked worktree state: "+changedStatusSummary(before, after))
		}
	}
}

func runSASTCommand(ctx context.Context, repoPath string, command SASTCommand, runner CommandRunner, result *Result) {
	invocation := CommandInvocation{
		ID:         command.ID,
		Argv:       append([]string(nil), command.Argv...),
		Parser:     command.Parser,
		WorkingDir: repoPath,
		Timeout:    command.Timeout,
		OutputCap:  lcdefaults.HookInputMaxBytes,
	}
	toolResult := ToolResult{
		ID:     command.ID,
		Argv:   append([]string(nil), command.Argv...),
		Parser: command.Parser,
	}
	run := runner.RunCommand(ctx, invocation)
	toolResult.DurationMS = run.Duration.Milliseconds()
	toolResult.ExitCode = run.ExitCode
	toolResult.OutputTruncated = run.OutputTruncated

	if run.Err != nil {
		toolResult.ParseStatus = "runtime-failure"
		toolResult.Error = run.Err.Error()
		result.ToolResults = append(result.ToolResults, toolResult)
		result.RuntimeFailures = append(result.RuntimeFailures, fmt.Sprintf("%s: %v", command.ID, run.Err))
		return
	}
	if run.TimedOut {
		toolResult.ParseStatus = "timeout"
		toolResult.Error = fmt.Sprintf("timed out after %s", command.Timeout)
		result.ToolResults = append(result.ToolResults, toolResult)
		result.RuntimeFailures = append(result.RuntimeFailures, fmt.Sprintf("%s timed out after %s", command.ID, command.Timeout))
		return
	}
	if run.OutputTruncated {
		toolResult.ParseStatus = "output-truncated"
		toolResult.Error = "command output exceeded audit output cap"
		result.ToolResults = append(result.ToolResults, toolResult)
		result.RuntimeFailures = append(result.RuntimeFailures, fmt.Sprintf("%s output exceeded audit output cap", command.ID))
		return
	}

	findings, parseErr := ParseToolOutput(command.Parser, command.ID, combinedParserOutput(run.Stdout, run.Stderr))
	if parseErr != nil {
		toolResult.ParseStatus = "parse-failed"
		toolResult.Error = parseErr.Error()
		result.ToolResults = append(result.ToolResults, toolResult)
		result.RuntimeFailures = append(result.RuntimeFailures, fmt.Sprintf("%s output could not be parsed: %v", command.ID, parseErr))
		return
	}
	toolResult.ParseStatus = "parsed"
	result.ToolResults = append(result.ToolResults, toolResult)
	result.Findings = append(result.Findings, findings...)
	if run.ExitCode != 0 && len(findings) == 0 {
		result.RuntimeFailures = append(result.RuntimeFailures, fmt.Sprintf("%s exited with code %d and no parseable findings", command.ID, run.ExitCode))
	}
}

func (ExecRunner) RunCommand(ctx context.Context, invocation CommandInvocation) CommandRunResult {
	start := time.Now()
	if len(invocation.Argv) == 0 {
		return CommandRunResult{
			ExitCode: 127,
			Duration: time.Since(start),
			Err:      errors.New("argv must be non-empty"),
		}
	}
	if invocation.OutputCap <= 0 {
		invocation.OutputCap = lcdefaults.HookInputMaxBytes
	}
	cmd := exec.Command(invocation.Argv[0], invocation.Argv[1:]...)
	cmd.Dir = invocation.WorkingDir

	stdout := &boundedBuffer{Limit: invocation.OutputCap}
	stderr := &boundedBuffer{Limit: invocation.OutputCap}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	run, err := supervisedexec.Run(ctx, cmd, supervisedexec.Options{HardCap: invocation.Timeout})
	result := CommandRunResult{
		ExitCode:        run.ExitCode,
		Stdout:          stdout.String(),
		Stderr:          stderr.String(),
		Duration:        run.Elapsed,
		TimedOut:        run.Outcome == supervisedexec.OutcomeDeadline,
		OutputTruncated: stdout.Truncated || stderr.Truncated,
		Err:             err,
	}
	if result.Duration <= 0 {
		result.Duration = time.Since(start)
	}
	if err != nil {
		return result
	}
	if result.TimedOut {
		return result
	}
	return result
}

type boundedBuffer struct {
	Limit     int64
	Truncated bool
	buf       bytes.Buffer
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.Limit <= 0 {
		return len(p), nil
	}
	remaining := b.Limit - int64(b.buf.Len())
	if remaining <= 0 {
		b.Truncated = true
		return len(p), nil
	}
	if int64(len(p)) > remaining {
		b.Truncated = true
		_, _ = b.buf.Write(p[:int(remaining)])
		return len(p), nil
	}
	_, _ = b.buf.Write(p)
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	return b.buf.String()
}

func combinedParserOutput(stdout, stderr string) string {
	if strings.TrimSpace(stdout) != "" {
		return stdout
	}
	return stderr
}

func containsLayer(layers []string, layer string) bool {
	for _, candidate := range layers {
		if candidate == layer {
			return true
		}
	}
	return false
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

func trackedStatus(repoPath string) (string, bool, error) {
	cmd := exec.Command("git", "-C", repoPath, "rev-parse", "--is-inside-work-tree")
	if output, err := cmd.Output(); err != nil || strings.TrimSpace(string(output)) != "true" {
		return "", false, nil
	}
	statusCmd := exec.Command("git", "-C", repoPath, "status", "--porcelain", "--untracked-files=no")
	var output bytes.Buffer
	statusCmd.Stdout = &output
	statusCmd.Stderr = &output
	run, err := supervisedexec.Run(context.Background(), statusCmd, supervisedexec.Options{HardCap: lcdefaults.GitCommandHardCap})
	if err != nil {
		return "", true, err
	}
	if run.Outcome == supervisedexec.OutcomeDeadline {
		return "", true, fmt.Errorf("git status timed out after %s", lcdefaults.GitCommandHardCap)
	}
	if run.ExitCode != 0 {
		return "", true, fmt.Errorf("git status exited with code %d: %s", run.ExitCode, strings.TrimSpace(output.String()))
	}
	return normalizeStatus(output.String()), true, nil
}

func normalizeStatus(status string) string {
	lines := strings.Split(strings.ReplaceAll(status, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

func changedStatusSummary(before, after string) string {
	if strings.TrimSpace(after) == "" {
		return "(tracked status changed to clean)"
	}
	return after
}
