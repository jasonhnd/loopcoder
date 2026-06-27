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

type GeminiRunner struct{}

func init() {
	registry["gemini"] = GeminiRunner{}
}

func BuildGeminiArgs(inv Invocation) []string {
	args := []string{
		"--prompt", inv.Prompt,
	}
	if inv.ReadOnly {
		args = append(args, "--approval-mode", "plan")
	} else {
		args = append(args, "--yolo")
	}
	if strings.TrimSpace(inv.Model) != "" {
		args = append(args, "-m", inv.Model)
	}
	// Gemini has no separate schema flag; JSON output is its structured-output mode.
	args = append(args, "--output-format", "json")
	return args
}

func (GeminiRunner) Run(ctx context.Context, inv Invocation) (Result, error) {
	if strings.TrimSpace(inv.LogPath) == "" {
		return Result{ExitCode: -1}, errors.New("gemini log path is required")
	}

	logFile, err := os.Create(inv.LogPath)
	if err != nil {
		return Result{ExitCode: -1}, fmt.Errorf("open gemini log: %w", err)
	}
	defer logFile.Close()

	if strings.TrimSpace(inv.Effort) != "" {
		fmt.Fprintf(logFile, "[loopcoder] advisory: gemini ignores effort %q; Gemini CLI has no reasoning-effort knob\n", inv.Effort)
	}

	var stdout bytes.Buffer
	cmd := exec.CommandContext(ctx, "gemini", BuildGeminiArgs(inv)...)
	cmd.Dir = inv.WorktreePath
	cmd.Stdout = io.MultiWriter(logFile, &stdout)
	cmd.Stderr = logFile

	runErr := cmd.Run()
	summary := parseGeminiSummary(stdout.Bytes())
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode := exitErr.ExitCode()
			if exitCode >= 0 {
				return Result{ExitCode: exitCode, Summary: summary}, nil
			}
		}
		return Result{ExitCode: -1}, runErr
	}
	return Result{ExitCode: 0, Summary: summary}, nil
}

func parseGeminiSummary(stdout []byte) string {
	trimmed := strings.TrimSpace(string(stdout))
	if trimmed == "" {
		return ""
	}

	var payload struct {
		Response string `json:"response"`
		Summary  string `json:"summary"`
		Text     string `json:"text"`
		Message  string `json:"message"`
	}
	if err := json.Unmarshal([]byte(trimmed), &payload); err != nil {
		return trimmed
	}

	for _, candidate := range []string{payload.Response, payload.Summary, payload.Text, payload.Message} {
		if strings.TrimSpace(candidate) != "" {
			return strings.TrimSpace(candidate)
		}
	}
	return ""
}
