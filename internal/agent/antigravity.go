package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/models"
)

type AntigravityRunner struct{}

func init() {
	registry["antigravity"] = AntigravityRunner{}
}

func BuildAntigravityArgs(inv Invocation) []string {
	args := []string{
		"-p", inv.Prompt,
		"--add-dir", inv.WorktreePath,
	}
	if selectedModel := AntigravitySelectedModel(inv.Model, inv.Effort); selectedModel != "" {
		args = append(args, "--model", selectedModel)
	}
	return args
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
		return resultWithTiming(1, "", metadata, startedAt, endedAt), errors.New("antigravity read-only mode is not available or verified")
	}
	if strings.TrimSpace(inv.LogPath) == "" {
		return Result{ExitCode: -1}, errors.New("antigravity log path is required")
	}
	if _, err := mcpServersForInvocation(inv); err != nil {
		return Result{ExitCode: -1}, fmt.Errorf("antigravity MCP configuration: %w", err)
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
