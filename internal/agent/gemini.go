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
	"path/filepath"
	"strings"
	"time"
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
		args = append(args, "--skip-trust", "--extensions", "none")
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
	if inv.ReadOnly {
		if err := os.WriteFile(geminiReadOnlySettingsPath(inv.LogPath), []byte(geminiReadOnlySettings), 0o644); err != nil {
			return Result{ExitCode: -1}, fmt.Errorf("write gemini read-only settings: %w", err)
		}
	}

	var stdout bytes.Buffer
	cmd := exec.CommandContext(ctx, "gemini", BuildGeminiArgs(inv)...)
	cmd.Dir = inv.WorktreePath
	if inv.ReadOnly {
		cmd.Env = append(os.Environ(), "GEMINI_CLI_SYSTEM_SETTINGS_PATH="+geminiReadOnlySettingsPath(inv.LogPath))
	}
	cmd.Stdout = io.MultiWriter(logFile, &stdout)
	cmd.Stderr = logFile

	started := time.Now()
	runErr := cmd.Run()
	ended := time.Now()
	summary := parseGeminiSummary(stdout.Bytes())
	metadata := parseGeminiMetadata(stdout.Bytes())
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

func geminiReadOnlySettingsPath(logPath string) string {
	return filepath.Join(filepath.Dir(logPath), "gemini-read-only-settings.json")
}

const geminiReadOnlySettings = `{"tools":{"core":[]}}`

func parseGeminiMetadata(stdout []byte) resultMetadata {
	var metadata resultMetadata
	root, ok := decodeJSONMap(stdout)
	if !ok {
		return metadata
	}

	metadata.Model = firstGeminiModel(root)
	metadata.Usage = firstUsageInObject(root, 6)
	return metadata
}

func firstGeminiModel(root map[string]any) string {
	if model := stringFromMap(root, "model", "modelVersion", "model_version"); model != "" {
		return model
	}
	if stats, ok := objectFromMap(root, "stats"); ok {
		if model := stringFromMap(stats, "model", "modelVersion", "model_version"); model != "" {
			return model
		}
		if models, ok := objectFromMap(stats, "models"); ok {
			return modelFromKeyedMap(models)
		}
	}
	return ""
}
