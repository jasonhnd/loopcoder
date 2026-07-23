package providerinventory

import (
	"sort"
	"strings"
)

// catalogSourcesFromAgyModels parses Antigravity `agy models` machine-readable
// output. This is intentionally separate from Grok/xAI parsers: source
// attribution, model IDs, and depth tokens must stay provider-correct.
//
// Live lines look like: gpt-oss-120b-medium, gemini-3.1-pro-low.
// CLI --model accepts display forms: "GPT-OSS 120B (Medium)".
func catalogSourcesFromAgyModels(adapter AdapterDeclaration, cliVersion, output string) ([]CatalogSourceInput, []string) {
	entries := parseAgyModelEntries(output)
	if len(entries) == 0 {
		return nil, []string{"catalog-empty-or-unrecognized"}
	}
	outEntries := make([]CatalogInputEntry, 0, len(entries))
	for _, e := range entries {
		if e.Slug == "" && e.CLIModel == "" {
			continue
		}
		constraints := []string{
			"provider=antigravity",
			"catalog_source=agy-models",
		}
		if e.Depth != "" {
			constraints = append(constraints, "supported_depth="+e.Depth)
		}
		if e.Slug != "" {
			constraints = append(constraints, "agy_slug="+e.Slug)
		}
		// Installed-agy smoke accepts both slug and display; prefer the
		// machine-readable slug as the exact invocable --model token when present.
		invokeToken := firstNonEmpty(e.Slug, e.CLIModel, e.Base)
		if invokeToken != "" {
			constraints = append(constraints, "cli_model="+invokeToken)
		}
		aliases := []string{}
		if e.CLIModel != "" && e.CLIModel != invokeToken {
			aliases = append(aliases, e.CLIModel)
		}
		if e.Base != "" && e.Base != invokeToken {
			aliases = append(aliases, e.Base)
		}
		outEntries = append(outEntries, CatalogInputEntry{
			// Canonical is the exact invocable token for routing/invocation.
			CanonicalModelID:    invokeToken,
			DisplayName:         firstNonEmpty(e.CLIModel, e.Base, e.Slug),
			Aliases:             aliases,
			LifecycleState:      LifecycleAvailable,
			AvailabilityState:   AvailabilityAvailable,
			ReadOnly:            CapabilityUnknown,
			JSONOutput:          CapabilityUnknown,
			NestedSubagents:     CapabilityFalse,
			MCPConfig:           CapabilityUnknown,
			Cancellation:        CapabilityTrue,
			TokenUsageReporting: CapabilityUnknown,
			ImageInput:          CapabilityUnknown,
			ImageOutput:         CapabilityUnknown,
			Constraints:         constraints,
		})
	}
	if len(outEntries) == 0 {
		return nil, []string{"catalog-empty-or-unrecognized"}
	}
	// Stable provider-local reference — never grok/xai attribution.
	ref := "provider-machine-readable:antigravity:agy-models"
	return []CatalogSourceInput{{
		Kind:                CatalogSourceProviderMachineReadable,
		Reference:           ref,
		SourceSchemaVersion: firstNonEmpty(adapter.CatalogProbeParser, "agy-models"),
		ProviderCLIVersion:  cliVersion,
		Precedence:          200,
		Confidence:          ConfidenceExact,
		FreshnessState:      FreshnessFresh,
		Entries:             outEntries,
	}}, nil
}

type agyModelEntry struct {
	Slug     string // gpt-oss-120b-medium
	Base     string // GPT-OSS 120B
	Depth    string // medium
	CLIModel string // GPT-OSS 120B (Medium)
}

