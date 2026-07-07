package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/audit"
	lcdefaults "github.com/jasonhnd/loopcoder/internal/defaults"
	"github.com/jasonhnd/loopcoder/internal/relay"
	"github.com/jasonhnd/loopcoder/internal/relaygate"
	"github.com/jasonhnd/loopcoder/internal/reporter"
)

const auditCommandFailureExitCode = 3

func runAudit(args []string, stdout, stderr io.Writer, deps Deps) int {
	if deps.Audit == nil {
		deps.Audit = DefaultDeps().Audit
	}

	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	fs.SetOutput(stderr)

	repoPath := "."
	var repoAlias string
	outputFormat := "text"
	var outputFormatAlias string
	var layerValues repeatStringFlag
	var layerAlias repeatStringFlag
	var layersValue string
	var layersAlias string
	var threshold string
	var thresholdAlias string
	var severityThresholdAlias string
	var baseBranch string
	var baseBranchAlias string
	var configFromBase bool
	var configFromBaseAlias bool
	var provider string
	var providerAlias string
	var model string
	var modelAlias string
	var effort string
	var effortAlias string
	var timeout time.Duration
	var timeoutAlias time.Duration
	var strict bool
	var strictAlias bool
	var pretty bool
	var prettyAlias bool
	var noPretty bool
	var noPrettyAlias bool

	fs.StringVar(&repoPath, "repo", ".", "repository path")
	fs.StringVar(&repoAlias, "Repo", "", "repository path")
	fs.StringVar(&outputFormat, "format", "text", "output format")
	fs.StringVar(&outputFormatAlias, "Format", "", "output format")
	fs.Var(&layerValues, "layer", "audit layer")
	fs.Var(&layerAlias, "Layer", "audit layer")
	fs.StringVar(&layersValue, "layers", "", "audit layers")
	fs.StringVar(&layersAlias, "Layers", "", "audit layers")
	fs.StringVar(&threshold, "severity-threshold", "", "severity threshold")
	fs.StringVar(&severityThresholdAlias, "SeverityThreshold", "", "severity threshold")
	fs.StringVar(&thresholdAlias, "threshold", "", "severity threshold")
	fs.StringVar(&baseBranch, "base-branch", lcdefaults.BaseBranch, "base branch")
	fs.StringVar(&baseBranchAlias, "BaseBranch", "", "base branch")
	fs.BoolVar(&configFromBase, "config-from-base", false, "read .delivery.yml from base branch when absent from working tree")
	fs.BoolVar(&configFromBaseAlias, "ConfigFromBase", false, "read .delivery.yml from base branch when absent from working tree")
	fs.StringVar(&provider, "provider", "", "LLM review provider")
	fs.StringVar(&providerAlias, "Provider", "", "LLM review provider")
	fs.StringVar(&model, "model", "", "LLM review model")
	fs.StringVar(&modelAlias, "Model", "", "LLM review model")
	fs.StringVar(&effort, "effort", "", "LLM review effort")
	fs.StringVar(&effortAlias, "Effort", "", "LLM review effort")
	fs.DurationVar(&timeout, "timeout", lcdefaults.VerifierTimeout, "LLM review timeout")
	fs.DurationVar(&timeoutAlias, "Timeout", 0, "LLM review timeout")
	fs.BoolVar(&strict, "strict", false, "reject invalid model/depth selections instead of warning")
	fs.BoolVar(&strictAlias, "Strict", false, "reject invalid model/depth selections instead of warning")
	fs.BoolVar(&pretty, "pretty", false, "render human-readable LLM review report on stderr")
	fs.BoolVar(&prettyAlias, "Pretty", false, "render human-readable LLM review report on stderr")
	fs.BoolVar(&noPretty, "no-pretty", false, "suppress human-readable LLM review report on stderr")
	fs.BoolVar(&noPrettyAlias, "NoPretty", false, "suppress human-readable LLM review report on stderr")

	if err := fs.Parse(args); err != nil {
		return auditCommandFailureExitCode
	}
	if repoAlias != "" {
		repoPath = repoAlias
	}
	if outputFormatAlias != "" {
		outputFormat = outputFormatAlias
	}
	if layersAlias != "" {
		layersValue = layersAlias
	}
	if severityThresholdAlias != "" {
		threshold = severityThresholdAlias
	}
	if thresholdAlias != "" {
		threshold = thresholdAlias
	}
	if baseBranchAlias != "" {
		baseBranch = baseBranchAlias
	}
	configFromBase = configFromBase || configFromBaseAlias
	if providerAlias != "" {
		provider = providerAlias
	}
	if modelAlias != "" {
		model = modelAlias
	}
	if effortAlias != "" {
		effort = effortAlias
	}
	if timeoutAlias != 0 {
		timeout = timeoutAlias
	}
	strict = strict || strictAlias
	pretty = pretty || prettyAlias
	noPretty = noPretty || noPrettyAlias
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "audit: unexpected argument %q\n", fs.Arg(0))
		return auditCommandFailureExitCode
	}
	outputFormat = strings.ToLower(strings.TrimSpace(outputFormat))
	switch outputFormat {
	case "text", "json", "both":
	default:
		fmt.Fprintf(stderr, "audit: invalid --format %q; want text, json, or both\n", outputFormat)
		return auditCommandFailureExitCode
	}

	layers := append([]string(nil), layerValues...)
	layers = append(layers, layerAlias...)
	if strings.TrimSpace(layersValue) != "" {
		layers = append(layers, layersValue)
	}
	if timeout <= 0 {
		fmt.Fprintln(stderr, "audit: --timeout must be positive")
		return auditCommandFailureExitCode
	}

	resolvedRepo, err := resolveRepo(repoPath)
	if err != nil {
		fmt.Fprintf(stderr, "audit: %v\n", err)
		return auditCommandFailureExitCode
	}
	if exitCode, blocked := checkRelayGate(resolvedRepo, stdout, stderr); blocked {
		return exitCode
	}
	if auditSelectionIncludesLLM(layers) {
		cfg, err := loadDeliveryConfig(resolvedRepo, baseBranch, configFromBase)
		if err != nil {
			fmt.Fprintf(stderr, "audit: %v\n", err)
			return auditCommandFailureExitCode
		}
		selection, ok := resolveAndValidateRoleSelection(roleSelectionInput{
			Role:           "verifier",
			Provider:       provider,
			Model:          model,
			Effort:         effort,
			ConfigProvider: cfg.Adapters.Verifier,
			ConfigModel:    cfg.Verifier.Model,
			ConfigEffort:   cfg.Verifier.ReasoningEffort,
			Strict:         cfg.Models.Strict || strict,
			Warnings:       stderr,
		})
		if !ok {
			return auditCommandFailureExitCode
		}
		provider = selection.Provider
		model = selection.Model
		effort = selection.Effort
	}

	result, err := deps.Audit(context.Background(), audit.Options{
		RepoPath:          resolvedRepo,
		Layers:            layers,
		ThresholdOverride: threshold,
		BaseBranch:        baseBranch,
		ConfigFromBase:    configFromBase,
		Provider:          provider,
		Model:             model,
		Effort:            effort,
		Timeout:           timeout,
		Stderr:            stderr,
	})
	if err != nil {
		fmt.Fprintf(stderr, "audit: %v\n", err)
		return auditCommandFailureExitCode
	}
	if result.Report != nil {
		if deps.Now == nil {
			deps.Now = DefaultDeps().Now
		}
		mode := prettyModeForTarget(stderr, deps, pretty)
		if err := writeAuditRelayLedger(resolvedRepo, *result.Report, mode, deps.Now()); err != nil {
			result.NeedsHuman = append(result.NeedsHuman, audit.NeedsHuman{
				Layer:  audit.LayerLLM,
				Reason: "write audit review relay record: " + err.Error(),
			})
			result = audit.Finalize(result)
		} else if shouldRenderPretty(noPretty) {
			if err := renderPrettyReport(stderr, *result.Report, mode); err != nil {
				fmt.Fprintf(stderr, "audit: write pretty report: %v\n", err)
				return auditCommandFailureExitCode
			}
		}
	}
	if err := renderAudit(stdout, result, outputFormat); err != nil {
		fmt.Fprintf(stderr, "audit: write output: %v\n", err)
		return auditCommandFailureExitCode
	}
	return audit.ExitCode(result)
}

