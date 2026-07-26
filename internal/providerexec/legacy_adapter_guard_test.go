package providerexec_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLegacyProviderExecPackagesNotImportedByProduction enforces that
// codexexec/grokexec/claudeexec/geminiexec/antigravityexec remain test-only.
// These packages still use request-as-actual success and must never re-enter
// production wiring via AgentAdapter/directrun/cli/goalrun.
func TestLegacyProviderExecPackagesNotImportedByProduction(t *testing.T) {
	legacy := []string{
		"github.com/jasonhnd/loopcoder/internal/codexexec",
		"github.com/jasonhnd/loopcoder/internal/grokexec",
		"github.com/jasonhnd/loopcoder/internal/claudeexec",
		"github.com/jasonhnd/loopcoder/internal/geminiexec",
		"github.com/jasonhnd/loopcoder/internal/antigravityexec",
	}
	// Walk from module root (two levels up from this package when run as package test).
	root := findModuleRoot(t)
	var offenders []string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			base := filepath.Base(path)
			if base == ".git" || base == "vendor" || base == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// Allow the legacy packages themselves and their tests.
		rel, _ := filepath.Rel(root, path)
		for _, pkg := range []string{"codexexec", "grokexec", "claudeexec", "geminiexec", "antigravityexec"} {
			if strings.HasPrefix(rel, filepath.Join("internal", pkg)+string(os.PathSeparator)) {
				return nil
			}
		}
		// Skip this guard test itself if it only mentions paths as strings.
		if strings.HasSuffix(rel, "legacy_adapter_guard_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		text := string(b)
		// Only count import statements, not string mentions in this file's list.
		if !strings.Contains(text, "import") {
			return nil
		}
		for _, imp := range legacy {
			// Import form: "github.com/.../codexexec"
			if strings.Contains(text, `"`+imp+`"`) {
				offenders = append(offenders, rel+" imports "+imp)
			}
		}
		return nil
	})
	if len(offenders) > 0 {
		t.Fatalf("legacy *exec packages must remain test-only; production imports:\n%s",
			strings.Join(offenders, "\n"))
	}
}

func findModuleRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := wd
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("go.mod not found")
	return ""
}
