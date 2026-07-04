package skills

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const (
	// DefaultPromptBudgetBytes bounds the full repo-skill prompt section.
	DefaultPromptBudgetBytes = 4 * 1024

	repoSkillDir     = ".claude/skills"
	skillFileName    = "SKILL.md"
	defaultSkillGlob = ".claude/skills/*/SKILL.md"
)

// PromptSectionOptions controls repo-skill prompt rendering.
type PromptSectionOptions struct {
	RepoPath            string
	Paths               []string
	MachineLibraryPaths []string
	Select              []string
	BudgetBytes         int
}

type summary struct {
	Name        string
	Path        string
	Description string
	Tags        []string
	Headings    []string
}

// BuildPromptSection discovers repo-local skills and renders a bounded
// metadata-only prompt section. By default it includes immediate children
// matching .claude/skills/*/SKILL.md. Configured domain skill paths extend that
// to ordered repo-relative file globs while preserving metadata-first injection.
func BuildPromptSection(opts PromptSectionOptions) (string, error) {
	budget := opts.BudgetBytes
	if budget <= 0 {
		budget = DefaultPromptBudgetBytes
	}
	paths, machinePaths, defaultDiscovery, err := effectivePatterns(opts.Paths, opts.MachineLibraryPaths)
	if err != nil {
		return "", err
	}
	summaries, err := readSummaries(opts.RepoPath, appendPatternLists(paths, machinePaths))
	if err != nil {
		return "", err
	}
	summaries = filterSummaries(summaries, opts.Select)
	if len(summaries) == 0 {
		return "", nil
	}
	return boundPromptSection(formatPromptSection(summaries, budget, paths, machinePaths, opts.Select, defaultDiscovery), budget), nil
}

func readSummaries(repoPath string, patterns []string) ([]summary, error) {
	files, err := discoverSkillFiles(repoPath, patterns)
	if err != nil {
		return nil, err
	}

	var summaries []summary
	for _, file := range files {
		data, err := os.ReadFile(file.abs)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		s := parseSummary(skillNameFromPath(file.rel), file.rel, string(data))
		summaries = append(summaries, s)
	}
	return summaries, nil
}

func parseSummary(dirName, relPath, content string) summary {
	frontmatter, body := splitFrontmatter(content)
	values := parseFrontmatterMetadata(frontmatter)
	name := strings.TrimSpace(values.Name)
	if name == "" {
		name = dirName
	}
	return summary{
		Name:        name,
		Path:        relPath,
		Description: strings.TrimSpace(values.Description),
		Tags:        values.Tags,
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
	var metadata frontmatterMetadata
	if strings.TrimSpace(frontmatter) == "" {
		return metadata
	}

	var values map[string]any
	if err := yaml.Unmarshal([]byte(frontmatter), &values); err == nil {
		for key, value := range values {
			switch strings.TrimSpace(strings.ToLower(key)) {
			case "name":
				metadata.Name = stringValue(value)
			case "description":
				metadata.Description = stringValue(value)
			case "tag", "tags":
				metadata.Tags = append(metadata.Tags, stringList(value)...)
			}
		}
		metadata.Tags = uniqueTrimmed(metadata.Tags)
		return metadata
	}

	for _, line := range strings.Split(frontmatter, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(strings.ToLower(key))
		switch key {
		case "name":
			metadata.Name = trimYAMLScalar(value)
		case "description":
			metadata.Description = trimYAMLScalar(value)
		case "tag", "tags":
			metadata.Tags = append(metadata.Tags, trimYAMLScalar(value))
		}
	}
	metadata.Tags = uniqueTrimmed(metadata.Tags)
	return metadata
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case nil:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprint(typed))
	}
}

func stringList(value any) []string {
	switch typed := value.(type) {
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			out = append(out, stringValue(item))
		}
		return out
	case []string:
		return typed
	case string:
		if strings.Contains(typed, ",") {
			parts := strings.Split(typed, ",")
			out := make([]string, 0, len(parts))
			for _, part := range parts {
				out = append(out, strings.TrimSpace(part))
			}
			return out
		}
		return []string{typed}
	case nil:
		return nil
	default:
		return []string{stringValue(typed)}
	}
}

