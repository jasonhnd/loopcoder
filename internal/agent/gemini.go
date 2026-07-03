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
	"sort"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/attestation"
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

	if advisory := geminiEffortAdvisory(inv.Effort); advisory != "" {
		fmt.Fprintln(logFile, advisory)
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

	startedAt := time.Now()
	supervision, runErr := runProviderCommand(ctx, cmd, inv, "gemini")
	endedAt := time.Now()
	summary := parseGeminiSummary(stdout.Bytes())
	metadata := parseGeminiInvocation(stdout.Bytes())
	result := resultWithSupervision(supervisedExitCode(supervision, runErr), summary, metadata, startedAt, endedAt, supervision, runErr, ctx)
	if runErr != nil {
		return result, runErr
	}
	return result, nil
}

func geminiEffortAdvisory(effort string) string {
	effort = strings.TrimSpace(effort)
	if effort == "" {
		return ""
	}
	return fmt.Sprintf("[loopcoder] advisory: gemini ignores effort %q; Gemini CLI has no reasoning-effort knob", effort)
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

func parseGeminiInvocation(stdout []byte) invocationMetadata {
	trimmed := bytes.TrimSpace(stdout)
	if len(trimmed) == 0 {
		return invocationMetadata{}
	}

	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	var payload any
	if err := decoder.Decode(&payload); err != nil {
		return invocationMetadata{}
	}
	root, ok := payload.(map[string]any)
	if !ok {
		return invocationMetadata{}
	}

	return invocationMetadata{
		Model: findGeminiModel(root),
		Usage: findGeminiUsage(root),
	}
}

func findGeminiModel(root map[string]any) string {
	for _, candidate := range []map[string]any{root, mapValue(root, "stats")} {
		if candidate == nil {
			continue
		}
		if model := firstStringValue(candidate, "model", "model_name", "modelName"); model != "" {
			return model
		}
		if model := modelFromGeminiModels(candidate["models"]); model != "" {
			return model
		}
	}
	return ""
}

func modelFromGeminiModels(value any) string {
	switch models := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(models))
		for key := range models {
			if strings.TrimSpace(key) != "" {
				keys = append(keys, strings.TrimSpace(key))
			}
		}
		sort.Strings(keys)
		if len(keys) > 0 {
			return keys[0]
		}
	case []any:
		for _, item := range models {
			if model, ok := item.(string); ok && strings.TrimSpace(model) != "" {
				return strings.TrimSpace(model)
			}
			if modelMap, ok := item.(map[string]any); ok {
				if model := firstStringValue(modelMap, "model", "model_name", "modelName"); model != "" {
					return model
				}
			}
		}
	}
	return ""
}

func findGeminiUsage(root map[string]any) attestation.Usage {
	var usage attestation.Usage
	for _, candidate := range geminiUsageCandidateMaps(root) {
		mergeUsage(&usage, usageFromMap(candidate))
	}
	return usage
}

func geminiUsageCandidateMaps(root map[string]any) []map[string]any {
	candidates := []map[string]any{root}
	for _, parent := range []map[string]any{root, mapValue(root, "stats")} {
		if parent == nil {
			continue
		}
		for _, key := range []string{"usage", "token_usage", "tokenUsage", "tokens"} {
			if candidate := mapValue(parent, key); candidate != nil {
				candidates = append(candidates, candidate)
			}
		}
		if models, ok := parent["models"].(map[string]any); ok {
			keys := make([]string, 0, len(models))
			for key := range models {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				if candidate, ok := models[key].(map[string]any); ok {
					candidates = append(candidates, candidate)
				}
			}
		}
	}
	return candidates
}

func usageFromMap(values map[string]any) attestation.Usage {
	var usage attestation.Usage
	usage.InputTokens = firstInt64Value(values, "input_tokens", "inputTokens", "inputTokenCount", "prompt_tokens", "promptTokens", "promptTokenCount")
	usage.OutputTokens = firstInt64Value(values, "output_tokens", "outputTokens", "outputTokenCount", "completion_tokens", "completionTokens", "candidate_tokens", "candidatesTokenCount")
	usage.TotalTokens = firstInt64Value(values, "total_tokens", "totalTokens", "totalTokenCount")
	return usage
}

func mergeUsage(dst *attestation.Usage, src attestation.Usage) {
	if dst.InputTokens == nil {
		dst.InputTokens = src.InputTokens
	}
	if dst.OutputTokens == nil {
		dst.OutputTokens = src.OutputTokens
	}
	if dst.TotalTokens == nil {
		dst.TotalTokens = src.TotalTokens
	}
}

func firstStringValue(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := values[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func firstInt64Value(values map[string]any, keys ...string) *int64 {
	for _, key := range keys {
		if value, ok := values[key]; ok {
			if parsed, ok := parseAnyInt64(value); ok {
				return parsed
			}
		}
	}
	return nil
}

func mapValue(values map[string]any, key string) map[string]any {
	value, ok := values[key].(map[string]any)
	if !ok {
		return nil
	}
	return value
}

func geminiReadOnlySettingsPath(logPath string) string {
	return filepath.Join(filepath.Dir(logPath), "gemini-read-only-settings.json")
}

const geminiReadOnlySettings = `{"tools":{"core":[]}}`
