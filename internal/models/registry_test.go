package models_test

import (
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/models"
)

func TestDefaultRegistryStaticRows(t *testing.T) {
	got := models.DefaultRegistry()
	// Structural expectations after CRO-005 reconciliation (medium defaults;
	// expanded codex/claude catalogs; grok remains dynamic-only).
	if len(got.Providers) != 4 {
		t.Fatalf("providers=%d", len(got.Providers))
	}
	codex, ok := models.LookupProvider("codex")
	if !ok || codex.DefaultModel != "gpt-5.5" || codex.DefaultDepth != "medium" {
		t.Fatalf("codex=%#v", codex)
	}
	if len(codex.Models) < 2 {
		t.Fatalf("codex models=%d want expanded catalog", len(codex.Models))
	}
	claude, ok := models.LookupProvider("claude")
	if !ok || claude.DefaultModel != "claude-sonnet-4-5" || claude.DefaultDepth != "medium" {
		t.Fatalf("claude=%#v", claude)
	}
	agy, ok := models.LookupProvider("antigravity")
	if !ok || agy.DefaultDepth != "medium" {
		t.Fatalf("antigravity=%#v", agy)
	}
	grok, ok := models.LookupProvider("grok")
	if !ok || grok.DefaultModel != "" || len(grok.Models) != 0 {
		t.Fatalf("grok must stay dynamic-only: %#v", grok)
	}
}

func TestGrokStaticRegistryRequiresDynamicInventory(t *testing.T) {
	provider, ok := models.LookupProvider("grok")
	if !ok {
		t.Fatal("LookupProvider grok returned false")
	}
	if provider.DefaultModel != "" || len(provider.Models) != 0 {
		t.Fatalf("grok provider = %#v, want provider default with no static model catalog", provider)
	}
	absent := models.ValidateSelection(models.Selection{
		Role:     "worker",
		Provider: "grok",
	}, models.ValidationOptions{Strict: true})
	if len(absent.Diagnostics) != 0 || absent.Selection.Model != "" {
		t.Fatalf("absent Grok selection = %#v diagnostics=%#v, want provider default pass-through", absent.Selection, absent.Diagnostics)
	}
	configured := models.ValidateSelection(models.Selection{
		Role:     "worker",
		Provider: "grok",
		Model:    "grok-custom",
	}, models.ValidationOptions{Strict: true})
	requireDiagnostic(t, configured, models.SeverityReject, models.ReasonUnknownModel, []string{
		`provider "grok"`,
		`model "grok-custom"`,
		"not listed",
	})
}

func TestDefaultRegistryInvariants(t *testing.T) {
	if violations := models.DefaultRegistry().InvariantViolations(); len(violations) > 0 {
		t.Fatalf("registry invariants failed: %v", violations)
	}
}

func TestLookupHelpers(t *testing.T) {
	if got, want := models.ProviderNames(), []string{"codex", "claude", "antigravity", "grok"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ProviderNames = %#v, want %#v", got, want)
	}

	provider, ok := models.LookupProvider("antigravity")
	if !ok {
		t.Fatal("LookupProvider antigravity returned false")
	}
	if provider.CLI != "agy" || provider.DefaultModel != "Gemini 3.1 Pro" || provider.DefaultDepth != "medium" {
		t.Fatalf("antigravity provider = %#v", provider)
	}
	if got, want := provider.ModelNames(), []string{"Gemini 3.1 Pro", "Opus 4.6", "GPT-OSS 120B"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ModelNames = %#v, want %#v", got, want)
	}

	model, ok := provider.LookupModel("Gemini 3.1 Pro")
	if !ok {
		t.Fatal("LookupModel Gemini 3.1 Pro returned false")
	}
	if got, want := model.DepthTokens(), []string{"low", "medium", "high"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("DepthTokens = %#v, want %#v", got, want)
	}
	if depth, ok := model.LookupDepth("medium"); !ok || depth.Label != "medium" {
		t.Fatalf("LookupDepth High = %#v/%t", depth, ok)
	}
	gpt, ok := provider.LookupModel("GPT-OSS 120B")
	if !ok {
		t.Fatal("LookupModel GPT-OSS 120B")
	}
	if got, want := gpt.DepthTokens(), []string{"medium"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("GPT-OSS DepthTokens = %#v, want medium only (no invented low/high)", got)
	}
	if _, ok := gpt.LookupDepth("low"); ok {
		t.Fatal("GPT-OSS must not advertise unsupported low depth")
	}
	if _, ok := models.LookupProvider("agy"); ok {
		t.Fatal("LookupProvider agy returned true, want provider key antigravity only")
	}
}

