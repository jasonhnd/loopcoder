package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommandDocsCoverRegisteredCommands(t *testing.T) {
	root := commandDocsGitRoot(t)
	docs := map[string]string{
		"README.md":               readCommandDocsFile(t, root, "README.md"),
		"docs/reference/usage.md": readCommandDocsFile(t, root, filepath.Join("docs", "reference", "usage.md")),
	}

	var missing []string
	for _, command := range Commands() {
		needle := "loopcoder " + command.Name
		for rel, text := range docs {
			if !strings.Contains(text, needle) {
				missing = append(missing, rel+" missing "+needle)
			}
		}
	}

	if len(missing) > 0 {
		t.Fatalf("registered command documentation inventory mismatch:\n%s", strings.Join(missing, "\n"))
	}
}

func commandDocsGitRoot(t *testing.T) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse --show-toplevel: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func readCommandDocsFile(t *testing.T, root, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}
