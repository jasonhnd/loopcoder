package providerinventory

import "testing"

func TestParseAgyModelEntries_GPTOSSMediumOnly(t *testing.T) {
	out := "gpt-oss-120b-medium\ngemini-3.1-pro-low\ngemini-3.1-pro-high\n"
	entries := parseAgyModelEntries(out)
	var gpt []agyModelEntry
	for _, e := range entries {
		if e.Base == "GPT-OSS 120B" || e.Slug == "gpt-oss-120b-medium" {
			gpt = append(gpt, e)
		}
	}
	if len(gpt) != 1 {
		t.Fatalf("want 1 gpt-oss entry, got %+v", gpt)
	}
	if gpt[0].Depth != "medium" {
		t.Fatalf("depth=%q", gpt[0].Depth)
	}
	if gpt[0].CLIModel != "GPT-OSS 120B (Medium)" {
		t.Fatalf("cli=%q", gpt[0].CLIModel)
	}
}

func TestCatalogSourcesFromAgyModels_NoGrokAttribution(t *testing.T) {
	adapter := AdapterDeclaration{AdapterID: "antigravity", CatalogProbeParser: "agy-models"}
	sources, gaps := catalogSourcesFromAgyModels(adapter, "1.1.5", "gpt-oss-120b-medium\n")
	if len(gaps) != 0 || len(sources) != 1 {
		t.Fatalf("sources=%d gaps=%v", len(sources), gaps)
	}
	if sources[0].Reference != "provider-machine-readable:antigravity:agy-models" {
		t.Fatalf("ref=%q (must not be grok/xai)", sources[0].Reference)
	}
	if sources[0].Confidence != ConfidenceExact || sources[0].FreshnessState != FreshnessFresh {
		t.Fatalf("conf/fresh %+v", sources[0])
	}
	e := sources[0].Entries[0]
	// Installed-agy smoke accepts both slug and display; machine-readable slug is
	// the preferred exact invocable --model token.
	if e.CanonicalModelID != "gpt-oss-120b-medium" {
		t.Fatalf("canonical invocable token=%q want gpt-oss-120b-medium", e.CanonicalModelID)
	}
	foundDepth, foundCLI := false, false
	for _, c := range e.Constraints {
		if c == "supported_depth=medium" {
			foundDepth = true
		}
		if c == "cli_model=gpt-oss-120b-medium" {
			foundCLI = true
		}
		if c == "provider=xai" || c == "provider=grok" {
			t.Fatalf("leaked grok constraint %q", c)
		}
	}
	if !foundDepth || !foundCLI {
		t.Fatalf("constraints %+v", e.Constraints)
	}
}

func TestIsAgyCatalogAdapter(t *testing.T) {
	if !isAgyCatalogAdapter(AdapterDeclaration{AdapterID: "antigravity"}) {
		t.Fatal("antigravity")
	}
	if !isAgyCatalogAdapter(AdapterDeclaration{CatalogProbeParser: "agy-models"}) {
		t.Fatal("parser")
	}
	if isAgyCatalogAdapter(AdapterDeclaration{AdapterID: "grok", CatalogProbeParser: "grok-models"}) {
		t.Fatal("grok must not use agy path")
	}
}
