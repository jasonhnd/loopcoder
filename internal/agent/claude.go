package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

type ClaudeRunner struct{}

func init() {
	registry["claude"] = ClaudeRunner{}
}

func BuildClaudeArgs(inv Invocation) []string {
	args := []string{
		"--print",
	}
	if inv.ReadOnly {
		args = append(args, "--allowedTools", "Read Grep Glob")
	} else {
		args = append(args, "--dangerously-skip-permissions")
	}
	args = append(args, "--output-format", "json")
	if strings.TrimSpace(inv.OutputSchema) != "" {
		args = append(args, "--json-schema", inv.OutputSchema)
	}
	if strings.TrimSpace(inv.Model) != "" {
		args = append(args, "--model", inv.Model)
	}
	if strings.TrimSpace(inv.Effort) != "" {
		args = append(args, "--effort", inv.Effort)
	}
	return args
}

func (ClaudeRunner) Run(ctx context.Context, inv Invocation) (Result, error) {
	if strings.TrimSpace(inv.LogPath) == "" {
		return Result{ExitCode: -1}, errors.New("claude log path is required")
	}

	logFile, err := os.Create(inv.LogPath)
	if err != nil {
		return Result{ExitCode: -1}, fmt.Errorf("open claude log: %w", err)
	}
	defer logFile.Close()

	var stdout bytes.Buffer
	cmd := exec.CommandContext(ctx, "claude", BuildClaudeArgs(inv)...)
	cmd.Dir = inv.WorktreePath
	cmd.Stdin = strings.NewReader(inv.Prompt)
	cmd.Stdout = io.MultiWriter(logFile, &stdout)
	cmd.Stderr = logFile

	startedAt := time.Now()
	runErr := cmd.Run()
	endedAt := time.Now()
	summary := parseClaudeSummary(stdout.Bytes())
	metadata := parseClaudeInvocation(stdout.Bytes(), inv)
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

func parseClaudeSummary(output []byte) string {
	trimmed := bytes.TrimSpace(output)
	if len(trimmed) == 0 {
		return ""
	}

	var payload struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(trimmed, &payload); err != nil {
		return ""
	}
	return strings.TrimSpace(payload.Result)
}

func parseClaudeInvocation(output []byte, inv Invocation) invocationMetadata {
	metadata := invocationMetadata{
		Effort: strings.TrimSpace(inv.Effort),
	}

	trimmed := bytes.TrimSpace(output)
	if len(trimmed) == 0 {
		return metadata
	}

	var payload struct {
		ModelUsage map[string]json.RawMessage `json:"modelUsage"`
		Usage      struct {
			InputTokens  json.RawMessage `json:"input_tokens"`
			OutputTokens json.RawMessage `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(trimmed, &payload); err != nil {
		return metadata
	}

	metadata.Model = firstSortedKey(payload.ModelUsage)
	if inputTokens, ok := parseRawInt64(payload.Usage.InputTokens); ok {
		metadata.Usage.InputTokens = inputTokens
	}
	if outputTokens, ok := parseRawInt64(payload.Usage.OutputTokens); ok {
		metadata.Usage.OutputTokens = outputTokens
	}
	return metadata
}
