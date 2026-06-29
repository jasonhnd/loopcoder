package loopcoder

import (
	"bytes"
	"os"
	"testing"
)

func TestEmbeddedPlaybookFilesAreNonEmptyAndMatchRootFiles(t *testing.T) {
	tests := []struct {
		name string
		read func() ([]byte, error)
	}{
		{name: "SKILL.md", read: SkillMarkdown},
		{name: "AGENTS.md", read: AgentsMarkdown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			embedded, err := tt.read()
			if err != nil {
				t.Fatalf("read embedded %s: %v", tt.name, err)
			}
			if len(bytes.TrimSpace(embedded)) == 0 {
				t.Fatalf("embedded %s is empty", tt.name)
			}

			source, err := os.ReadFile(tt.name)
			if err != nil {
				t.Fatalf("read source %s: %v", tt.name, err)
			}
			if !bytes.Equal(embedded, source) {
				t.Fatalf("embedded %s does not match repository-root source file", tt.name)
			}
		})
	}
}
