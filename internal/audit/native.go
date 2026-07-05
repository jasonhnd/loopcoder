package audit

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const maxNativeFileBytes int64 = 1 << 20

var (
	githubTokenPattern = regexp.MustCompile(`\bghp_[A-Za-z0-9_]{20,}\b|\bgithub_pat_[A-Za-z0-9_]{20,}\b`)
	apiTokenPattern    = regexp.MustCompile(`\bsk-[A-Za-z0-9]{20,}\b`)
	assignmentPattern  = regexp.MustCompile(`(?i)\b(api[_-]?key|token|secret|password)\b\s*[:=]\s*["']?([A-Za-z0-9_./+=-]{8,})`)
	modeLiteralPattern = regexp.MustCompile(`\b(?:0o|0)?([0-7]{3})\b|os\.ModePerm`)
	writeCallPattern   = regexp.MustCompile(`\b(WriteFile|OpenFile)\s*\(`)
	sensitiveWords     = regexp.MustCompile(`(?i)\b(prompt|schema|summary|log|recovery|token|key|secret|password|attestation)\b`)
)

func ScanNative(repoPath string, plan NativePlan) []Finding {
	findings := []Finding{}
	if !plan.Secrets && !plan.FilePermissions {
		return findings
	}
	files := nativeFiles(repoPath, plan)
	for _, file := range files {
		if plan.FilePermissions {
			findings = append(findings, scanSensitiveFileMode(file)...)
		}
		if !file.Text {
			continue
		}
		content, err := os.ReadFile(filepath.Join(repoPath, filepath.FromSlash(file.Rel)))
		if err != nil {
			continue
		}
		text := string(content)
		if plan.Secrets {
			findings = append(findings, scanSecrets(file.Rel, text)...)
		}
		if plan.FilePermissions {
			findings = append(findings, scanSensitiveWrites(file.Rel, text)...)
		}
	}
	return findings
}

type nativeFile struct {
	RepoPath string
	Rel      string
	Mode     fs.FileMode
	Text     bool
}

func nativeFiles(repoPath string, plan NativePlan) []nativeFile {
	files := []nativeFile{}
	_ = filepath.WalkDir(repoPath, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		rel, err := filepath.Rel(repoPath, path)
		if err != nil {
			return nil
		}
		rel = normalizeRepoPath(rel)
		if rel == "." || rel == "" {
			return nil
		}
		if entry.IsDir() {
			if nativePathExcluded(rel+"/", plan.Exclude) {
				return filepath.SkipDir
			}
			return nil
		}
		if !nativePathIncluded(rel, plan.Include) || nativePathExcluded(rel, plan.Exclude) {
			return nil
		}
		info, err := entry.Info()
		if err != nil || info.Size() > maxNativeFileBytes {
			return nil
		}
		text := false
		if data, err := os.ReadFile(path); err == nil {
			text = looksText(data)
		}
		files = append(files, nativeFile{
			RepoPath: repoPath,
			Rel:      rel,
			Mode:     info.Mode().Perm(),
			Text:     text,
		})
		return nil
	})
	return files
}

func scanSecrets(file string, text string) []Finding {
	findings := []Finding{}
	for index, line := range splitTextLines(text) {
		if !lineLooksSecret(line) {
			continue
		}
		redacted := RedactSecrets(line)
		if redacted == line {
			continue
		}
		findings = append(findings, NewFinding(
			LayerSAST,
			"native",
			SeverityHigh,
			file,
			index+1,
			0,
			"native:secret",
			"secret-disclosure",
			"Potential hardcoded secret material was found.",
			redacted,
		))
	}
	return findings
}

