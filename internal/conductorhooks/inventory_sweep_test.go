package conductorhooks

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jasonhnd/loopcoder/internal/gitutil"
)

func TestReporterRenameInventorySweep(t *testing.T) {
	root := gitRoot(t)
	files := gitTrackedFiles(t, root)

	var violations []string
	for _, rel := range files {
		rel = filepath.ToSlash(rel)
		if !inventorySweepTextFile(rel) || frozenInventoryPath(rel) {
			continue
		}
		path := filepath.Join(root, filepath.FromSlash(rel))
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if !utf8.Valid(data) {
			continue
		}
		text := string(data)
		if strings.HasSuffix(rel, ".go") {
			violations = append(violations, attestationIdentifierViolations(rel, path, text)...)
		}
		violations = append(violations, attestationTextViolations(rel, text)...)
	}

	if len(violations) > 0 {
		t.Fatalf("non-frozen attestation inventory remains:\n%s", strings.Join(violations, "\n"))
	}
}

func gitRoot(t *testing.T) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = "."
	cmd.Env = gitutil.CleanEnv(os.Environ())
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse --show-toplevel: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func gitTrackedFiles(t *testing.T, root string) []string {
	t.Helper()
	cmd := exec.Command("git", "ls-files", "-z")
	cmd.Dir = root
	cmd.Env = gitutil.CleanEnv(os.Environ())
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files -z: %v", err)
	}
	parts := bytes.Split(bytes.TrimSuffix(out, []byte{0}), []byte{0})
	files := make([]string, 0, len(parts))
	for _, part := range parts {
		if len(part) > 0 {
			files = append(files, string(part))
		}
	}
	return files
}

func attestationIdentifierViolations(rel, path, text string) []string {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, text, 0)
	if err != nil {
		return []string{fmt.Sprintf("%s: parse Go file: %v", rel, err)}
	}
	var violations []string
	ast.Inspect(file, func(n ast.Node) bool {
		ident, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		if strings.Contains(ident.Name, "Attestation") || strings.Contains(ident.Name, "attestation") {
			pos := fset.Position(ident.Pos())
			violations = append(violations, fmt.Sprintf("%s:%d: Go identifier %q must use report wording", rel, pos.Line, ident.Name))
		}
		return true
	})
	return violations
}

func attestationTextViolations(rel, text string) []string {
	var violations []string
	for i, line := range strings.Split(text, "\n") {
		if !lineMentionsAttestation(line) {
			continue
		}
		if frozenAttestationText(rel, line) {
			continue
		}
		violations = append(violations, fmt.Sprintf("%s:%d: %s", rel, i+1, strings.TrimSpace(line)))
	}
	return violations
}

func lineMentionsAttestation(line string) bool {
	return strings.Contains(strings.ToLower(line), "attestation") ||
		strings.Contains(line, "AttestationRecord")
}

func inventorySweepTextFile(rel string) bool {
	switch filepath.Ext(rel) {
	case ".go", ".md", ".json", ".yml", ".yaml", ".js", ".sh", ".ps1", ".txt":
		return true
	default:
		return false
	}
}

func frozenInventoryPath(rel string) bool {
	// Historical release/spec/planning notes are not live rename surfaces.
	switch rel {
	case "CHANGELOG.md",
		"ROADMAP.md",
		"docs/learnings.md",
		"internal/conductorhooks/inventory_sweep_test.go":
		return true
	}
	return strings.HasPrefix(rel, "docs/specs/")
}

func frozenAttestationText(rel, line string) bool {
	// Keep this closed over the spec's frozen transition surfaces. Go identifiers
	// are never allowed through this text classifier.
	if historicalSpecReference(line) {
		return true
	}
	if releaseTransitionDocumentation(rel) {
		return true
	}
	if dualTokenMatcher(line) {
		return true
	}
	if legacyTokenFixture(rel, line) {
		return true
	}
	if legacyEnvelopeKey(rel, line) {
		return true
	}
	return false
}

func historicalSpecReference(line string) bool {
	lower := strings.ToLower(line)
	return strings.Contains(lower, "specs/") && strings.Contains(lower, "-attestation")
}

func releaseTransitionDocumentation(rel string) bool {
	if rel == "README.md" || rel == "docs/reference/releasing.md" {
		return true
	}
	matched, err := path.Match(".github/release-notes/*.md", rel)
	return err == nil && matched
}

func dualTokenMatcher(line string) bool {
	return strings.Contains(line, `(?:attestation|reporter)`) ||
		strings.Contains(line, `(?:reporter|attestation)`) ||
		strings.Contains(line, `(reporter|attestation)`)
}

func legacyTokenFixture(rel, line string) bool {
	if !strings.Contains(line, "[attestation]") {
		return false
	}
	switch rel {
	case "README.md",
		"SKILL.md",
		"docs/reference/usage.md",
		"docs/reference/worker.md",
		"internal/cli/cli_test.go",
		"internal/conductorhooks/attest_test.go",
		"internal/conductorhooks/relay_escape_test.go",
		"internal/conductorhooks/relay_guard_test.go",
		"internal/worker/worker_test.go":
		return true
	default:
		return false
	}
}

func legacyEnvelopeKey(rel, line string) bool {
	hasKey := strings.Contains(line, `"attestation"`) || strings.Contains(line, "`attestation`")
	if !hasKey {
		return false
	}
	switch rel {
	case "SKILL.md",
		"docs/reference/usage.md",
		"docs/reference/worker.md",
		"internal/cli/audit_test.go",
		"internal/cli/cli_test.go",
		"internal/migration/migration.go",
		"internal/reportquery/reportquery.go",
		"internal/reportquery/reportquery_test.go",
		"internal/runstatus/runstatus.go",
		"internal/runstatus/runstatus_test.go",
		"internal/state/state.go",
		"internal/state/state_test.go":
		return true
	default:
		return false
	}
}
