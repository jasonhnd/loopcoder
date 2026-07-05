package audit

import (
	"regexp"
	"strings"
)

func normalizeRepoPath(path string) string {
	path = strings.TrimSpace(strings.ReplaceAll(path, `\`, `/`))
	for strings.HasPrefix(path, "./") {
		path = strings.TrimPrefix(path, "./")
	}
	path = strings.TrimPrefix(path, "/")
	return path
}

func matchesAnyRepoGlob(path string, patterns []string) bool {
	for _, pattern := range patterns {
		if matchRepoGlob(pattern, path) {
			return true
		}
	}
	return false
}

func matchRepoGlob(pattern, value string) bool {
	pattern = normalizeRepoPath(pattern)
	value = normalizeRepoPath(value)
	if pattern == "" || value == "" {
		return false
	}
	anchored := strings.HasPrefix(pattern, "/")
	pattern = strings.TrimPrefix(pattern, "/")
	if strings.HasSuffix(pattern, "/") {
		pattern += "**"
	}
	if !anchored && !strings.Contains(pattern, "/") {
		return matchGlobPattern(pattern, pathBase(value))
	}
	return matchGlobPattern(pattern, value)
}

func matchGlobPattern(pattern, value string) bool {
	re, err := regexp.Compile(globPatternRegexp(pattern))
	if err != nil {
		return false
	}
	return re.MatchString(value)
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

func pathBase(path string) string {
	path = normalizeRepoPath(path)
	if path == "" {
		return ""
	}
	index := strings.LastIndex(path, "/")
	if index < 0 {
		return path
	}
	return path[index+1:]
}
