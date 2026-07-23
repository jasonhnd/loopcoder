package goalpr

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStripANSIAndParseColorizedPRChecksJSON(t *testing.T) {
	// Real host regression: CLICOLOR_FORCE paints gh --json with SGR sequences.
	colorized := "\x1b[1;37m[\x1b[m\n  \x1b[1;37m{\x1b[m\n    \x1b[1;34m\"bucket\"\x1b[m\x1b[1;37m:\x1b[m \x1b[32m\"pass\"\x1b[m\x1b[1;37m,\x1b[m\n    \x1b[1;34m\"name\"\x1b[m\x1b[1;37m:\x1b[m \x1b[32m\"product-tests\"\x1b[m\x1b[1;37m,\x1b[m\n    \x1b[1;34m\"state\"\x1b[m\x1b[1;37m:\x1b[m \x1b[32m\"SUCCESS\"\x1b[m\n  \x1b[1;37m}\x1b[m\x1b[1;37m,\x1b[m\n  \x1b[1;37m{\x1b[m\n    \x1b[1;34m\"bucket\"\x1b[m\x1b[1;37m:\x1b[m \x1b[32m\"pass\"\x1b[m\x1b[1;37m,\x1b[m\n    \x1b[1;34m\"name\"\x1b[m\x1b[1;37m:\x1b[m \x1b[32m\"product-build\"\x1b[m\x1b[1;37m,\x1b[m\n    \x1b[1;34m\"state\"\x1b[m\x1b[1;37m:\x1b[m \x1b[32m\"SUCCESS\"\x1b[m\n  \x1b[1;37m}\x1b[m\n\x1b[1;37m]\x1b[m\n"
	var dump []any
	if err := json.Unmarshal([]byte(colorized), &dump); err == nil {
		t.Fatal("expected colorized JSON to fail without strip")
	}
	clean := strings.TrimSpace(stripANSI(colorized))
	names, green, err := parsePRChecksJSON(clean)
	if err != nil {
		t.Fatalf("parse: %v clean=%q", err, clean)
	}
	if !green {
		t.Fatalf("green=false names=%v", names)
	}
	if len(names) != 2 {
		t.Fatalf("names=%v", names)
	}
	// strip is identity on clean JSON
	plain := `[{"bucket":"pass","name":"product-tests","state":"SUCCESS"}]`
	if got := stripANSI(plain); got != plain {
		t.Fatalf("strip changed plain JSON: %q", got)
	}
}

func TestGHColorSafeEnvDropsForceColor(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"CLICOLOR_FORCE=1",
		"FORCE_COLOR=1",
		"COLORTERM=truecolor",
		"NO_COLOR=",
		"HOME=/tmp",
	}
	out := ghColorSafeEnv(in)
	joined := strings.Join(out, "\n")
	for _, bad := range []string{"CLICOLOR_FORCE=", "FORCE_COLOR=", "COLORTERM="} {
		if strings.Contains(joined, bad) {
			t.Fatalf("still has %s in %v", bad, out)
		}
	}
	if !strings.Contains(joined, "NO_COLOR=1") || !strings.Contains(joined, "CLICOLOR=0") {
		t.Fatalf("missing NO_COLOR/CLICOLOR in %v", out)
	}
}
