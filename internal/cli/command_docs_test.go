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
		needle := "loopcoder " + command.Name
		for rel, text := range docs {
			if !containsLoopcoderCommand(text, command.Name) {
				missing = append(missing, rel+" missing "+needle)
			}
		}
	}

	if len(missing) > 0 {
		t.Fatalf("registered command documentation inventory mismatch:\n%s", strings.Join(missing, "\n"))
	}
}

func TestCommandDocsMatchCommandTokenBoundaries(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		command string
		want    bool
	}{
		{
			name:    "exact code span",
			text:    "`loopcoder dispatch`",
			command: "dispatch",
			want:    true,
		},
		{
			name:    "followed by subcommand argument",
			text:    "loopcoder state push",
			command: "state",
			want:    true,
		},
		{
			name:    "prefix collision with hyphenated command",
			text:    "`loopcoder dispatch-wave`",
			command: "dispatch",
			want:    false,
		},
		{
			name:    "prefix collision with longer token",
			text:    "`loopcoder verify-local`",
			command: "verify",
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := containsLoopcoderCommand(tt.text, tt.command); got != tt.want {
				t.Fatalf("containsLoopcoderCommand(%q, %q) = %t, want %t", tt.text, tt.command, got, tt.want)
			}
		})
	}
}

func containsLoopcoderCommand(text, name string) bool {
	needle := "loopcoder " + name
	for offset := 0; ; {
		index := strings.Index(text[offset:], needle)
		if index < 0 {
			return false
		}
		end := offset + index + len(needle)
		if isCommandTokenBoundary(text[end:]) {
			return true
		}
		offset = end
	}
}

func isCommandTokenBoundary(rest string) bool {
	if rest == "" {
		return true
	}
	r, _ := utf8.DecodeRuneInString(rest)
	return !isCommandTokenRune(r)
}

func isCommandTokenRune(r rune) bool {
	return r == '-' || r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
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
