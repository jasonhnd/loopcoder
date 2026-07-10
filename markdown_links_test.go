package loopcoder

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode"
)

var markdownLinkPattern = regexp.MustCompile(`!?\[[^\]]*\]\(([^)\s]+(?:\s+"[^"]*")?)\)`)

func TestMarkdownInternalLinks(t *testing.T) {
	root := markdownLinksGitRoot(t)
	files := markdownFiles(t, root)
	anchors := map[string]map[string]bool{}
	for _, file := range files {
		anchors[file] = markdownAnchors(t, file)
	}

	var failures []string
	for _, file := range files {
		dir := filepath.Dir(file)
		for _, link := range markdownLinks(t, file) {
			target, fragment := splitMarkdownTarget(link)
			if shouldSkipMarkdownTarget(target) {
				continue
			}
			targetFile := file
			if target != "" {
				unescaped, err := url.PathUnescape(target)
				if err != nil {
					failures = append(failures, fmt.Sprintf("%s: invalid URL escape in %q: %v", relToRoot(root, file), link, err))
					continue
				}
				targetFile = filepath.Clean(filepath.Join(dir, filepath.FromSlash(unescaped)))
				if !pathInside(root, targetFile) {
					failures = append(failures, fmt.Sprintf("%s: link escapes repository: %q", relToRoot(root, file), link))
					continue
				}
				info, err := os.Stat(targetFile)
				if err != nil {
					failures = append(failures, fmt.Sprintf("%s: missing target for %q", relToRoot(root, file), link))
					continue
				}
				if info.IsDir() {
					continue
				}
			}
			if fragment == "" {
				continue
			}
			targetAnchors, ok := anchors[targetFile]
			if !ok {
				if filepath.Ext(targetFile) != ".md" {
					continue
				}
				targetAnchors = markdownAnchors(t, targetFile)
				anchors[targetFile] = targetAnchors
			}
			if !targetAnchors[fragment] {
				failures = append(failures, fmt.Sprintf("%s: missing anchor #%s for %q", relToRoot(root, file), fragment, link))
			}
		}
	}

	if len(failures) > 0 {
		sort.Strings(failures)
		t.Fatalf("Markdown internal link check failed:\n%s", strings.Join(failures, "\n"))
	}
}

func markdownFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			switch name {
			case ".git", ".loopcoder", "dist", "node_modules", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.EqualFold(filepath.Ext(name), ".md") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk markdown files: %v", err)
	}
	sort.Strings(files)
	return files
}

func markdownLinks(t *testing.T, path string) []string {
	t.Helper()
	lines := readMarkdownLines(t, path)
	var links []string
	inFence := false
	for _, line := range lines {
		if isFenceLine(line) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		for _, match := range markdownLinkPattern.FindAllStringSubmatch(line, -1) {
			target := strings.TrimSpace(match[1])
			if cut := strings.IndexAny(target, " \t"); cut >= 0 {
				target = target[:cut]
			}
			target = strings.Trim(target, "<>")
			links = append(links, target)
		}
	}
	return links
}

func markdownAnchors(t *testing.T, path string) map[string]bool {
	t.Helper()
	lines := readMarkdownLines(t, path)
	anchors := map[string]bool{}
	seen := map[string]int{}
	inFence := false
	for _, line := range lines {
		if isFenceLine(line) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "#") {
			continue
		}
		level := 0
		for level < len(trimmed) && trimmed[level] == '#' {
			level++
		}
		if level == 0 || level > 6 || level >= len(trimmed) || trimmed[level] != ' ' {
			continue
		}
		heading := strings.TrimSpace(trimmed[level:])
		if heading == "" {
			continue
		}
		slug := githubHeadingSlug(heading)
		count := seen[slug]
		seen[slug] = count + 1
		if count > 0 {
			slug = fmt.Sprintf("%s-%d", slug, count)
		}
		anchors[slug] = true
	}
	return anchors
}

func githubHeadingSlug(heading string) string {
	heading = strings.TrimSpace(stripInlineMarkdown(heading))
	var b strings.Builder
	lastHyphen := false
	for _, r := range strings.ToLower(heading) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			lastHyphen = false
		case unicode.IsSpace(r) || r == '-':
			if !lastHyphen && b.Len() > 0 {
				b.WriteByte('-')
				lastHyphen = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

func stripInlineMarkdown(s string) string {
	replacer := strings.NewReplacer("`", "", "*", "", "_", "", "[", "", "]", "")
	return replacer.Replace(s)
}

func splitMarkdownTarget(target string) (string, string) {
	before, after, ok := strings.Cut(target, "#")
	if !ok {
		return target, ""
	}
	fragment, err := url.QueryUnescape(after)
	if err != nil {
		fragment = after
	}
	return before, fragment
}

func shouldSkipMarkdownTarget(target string) bool {
	lower := strings.ToLower(target)
	return strings.HasPrefix(lower, "http://") ||
		strings.HasPrefix(lower, "https://") ||
		strings.HasPrefix(lower, "mailto:") ||
		strings.HasPrefix(lower, "tel:") ||
		strings.HasPrefix(lower, "ftp://") ||
		strings.HasPrefix(lower, "javascript:")
}

func isFenceLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	return strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~")
}

func readMarkdownLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.Split(string(data), "\n")
}

func pathInside(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != "." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".."
}

func relToRoot(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(rel)
}

func markdownLinksGitRoot(t *testing.T) string {
	t.Helper()
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(root, ".git")); err == nil {
			return root
		}
		parent := filepath.Dir(root)
		if parent == root {
			t.Fatalf("could not find repository root from working directory")
		}
		root = parent
	}
}
