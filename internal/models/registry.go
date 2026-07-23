// Package models owns loopcoder's static provider model registry.
//
// The package is intentionally leaf-only. It must remain free of imports from
// orchestration, config, agent, CLI, and provider-runner packages.
package models

import (
	"fmt"
	"sort"
	"strings"
)

type Registry struct {
	Providers []Provider
}

type Provider struct {
	Name         string
	DisplayName  string
	Vendor       string
	CLI          string
	DefaultModel string
	DefaultDepth string
	Models       []Model
}

type Model struct {
	Name         string
	Depths       []Depth
	DefaultDepth string
}

type Depth struct {
	Token string
	Label string
}

type Selection struct {
	Role     string
	Provider string
	Model    string
	Depth    string
}

type ValidationOptions struct {
	Strict bool
}

type ValidationResult struct {
	Selection   Selection
	Diagnostics []Diagnostic
}

type Diagnostic struct {
	Severity DiagnosticSeverity
	Reason   DiagnosticReason
	Role     string
	Provider string
	Model    string
	Depth    string
	Message  string
}

type DiagnosticSeverity string

const (
	SeverityWarn   DiagnosticSeverity = "warn"
	SeverityReject DiagnosticSeverity = "reject"
)

type DiagnosticReason string

const (
	ReasonUnknownProvider  DiagnosticReason = "unknown-provider"
	ReasonUnknownModel     DiagnosticReason = "unknown-model"
	ReasonInvalidDepth     DiagnosticReason = "invalid-depth"
	ReasonUnsupportedDepth DiagnosticReason = "unsupported-depth"
)

var staticRegistry = Registry{
	Providers: []Provider{
		{
			Name:         "codex",
			DisplayName:  "Codex",
			Vendor:       "OpenAI Codex",
			CLI:          "codex",
			DefaultModel: "gpt-5.5",
			// DefaultDepth is the catalog default for unspecified ordinary work;
			// depthpolicy selects lower/higher from task difficulty (CRO-005).
			DefaultDepth: "medium",
			Models: []Model{
				{
					// Prefer gpt-5.5: ChatGPT-account Codex rejects gpt-5.3-codex.
					// capacitysnapshot filters gpt-5.3-codex from unattended inventory.
					Name:         "gpt-5.5",
					DefaultDepth: "medium",
					Depths: []Depth{
						{Token: "low", Label: "low"},
						{Token: "medium", Label: "medium"},
						{Token: "high", Label: "high"},
						{Token: "xhigh", Label: "xhigh"},
					},
				},
				{
					Name:         "gpt-5.3-codex",
					DefaultDepth: "medium",
					Depths: []Depth{
						{Token: "low", Label: "low"},
						{Token: "medium", Label: "medium"},
						{Token: "high", Label: "high"},
						{Token: "xhigh", Label: "xhigh"},
					},
				},
			},
		},
		{
			Name:         "claude",
			DisplayName:  "Claude",
			Vendor:       "Anthropic",
			CLI:          "claude",
			DefaultModel: "claude-sonnet-4-5",
			DefaultDepth: "medium",
			Models: []Model{
				{
					Name:         "claude-opus-4-8[1m]",
					DefaultDepth: "high",
					Depths: []Depth{
						{Token: "low", Label: "low"},
						{Token: "medium", Label: "medium"},
						{Token: "high", Label: "high"},
						{Token: "max", Label: "max"},
					},
				},
				{
					Name:         "claude-sonnet-4-5",
					DefaultDepth: "medium",
					Depths: []Depth{
						{Token: "low", Label: "low"},
						{Token: "medium", Label: "medium"},
						{Token: "high", Label: "high"},
					},
				},
				{
					Name:         "claude-haiku-4-5",
					DefaultDepth: "low",
					Depths: []Depth{
						{Token: "low", Label: "low"},
						{Token: "medium", Label: "medium"},
					},
				},
			},
		},
		{
			Name:         "antigravity",
			DisplayName:  "Antigravity",
			Vendor:       "Google Antigravity",
			CLI:          "agy",
			DefaultModel: "Gemini 3.1 Pro",
			DefaultDepth: "medium",
			Models: []Model{
				{
					Name:         "Gemini 3.1 Pro",
					DefaultDepth: "medium",
					Depths: []Depth{
						{Token: "low", Label: "low"},
						{Token: "medium", Label: "medium"},
						{Token: "high", Label: "high"},
					},
				},
				{
					Name:         "Opus 4.6",
					DefaultDepth: "high",
					Depths: []Depth{
						{Token: "medium", Label: "medium"},
						{Token: "high", Label: "high"},
						{Token: "xhigh", Label: "xhigh"},
					},
				},
				{
					// Live `agy models` currently exposes only GPT-OSS 120B (Medium).
					// Do not invent low/high support in the static catalog — routing
					// must fail closed or pick another model for those depths.
					Name:         "GPT-OSS 120B",
					DefaultDepth: "medium",
					Depths: []Depth{
						{Token: "medium", Label: "medium"},
					},
				},
			},
		},
		{
			Name:        "grok",
			DisplayName: "Grok Build",
			Vendor:      "xAI",
			CLI:         "grok",
			// Grok requires dynamic inventory for authoritative model/depth lists.
			// Static catalog remains empty by design (account-observed only).
		},
	},
}

