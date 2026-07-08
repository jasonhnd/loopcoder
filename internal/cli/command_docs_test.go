package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestCommandDocsCoverRegisteredCommands(t *testing.T) {
	root := commandDocsGitRoot(t)
	docs := map[string]string{
		"README.md":               readCommandDocsFile(t, root, "README.md"),
		"docs/reference/usage.md": readCommandDocsFile(t, root, filepath.Join("docs", "reference", "usage.md")),
	}

	var missing []string
	for _, command := range Commands() {
		for rel, text := range docs {
			if !commandDocsContainCommand(text, command.Name) {
				missing = append(missing, rel+" missing loopcoder "+command.Name)
			}
		}
	}

	if len(missing) > 0 {
		t.Fatalf("registered command documentation inventory mismatch:\n%s", strings.Join(missing, "\n"))
	}
}

func TestCommandDocsCommandMatcherRejectsPrefixCollisions(t *testing.T) {
	if commandDocsContainCommand("`loopcoder dispatch-wave`", "dispatch") {
		t.Fatal("matcher accepted dispatch from dispatch-wave")
	}
	for _, text := range []string{
		"`loopcoder dispatch`",
		"loopcoder dispatch --repo .",
		"loopcoder dispatch\n",
		"loopcoder dispatch",
	} {
		if !commandDocsContainCommand(text, "dispatch") {
			t.Fatalf("matcher rejected exact command in %q", text)
		}
	}
}

func commandDocsContainCommand(text, name string) bool {
	needle := "loopcoder " + name
	for search := text; ; {
		index := strings.Index(search, needle)
		if index < 0 {
			return false
		}
		after := search[index+len(needle):]
		if after == "" {
			return true
		}
		next, _ := utf8.DecodeRuneInString(after)
		if commandDocsCommandBoundary(next) {
			return true
		}
		search = after
	}
}

func commandDocsCommandBoundary(r rune) bool {
	return r == '`' || unicode.IsSpace(r)
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
