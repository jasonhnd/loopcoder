package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
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
	defer func() {
		if logFile != nil {
			_ = logFile.Close()
		}
	}()

	cmd := exec.CommandContext(ctx, "codex", BuildCodexArgs(inv)...)
	cmd.Stdin = prompt
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	started := time.Now()
	runErr := cmd.Run()
	ended := time.Now()
	_ = logFile.Sync()
	_ = logFile.Close()
	logFile = nil
	logOutput, _ := os.ReadFile(inv.LogPath)
	summary := readCodexSummary(inv.LogPath)
	metadata := parseCodexMetadata(logOutput)

	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode := exitErr.ExitCode()
			if exitCode >= 0 {
				return resultWithTiming(exitCode, summary, metadata, started, ended), nil
			}
		}
		return resultWithTiming(-1, summary, metadata, started, ended), runErr
	}
	return resultWithTiming(0, summary, metadata, started, ended), nil
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

var (
	codexModelLinePattern  = regexp.MustCompile(`(?i)^\s*model\s*:\s*(.+?)\s*$`)
	codexEffortLinePattern = regexp.MustCompile(`(?i)^\s*reasoning\s+effort\s*:\s*(.+?)\s*$`)
	codexTokensPattern     = regexp.MustCompile(`(?i)\btokens\s+used\b\s*:?\s*([0-9][0-9, _]*)`)
)

func parseCodexMetadata(output []byte) resultMetadata {
	var metadata resultMetadata
	lines := strings.Split(strings.ReplaceAll(string(output), "\r\n", "\n"), "\n")
	for _, line := range lines {
		if metadata.Model == "" {
			if matches := codexModelLinePattern.FindStringSubmatch(line); len(matches) == 2 {
				metadata.Model = strings.TrimSpace(matches[1])
			}
		}
		if metadata.Effort == "" {
			if matches := codexEffortLinePattern.FindStringSubmatch(line); len(matches) == 2 {
				metadata.Effort = strings.TrimSpace(matches[1])
			}
		}
		if metadata.Usage.TotalTokens == nil {
			if matches := codexTokensPattern.FindStringSubmatch(line); len(matches) == 2 {
				if total, ok := parseInt64Text(matches[1]); ok {
					metadata.Usage.TotalTokens = int64Ptr(total)
				}
			}
		}
	}
	return metadata
}