func DefaultRegistry() Registry {
	return cloneRegistry(staticRegistry)
}

func ProviderNames() []string {
	return DefaultRegistry().ProviderNames()
}

func LookupProvider(name string) (Provider, bool) {
	return DefaultRegistry().LookupProvider(name)
}

func (r Registry) ProviderNames() []string {
	names := make([]string, 0, len(r.Providers))
	for _, provider := range r.Providers {
		names = append(names, provider.Name)
	}
	return names
}

func (r Registry) LookupProvider(name string) (Provider, bool) {
	for _, provider := range r.Providers {
		if provider.Name == name {
			return cloneProvider(provider), true
		}
	}
	return Provider{}, false
}

func (p Provider) LookupModel(name string) (Model, bool) {
	for _, model := range p.Models {
		if model.Name == name {
			return cloneModel(model), true
		}
	}
	return Model{}, false
}

func (p Provider) ModelNames() []string {
	names := make([]string, 0, len(p.Models))
	for _, model := range p.Models {
		names = append(names, model.Name)
	}
	return names
}

func (m Model) LookupDepth(token string) (Depth, bool) {
	for _, depth := range m.Depths {
		if depth.Token == token {
			return depth, true
		}
	}
	return Depth{}, false
}

func (m Model) DepthTokens() []string {
	tokens := make([]string, 0, len(m.Depths))
	for _, depth := range m.Depths {
		tokens = append(tokens, depth.Token)
	}
	return tokens
}

func ValidateSelection(selection Selection, opts ValidationOptions) ValidationResult {
	return DefaultRegistry().ValidateSelection(selection, opts)
}

func (r Registry) ValidateSelection(selection Selection, opts ValidationOptions) ValidationResult {
	result := ValidationResult{Selection: selection}
	severity := SeverityWarn
	if opts.Strict {
		severity = SeverityReject
	}

	provider, ok := r.LookupProvider(selection.Provider)
	if !ok {
		result.Diagnostics = append(result.Diagnostics, diagnostic(severity, ReasonUnknownProvider, result.Selection, "provider is not in the model registry"))
		return result
	}

	if result.Selection.Model == "" {
		result.Selection.Model = provider.DefaultModel
	}
	if result.Selection.Model == "" {
		return result
	}
	model, ok := provider.LookupModel(result.Selection.Model)
	if !ok {
		result.Diagnostics = append(result.Diagnostics, diagnostic(severity, ReasonUnknownModel, result.Selection, fmt.Sprintf("model is not listed for provider %q", provider.Name)))
		return result
	}

	if result.Selection.Depth == "" {
		result.Selection.Depth = model.DefaultDepth
		return result
	}
	if len(model.Depths) == 0 {
		result.Diagnostics = append(result.Diagnostics, diagnostic(severity, ReasonUnsupportedDepth, result.Selection, fmt.Sprintf("model %q has no curated valid depths", model.Name)))
		return result
	}
	if _, ok := model.LookupDepth(result.Selection.Depth); !ok {
		result.Diagnostics = append(result.Diagnostics, diagnostic(severity, ReasonInvalidDepth, result.Selection, fmt.Sprintf("valid depths: %s", strings.Join(model.DepthTokens(), ", "))))
	}
	return result
}

