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
		args = append(args,
			"--safe-mode",
			"--no-session-persistence",
			"--allowedTools", "Read Grep Glob",
		)
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
	supervision, runErr := runProviderCommand(ctx, cmd, inv, "claude")
	endedAt := time.Now()
	summary := parseClaudeSummary(stdout.Bytes())
	metadata := parseClaudeInvocation(stdout.Bytes(), inv)
	result := resultWithSupervision(supervisedExitCode(supervision, runErr), summary, metadata, startedAt, endedAt, supervision, runErr, ctx)
	if runErr != nil {
		return result, runErr
	}
	return result, nil
}

func parseClaudeSummary(output []byte) string {
	trimmed := bytes.TrimSpace(output)
	if len(trimmed) == 0 {
		return ""
	}

	var payload struct {
		Result           string          `json:"result"`
		StructuredOutput json.RawMessage `json:"structured_output"`
	}
	if err := json.Unmarshal(trimmed, &payload); err != nil {
		return ""
	}
	if structured := bytes.TrimSpace(payload.StructuredOutput); len(structured) > 0 && !bytes.Equal(structured, []byte("null")) {
		var compacted bytes.Buffer
		if err := json.Compact(&compacted, structured); err == nil {
			return compacted.String()
		}
		return string(structured)
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
			TotalTokens  json.RawMessage `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(trimmed, &payload); err != nil {
		return metadata
	}

	metadata.Model = claudePrimaryModel(payload.ModelUsage)
	if inv.Model != "" {
		if _, ok := payload.ModelUsage[inv.Model]; ok {
			metadata.Model = inv.Model
		}
	}
	if inputTokens, ok := parseRawInt64(payload.Usage.InputTokens); ok {
		metadata.Usage.InputTokens = inputTokens
	}
	if outputTokens, ok := parseRawInt64(payload.Usage.OutputTokens); ok {
		metadata.Usage.OutputTokens = outputTokens
	}
	if totalTokens, ok := parseRawInt64(payload.Usage.TotalTokens); ok {
		metadata.Usage.TotalTokens = totalTokens
	}
	return metadata
}

func claudePrimaryModel(values map[string]json.RawMessage) string {
	if len(values) == 0 {
		return ""
	}
	type candidate struct {
		model  string
		tokens int64
	}
	best := candidate{}
	for model, raw := range values {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		var usage struct {
			InputTokens  json.RawMessage `json:"inputTokens"`
			OutputTokens json.RawMessage `json:"outputTokens"`
		}
		_ = json.Unmarshal(raw, &usage)
		tokens := int64(0)
		if input, ok := parseRawInt64(usage.InputTokens); ok && input != nil {
			tokens += *input
		}
		if output, ok := parseRawInt64(usage.OutputTokens); ok && output != nil {
			tokens += *output
		}
		if best.model == "" || tokens > best.tokens || (tokens == best.tokens && model < best.model) {
			best = candidate{model: model, tokens: tokens}
		}
	}
	if best.model == "" {
		return firstSortedKey(values)
	}
	return best.model
}