func TestDefaultRegistryReturnsCopies(t *testing.T) {
	registry := models.DefaultRegistry()
	registry.Providers[0].Name = "changed"
	registry.Providers[0].Models[0].Name = "changed"
	registry.Providers[0].Models[0].Depths[0].Token = "changed"
	registry.Providers[1].Models[0].Aliases[0] = "changed"
	if got := models.DefaultRegistry().Providers[0]; got.Name != "codex" || got.Models[0].Name != "gpt-5.5" || got.Models[0].Depths[0].Token != "low" {
		t.Fatalf("DefaultRegistry leaked mutation: %#v", got)
	}
	if got := models.DefaultRegistry().Providers[1].Models[0].Aliases[0]; got != "opus" {
		t.Fatalf("DefaultRegistry leaked alias mutation: %q", got)
	}

	provider, ok := models.LookupProvider("codex")
	if !ok {
		t.Fatal("LookupProvider codex returned false")
	}
	provider.Models[0].Depths[0].Token = "changed"
	next, _ := models.LookupProvider("codex")
	if got := next.Models[0].Depths[0].Token; got != "low" {
		t.Fatalf("LookupProvider leaked mutation: %q", got)
	}
}

func TestValidateSelectionAppliesDefaults(t *testing.T) {
	result := models.ValidateSelection(models.Selection{
		Role:     "worker",
		Provider: "codex",
	}, models.ValidationOptions{})

	if len(result.Diagnostics) != 0 {
		t.Fatalf("Diagnostics = %#v, want none", result.Diagnostics)
	}
	if want := (models.Selection{Role: "worker", Provider: "codex", Model: "gpt-5.5", Depth: "medium"}); result.Selection != want {
		t.Fatalf("Selection = %#v, want %#v", result.Selection, want)
	}
}

func TestValidateSelectionUsesModelDefaultDepth(t *testing.T) {
	result := models.ValidateSelection(models.Selection{
		Role:     "worker",
		Provider: "antigravity",
		Model:    "Opus 4.6",
	}, models.ValidationOptions{})

	if len(result.Diagnostics) != 0 {
		t.Fatalf("Diagnostics = %#v, want none", result.Diagnostics)
	}
	if result.Selection.Depth != "high" {
		t.Fatalf("Depth = %q, want high", result.Selection.Depth)
	}
}

func TestValidateSelectionWarnsByDefaultAndPreservesInvalidValues(t *testing.T) {
	result := models.ValidateSelection(models.Selection{
		Role:     "worker",
		Provider: "codex",
		Model:    "gpt-5.5",
		Depth:    "deeper",
	}, models.ValidationOptions{})

	if got, want := result.Selection.Depth, "deeper"; got != want {
		t.Fatalf("Depth = %q, want preserved %q", got, want)
	}
	requireDiagnostic(t, result, models.SeverityWarn, models.ReasonInvalidDepth, []string{
		"worker",
		`provider "codex"`,
		`model "gpt-5.5"`,
		`depth "deeper"`,
		"valid depths: low, medium, high, xhigh",
	})
}

func TestValidateSelectionRejectsInStrictMode(t *testing.T) {
	result := models.ValidateSelection(models.Selection{
		Role:     "verifier",
		Provider: "claude",
		Model:    "claude-opus-4-8[1m]",
		Depth:    "xhigh",
	}, models.ValidationOptions{Strict: true})

	requireDiagnostic(t, result, models.SeverityReject, models.ReasonInvalidDepth, []string{
		"verifier",
		`provider "claude"`,
		`model "claude-opus-4-8[1m]"`,
		`depth "xhigh"`,
		"valid depths: low, medium, high, max",
	})
}

