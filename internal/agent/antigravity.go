package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/models"
	"github.com/jasonhnd/loopcoder/internal/runtimecap"
)

type AntigravityRunner struct{}

func init() {
	registry["antigravity"] = AntigravityRunner{}
}

func BuildAntigravityArgs(inv Invocation) []string {
	// --dangerously-skip-permissions is required for non-interactive/headless agy:
	// without it, jetski auto-denies tool permission prompts and can exit 0 with
	// "no output produced" while writing nothing useful into the worktree.
	// --new-project binds agy project affinity to the worktree session so writes
	// do not land in a shared global project root outside the assigned worktree.
	// Read-only invocations never reach agy (fail closed in Run before launch).
	worktree := strings.TrimSpace(inv.WorktreePath)
	if abs, err := filepath.Abs(worktree); err == nil && abs != "" {
		worktree = abs
	}
	// Isolated child workspace is the sole project context (no parent root writes).
	args := []string{
		"-p", inv.Prompt,
		"--add-dir", worktree,
		"--dangerously-skip-permissions",
		"--new-project",
	}
	if selectedModel := AntigravitySelectedModel(inv.Model, inv.Effort); selectedModel != "" {
		args = append(args, "--model", selectedModel)
	}
	return args
}

// antigravityHeadlessPermissionDenied reports jetski headless permission auto-denial.
// These runs are not useful capacity consumption and must not close as succeeded.
func antigravityHeadlessPermissionDenied(stdout, logText string) bool {
	blob := strings.ToLower(stdout + "\n" + logText)
	if strings.Contains(blob, "dangerously-skip-permissions") &&
		(strings.Contains(blob, "no output produced") || strings.Contains(blob, "auto-denied") || strings.Contains(blob, "cannot prompt")) {
		return true
	}
	if strings.Contains(blob, "jetski: no output produced") {
		return true
	}
	return false
}

func AntigravitySelectedModel(model, effort string) string {
	model = strings.TrimSpace(model)
	effort = strings.TrimSpace(effort)
	// Exact observed CLI tokens pass through unchanged — never re-wrap or
	// rewrite depth. Includes parenthetical display forms and agy slugs.
	if isExactAgyModelToken(model) {
		return model
	}
	if provider, ok := models.LookupProvider("antigravity"); ok {
		if model == "" {
			model = provider.DefaultModel
		}
		// Empty effort may use curated default depth only (not a silent downgrade
		// of an explicit unsupported required depth).
		if effort == "" {
			if selected, ok := provider.LookupModel(model); ok {
				effort = selected.DefaultDepth
			}
		} else if selected, ok := provider.LookupModel(model); ok && len(selected.Depths) > 0 {
			// Explicit effort not listed for this model: refuse to invent
			// "base (unsupported-depth)" or silently swap to DefaultDepth.
			if _, ok := selected.LookupDepth(effort); !ok {
				return model
			}
		}
	}
	if model == "" {
		return ""
	}
	if effort == "" {
		return model
	}
	// Curated base + supported depth only.
	return formatAgyCLIModel(model, effort)
}

// isExactAgyModelToken reports an already-observed invocation token that must
// not be rewritten (CLI display form or machine slug).
func isExactAgyModelToken(model string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	if strings.Contains(model, " (") && strings.HasSuffix(model, ")") {
		return true
	}
	// Machine-readable slug (gpt-oss-120b-medium) — only treated as exact when
	// it is a single token (no spaces), i.e. not a human base name.
	if strings.Contains(model, "-") && !strings.ContainsAny(model, " /") {
		return true
	}
	return false
}

func formatAgyCLIModel(base, depth string) string {
	base = strings.TrimSpace(base)
	depth = strings.ToLower(strings.TrimSpace(depth))
	if base == "" {
		return ""
	}
	if depth == "" {
		return base
	}
	return base + " (" + strings.ToUpper(depth[:1]) + depth[1:] + ")"
}

