package skills

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const (
	// DefaultPromptBudgetBytes bounds the full repo-skill prompt section.
	DefaultPromptBudgetBytes = 4 * 1024

	defaultSkillPath = ".claude/skills/*/SKILL.md"
	repoSkillDir     = ".claude/skills"
	skillFileName    = "SKILL.md"
)

// PromptSectionOptions controls repo-skill prompt rendering.
type PromptSectionOptions struct {
	RepoPath            string
	Paths               []string
	MachineLibraryPaths []string
	Select              []string
	BudgetBytes         int
}

type discoverySource struct {
	Pattern        string
	MachineLibrary bool
}

type summary struct {
	Name        string
	Path        string
	Description string
	Tags        []string
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
	sources := discoverySources(opts)
	selectors, selectorSet := normalizeSelectors(opts.Select)
	summaries, err := readSummaries(opts.RepoPath, sources, selectorSet)
	if err != nil {
		return "", err
	}
	if len(summaries) == 0 {
		return "", nil
	}
	return boundPromptSection(formatPromptSection(summaries, budget, sources, selectors), budget), nil
}

func discoverySources(opts PromptSectionOptions) []discoverySource {
	paths := nonEmptyStrings(opts.Paths)
	if len(paths) == 0 {
		paths = []string{defaultSkillPath}
	}
	sources := make([]discoverySource, 0, len(paths)+len(opts.MachineLibraryPaths))
	for _, pattern := range paths {
		sources = append(sources, discoverySource{Pattern: pattern})
	}
	for _, pattern := range nonEmptyStrings(opts.MachineLibraryPaths) {
		sources = append(sources, discoverySource{Pattern: pattern, MachineLibrary: true})
	}
	return sources
}

func readSummaries(repoPath string, sources []discoverySource, selectors map[string]struct{}) ([]summary, error) {
	if strings.TrimSpace(repoPath) == "" {
		repoPath = "."
	}
	var summaries []summary
	seen := map[string]struct{}{}
	for _, source := range sources {
		matches, err := expandRepoGlob(repoPath, source.Pattern)
		if err != nil {
			return nil, fmt.Errorf("discover skills for %q: %w", source.Pattern, err)
		}
		for _, relPath := range matches {
			if _, ok := seen[relPath]; ok {
				continue
			}
			seen[relPath] = struct{}{}
			data, err := os.ReadFile(filepath.Join(repoPath, filepath.FromSlash(relPath)))
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return nil, err
			}
			s := parseSummary(skillPathStem(relPath), relPath, string(data))
			if !summarySelected(s, selectors) {
				continue
			}
			summaries = append(summaries, s)
		}
	}
	return summaries, nil
}

func expandRepoGlob(repoPath, pattern string) ([]string, error) {
	normalized, err := normalizeSkillPattern(pattern)
	if err != nil || normalized == "" {
		return nil, err
	}
	if !strings.Contains(normalized, "**") {
		return filepathGlobRepoFiles(repoPath, normalized)
	}
	return walkRepoGlob(repoPath, normalized)
}

func filepathGlobRepoFiles(repoPath, pattern string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(repoPath, filepath.FromSlash(pattern)))
	if err != nil {
		return nil, err
	}
	relPaths := make([]string, 0, len(matches))
	for _, match := range matches {
		info, err := os.Stat(match)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		if info.IsDir() {
			continue
		}
		rel, ok, err := repoRelativePath(repoPath, match)
		if err != nil {
			return nil, err
		}
		if ok {
			relPaths = append(relPaths, rel)
		}
	}
	sort.Strings(relPaths)
	return relPaths, nil
}

