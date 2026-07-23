package capacitysnapshot

import (
	"strings"

	"github.com/jasonhnd/loopcoder/internal/depthpolicy"
	"github.com/jasonhnd/loopcoder/internal/models"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
)

// modelDepthSpec is a catalog model with only the depths actually observed or
// curated — never an invented full low/medium/high ladder.
type modelDepthSpec struct {
	ModelID         string
	SupportedDepths []string
	DefaultDepth    string
	CLIModel        string // optional exact CLI --model token (e.g. "GPT-OSS 120B (Medium)")
}

// modelSpecFromCapability maps one inventory capability row into a ModelSpec.
// ModelID is the exact observed invocation token when available (CLI display
// form or machine slug). SupportedDepths come only from observed constraints /
// tokens — never an invented full ladder. base is retained only for static-seed
// dedup keys, not as the routed ModelID.
func modelSpecFromCapability(adapterID string, m providerinventory.ModelCapability) modelDepthSpec {
	canonical := strings.TrimSpace(m.CanonicalModelID)
	display := strings.TrimSpace(m.DisplayName)
	if canonical == "" {
		canonical = display
	}
	// AGY slug/parenthetical parsing is antigravity-only (never apply to grok).
	agyOnly := strings.EqualFold(strings.TrimSpace(adapterID), "antigravity")
	depths, base, def, cli := parseObservedModelDepths(adapterID, agyOnly, canonical, display, m.Constraints)
	// Exact invocation token: prefer cli_model constraint, then exact canonical
	// (parenthetical / slug), never drop to bare base when an exact token exists.
	exact := strings.TrimSpace(cli)
	if exact == "" {
		if agyOnly {
			if _, _, ok := splitParentheticalDepth(canonical); ok {
				exact = canonical
			} else if _, _, ok := splitSlugDepth(canonical); ok {
				exact = canonical
			} else if _, _, ok := splitParentheticalDepth(display); ok {
				exact = display
			}
		}
	}
	if exact == "" {
		exact = firstNonEmpty(canonical, base, display)
	}
	lookupBase := firstNonEmpty(base, peelBaseName(exact), exact)
	if len(depths) == 0 {
		// Curated static depths only — never invent a full ladder for live rows.
		if p, ok := models.LookupProvider(adapterID); ok {
			if mod, ok := p.LookupModel(lookupBase); ok && len(mod.Depths) > 0 {
				for _, d := range mod.Depths {
					if t := depthpolicy.NormalizeDepth(d.Token); t != "" {
						depths = append(depths, t)
					}
				}
				if def == "" {
					def = depthpolicy.NormalizeDepth(mod.DefaultDepth)
				}
			}
		}
	}
	if len(depths) == 0 {
		// Unknown live model: medium-only fail-closed default (not low+medium+high).
		depths = []string{"medium"}
		def = "medium"
	}
	if def == "" {
		def = depths[0]
	}
	return modelDepthSpec{
		ModelID: exact, SupportedDepths: uniqueDepths(depths), DefaultDepth: def, CLIModel: exact,
	}
}

