package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
		schemaArg := strings.TrimSpace(inv.OutputSchema)
		if strings.HasPrefix(schemaArg, "{") || strings.HasPrefix(schemaArg, "[") {
			schemaArg = codexSchemaPath(inv.LogPath)
		}
		args = append(args, "--output-schema", schemaArg)
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
	if strings.TrimSpace(inv.OutputSchema) != "" {
		if err := os.WriteFile(codexSchemaPath(inv.LogPath), []byte(inv.OutputSchema), 0o644); err != nil {
			return Result{ExitCode: -1}, fmt.Errorf("write output schema: %w", err)
		}
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

	startedAt := time.Now()
	runErr := cmd.Run()
	endedAt := time.Now()
	_ = logFile.Sync()
	logBytes, _ := os.ReadFile(inv.LogPath)
	summary := readCodexSummary(inv.LogPath)
	metadata := parseCodexInvocation(logBytes)
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode := exitErr.ExitCode()
			if exitCode >= 0 {
				return resultWithTiming(exitCode, summary, metadata, startedAt, endedAt), nil
			}
		}
		return resultWithTiming(-1, summary, metadata, startedAt, endedAt), runErr
	}
	return resultWithTiming(0, summary, metadata, startedAt, endedAt), nil
}

func codexPromptPath(logPath string) string {
	return filepath.Join(filepath.Dir(logPath), "prompt.txt")
}

func codexSummaryPath(logPath string) string {
	return filepath.Join(filepath.Dir(logPath), "summary.txt")
}

func codexSchemaPath(logPath string) string {
	return filepath.Join(filepath.Dir(logPath), "schema.json")
}

func readCodexSummary(logPath string) string {
	summaryBytes, err := os.ReadFile(codexSummaryPath(logPath))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(summaryBytes))
}

func parseCodexInvocation(output []byte) invocationMetadata {
	text := string(output)
	metadata := invocationMetadata{
		Model:  parseCodexHeaderValue(text, "model"),
		Effort: parseCodexHeaderValue(text, "reasoning effort"),
	}
	if totalTokens, ok := parseCodexTotalTokens(text); ok {
		metadata.Usage.TotalTokens = &totalTokens
	}
	return metadata
}

func parseCodexHeaderValue(text, label string) string {
	prefix := strings.ToLower(label) + ":"
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(trimmed), prefix) {
			return strings.TrimSpace(trimmed[len(prefix):])
		}
	}
	return ""
}

func parseCodexTotalTokens(text string) (int64, bool) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for index, line := range lines {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)
		if !strings.HasPrefix(lower, "tokens used") {
			continue
		}

		remainder := strings.TrimSpace(trimmed[len("tokens used"):])
		remainder = strings.TrimPrefix(remainder, ":")
		remainder = strings.TrimSpace(remainder)
		if total, ok := parseTokenCount(remainder); ok {
			return total, true
		}

		for next := index + 1; next < len(lines); next++ {
			nextLine := strings.TrimSpace(lines[next])
			if nextLine == "" {
				continue
			}
			return parseTokenCount(nextLine)
		}
	}
	return 0, false
}

func parseTokenCount(text string) (int64, bool) {
	var cleaned strings.Builder
	for _, r := range strings.TrimSpace(text) {
		switch {
		case r >= '0' && r <= '9':
			cleaned.WriteRune(r)
		case r == ',' || r == ' ' || r == '\t':
			continue
		default:
			return 0, false
		}
	}
	if cleaned.Len() == 0 {
		return 0, false
	}
	value, err := strconv.ParseInt(cleaned.String(), 10, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}
