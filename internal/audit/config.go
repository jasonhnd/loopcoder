package audit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/config"
	lcdefaults "github.com/jasonhnd/loopcoder/internal/defaults"
)

var defaultNativeExclude = []string{
	".git/**",
	"vendor/**",
	"dist/**",
	"node_modules/**",
}

func LoadPlan(ctx context.Context, repoPath string, opts Options) (Plan, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg, err := config.LoadForRepo(ctx, repoPath, config.LoadOptions{
		BaseBranch:     opts.BaseBranch,
		ConfigFromBase: opts.ConfigFromBase,
	})
	if err != nil {
		return Plan{}, err
	}
	return BuildPlan(repoPath, cfg.Audit, opts)
}

func BuildPlan(repoPath string, cfg config.Audit, opts Options) (Plan, error) {
	threshold := strings.TrimSpace(opts.ThresholdOverride)
	if threshold == "" {
		threshold = strings.TrimSpace(cfg.SeverityThreshold)
	}
	if threshold == "" {
		threshold = SeverityMedium
	}
	threshold = NormalizeSeverity(threshold)
	if threshold == "" {
		return Plan{}, fmt.Errorf("invalid severity threshold %q", firstNonEmpty(opts.ThresholdOverride, cfg.SeverityThreshold))
	}

	layers, err := NormalizeLayers(opts.Layers)
	if err != nil {
		return Plan{}, err
	}

	commands, err := effectiveCommands(repoPath, cfg.SAST.Commands)
	if err != nil {
		return Plan{}, err
	}
	native := effectiveNative(cfg.SAST.Native)

	return Plan{
		Threshold: threshold,
		Layers:    layers,
		Commands:  commands,
		Native:    native,
	}, nil
}

func NormalizeLayers(values []string) ([]string, error) {
	if len(values) == 0 {
		return []string{LayerSAST}, nil
	}
	seen := map[string]bool{}
	out := []string{}
	for _, raw := range values {
		for _, part := range strings.Split(raw, ",") {
			layer := strings.ToLower(strings.TrimSpace(part))
			if layer == "" {
				continue
			}
			if layer == "all" {
				layer = LayerSAST
			}
			switch layer {
			case LayerSAST:
				if !seen[layer] {
					seen[layer] = true
					out = append(out, layer)
				}
			case LayerLLM:
				return nil, errors.New("llm audit layer is not implemented in this slice")
			default:
				return nil, fmt.Errorf("unknown audit layer %q", part)
			}
		}
	}
	if len(out) == 0 {
		return nil, errors.New("at least one audit layer is required")
	}
	return out, nil
}

func effectiveCommands(repoPath string, configured []config.AuditSASTCommand) ([]SASTCommand, error) {
	if configured != nil {
		commands := make([]SASTCommand, 0, len(configured))
		for index, command := range configured {
			resolved, err := configuredCommand(index, command)
			if err != nil {
				return nil, err
			}
			commands = append(commands, resolved)
		}
		return commands, nil
	}
	if hasGoMod(repoPath) {
		return DefaultGoCommands(), nil
	}
	return nil, nil
}

func configuredCommand(index int, command config.AuditSASTCommand) (SASTCommand, error) {
	id := strings.TrimSpace(command.ID)
	if id == "" {
		return SASTCommand{}, fmt.Errorf("audit.sast.commands[%d].id is required", index)
	}
	if len(command.Argv) == 0 {
		return SASTCommand{}, fmt.Errorf("audit.sast.commands[%d].argv must be a non-empty array", index)
	}
	argv := make([]string, 0, len(command.Argv))
	for argIndex, arg := range command.Argv {
		arg = strings.TrimSpace(arg)
		if arg == "" {
			return SASTCommand{}, fmt.Errorf("audit.sast.commands[%d].argv[%d] must not be empty", index, argIndex)
		}
		argv = append(argv, arg)
	}
	parser := strings.TrimSpace(command.Parser)
	if parser == "" {
		return SASTCommand{}, fmt.Errorf("audit.sast.commands[%d].parser is required", index)
	}
	if !KnownParser(parser) {
		return SASTCommand{}, fmt.Errorf("audit.sast.commands[%d].parser %q is not supported", index, parser)
	}
	if command.TimeoutSeconds < 0 {
		return SASTCommand{}, fmt.Errorf("audit.sast.commands[%d].timeout_seconds must not be negative", index)
	}
	return SASTCommand{
		ID:      id,
		Argv:    argv,
		Parser:  parser,
		Timeout: config.DurationSeconds(command.TimeoutSeconds, lcdefaults.SupervisedExecHardCap),
	}, nil
}

func DefaultGoCommands() []SASTCommand {
	timeout := lcdefaults.SupervisedExecHardCap
	return []SASTCommand{
		{
			ID:      "govulncheck",
			Argv:    []string{"govulncheck", "-json", "./..."},
			Parser:  ParserGovulncheckJSON,
			Timeout: timeout,
		},
		{
			ID:      "staticcheck",
			Argv:    []string{"staticcheck", "-f", "json", "./..."},
			Parser:  ParserStaticcheckJSON,
			Timeout: timeout,
		},
		{
			ID:      "gosec",
			Argv:    []string{"gosec", "-fmt", "json", "-quiet", "./..."},
			Parser:  ParserGosecJSON,
			Timeout: timeout,
		},
	}
}

func effectiveNative(native config.AuditSASTNative) NativeConfig {
	return NativeConfig{
		Secrets:         boolDefault(native.Secrets, true),
		FilePermissions: boolDefault(native.FilePermissions, true),
		Include:         stringListDefault(native.Include, []string{"**/*"}),
		Exclude:         stringListDefault(native.Exclude, defaultNativeExclude),
	}
}

func boolDefault(value *bool, def bool) bool {
	if value == nil {
		return def
	}
	return *value
}

func stringListDefault(values []string, def []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	if len(out) == 0 {
		return append([]string(nil), def...)
	}
	return out
}

func hasGoMod(repoPath string) bool {
	info, err := os.Stat(filepath.Join(repoPath, "go.mod"))
	return err == nil && !info.IsDir()
}
