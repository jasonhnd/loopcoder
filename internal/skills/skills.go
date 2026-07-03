package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	// DefaultPromptBudgetBytes bounds the full repo-skill prompt section.
	DefaultPromptBudgetBytes = 4 * 1024

	repoSkillDir  = ".claude/skills"
	skillFileName = "SKILL.md"
)

// PromptSectionOptions controls repo-skill prompt rendering.
type PromptSectionOptions struct {
	RepoPath    string
	BudgetBytes int
}

type summary struct {
	Name        string
	Path        string
	Description string
	Headings    []string
}

// BuildPromptSection discovers repo-local skills and renders a bounded
// metadata-only prompt section. The discovery rule is intentionally simple:
// immediate children matching .claude/skills/*/SKILL.md are included.
func BuildPromptSection(opts PromptSectionOptions) (string, error) {
	budget := opts.BudgetBytes
	if budget <= 0 {
		budget = DefaultPromptBudgetBytes
	}
	summaries, err := readSummaries(opts.RepoPath)
	if err != nil {
		return "", err
	}
	if len(summaries) == 0 {
		return "", nil
	}
	return boundPromptSection(formatPromptSection(summaries, budget), budget), nil
}

func readSummaries(repoPath string) ([]summary, error) {
	root := filepath.Join(repoPath, repoSkillDir)
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var summaries []summary
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(root, entry.Name(), skillFileName)
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		rel, err := filepath.Rel(repoPath, path)
		if err != nil {
			rel = path
		}
		s := parseSummary(entry.Name(), filepath.ToSlash(rel), string(data))
		summaries = append(summaries, s)
	}

	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].Path < summaries[j].Path
	})
	return summaries, nil
}

func parseSummary(dirName, relPath, content string) summary {
	frontmatter, body := splitFrontmatter(content)
	values := parseFrontmatterValues(frontmatter)
	name := strings.TrimSpace(values["name"])
	if name == "" {
		name = dirName
	}
	return summary{
		Name:        name,
		Path:        relPath,
		Description: strings.TrimSpace(values["description"]),
		Headings:    markdownHeadings(body),
	}
}

func splitFrontmatter(content string) (string, string) {
	normalized := strings.ReplaceAll(content, "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return "", normalized
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			return strings.Join(lines[1:i], "\n"), strings.Join(lines[i+1:], "\n")
		}
	}
	return "", normalized
}

func parseFrontmatterValues(frontmatter string) map[string]string {
	values := map[string]string{}
	for _, line := range strings.Split(frontmatter, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(strings.ToLower(key))
		if key != "name" && key != "description" {
			continue
		}
		values[key] = trimYAMLScalar(value)
	}
	return values
}

func trimYAMLScalar(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 {
		if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
			return strings.TrimSpace(value[1 : len(value)-1])
		}
	}
	return value
}

func markdownHeadings(content string) []string {
	var headings []string
	for _, line := range strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		level := headingLevel(trimmed)
		if level == 0 {
			continue
		}
		headings = append(headings, trimmed)
	}
	return headings
}

func headingLevel(line string) int {
	level := 0
	for level < len(line) && line[level] == '#' {
		level++
	}
	if level == 0 || level > 6 || level >= len(line) || line[level] != ' ' {
		return 0
	}
	return level
}

func formatPromptSection(summaries []summary, budget int) string {
	var out strings.Builder
	fmt.Fprintf(&out, "## Repo-local skills\n")
	fmt.Fprintf(&out, "Discovery rule: include immediate repo-local skill files matching `.claude/skills/*/SKILL.md`.\n")
	fmt.Fprintf(&out, "Use these headers and summaries as project conventions when relevant; read the skill files if full instructions are needed.\n")
	fmt.Fprintf(&out, "Budget: %d bytes.\n", budget)
	for _, s := range summaries {
		fmt.Fprintf(&out, "\n### %s\n", emptyDefault(s.Name, "(unnamed skill)"))
		fmt.Fprintf(&out, "Path: `%s`\n", s.Path)
		if strings.TrimSpace(s.Description) != "" {
			fmt.Fprintf(&out, "Summary: %s\n", s.Description)
		} else {
			fmt.Fprintf(&out, "Summary: (none provided)\n")
		}
		if len(s.Headings) > 0 {
			fmt.Fprintf(&out, "Headers:\n")
			for _, heading := range s.Headings {
				fmt.Fprintf(&out, "- %s\n", heading)
			}
		} else {
			fmt.Fprintf(&out, "Headers: (none found)\n")
		}
	}
	return out.String()
}

func emptyDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func boundPromptSection(text string, budget int) string {
	if budget <= 0 || len(text) <= budget {
		return text
	}

	prefixBudget := budget
	for i := 0; i < 12; i++ {
		prefix, omittedBytes, omittedLines := truncateUTF8(text, prefixBudget)
		marker := fmt.Sprintf("\n[TRUNCATED repo skills: omitted %d bytes, %d lines]", omittedBytes, omittedLines)
		allowedPrefix := budget - len(marker)
		if allowedPrefix < 0 {
			truncatedMarker, _, _ := truncateUTF8(strings.TrimSpace(marker), budget)
			return truncatedMarker
		}
		if allowedPrefix == prefixBudget {
			return prefix + marker
		}
		prefixBudget = allowedPrefix
	}

	prefix, omittedBytes, omittedLines := truncateUTF8(text, prefixBudget)
	marker := fmt.Sprintf("\n[TRUNCATED repo skills: omitted %d bytes, %d lines]", omittedBytes, omittedLines)
	if len(prefix)+len(marker) <= budget {
		return prefix + marker
	}
	truncated, _, _ := truncateUTF8(prefix+marker, budget)
	return truncated
}

func truncateUTF8(text string, byteBudget int) (string, int, int) {
	if len(text) <= byteBudget {
		return text, 0, 0
	}
	if byteBudget < 0 {
		byteBudget = 0
	}
	end := byteBudget
	if end > len(text) {
		end = len(text)
	}
	for end > 0 && !utf8.ValidString(text[:end]) {
		end--
	}
	omitted := text[end:]
	return text[:end], len(text) - end, countOmittedLines(omitted)
}

func countLines(text string) int {
	if text == "" {
		return 0
	}
	return strings.Count(text, "\n") + 1
}

func countOmittedLines(text string) int {
	text = strings.TrimPrefix(text, "\r\n")
	text = strings.TrimPrefix(text, "\n")
	return countLines(text)
}
