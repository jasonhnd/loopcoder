package reporter

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type legacySurfaceRule struct {
	name     string
	path     string
	prefix   string
	contains string
}

func TestReporterRenameSweepLiveTree(t *testing.T) {
	root := repoRoot(t)
	files := trackedFiles(t, root)
	hits := map[string]int{}
	var unexpected []string

	for _, rel := range files {
		rel = filepath.ToSlash(rel)
		if renameSweepExcludesPath(rel) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		text := strings.ReplaceAll(string(data), "\r\n", "\n")
		for lineNo, line := range strings.Split(text, "\n") {
			if !containsLegacyReporterWording(line) {
				continue
			}
			ruleName, ok := allowedLegacyReporterSurface(rel, line)
			if ok {
				hits[ruleName]++
				continue
			}
			unexpected = append(unexpected, fmt.Sprintf("%s:%d: %s", rel, lineNo+1, strings.TrimSpace(line)))
		}
	}

	if len(unexpected) > 0 {
		t.Fatalf("unexpected legacy attestation wording in live tree; rename to reporter/Report or add a narrow intentional exclusion:\n%s", strings.Join(unexpected, "\n"))
	}
	for _, rule := range legacyReporterSurfaceRules {
		if hits[rule.name] == 0 {
			t.Fatalf("legacy reporter sweep rule %q did not match any live line; remove stale exclusion or update the rule", rule.name)
		}
	}
}

func TestReporterRenameSweepExclusionsAreIntentional(t *testing.T) {
	root := repoRoot(t)

	for _, tc := range []struct {
		path   string
		needle string
	}{
		{"CHANGELOG.md", "attestation"},
		{"docs/specs/0146-attestation.md", "attestation"},
		{"docs/specs/0567-reporter.md", "Frozen Surfaces"},
		{"ROADMAP.md", "attestation"},
		{"docs/learnings.md", "attestation"},
		{"internal/relay/relay.go", `+".attest"`},
		{"internal/conductorhooks/relay_guard.go", `".attest"`},
		{"internal/conductorhooks/attest.go", `\[(?:attestation|reporter)\]`},
		{"internal/cli/cli.go", `Name: "attest"`},
		{"internal/cli/hook.go", `"conductor-reporter", "conductor-attest"`},
		{"internal/conductorhooks/attest.go", `LOOPCODER_CONDUCTOR_ATTEST_SCOPE`},
		{"internal/conductorhooks/attest.go", `LOOPCODER_CONDUCTOR_ATTEST_STATE_DIR`},
		{"internal/state/state.go", `json:"attestation"`},
		{"internal/reportquery/reportquery.go", `typed["attestation"]`},
		{"internal/runstatus/runstatus.go", `values["attestation"]`},
	} {
		assertRepoFileContains(t, root, tc.path, tc.needle)
	}

	record := validRecord()
	data, err := record.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON returned error: %v", err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("canonical JSON invalid: %v", err)
	}
	for _, field := range []string{
		"role",
		"provider",
		"model",
		"model_source",
		"effort",
		"permission",
		"action",
		"exit_code",
		"started_at",
		"ended_at",
		"duration_ms",
		"usage",
		"verified",
	} {
		if _, ok := payload[field]; !ok {
			t.Fatalf("CanonicalJSON missing frozen field %q: %s", field, string(data))
		}
	}
	var usage map[string]json.RawMessage
	if err := json.Unmarshal(payload["usage"], &usage); err != nil {
		t.Fatalf("canonical usage JSON invalid: %v", err)
	}
	for _, field := range []string{"input_tokens", "output_tokens", "total_tokens"} {
		if _, ok := usage[field]; !ok {
			t.Fatalf("CanonicalJSON usage missing frozen field %q: %s", field, string(payload["usage"]))
		}
	}
	if strings.Contains(strings.ToLower(string(data)), "attestation") {
		t.Fatalf("CanonicalJSON used legacy reporter terminology: %s", string(data))
	}
}

