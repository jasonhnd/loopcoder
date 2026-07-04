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

func TestBuildPromptSectionUsesConfiguredPathsMachineLibraryAndSelect(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, repo, filepath.Join("governance", "review", "skill.md"), `---
name: Governance Review
description: Governance review conventions
tags: [governance, disclosure]
---

# Governance Review

Full governance body should not be injected.
`)
	writeFile(t, repo, filepath.Join(".loopcoder", "skill-library", "disclosure.md"), `---
description: Disclosure library metadata
---

# Disclosure Library
`)
	writeFile(t, repo, filepath.Join("governance", "ignored", "skill.md"), `---
name: Ignored Skill
---

# Ignored
`)

	section, err := BuildPromptSection(PromptSectionOptions{
		RepoPath:            repo,
		Paths:               []string{"governance/**/skill.md"},
		MachineLibraryPaths: []string{".loopcoder/skill-library/**/*.md"},
		Select:              []string{"governance", "disclosure"},
		BudgetBytes:         4096,
	})
	if err != nil {
		t.Fatalf("BuildPromptSection returned error: %v", err)
	}
	for _, want := range []string{
		"Discovery paths:",
		"- skills: `governance/**/skill.md`",
		"- machine_library: `.loopcoder/skill-library/**/*.md`",
		"Selection filter: governance, disclosure.",
		"### Governance Review",
		"Path: `governance/review/skill.md`",
		"Summary: Governance review conventions",
		"Tags: `governance`, `disclosure`",
		"### disclosure",
		"Path: `.loopcoder/skill-library/disclosure.md`",
		"Summary: Disclosure library metadata",
	} {
		if !strings.Contains(section, want) {
			t.Fatalf("section missing %q:\n%s", want, section)
		}
	}
	for _, notWant := range []string{
		"Full governance body should not be injected",
		"Ignored Skill",
	} {
		if strings.Contains(section, notWant) {
			t.Fatalf("section contained %q:\n%s", notWant, section)
		}
	}
	if strings.Index(section, "governance/review/skill.md") > strings.Index(section, ".loopcoder/skill-library/disclosure.md") {
		t.Fatalf("configured paths are not rendered before machine library paths:\n%s", section)
	}
}

func TestBuildPromptSectionSelectMatchesNamePathStemAndTag(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, repo, filepath.Join("skills", "name.md"), `---
name: Human Review
---

# Name
`)
	writeFile(t, repo, filepath.Join("skills", "path-stem.md"), `---
description: Path stem match
---

# Path Stem
`)
	writeFile(t, repo, filepath.Join("skills", "tagged.md"), `---
tags:
  - governance
---

# Tagged
`)
	writeFile(t, repo, filepath.Join("skills", "skip.md"), `---
name: Skip
---

# Skip
`)

	section, err := BuildPromptSection(PromptSectionOptions{
		RepoPath:    repo,
		Paths:       []string{"skills/*.md"},
		Select:      []string{"human review", "path-stem", "governance"},
		BudgetBytes: 4096,
	})
	if err != nil {
		t.Fatalf("BuildPromptSection returned error: %v", err)
	}
	for _, want := range []string{
		"### Human Review",
		"Path: `skills/name.md`",
		"### path-stem",
		"Path: `skills/path-stem.md`",
		"### tagged",
		"Tags: `governance`",
	} {
		if !strings.Contains(section, want) {
			t.Fatalf("section missing %q:\n%s", want, section)
		}
	}
	if strings.Contains(section, "Path: `skills/skip.md`") {
		t.Fatalf("section included unselected skill:\n%s", section)
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
	writeFile(t, repo, filepath.Join(repoSkillDir, name, skillFileName), content)
}

func writeFile(t *testing.T, repo, relPath, content string) {
	t.Helper()
	path := filepath.Join(repo, relPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}