func TestValidateSelectionUnknownProviderDoesNotApplyDefaults(t *testing.T) {
	result := models.ValidateSelection(models.Selection{
		Role:     "worker",
		Provider: "nope",
	}, models.ValidationOptions{})

	if result.Selection.Model != "" || result.Selection.Depth != "" {
		t.Fatalf("Selection = %#v, want no registry defaults for unknown provider", result.Selection)
	}
	requireDiagnostic(t, result, models.SeverityWarn, models.ReasonUnknownProvider, []string{
		"worker",
		`provider "nope"`,
		`model ""`,
		`depth ""`,
		"not in the model registry",
	})
}

func TestValidateSelectionUnknownModelPreservesModelAndDepth(t *testing.T) {
	result := models.ValidateSelection(models.Selection{
		Role:     "worker",
		Provider: "codex",
		Model:    "custom-model",
		Depth:    "custom-depth",
	}, models.ValidationOptions{})

	if want := (models.Selection{Role: "worker", Provider: "codex", Model: "custom-model", Depth: "custom-depth"}); result.Selection != want {
		t.Fatalf("Selection = %#v, want %#v", result.Selection, want)
	}
	requireDiagnostic(t, result, models.SeverityWarn, models.ReasonUnknownModel, []string{
		`provider "codex"`,
		`model "custom-model"`,
		`depth "custom-depth"`,
		"not listed",
	})
}

func TestValidateSelectionSupportsEmptyDepthLists(t *testing.T) {
	registry := models.Registry{
		Providers: []models.Provider{
			{
				Name:         "custom",
				DisplayName:  "Custom",
				Vendor:       "Custom",
				CLI:          "custom",
				DefaultModel: "depthless",
				Models: []models.Model{
					{Name: "depthless"},
				},
			},
		},
	}
	if violations := registry.InvariantViolations(); len(violations) > 0 {
		t.Fatalf("empty depth registry should be valid: %v", violations)
	}

	absent := registry.ValidateSelection(models.Selection{
		Role:     "worker",
		Provider: "custom",
	}, models.ValidationOptions{Strict: true})
	if len(absent.Diagnostics) != 0 {
		t.Fatalf("Diagnostics = %#v, want none for absent depth on depthless model", absent.Diagnostics)
	}
	if absent.Selection.Depth != "" {
		t.Fatalf("Depth = %q, want empty", absent.Selection.Depth)
	}

	configured := registry.ValidateSelection(models.Selection{
		Role:     "worker",
		Provider: "custom",
		Model:    "depthless",
		Depth:    "pass-through",
	}, models.ValidationOptions{})
	requireDiagnostic(t, configured, models.SeverityWarn, models.ReasonUnsupportedDepth, []string{
		`provider "custom"`,
		`model "depthless"`,
		`depth "pass-through"`,
		"no curated valid depths",
	})
}

func TestModelsPackageImportsNoInternalPackages(t *testing.T) {
	cmd := exec.Command("go", "list", "-f", "{{join .Imports \"\\n\"}}", ".")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list imports failed: %v\n%s", err, output)
	}
	for _, imported := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if strings.HasPrefix(imported, "github.com/jasonhnd/loopcoder/internal/") {
			t.Fatalf("internal/models imports non-leaf package %q", imported)
		}
	}
}

func requireDiagnostic(t *testing.T, result models.ValidationResult, severity models.DiagnosticSeverity, reason models.DiagnosticReason, contains []string) {
	t.Helper()
	if len(result.Diagnostics) != 1 {
		t.Fatalf("Diagnostics = %#v, want one", result.Diagnostics)
	}
	diagnostic := result.Diagnostics[0]
	if diagnostic.Severity != severity || diagnostic.Reason != reason {
		t.Fatalf("Diagnostic = %#v, want severity=%s reason=%s", diagnostic, severity, reason)
	}
	for _, want := range contains {
		if !strings.Contains(diagnostic.Message, want) {
			t.Fatalf("Message = %q, want substring %q", diagnostic.Message, want)
		}
	}
}