func auditSelectionIncludesLLM(layers []string) bool {
	for _, raw := range layers {
		for _, part := range strings.Split(raw, ",") {
			switch strings.ToLower(strings.TrimSpace(part)) {
			case audit.LayerLLM, "all":
				return true
			}
		}
	}
	return false
}

func writeAuditRelayLedger(repoPath string, record reporter.Report, mode reporter.PrettyMode, now time.Time) error {
	pretty := record.Pretty(reporter.PrettyOptions{Mode: mode})
	runID := "audit-llm"
	invocationID := fmt.Sprintf("audit-llm-%d", now.UTC().UnixNano())
	if _, err := relay.Write(relay.Entry{
		RepoPath:     repoPath,
		RunID:        runID,
		InvocationID: invocationID,
		Command:      "audit",
		Role:         reporter.RoleVerifier,
		CreatedAt:    now,
		Header:       record.Header(),
		Pretty:       pretty,
		Report:       &record,
	}); err != nil {
		return err
	}
	_, err := relaygate.Write(relaygate.WriteOptions{
		RepoPath: repoPath,
		RunID:    invocationID,
		Role:     string(reporter.RoleVerifier),
		PRNumber: 0,
		Block:    pretty,
		Report:   &record,
	})
	return err
}

func renderAudit(w io.Writer, result audit.Result, outputFormat string) error {
	if outputFormat == "text" || outputFormat == "both" {
		if err := audit.RenderText(w, result); err != nil {
			return err
		}
	}
	if outputFormat == "both" {
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w, "JSON RESULT"); err != nil {
			return err
		}
	}
	if outputFormat == "json" || outputFormat == "both" {
		if err := audit.RenderJSON(w, result); err != nil {
			return err
		}
	}
	return nil
}