func parseAgyModelEntries(output string) []agyModelEntry {
	seen := map[string]agyModelEntry{}
	var order []string
	for _, line := range strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(strings.TrimLeft(line, "-*• "))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "usage:") ||
			strings.HasPrefix(lower, "available") ||
			strings.Contains(lower, "not logged") ||
			strings.Contains(lower, "not authenticated") ||
			networkFailureText(lower) {
			continue
		}
		// Skip JSON noise / multi-word help.
		if strings.ContainsAny(line, "{}") {
			continue
		}
		// Prefer first field as machine id (agy models: one slug per line).
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		slug := strings.Trim(fields[0], "`'\",")
		if slug == "" || secretLike(slug) {
			continue
		}
		// Full-line parenthetical display form is also accepted.
		entry := agyModelEntry{Slug: strings.ToLower(slug)}
		if b, d, ok := splitAgyParenthetical(line); ok {
			entry.Base = b
			entry.Depth = d
			entry.CLIModel = strings.TrimSpace(line)
			entry.Slug = slugifyAgyBase(b) + "-" + d
		} else if b, d, ok := splitAgySlug(slug); ok {
			entry.Base = humanizeAgyBase(b)
			entry.Depth = d
			entry.CLIModel = entry.Base + " (" + titleAgyDepth(d) + ")"
		} else {
			// Depth-less slug (e.g. claude-sonnet-4-6) — exact id only.
			entry.Base = humanizeAgyBase(slug)
			entry.CLIModel = entry.Base
			if entry.Base == slug {
				entry.CLIModel = slug
			}
		}
		key := firstNonEmpty(entry.CLIModel, entry.Slug)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = entry
		order = append(order, key)
	}
	sort.Strings(order)
	out := make([]agyModelEntry, 0, len(order))
	for _, k := range order {
		out = append(out, seen[k])
	}
	return out
}

func splitAgyParenthetical(s string) (base, depth string, ok bool) {
	s = strings.TrimSpace(s)
	i := strings.LastIndex(s, " (")
	if i <= 0 || !strings.HasSuffix(s, ")") {
		return "", "", false
	}
	raw := strings.TrimSpace(s[i+2 : len(s)-1])
	d := normalizeAgyDepth(raw)
	if d == "" {
		return "", "", false
	}
	return strings.TrimSpace(s[:i]), d, true
}

func splitAgySlug(s string) (base, depth string, ok bool) {
	s = strings.TrimSpace(strings.ToLower(s))
	for _, d := range []string{"medium", "high", "low", "xhigh", "max"} {
		suf := "-" + d
		if strings.HasSuffix(s, suf) && len(s) > len(suf) {
			return strings.TrimSuffix(s, suf), d, true
		}
	}
	return "", "", false
}

func normalizeAgyDepth(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "low", "medium", "high", "xhigh", "max":
		return s
	default:
		return ""
	}
}

func titleAgyDepth(d string) string {
	d = normalizeAgyDepth(d)
	if d == "" {
		return ""
	}
	return strings.ToUpper(d[:1]) + d[1:]
}

func humanizeAgyBase(slugBase string) string {
	switch strings.ToLower(strings.TrimSpace(slugBase)) {
	case "gpt-oss-120b":
		return "GPT-OSS 120B"
	case "gemini-3.1-pro":
		return "Gemini 3.1 Pro"
	case "gemini-3.5-flash":
		return "Gemini 3.5 Flash"
	case "gemini-3.6-flash":
		return "Gemini 3.6 Flash"
	case "claude-sonnet-4-6":
		return "Claude Sonnet 4.6"
	case "claude-opus-4-6-thinking":
		return "Claude Opus 4.6 (Thinking)"
	case "claude-opus-4-6":
		return "Claude Opus 4.6"
	default:
		return slugBase
	}
}

func slugifyAgyBase(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	prev := false
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' {
			b.WriteRune(r)
			prev = false
			continue
		}
		if !prev {
			b.WriteByte('-')
			prev = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// isAgyCatalogAdapter reports whether catalog probe output must use the
// Antigravity-specific parser (never Grok/xAI attribution).
func isAgyCatalogAdapter(adapter AdapterDeclaration) bool {
	if strings.EqualFold(strings.TrimSpace(adapter.AdapterID), "antigravity") {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(adapter.CatalogProbeParser), "agy-models") {
		return true
	}
	return false
}