func (r Registry) InvariantViolations() []string {
	var violations []string
	seenProviders := map[string]bool{}
	for _, provider := range r.Providers {
		if provider.Name == "" {
			violations = append(violations, "provider name is empty")
		}
		if seenProviders[provider.Name] {
			violations = append(violations, fmt.Sprintf("provider %q is duplicated", provider.Name))
		}
		seenProviders[provider.Name] = true

		seenModels := map[string]bool{}
		defaultModel, defaultModelOK := provider.LookupModel(provider.DefaultModel)
		if provider.DefaultModel != "" && !defaultModelOK {
			violations = append(violations, fmt.Sprintf("provider %q default model %q is not listed", provider.Name, provider.DefaultModel))
		}
		for _, model := range provider.Models {
			if model.Name == "" {
				violations = append(violations, fmt.Sprintf("provider %q has an empty model name", provider.Name))
			}
			if seenModels[model.Name] {
				violations = append(violations, fmt.Sprintf("provider %q model %q is duplicated", provider.Name, model.Name))
			}
			seenModels[model.Name] = true

			seenDepths := map[string]bool{}
			defaultDepthOK := false
			for _, depth := range model.Depths {
				if depth.Token == "" {
					violations = append(violations, fmt.Sprintf("provider %q model %q has an empty depth token", provider.Name, model.Name))
				}
				if seenDepths[depth.Token] {
					violations = append(violations, fmt.Sprintf("provider %q model %q depth %q is duplicated", provider.Name, model.Name, depth.Token))
				}
				seenDepths[depth.Token] = true
				if depth.Token == model.DefaultDepth {
					defaultDepthOK = true
				}
			}
			if len(model.Depths) == 0 && model.DefaultDepth != "" {
				violations = append(violations, fmt.Sprintf("provider %q model %q has empty depths but default depth %q", provider.Name, model.Name, model.DefaultDepth))
			}
			if len(model.Depths) > 0 && !defaultDepthOK {
				violations = append(violations, fmt.Sprintf("provider %q model %q default depth %q is not listed", provider.Name, model.Name, model.DefaultDepth))
			}
		}
		if defaultModelOK && provider.DefaultDepth != defaultModel.DefaultDepth {
			violations = append(violations, fmt.Sprintf("provider %q default depth %q does not match default model depth %q", provider.Name, provider.DefaultDepth, defaultModel.DefaultDepth))
		}
		if provider.DefaultModel == "" && provider.DefaultDepth != "" {
			violations = append(violations, fmt.Sprintf("provider %q default depth %q is set without a default model", provider.Name, provider.DefaultDepth))
		}
	}
	sort.Strings(violations)
	return violations
}

func (r Registry) MustBeValid() {
	if violations := r.InvariantViolations(); len(violations) > 0 {
		panic("invalid model registry: " + strings.Join(violations, "; "))
	}
}

func diagnostic(severity DiagnosticSeverity, reason DiagnosticReason, selection Selection, detail string) Diagnostic {
	messageSeverity := "warning"
	if severity == SeverityReject {
		messageSeverity = "reject"
	}
	return Diagnostic{
		Severity: severity,
		Reason:   reason,
		Role:     selection.Role,
		Provider: selection.Provider,
		Model:    selection.Model,
		Depth:    selection.Depth,
		Message: fmt.Sprintf(
			"[loopcoder] %s: %s model selection: provider %q model %q depth %q: %s",
			messageSeverity,
			selection.Role,
			selection.Provider,
			selection.Model,
			selection.Depth,
			detail,
		),
	}
}

func cloneRegistry(registry Registry) Registry {
	providers := make([]Provider, 0, len(registry.Providers))
	for _, provider := range registry.Providers {
		providers = append(providers, cloneProvider(provider))
	}
	return Registry{Providers: providers}
}

func cloneProvider(provider Provider) Provider {
	provider.Models = append([]Model(nil), provider.Models...)
	for index := range provider.Models {
		provider.Models[index] = cloneModel(provider.Models[index])
	}
	return provider
}

func cloneModel(model Model) Model {
	model.Depths = append([]Depth(nil), model.Depths...)
	return model
}

func init() {
	staticRegistry.MustBeValid()
}