func splitAgySlugDepth(s string) (base, depth string, ok bool) {
	s = strings.TrimSpace(strings.ToLower(s))
	for _, d := range []string{"medium", "high", "low", "xhigh", "max"} {
		suf := "-" + d
		if strings.HasSuffix(s, suf) && len(s) > len(suf) {
			return strings.TrimSuffix(s, suf), d, true
		}
	}
	return "", "", false
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
	// Fail closed BEFORE exec: exact token embedded depth / slug depth must match
	// inv.Effort when both are present; static base must support explicit effort.
	// Never launch agy with a mismatched or unobserved model-depth selection.
	if err := validateAntigravityModelEffort(inv.Model, inv.Effort, selectedModel); err != nil {
		endedAt := time.Now()
		res := resultWithTiming(1, err.Error(), metadata, startedAt, endedAt)
		res.FailureClass = "model_unavailable"
		return res, err
	}
	if inv.ReadOnly {
		endedAt := time.Now()
		return resultWithTiming(1, "", metadata, startedAt, endedAt), runtimecap.RequireProviderCapability("antigravity", runtimecap.ProviderReadOnly)
	}
	if strings.TrimSpace(inv.LogPath) == "" {
		return Result{ExitCode: -1}, errors.New("antigravity log path is required")
	}
	mcpServers, err := mcpServersForInvocation(inv)
	if err != nil {
		return Result{ExitCode: -1}, fmt.Errorf("antigravity MCP configuration: %w", err)
	}
	if len(mcpServers) > 0 {
		return Result{ExitCode: -1}, fmt.Errorf("antigravity MCP configuration: %w", runtimecap.RequireProviderCapability("antigravity", runtimecap.ProviderMCPConfig))
	}
	if strings.TrimSpace(inv.OutputSchema) != "" {
		return Result{ExitCode: -1}, fmt.Errorf("antigravity output schema: %w", runtimecap.RequireProviderCapability("antigravity", runtimecap.ProviderJSONOutput))
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
	// Best-effort re-read log for denial phrases that may land only on stderr/log.
	logText := ""
	if b, rerr := os.ReadFile(inv.LogPath); rerr == nil {
		logText = string(b)
	}
	if antigravityHeadlessPermissionDenied(summary, logText) {
		msg := "antigravity headless permission denial (no useful output); require --dangerously-skip-permissions"
		if summary == "" {
			summary = msg
		}
		result := resultWithSupervision(1, summary, metadata, startedAt, endedAt, supervision, runErr, ctx)
		if runErr != nil {
			return result, runErr
		}
		return result, errors.New(msg)
	}
	result := resultWithSupervision(supervisedExitCode(supervision, runErr), summary, metadata, startedAt, endedAt, supervision, runErr, ctx)
	// Typed model_unavailable: CLI rejected --model (unsupported depth combo etc.).
	// Upstream may auto-reroute to another eligible model+depth; do not invent capacity.
	if antigravityInvalidModelSelection(summary, logText) {
		result.FailureClass = "model_unavailable"
		if runErr == nil {
			runErr = errors.New("antigravity invalid model selection (model_unavailable)")
		}
	}
	if runErr != nil {
		return result, runErr
	}
	return result, nil
}

func antigravityInvalidModelSelection(stdout, logText string) bool {
	blob := strings.ToLower(stdout + "\n" + logText)
	return strings.Contains(blob, "invalid model selection") ||
		(strings.Contains(blob, "not recognized as a known model") && strings.Contains(blob, "available models"))
}

// validateAntigravityModelEffort fail-closes before exec when the selected
// token's embedded depth disagrees with inv.Effort, or a static base model is
// asked for an unsupported explicit depth. Empty effort is allowed.
func validateAntigravityModelEffort(rawModel, effort, selected string) error {
	effort = strings.ToLower(strings.TrimSpace(effort))
	rawModel = strings.TrimSpace(rawModel)
	selected = strings.TrimSpace(selected)
	if effort == "" {
		return nil
	}
	// Exact parenthetical: embedded depth must equal effort.
	if b, d, ok := splitParentheticalModelDepth(selected); ok {
		_ = b
		if d != effort {
			return fmt.Errorf("antigravity model_unavailable: exact token %q embeds depth %q != effort %q", selected, d, effort)
		}
		return nil
	}
	if b, d, ok := splitParentheticalModelDepth(rawModel); ok {
		_ = b
		if d != effort {
			return fmt.Errorf("antigravity model_unavailable: exact token %q embeds depth %q != effort %q", rawModel, d, effort)
		}
		return nil
	}
	// Exact slug: embedded depth must equal effort.
	if _, d, ok := splitAgySlugDepth(selected); ok {
		if d != effort {
			return fmt.Errorf("antigravity model_unavailable: slug %q embeds depth %q != effort %q", selected, d, effort)
		}
		return nil
	}
	if _, d, ok := splitAgySlugDepth(rawModel); ok {
		if d != effort {
			return fmt.Errorf("antigravity model_unavailable: slug %q embeds depth %q != effort %q", rawModel, d, effort)
		}
		return nil
	}
	// Static base name: explicit effort must be curated-supported.
	base := firstNonEmpty(rawModel, selected)
	if provider, ok := models.LookupProvider("antigravity"); ok {
		if mod, ok := provider.LookupModel(base); ok && len(mod.Depths) > 0 {
			if _, ok := mod.LookupDepth(effort); !ok {
				return fmt.Errorf("antigravity model_unavailable: model %q does not support depth %q", base, effort)
			}
		}
	}
	return nil
}

func splitParentheticalModelDepth(s string) (base, depth string, ok bool) {
	s = strings.TrimSpace(s)
	i := strings.LastIndex(s, " (")
	if i <= 0 || !strings.HasSuffix(s, ")") {
		return "", "", false
	}
	raw := strings.ToLower(strings.TrimSpace(s[i+2 : len(s)-1]))
	switch raw {
	case "low", "medium", "high", "xhigh", "max":
		return strings.TrimSpace(s[:i]), raw, true
	default:
		return "", "", false
	}
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return strings.TrimSpace(a)
	}
	return strings.TrimSpace(b)
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