func uniqueTrimmed(values []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	return out
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

func formatPromptSection(summaries []summary, budget int, paths, machinePaths, selects []string, defaultDiscovery bool) string {
	var out strings.Builder
	fmt.Fprintf(&out, "## Repo-local skills\n")
	if defaultDiscovery {
		fmt.Fprintf(&out, "Discovery rule: include immediate repo-local skill files matching `.claude/skills/*/SKILL.md`.\n")
	} else {
		fmt.Fprintf(&out, "Discovery rule: include repo-local skill metadata files from configured `domain.skills` globs.\n")
		fmt.Fprintf(&out, "Skill paths:\n")
		for _, pattern := range paths {
			fmt.Fprintf(&out, "- `%s`\n", pattern)
		}
		if len(machinePaths) > 0 {
			fmt.Fprintf(&out, "Machine library paths:\n")
			for _, pattern := range machinePaths {
				fmt.Fprintf(&out, "- `%s`\n", pattern)
			}
		}
		if selected := nonEmptyTrimmed(selects); len(selected) > 0 {
			fmt.Fprintf(&out, "Selection: `%s`.\n", strings.Join(selected, "`, `"))
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
		if len(s.Tags) > 0 {
			fmt.Fprintf(&out, "Tags: %s\n", strings.Join(s.Tags, ", "))
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

type discoveredSkillFile struct {
	abs string
	rel string
}

func effectivePatterns(paths, machinePaths []string) ([]string, []string, bool, error) {
	cleanedPaths, err := cleanPatterns(paths)
	if err != nil {
		return nil, nil, false, err
	}
	cleanedMachinePaths, err := cleanPatterns(machinePaths)
	if err != nil {
		return nil, nil, false, err
	}
	defaultDiscovery := len(cleanedPaths) == 0 && len(cleanedMachinePaths) == 0
	if len(cleanedPaths) == 0 {
		cleanedPaths = []string{defaultSkillGlob}
	}
	return cleanedPaths, cleanedMachinePaths, defaultDiscovery, nil
}

func appendPatternLists(paths, machinePaths []string) []string {
	out := make([]string, 0, len(paths)+len(machinePaths))
	out = append(out, paths...)
	out = append(out, machinePaths...)
	return out
}

func cleanPatterns(patterns []string) ([]string, error) {
	var cleaned []string
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		if filepath.IsAbs(pattern) || path.IsAbs(filepath.ToSlash(pattern)) {
			return nil, fmt.Errorf("skill path glob must be repo-relative: %s", pattern)
		}
		normalized := path.Clean(filepath.ToSlash(pattern))
		normalized = strings.TrimPrefix(normalized, "./")
		if normalized == "." || normalized == ".." || strings.HasPrefix(normalized, "../") {
			return nil, fmt.Errorf("skill path glob must stay within the repo: %s", pattern)
		}
		cleaned = append(cleaned, normalized)
	}
	return cleaned, nil
}

func discoverSkillFiles(repoPath string, patterns []string) ([]discoveredSkillFile, error) {
	seen := map[string]struct{}{}
	var files []discoveredSkillFile
	for _, pattern := range patterns {
		matches, err := expandPattern(repoPath, pattern)
		if err != nil {
			return nil, err
		}
		for _, file := range matches {
			if _, ok := seen[file.rel]; ok {
				continue
			}
			seen[file.rel] = struct{}{}
			files = append(files, file)
		}
	}
	return files, nil
}

func expandPattern(repoPath, pattern string) ([]discoveredSkillFile, error) {
	if err := validateRepoGlobPattern(pattern); err != nil {
		return nil, fmt.Errorf("invalid skill path glob %q: %w", pattern, err)
	}
	if strings.Contains(pattern, "**") {
		return expandRecursivePattern(repoPath, pattern)
	}
	matches, err := filepath.Glob(filepath.Join(repoPath, filepath.FromSlash(pattern)))
	if err != nil {
		return nil, fmt.Errorf("invalid skill path glob %q: %w", pattern, err)
	}
	sort.Strings(matches)
	files := make([]discoveredSkillFile, 0, len(matches))
	for _, match := range matches {
		file, ok, err := discoveredFile(repoPath, match)
		if err != nil {
			return nil, err
		}
		if ok {
			files = append(files, file)
		}
	}
	return files, nil
}

func validateRepoGlobPattern(pattern string) error {
	for _, segment := range strings.Split(pattern, "/") {
		if segment == "**" {
			continue
		}
		if _, err := path.Match(segment, ""); err != nil {
			return err
		}
	}
	return nil
}

func expandRecursivePattern(repoPath, pattern string) ([]discoveredSkillFile, error) {
	root := recursiveWalkRoot(repoPath, pattern)
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var files []discoveredSkillFile
	err := filepath.WalkDir(root, func(candidate string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		file, ok, err := discoveredFile(repoPath, candidate)
		if err != nil || !ok {
			return err
		}
		matched, err := matchRepoGlob(pattern, file.rel)
		if err != nil {
			return fmt.Errorf("invalid skill path glob %q: %w", pattern, err)
		}
		if matched {
			files = append(files, file)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

func recursiveWalkRoot(repoPath, pattern string) string {
	parts := strings.Split(pattern, "/")
	var prefix []string
	for _, part := range parts {
		if part == "**" || strings.ContainsAny(part, "*?[") {
			break
		}
		prefix = append(prefix, part)
	}
	root := repoPath
	for _, part := range prefix {
		root = filepath.Join(root, filepath.FromSlash(part))
	}
	return root
}

func discoveredFile(repoPath, abs string) (discoveredSkillFile, bool, error) {
	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return discoveredSkillFile{}, false, nil
		}
		return discoveredSkillFile{}, false, err
	}
	if info.IsDir() {
		return discoveredSkillFile{}, false, nil
	}
	rel, err := filepath.Rel(repoPath, abs)
	if err != nil {
		return discoveredSkillFile{}, false, err
	}
	rel = filepath.ToSlash(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return discoveredSkillFile{}, false, fmt.Errorf("skill path escaped repo: %s", abs)
	}
	return discoveredSkillFile{abs: abs, rel: rel}, true, nil
}

func matchRepoGlob(pattern, rel string) (bool, error) {
	pattern = strings.Trim(pattern, "/")
	rel = strings.Trim(rel, "/")
	if pattern == "" || rel == "" {
		return pattern == rel, nil
	}
	return matchSegments(strings.Split(pattern, "/"), strings.Split(rel, "/"))
}

func matchSegments(pattern, rel []string) (bool, error) {
	if len(pattern) == 0 {
		return len(rel) == 0, nil
	}
	if pattern[0] == "**" {
		if matched, err := matchSegments(pattern[1:], rel); err != nil || matched {
			return matched, err
		}
		for i := range rel {
			if matched, err := matchSegments(pattern[1:], rel[i+1:]); err != nil || matched {
				return matched, err
			}
		}
		return false, nil
	}
	if len(rel) == 0 {
		return false, nil
	}
	matched, err := path.Match(pattern[0], rel[0])
	if err != nil || !matched {
		return false, err
	}
	return matchSegments(pattern[1:], rel[1:])
}

func filterSummaries(summaries []summary, selects []string) []summary {
	selectSet := normalizeSelectSet(selects)
	if len(selectSet) == 0 {
		return summaries
	}
	var filtered []summary
	for _, s := range summaries {
		if summaryMatchesSelect(s, selectSet) {
			filtered = append(filtered, s)
		}
	}
	return filtered
}

func summaryMatchesSelect(s summary, selectSet map[string]struct{}) bool {
	candidates := []string{s.Name, pathStemForSelect(s.Path), fileStemForSelect(s.Path)}
	candidates = append(candidates, s.Tags...)
	for _, candidate := range candidates {
		if _, ok := selectSet[normalizeSelectToken(candidate)]; ok {
			return true
		}
	}
	return false
}

func normalizeSelectSet(selects []string) map[string]struct{} {
	selectSet := map[string]struct{}{}
	for _, selectValue := range selects {
		normalized := normalizeSelectToken(selectValue)
		if normalized != "" {
			selectSet[normalized] = struct{}{}
		}
	}
	return selectSet
}

func normalizeSelectToken(value string) string {
	value = strings.TrimSpace(strings.ToLower(filepath.ToSlash(value)))
	var out strings.Builder
	lastDash := false
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			out.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			out.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(out.String(), "-")
}

func pathStemForSelect(relPath string) string {
	stem := skillNameFromPath(relPath)
	if stem != "" {
		return stem
	}
	return fileStemForSelect(relPath)
}

func fileStemForSelect(relPath string) string {
	base := path.Base(filepath.ToSlash(relPath))
	return strings.TrimSuffix(base, path.Ext(base))
}

func skillNameFromPath(relPath string) string {
	relPath = strings.Trim(filepath.ToSlash(relPath), "/")
	base := path.Base(relPath)
	stem := strings.TrimSuffix(base, path.Ext(base))
	if strings.EqualFold(stem, "skill") && path.Dir(relPath) != "." {
		parent := path.Base(path.Dir(relPath))
		if parent != "." && parent != "/" {
			return parent
		}
	}
	return stem
}

func nonEmptyTrimmed(values []string) []string {
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
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
