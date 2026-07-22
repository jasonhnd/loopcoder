package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/models"
	"github.com/jasonhnd/loopcoder/internal/runtimecap"
)

type AntigravityRunner struct{}

func init() {
	registry["antigravity"] = AntigravityRunner{}
}

func BuildAntigravityArgs(inv Invocation) []string {
	// --dangerously-skip-permissions is required for non-interactive/headless agy:
	// without it, jetski auto-denies tool permission prompts and can exit 0 with
	// "no output produced" while writing nothing useful into the worktree.
	// --new-project binds agy project affinity to the worktree session so writes
	// do not land in a shared global project root outside the assigned worktree.
	// Read-only invocations never reach agy (fail closed in Run before launch).
	worktree := strings.TrimSpace(inv.WorktreePath)
	if abs, err := filepath.Abs(worktree); err == nil && abs != "" {
		worktree = abs
	}
	// Isolated child workspace is the sole project context (no parent root writes).
	args := []string{
		"-p", inv.Prompt,
		"--add-dir", worktree,
		"--dangerously-skip-permissions",
		"--new-project",
	}
	if selectedModel := AntigravitySelectedModel(inv.Model, inv.Effort); selectedModel != "" {
		args = append(args, "--model", selectedModel)
	}
	return args
}

// antigravityHeadlessPermissionDenied reports jetski headless permission auto-denial.
// These runs are not useful capacity consumption and must not close as succeeded.
func antigravityHeadlessPermissionDenied(stdout, logText string) bool {
	blob := strings.ToLower(stdout + "\n" + logText)
	if strings.Contains(blob, "dangerously-skip-permissions") &&
		(strings.Contains(blob, "no output produced") || strings.Contains(blob, "auto-denied") || strings.Contains(blob, "cannot prompt")) {
		return true
	}
	if strings.Contains(blob, "jetski: no output produced") {
		return true
	}
	return false
}

func AntigravitySelectedModel(model, effort string) string {
	model = strings.TrimSpace(model)
	effort = strings.TrimSpace(effort)
	if provider, ok := models.LookupProvider("antigravity"); ok {
		if model == "" {
			model = provider.DefaultModel
		}
		if effort == "" {
			if selected, ok := provider.LookupModel(model); ok {
				effort = selected.DefaultDepth
			}
		}
	}
	if model == "" {
		return ""
	}
	if effort == "" {
		return model
	}
	return fmt.Sprintf("%s (%s)", model, effort)
}

func (AntigravityRunner) Run(ctx context.Context, inv Invocation) (Result, error) {
	selectedModel := AntigravitySelectedModel(inv.Model, inv.Effort)
	selectedEffort := strings.TrimSpace(inv.Effort)
	if selectedEffort == "" {
		selectedEffort = antigravityDefaultEffort(strings.TrimSpace(inv.Model))
	}
	metadata := invocationMetadata{
		Model:  selectedModel,
		Effort: selectedEffort,
	}
	startedAt := time.Now()
	if inv.ReadOnly {
		endedAt := time.Now()
		return resultWithTiming(1, "", metadata, startedAt, endedAt), runtimecap.RequireProviderCapability("antigravity", runtimecap.ProviderReadOnly)
	}
	if strings.TrimSpace(inv.LogPath) == "" {
		return Result{ExitCode: -1}, errors.New("antigravity log path is required")
	}
	mcpServers, err := mcpServersForInvocation(inv)
	if err != nil {
		return Result{ExitCode: -1}, fmt.Errorf("antigravity MCP configuration: %w", err)
	}
	if len(mcpServers) > 0 {
		return Result{ExitCode: -1}, fmt.Errorf("antigravity MCP configuration: %w", runtimecap.RequireProviderCapability("antigravity", runtimecap.ProviderMCPConfig))
	}
	if strings.TrimSpace(inv.OutputSchema) != "" {
		return Result{ExitCode: -1}, fmt.Errorf("antigravity output schema: %w", runtimecap.RequireProviderCapability("antigravity", runtimecap.ProviderJSONOutput))
	}

	logFile, err := createSensitiveFile(inv.LogPath)
	if err != nil {
		return Result{ExitCode: -1}, fmt.Errorf("open antigravity log: %w", err)
	}
	defer logFile.Close()

	var stdout bytes.Buffer
	cmd := exec.CommandContext(ctx, "agy", BuildAntigravityArgs(inv)...)
	cmd.Dir = inv.WorktreePath
	cmd.Stdin = nil
	cmd.Stdout = io.MultiWriter(logFile, &stdout)
	cmd.Stderr = logFile

	startedAt = time.Now()
	supervision, runErr := runProviderCommand(ctx, cmd, inv, "antigravity")
	endedAt := time.Now()
	summary := strings.TrimSpace(stdout.String())
	// Best-effort re-read log for denial phrases that may land only on stderr/log.
	logText := ""
	if b, rerr := os.ReadFile(inv.LogPath); rerr == nil {
		logText = string(b)
	}
	if antigravityHeadlessPermissionDenied(summary, logText) {
		msg := "antigravity headless permission denial (no useful output); require --dangerously-skip-permissions"
		if summary == "" {
			summary = msg
		}
		result := resultWithSupervision(1, summary, metadata, startedAt, endedAt, supervision, runErr, ctx)
		if runErr != nil {
			return result, runErr
		}
		return result, errors.New(msg)
	}
	result := resultWithSupervision(supervisedExitCode(supervision, runErr), summary, metadata, startedAt, endedAt, supervision, runErr, ctx)
	if runErr != nil {
		return result, runErr
	}
	return result, nil
}

func antigravityDefaultEffort(model string) string {
	provider, ok := models.LookupProvider("antigravity")
	if !ok {
		return ""
	}
	if model == "" {
		model = provider.DefaultModel
	}
	selected, ok := provider.LookupModel(model)
	if !ok {
		return ""
	}
	return selected.DefaultDepth
}
