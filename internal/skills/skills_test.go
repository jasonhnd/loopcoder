package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestBuildPromptSectionInjectsRepoSkillMetadata(t *testing.T) {
	repo := t.TempDir()
	writeSkill(t, repo, "go-style", `---
name: go-style
description: "Go style rules for this repository"
---

# Go Style

Implementation body should not be injected.

## Testing
`)
	writeSkill(t, repo, "api", `---
description: API conventions
---

# API
`)

	section, err := BuildPromptSection(PromptSectionOptions{RepoPath: repo, BudgetBytes: 4096})
	if err != nil {
		t.Fatalf("BuildPromptSection returned error: %v", err)
	}
	for _, want := range []string{
		"## Repo-local skills",
		"Discovery rule: include immediate repo-local skill files matching `.claude/skills/*/SKILL.md`.",
		"### api",
		"Path: `.claude/skills/api/SKILL.md`",
		"Summary: API conventions",
		"- # API",
		"### go-style",
		"Summary: Go style rules for this repository",
		"- # Go Style",
		"- ## Testing",
	} {
		if !strings.Contains(section, want) {
			t.Fatalf("section missing %q:\n%s", want, section)
		}
	}
	if strings.Contains(section, "Implementation body should not be injected") {
		t.Fatalf("section included skill body text:\n%s", section)
	}
	if strings.Index(section, "### api") > strings.Index(section, "### go-style") {
		t.Fatalf("skills are not rendered in deterministic path order:\n%s", section)
	}
}

func TestBuildPromptSectionAbsentWhenNoRepoSkills(t *testing.T) {
	section, err := BuildPromptSection(PromptSectionOptions{RepoPath: t.TempDir()})
	if err != nil {
		t.Fatalf("BuildPromptSection returned error: %v", err)
	}
	if section != "" {
		t.Fatalf("section = %q, want empty", section)
	}
}

func TestBuildPromptSectionEnforcesByteBudget(t *testing.T) {
	repo := t.TempDir()
	writeSkill(t, repo, "large", `---
name: large
description: `+strings.Repeat("long summary ", 40)+`
---

# Large
## `+strings.Repeat("Long Heading ", 40)+`
`)

	const budget = 260
	section, err := BuildPromptSection(PromptSectionOptions{RepoPath: repo, BudgetBytes: budget})
	if err != nil {
		t.Fatalf("BuildPromptSection returned error: %v", err)
	}
	if len(section) > budget {
		t.Fatalf("section length = %d, want <= %d:\n%s", len(section), budget, section)
	}
	if !utf8.ValidString(section) {
		t.Fatalf("section is not valid UTF-8: %q", section)
	}
	if !strings.Contains(section, "TRUNCATED repo skills") {
		t.Fatalf("section missing truncation marker:\n%s", section)
	}
}

func writeSkill(t *testing.T, repo, name, content string) {
	t.Helper()
	dir := filepath.Join(repo, repoSkillDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll skill dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, skillFileName), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile skill: %v", err)
	}
}
