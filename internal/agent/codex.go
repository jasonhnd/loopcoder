package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type ExecCodexRunner struct{}

func BuildCodexArgs(inv Invocation) []string {
	args := []string{
		"exec",
		"--cd", inv.WorktreePath,
	}
	if inv.ReadOnly {
		args = append(args, "-s", "read-only")
	} else {
		args = append(args, "--dangerously-bypass-approvals-and-sandbox")
	}
	args = append(args, "--skip-git-repo-check")
	if strings.TrimSpace(inv.OutputSchema) != "" {
		args = append(args, "--output-schema", inv.OutputSchema)
	}
	if strings.TrimSpace(inv.Model) != "" {
		args = append(args, "-m", inv.Model)
	}
	if strings.TrimSpace(inv.Effort) != "" {
		args = append(args, "-c", "model_reasoning_effort="+inv.Effort)
	}
	args = append(args, "-o", codexSummaryPath(inv.LogPath), "-")
	return args
}

func (ExecCodexRunner) Run(ctx context.Context, inv Invocation) (Result, error) {
	if strings.TrimSpace(inv.LogPath) == "" {
		return Result{ExitCode: -1}, errors.New("codex log path is required")
	}
	promptPath := codexPromptPath(inv.LogPath)
	if err := os.WriteFile(promptPath, []byte(inv.Prompt), 0o644); err != nil {
		return Result{ExitCode: -1}, fmt.Errorf("write prompt: %w", err)
	}
	prompt, err := os.Open(promptPath)
	if err != nil {
		return Result{ExitCode: -1}, fmt.Errorf("open prompt: %w", err)
	}
	defer prompt.Close()

	logFile, err := os.Create(inv.LogPath)
	if err != nil {
		return Result{ExitCode: -1}, fmt.Errorf("open codex log: %w", err)
	}
	defer logFile.Close()

	cmd := exec.CommandContext(ctx, "codex", BuildCodexArgs(inv)...)
	cmd.Stdin = prompt
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode := exitErr.ExitCode()
			if exitCode >= 0 {
				return Result{ExitCode: exitCode, Summary: readCodexSummary(inv.LogPath)}, nil
			}
		}
		return Result{ExitCode: -1}, err
	}
	return Result{ExitCode: 0, Summary: readCodexSummary(inv.LogPath)}, nil
}

func codexPromptPath(logPath string) string {
	return filepath.Join(filepath.Dir(logPath), "prompt.txt")
}

func codexSummaryPath(logPath string) string {
	return filepath.Join(filepath.Dir(logPath), "summary.txt")
}

func readCodexSummary(logPath string) string {
	summaryBytes, err := os.ReadFile(codexSummaryPath(logPath))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(summaryBytes))
}