func walkRepoGlob(repoPath, pattern string) ([]string, error) {
	root := filepath.Join(repoPath, filepath.FromSlash(globWalkRoot(pattern)))
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var matches []string
	err := filepath.WalkDir(root, func(filePath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, ok, err := repoRelativePath(repoPath, filePath)
		if err != nil || !ok {
			return err
		}
		matched, err := matchSkillGlob(pattern, rel)
		if err != nil || !matched {
			return err
		}
		matches = append(matches, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	return matches, nil
}

func repoRelativePath(repoPath, filePath string) (string, bool, error) {
	rel, err := filepath.Rel(repoPath, filePath)
	if err != nil {
		return "", false, err
	}
	rel = filepath.ToSlash(rel)
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false, nil
	}
	return rel, true, nil
}

func normalizeSkillPattern(pattern string) (string, error) {
	original := strings.TrimSpace(pattern)
	if original == "" {
		return "", nil
	}
	if filepath.IsAbs(original) || filepath.VolumeName(original) != "" {
		return "", fmt.Errorf("must be repo-relative")
	}
	normalized := strings.TrimPrefix(filepath.ToSlash(original), "./")
	if strings.HasPrefix(normalized, "/") {
		return "", fmt.Errorf("must be repo-relative")
	}
	normalized = path.Clean(normalized)
	if normalized == "." {
		return "", nil
	}
	if normalized == ".." || strings.HasPrefix(normalized, "../") {
		return "", fmt.Errorf("must be repo-relative")
	}
	if strings.ContainsAny(normalized, "[]") {
		return "", fmt.Errorf("character classes are not supported")
	}
	if _, err := path.Match(normalized, ""); err != nil {
		return "", err
	}
	return normalized, nil
}

func globWalkRoot(pattern string) string {
	index := strings.IndexAny(pattern, "*?")
	if index < 0 {
		dir := path.Dir(pattern)
		if dir == "." {
			return "."
		}
		return dir
	}
	prefix := pattern[:index]
	if strings.HasSuffix(prefix, "/") {
		prefix = strings.TrimSuffix(prefix, "/")
		if prefix == "" {
			return "."
		}
		return prefix
	}
	dir := path.Dir(prefix)
	if dir == "." || dir == "/" {
		return "."
	}
	return dir
}

func matchSkillGlob(pattern, value string) (bool, error) {
	re, err := regexp.Compile(globPatternRegexp(pattern))
	if err != nil {
		return false, err
	}
	return re.MatchString(filepath.ToSlash(value)), nil
}

func globPatternRegexp(pattern string) string {
	var out strings.Builder
	out.WriteString("^")
	for i := 0; i < len(pattern); {
		switch {
		case strings.HasPrefix(pattern[i:], "**/"):
			out.WriteString("(?:.*/)?")
			i += 3
		case strings.HasPrefix(pattern[i:], "**"):
			out.WriteString(".*")
			i += 2
		default:
			ch := pattern[i]
			switch ch {
			case '*':
				out.WriteString("[^/]*")
			case '?':
				out.WriteString("[^/]")
			default:
				out.WriteString(regexp.QuoteMeta(string(ch)))
			}
			i++
		}
	}
	out.WriteString("$")
	return out.String()
}

func parseSummary(defaultName, relPath, content string) summary {
	frontmatter, body := splitFrontmatter(content)
	metadata := parseFrontmatterMetadata(frontmatter)
	name := strings.TrimSpace(metadata.Name)
	if name == "" {
		name = defaultName
	}
	return summary{
		Name:        name,
		Path:        relPath,
		Description: strings.TrimSpace(metadata.Description),
		Tags:        metadata.Tags,
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

type frontmatterMetadata struct {
	Name        string
	Description string
	Tags        []string
}

func parseFrontmatterMetadata(frontmatter string) frontmatterMetadata {
	if strings.TrimSpace(frontmatter) == "" {
		return frontmatterMetadata{}
	}
	legacy := parseFrontmatterValues(frontmatter)
	metadata := frontmatterMetadata{
		Name:        legacy["name"],
		Description: legacy["description"],
	}
	var values map[string]any
	if err := yaml.Unmarshal([]byte(frontmatter), &values); err == nil {
		for key, value := range values {
			if strings.ToLower(strings.TrimSpace(key)) == "tags" {
				metadata.Tags = yamlStringSlice(value)
				break
			}
		}
	}
	return metadata
}

func yamlScalarString(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(v)
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func yamlStringSlice(value any) []string {
	switch v := value.(type) {
	case nil:
		return nil
	case []string:
		return nonEmptyStrings(v)
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if text := yamlScalarString(item); text != "" {
				out = append(out, text)
			}
		}
		return out
	case string:
		return splitInlineTags(v)
	default:
		if text := yamlScalarString(v); text != "" {
			return []string{text}
		}
		return nil
	}
}

func splitInlineTags(value string) []string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "[")
	value = strings.TrimSuffix(value, "]")
	parts := strings.Split(value, ",")
	if len(parts) == 1 {
		return nonEmptyStrings([]string{trimYAMLScalar(parts[0])})
	}
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if text := trimYAMLScalar(part); text != "" {
			out = append(out, text)
		}
	}
	return out
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

func formatPromptSection(summaries []summary, budget int, sources []discoverySource, selectors []string) string {
	var out strings.Builder
	legacyDiscovery := defaultDiscovery(sources, selectors)
	fmt.Fprintf(&out, "## Repo-local skills\n")
	if legacyDiscovery {
		fmt.Fprintf(&out, "Discovery rule: include immediate repo-local skill files matching `.claude/skills/*/SKILL.md`.\n")
	} else {
		fmt.Fprintf(&out, "Discovery paths:\n")
		for _, source := range sources {
			label := "skills"
			if source.MachineLibrary {
				label = "machine_library"
			}
			fmt.Fprintf(&out, "- %s: `%s`\n", label, source.Pattern)
		}
		if len(selectors) > 0 {
			fmt.Fprintf(&out, "Selection filter: %s.\n", strings.Join(selectors, ", "))
		}
	}
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
		if !legacyDiscovery && len(s.Tags) > 0 {
			fmt.Fprintf(&out, "Tags: `%s`\n", strings.Join(s.Tags, "`, `"))
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

func defaultDiscovery(sources []discoverySource, selectors []string) bool {
	return len(sources) == 1 && !sources[0].MachineLibrary && sources[0].Pattern == defaultSkillPath && len(selectors) == 0
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

func normalizeSelectors(values []string) ([]string, map[string]struct{}) {
	normalized := make([]string, 0, len(values))
	set := map[string]struct{}{}
	for _, value := range values {
		value = normalizeSelector(value)
		if value == "" {
			continue
		}
		if _, ok := set[value]; ok {
			continue
		}
		set[value] = struct{}{}
		normalized = append(normalized, value)
	}
	return normalized, set
}

func summarySelected(s summary, selectors map[string]struct{}) bool {
	if len(selectors) == 0 {
		return true
	}
	for _, candidate := range summarySelectionCandidates(s) {
		if _, ok := selectors[normalizeSelector(candidate)]; ok {
			return true
		}
	}
	return false
}

func summarySelectionCandidates(s summary) []string {
	candidates := []string{s.Name, skillPathStem(s.Path)}
	parent := path.Base(path.Dir(filepath.ToSlash(s.Path)))
	if parent != "." && parent != "/" {
		candidates = append(candidates, parent)
	}
	candidates = append(candidates, s.Tags...)
	return candidates
}

func skillPathStem(relPath string) string {
	relPath = filepath.ToSlash(strings.TrimSpace(relPath))
	base := path.Base(relPath)
	ext := path.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	if strings.EqualFold(stem, "skill") {
		parent := path.Base(path.Dir(relPath))
		if parent != "." && parent != "/" && parent != "" {
			return parent
		}
	}
	return stem
}

func normalizeSelector(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	var out strings.Builder
	lastDash := false
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			out.WriteRune(r)
			lastDash = false
			continue
		}
		if out.Len() > 0 && !lastDash {
			out.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(out.String(), "-")
}

func nonEmptyStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