func renameSweepExcludesPath(rel string) bool {
	if rel == "CHANGELOG.md" || rel == "ROADMAP.md" || rel == "docs/learnings.md" {
		return true
	}
	if strings.HasPrefix(rel, "docs/specs/") {
		return true
	}
	return strings.HasSuffix(rel, "_test.go")
}

func containsLegacyReporterWording(line string) bool {
	return strings.Contains(strings.ToLower(line), "attestation")
}

var legacyReporterSurfaceRules = []legacySurfaceRule{
	{
		name:     "readme transition token",
		path:     "README.md",
		contains: "During the 0.6.0 transition, relay and hook matchers accept old `[attestation]` tokens",
	},
	{
		name:     "readme historical spec link",
		path:     "README.md",
		contains: "docs/specs/0146-attestation.md",
	},
	{
		name:     "readme old hook and token alias",
		path:     "README.md",
		contains: "The old `conductor-attest` hook command and `[attestation]` token remain one-version compatibility inputs",
	},
	{
		name:     "skill transition alias",
		path:     "SKILL.md",
		contains: "`[attestation]` headers and `attestation` result objects for one version",
	},
	{
		name:     "reference transition legacy wording",
		prefix:   "docs/reference/",
		contains: "legacy",
	},
	{
		name:     "reference legacy envelope continuation",
		prefix:   "docs/reference/",
		contains: "nested `attestation` objects",
	},
	{
		name:     "reference historical spec link",
		prefix:   "docs/reference/",
		contains: "attestation.md",
	},
	{
		name:     "reference historical display polish spec link",
		prefix:   "docs/reference/",
		contains: "attestation-display-polish.md",
	},
	{
		name:     "audit security category",
		path:     "internal/audit/review.go",
		contains: "local-only-attestation",
	},
	{
		name:     "audit relay obligation prose",
		path:     "internal/audit/review.go",
		contains: "local-only attestation and relay obligations",
	},
	{
		name:     "cli attest command summary",
		path:     "internal/cli/cli.go",
		contains: `Summary: "emit conductor self-attestation"`,
	},
	{
		name:     "cli relay compatibility summary",
		path:     "internal/cli/cli.go",
		contains: `Summary: "flush or list pending local attestation relay blocks"`,
	},
	{
		name:     "cli attest role help",
		path:     "internal/cli/cli.go",
		contains: "attestation role",
	},
	{
		name:     "conductor hook dual token regex",
		path:     "internal/conductorhooks/attest.go",
		contains: `\[(?:attestation|reporter)\]\s+role=conductor\b`,
	},
	{
		name:     "conductor hook self report comment",
		path:     "internal/conductorhooks/attest.go",
		contains: "Conductor self-attestation",
	},
	{
		name:     "conductor hook required prompt",
		path:     "internal/conductorhooks/attest.go",
		contains: "loopcoder conductor attestation is required",
	},
	{
		name:     "conductor hook local-only prompt",
		path:     "internal/conductorhooks/attest.go",
		contains: "Keep the emitted attestation local",
	},
	{
		name:     "conductor hook successful marker comment",
		path:     "internal/conductorhooks/attest.go",
		contains: "successful Conductor attestation",
	},
	{
		name:     "conductor hook command comment",
		path:     "internal/conductorhooks/attest.go",
		contains: "a conductor attestation.",
	},
	{
		name:     "conductor hook helper name",
		path:     "internal/conductorhooks/attest.go",
		contains: "containsConductorAttestation",
	},
	{
		name:     "conductor hook header comment",
		path:     "internal/conductorhooks/attest.go",
		contains: "conductor attestation header",
	},
	{
		name:     "relay guard dual token regex",
		path:     "internal/conductorhooks/relay_guard.go",
		contains: `\[(?:attestation|reporter)\]\s+role=(worker|verifier)\b`,
	},
	{
		name:     "relay guard record comment",
		path:     "internal/conductorhooks/relay_guard.go",
		contains: "ledger attestation",
	},
	{
		name:     "relay guard pending comment",
		path:     "internal/conductorhooks/relay_guard.go",
		contains: "relay attestation is still",
	},
	{
		name:     "relay guard unreadable ledger message",
		path:     "internal/conductorhooks/relay_guard.go",
		contains: "no attestation block found",
	},
	{
		name:     "relay guard missing block comment",
		path:     "internal/conductorhooks/relay_guard.go",
		contains: "file has no attestation block",
	},
	{
		name:     "relay guard missing relay prompt",
		path:     "internal/conductorhooks/relay_guard.go",
		contains: "local verbatim attestation relay was missing",
	},
	{
		name:     "relay guard missing block prompt",
		path:     "internal/conductorhooks/relay_guard.go",
		contains: "local-only attestation block(s)",
	},
	{
		name:     "relay guard local-only prompt",
		path:     "internal/conductorhooks/relay_guard.go",
		contains: "Keep these attestations local-only",
	},
	{
		name:     "relay guard ledger helper name",
		path:     "internal/conductorhooks/relay_guard.go",
		contains: "ledgerAttestationBlock",
	},
	{
		name:     "relay guard surfaced helper name",
		path:     "internal/conductorhooks/relay_guard.go",
		contains: "containsSurfacedAttestation",
	},
	{
		name:     "relay guard surfaced comment",
		path:     "internal/conductorhooks/relay_guard.go",
		contains: "record's attestation",
	},
	{
		name:     "relay guard role pattern dual token",
		path:     "internal/conductorhooks/relay_guard.go",
		contains: `\[(?:attestation|reporter)\]\s+role=`,
	},
	{
		name:     "conductor hook shared comment",
		path:     "internal/conductorhooks/shared.go",
		contains: "recorded conductor attestation",
	},
	{
		name:     "conductor relay shared comment",
		path:     "internal/conductorhooks/shared.go",
		contains: "un-surfaced relay attestation",
	},
	{
		name:     "tick local-only comment",
		path:     "internal/orchestration/tick.go",
		contains: "loopreview attestations remain surfaced",
	},
	{
		name:     "report query header reader transition",
		path:     "internal/reportquery/reportquery.go",
		contains: `(?:reporter|attestation)`,
	},
	{
		name:     "report query legacy envelope reader",
		path:     "internal/reportquery/reportquery.go",
		contains: `typed["attestation"]`,
	},
	{
		name:     "runstatus legacy verifier envelope reader",
		path:     "internal/runstatus/runstatus.go",
		contains: `values["attestation"]`,
	},
	{
		name:     "state legacy attempt envelope reader",
		path:     "internal/state/state.go",
		contains: `json:"attestation"`,
	},
}

func allowedLegacyReporterSurface(rel, line string) (string, bool) {
	for _, rule := range legacyReporterSurfaceRules {
		if rule.path != "" && rel != rule.path {
			continue
		}
		if rule.prefix != "" && !strings.HasPrefix(rel, rule.prefix) {
			continue
		}
		if strings.Contains(line, rule.contains) {
			return rule.name, true
		}
	}
	return "", false
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
}

func trackedFiles(t *testing.T, root string) []string {
	t.Helper()
	cmd := exec.Command("git", "ls-files")
	cmd.Dir = root
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	var files []string
	for _, line := range strings.Split(strings.ReplaceAll(string(output), "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			files = append(files, line)
		}
	}
	if len(files) == 0 {
		t.Fatal("git ls-files returned no files")
	}
	return files
}

func assertRepoFileContains(t *testing.T, root, rel, needle string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	if !strings.Contains(string(data), needle) {
		t.Fatalf("%s missing intentional exclusion marker %q", rel, needle)
	}
}