func scanSensitiveWrites(file string, text string) []Finding {
	lines := splitTextLines(text)
	findings := []Finding{}
	for index, line := range lines {
		if !writeCallPattern.MatchString(line) {
			continue
		}
		window := sourceWindow(lines, index)
		if !hasBroadFileMode(window) || !sensitiveWords.MatchString(window) {
			continue
		}
		findings = append(findings, NewFinding(
			LayerSAST,
			"native",
			SeverityMedium,
			file,
			index+1,
			0,
			"native:sensitive-write",
			"shared-host-disclosure",
			"Sensitive material appears to be written with permissions broader than 0600.",
			strings.TrimSpace(line),
		))
	}
	return findings
}

func hasBroadFileMode(text string) bool {
	if strings.Contains(text, "os.ModePerm") {
		return true
	}
	for _, match := range modeLiteralPattern.FindAllStringSubmatch(text, -1) {
		if len(match) < 2 || match[1] == "" {
			continue
		}
		mode := 0
		for _, r := range match[1] {
			mode = mode*8 + int(r-'0')
		}
		if mode&0o077 != 0 {
			return true
		}
	}
	return false
}

func scanSensitiveFileMode(file nativeFile) []Finding {
	if !isSensitivePath(file.Rel) {
		return nil
	}
	if file.Mode&0o077 == 0 {
		return nil
	}
	return []Finding{NewFinding(
		LayerSAST,
		"native",
		SeverityMedium,
		file.Rel,
		0,
		0,
		"native:file-permission",
		"shared-host-disclosure",
		"Sensitive local file is group/world readable or writable.",
		fmt.Sprintf("%s mode %#o", file.Rel, file.Mode),
	)}
}

func RedactSecrets(text string) string {
	text = githubTokenPattern.ReplaceAllString(text, "[REDACTED_GITHUB_TOKEN]")
	text = apiTokenPattern.ReplaceAllString(text, "[REDACTED_API_KEY]")
	text = assignmentPattern.ReplaceAllString(text, "${1}=[REDACTED_SECRET]")
	return text
}

func lineLooksSecret(line string) bool {
	return githubTokenPattern.MatchString(line) ||
		apiTokenPattern.MatchString(line) ||
		assignmentPattern.MatchString(line)
}

func sourceWindow(lines []string, index int) string {
	start := index - 2
	if start < 0 {
		start = 0
	}
	end := index + 3
	if end > len(lines) {
		end = len(lines)
	}
	return strings.Join(lines[start:end], "\n")
}

func isSensitivePath(path string) bool {
	path = strings.ToLower(normalizeRepoPath(path))
	base := pathBase(path)
	if base == ".env" || strings.HasSuffix(base, ".pem") || strings.HasSuffix(base, ".key") || base == "auth.json" {
		return true
	}
	for _, signal := range []string{"token", "secret", "credential", "private-key"} {
		if strings.Contains(base, signal) {
			return true
		}
	}
	return strings.Contains(path, ".loopcoder/runs/") && strings.Contains(path, "/recovery/")
}

func nativePathIncluded(path string, includes []string) bool {
	if len(includes) == 0 {
		return true
	}
	for _, pattern := range includes {
		if matchRepoGlob(pattern, path) {
			return true
		}
	}
	return false
}

func nativePathExcluded(path string, excludes []string) bool {
	for _, pattern := range excludes {
		if matchRepoGlob(pattern, path) {
			return true
		}
	}
	return false
}

func looksText(data []byte) bool {
	if len(data) == 0 {
		return true
	}
	for _, b := range data {
		if b == 0 {
			return false
		}
	}
	return true
}

func matchRepoGlob(pattern, path string) bool {
	pattern = normalizeRepoPath(pattern)
	path = normalizeRepoPath(path)
	if pattern == "" || path == "" {
		return false
	}
	anchored := strings.HasPrefix(pattern, "/")
	pattern = strings.TrimPrefix(pattern, "/")
	if strings.HasSuffix(pattern, "/") {
		pattern += "**"
	}
	if !anchored && !strings.Contains(pattern, "/") {
		return matchGlobPattern(pattern, pathBase(path))
	}
	return matchGlobPattern(pattern, path)
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
	index := strings.LastIndex(path, "/")
	if index < 0 {
		return path
	}
	return path[index+1:]
}