func peelBaseName(s string) string {
	s = strings.TrimSpace(s)
	if b, _, ok := splitParentheticalDepth(s); ok {
		return b
	}
	if b, _, ok := splitSlugDepth(s); ok {
		return humanizeAgySlugBase("antigravity", b)
	}
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// parseObservedModelDepths extracts base model id + supported depths from live
// catalog identifiers. Prefer explicit constraints over slug/parenthetical parse.
// Slug/parenthetical AGY parsing runs only when agyOnly is true.
func parseObservedModelDepths(adapterID string, agyOnly bool, canonical, display string, constraints []string) (depths []string, base, def, cli string) {
	for _, c := range constraints {
		c = strings.TrimSpace(c)
		if strings.HasPrefix(strings.ToLower(c), "supported_depth=") {
			_, v, _ := strings.Cut(c, "=")
			if n := depthpolicy.NormalizeDepth(v); n != "" {
				depths = append(depths, n)
			}
		}
		if strings.HasPrefix(strings.ToLower(c), "cli_model=") {
			_, cli, _ = strings.Cut(c, "=")
			cli = strings.TrimSpace(cli)
		}
	}
	if agyOnly {
		// Parenthetical form used by agy error lists: "GPT-OSS 120B (Medium)"
		for _, s := range []string{display, canonical, cli} {
			if b, d, ok := splitParentheticalDepth(s); ok {
				base = b
				if len(depths) == 0 {
					depths = []string{d}
				}
				if cli == "" {
					cli = strings.TrimSpace(s)
				}
				if def == "" {
					def = d
				}
				return uniqueDepths(depths), base, def, cli
			}
		}
		// Slug form from `agy models`: gpt-oss-120b-medium — keep slug as exact cli.
		if b, d, ok := splitSlugDepth(canonical); ok {
			base = humanizeAgySlugBase(adapterID, b)
			if len(depths) == 0 {
				depths = []string{d}
			}
			if def == "" {
				def = d
			}
			// Exact observed slug is the invocation token (do not rebuild display).
			if cli == "" {
				cli = canonical
			}
			return uniqueDepths(depths), base, def, cli
		}
	}
	// No depth in id — base is canonical, depths left for static fill.
	base = canonical
	if base == "" {
		base = display
	}
	return uniqueDepths(depths), base, def, cli
}

func splitParentheticalDepth(s string) (base, depth string, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", false
	}
	// "Name (Depth)" — last parentheses group.
	i := strings.LastIndex(s, " (")
	if i <= 0 || !strings.HasSuffix(s, ")") {
		return "", "", false
	}
	depthRaw := strings.TrimSpace(s[i+2 : len(s)-1])
	d := depthpolicy.NormalizeDepth(depthRaw)
	if d == "" {
		// Thinking / special suffixes are not depth tokens.
		return "", "", false
	}
	return strings.TrimSpace(s[:i]), d, true
}

func splitSlugDepth(s string) (baseSlug, depth string, ok bool) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return "", "", false
	}
	for _, d := range []string{"medium", "high", "low", "xhigh", "max"} {
		suf := "-" + d
		if strings.HasSuffix(s, suf) && len(s) > len(suf) {
			return strings.TrimSuffix(s, suf), d, true
		}
	}
	return "", "", false
}

// humanizeAgySlugBase maps live agy slugs to the display model names the CLI
// accepts in --model "Name (Depth)" form.
func humanizeAgySlugBase(adapterID, slugBase string) string {
	slugBase = strings.TrimSpace(strings.ToLower(slugBase))
	switch slugBase {
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
	case "claude-opus-4-6-thinking", "claude-opus-4-6":
		return "Claude Opus 4.6"
	}
	// Fall back to static registry match by normalized tokens.
	if p, ok := models.LookupProvider(adapterID); ok {
		for _, m := range p.Models {
			if slugifyModelName(m.Name) == slugBase {
				return m.Name
			}
		}
	}
	return slugBase
}

func slugifyModelName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	prevDash := false
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' {
			b.WriteRune(r)
			prevDash = false
			continue
		}
		if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// formatAgyCLIModel builds the CLI-accepted --model token (Title-case depth).
func formatAgyCLIModel(base, depth string) string {
	base = strings.TrimSpace(base)
	depth = depthpolicy.NormalizeDepth(depth)
	if base == "" {
		return ""
	}
	if depth == "" {
		return base
	}
	return base + " (" + titleDepth(depth) + ")"
}

func titleDepth(depth string) string {
	depth = depthpolicy.NormalizeDepth(depth)
	if depth == "" {
		return ""
	}
	return strings.ToUpper(depth[:1]) + depth[1:]
}

func uniqueDepths(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range in {
		n := depthpolicy.NormalizeDepth(d)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// mergeModelSpec unions SupportedDepths for the same ModelID (live multi-depth
// catalogs emit one capability row per depth slug).
func mergeModelSpec(dst *ModelSpec, src modelDepthSpec) {
	if dst == nil {
		return
	}
	if dst.ModelID == "" {
		dst.ModelID = src.ModelID
	}
	dst.Present = true
	set := map[string]bool{}
	for _, d := range dst.SupportedDepths {
		set[d] = true
	}
	for _, d := range src.SupportedDepths {
		if !set[d] {
			dst.SupportedDepths = append(dst.SupportedDepths, d)
			set[d] = true
		}
	}
	if dst.DefaultDepth == "" {
		dst.DefaultDepth = src.DefaultDepth
	}
	// Prefer medium as default when present.
	for _, d := range dst.SupportedDepths {
		if d == "medium" {
			dst.DefaultDepth = "medium"
			break
		}
	}
}
