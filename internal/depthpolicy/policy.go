package depthpolicy

import (
	"fmt"
	"strings"
)

// Difficulty is a coarse task difficulty band used for automatic depth selection.
type Difficulty string

const (
	DifficultyTiny     Difficulty = "tiny"     // docs, typos, one-line fixes
	DifficultyStandard Difficulty = "standard" // ordinary implementation
	DifficultyHard     Difficulty = "hard"     // architecture, security, migration, ambiguous
	DifficultyHuman    Difficulty = "human"    // needs-human; no auto high burn
)

// Preference is the preferred depth token for a difficulty band.
func Preference(d Difficulty) string {
	switch d {
	case DifficultyTiny:
		return "low"
	case DifficultyStandard:
		return "medium"
	case DifficultyHard:
		return "high"
	case DifficultyHuman:
		return "medium" // do not auto-escalate to max/xhigh without human
	default:
		return "medium"
	}
}

// NormalizeDepth maps common synonyms onto the canonical low/medium/high/xhigh ladder.
// Empty input stays empty (not medium) so callers can distinguish "unset".
func NormalizeDepth(d string) string {
	d = strings.TrimSpace(d)
	if d == "" {
		return ""
	}
	switch strings.ToLower(d) {
	case "low", "minimal", "light":
		return "low"
	case "medium", "mid", "standard", "default":
		return "medium"
	case "high":
		return "high"
	case "xhigh", "max", "deep", "thinking":
		return "xhigh"
	default:
		return strings.ToLower(d)
	}
}

var ladder = []string{"low", "medium", "high", "xhigh"}

// Select picks a supported depth for the difficulty band.
// Explicit pin, when non-empty and supported, wins.
func Select(difficulty Difficulty, supported []string, explicitPin string) (string, error) {
	normSupported := make([]string, 0, len(supported))
	seen := map[string]bool{}
	for _, s := range supported {
		n := NormalizeDepth(s)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		normSupported = append(normSupported, n)
	}
	if len(normSupported) == 0 {
		return "", fmt.Errorf("depthpolicy: no supported depths")
	}
	if pin := NormalizeDepth(explicitPin); pin != "" {
		if seen[pin] {
			return pin, nil
		}
		// Pin unsupported: fail closed (owner pin must not be silently substituted).
		return "", fmt.Errorf("depthpolicy: explicit depth %q not supported by model", pin)
	}
	want := Preference(difficulty)
	// Exact match
	if seen[want] {
		return want, nil
	}
	// Prefer nearest lower, then nearest higher.
	wantIdx := indexOf(want)
	for i := wantIdx; i >= 0; i-- {
		if seen[ladder[i]] {
			return ladder[i], nil
		}
	}
	for i := wantIdx + 1; i < len(ladder); i++ {
		if seen[ladder[i]] {
			return ladder[i], nil
		}
	}
	// Fall back to first supported (already normalized).
	return normSupported[0], nil
}

func indexOf(d string) int {
	for i, x := range ladder {
		if x == d {
			return i
		}
	}
	return 1 // medium
}

// ClassifyDifficulty is a lightweight heuristic from text signals.
// Full taskrequirements integration is CRO-006; this is the depth-side policy.
func ClassifyDifficulty(signals ...string) Difficulty {
	joined := strings.ToLower(strings.Join(signals, " "))
	hardHints := []string{
		"security", "migration", "architecture", "ambiguous", "auth", "crypto",
		"release", "quota", "race", "concurrency", "threat", "credential",
	}
	for _, h := range hardHints {
		if strings.Contains(joined, h) {
			return DifficultyHard
		}
	}
	tinyHints := []string{"docs", "readme", "typo", "comment", "changelog", "markdown only", "tiny"}
	for _, h := range tinyHints {
		if strings.Contains(joined, h) {
			return DifficultyTiny
		}
	}
	if strings.Contains(joined, "needs-human") || strings.Contains(joined, "human_gate") {
		return DifficultyHuman
	}
	return DifficultyStandard
}
