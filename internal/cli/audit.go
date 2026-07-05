package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/audit"
	lcdefaults "github.com/jasonhnd/loopcoder/internal/defaults"
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

	resolvedRepo, err := resolveRepo(repoPath)
	if err != nil {
		fmt.Fprintf(stderr, "audit: %v\n", err)
		return auditCommandFailureExitCode
	}
	if exitCode, blocked := checkRelayGate(resolvedRepo, stdout, stderr); blocked {
		return exitCode
	}

	result, err := deps.Audit(context.Background(), audit.Options{
		RepoPath:          resolvedRepo,
		Layers:            layers,
		ThresholdOverride: threshold,
		BaseBranch:        baseBranch,
		ConfigFromBase:    configFromBase,
	})
	if err != nil {
		fmt.Fprintf(stderr, "audit: %v\n", err)
		return auditCommandFailureExitCode
	}
	if err := renderAudit(stdout, result, outputFormat); err != nil {
		fmt.Fprintf(stderr, "audit: write output: %v\n", err)
		return auditCommandFailureExitCode
	}
	return audit.ExitCode(result)
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
