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

	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode := exitErr.ExitCode()
			if exitCode >= 0 {
				return Result{ExitCode: exitCode, Summary: parseClaudeSummary(stdout.Bytes())}, nil
			}
		}
		return Result{ExitCode: -1}, err
	}
	return Result{ExitCode: 0, Summary: parseClaudeSummary(stdout.Bytes())}, nil
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
