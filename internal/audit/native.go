package audit

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	lcdefaults "github.com/jasonhnd/loopcoder/internal/defaults"
)

var (
	secretAssignmentPattern = regexp.MustCompile(`(?i)\b(api[_-]?key|secret|token|password|private[_-]?key)\b\s*[:=]\s*["']?([A-Za-z0-9][A-Za-z0-9_./+=-]{15,})["']?`)
	awsAccessKeyPattern     = regexp.MustCompile(`\b(AKIA[0-9A-Z]{16})\b`)
	sensitiveModePattern    = regexp.MustCompile(`(?i)\b(?:os\.)?(?:WriteFile|OpenFile|Chmod)\s*\([^)]*(0o[0-7]{3,4}|0[0-7]{3,4})`)
)

func RunNativeScans(repoPath string, cfg NativeConfig) ([]Finding, error) {
	files, err := auditFiles(repoPath, cfg.Include, cfg.Exclude)
	if err != nil {
		return nil, err
	}
	findings := []Finding{}
	for _, file := range files {
		if cfg.FilePermissions {
			permissionFinding, ok := nativePermissionFinding(repoPath, file)
			if ok {
				findings = append(findings, permissionFinding)
			}
		}
		if !shouldReadNativeFile(repoPath, file) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(repoPath, filepath.FromSlash(file)))
		if err != nil {
			continue
		}
		if looksBinary(data) {
			continue
		}
		if cfg.Secrets {
			findings = append(findings, nativeSecretFindings(file, string(data))...)
		}
		if cfg.FilePermissions {
			findings = append(findings, nativeSensitiveWriteFindings(file, string(data))...)
		}
	}
	return findings, nil
}

func auditFiles(repoPath string, include, exclude []string) ([]string, error) {
	if len(include) == 0 {
		include = []string{"**/*"}
	}
	files := []string{}
	err := filepath.WalkDir(repoPath, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if path == repoPath {
				return walkErr
			}
			return nil
		}
		if path == repoPath {
			return nil
		}
		rel, err := filepath.Rel(repoPath, path)
		if err != nil {
			return nil
		}
		rel = normalizeRepoPath(rel)
		if entry.IsDir() {
			if matchesAnyRepoGlob(rel+"/", exclude) || matchesAnyRepoGlob(rel+"/**", exclude) {
				return filepath.SkipDir
			}
			return nil
		}
		if !matchesAnyRepoGlob(rel, include) {
			return nil
		}
		if matchesAnyRepoGlob(rel, exclude) {
			return nil
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("native audit scan: %w", err)
	}
	return files, nil
}

func nativeSecretFindings(file, text string) []Finding {
	findings := []Finding{}
	for index, line := range splitLines(text) {
		lineNumber := index + 1
		findings = append(findings, secretAssignmentFindings(file, lineNumber, line)...)
		findings = append(findings, awsSecretFindings(file, lineNumber, line)...)
	}
	return findings
}

func secretAssignmentFindings(file string, lineNumber int, line string) []Finding {
	matches := secretAssignmentPattern.FindAllStringSubmatchIndex(line, -1)
	findings := make([]Finding, 0, len(matches))
	for _, match := range matches {
		if len(match) < 6 {
			continue
		}
		redacted := redactCapture(line, match[4], match[5])
		findings = append(findings, makeFinding(Finding{
			Layer:    LayerSAST,
			Tool:     "native",
			Severity: SeverityHigh,
			File:     file,
			Line:     lineNumber,
			Rule:     "native:secret",
			Category: "secret-disclosure",
			Message:  "Potential hardcoded secret detected.",
			Evidence: boundedEvidence(redacted),
		}))
	}
	return findings
}

func awsSecretFindings(file string, lineNumber int, line string) []Finding {
	matches := awsAccessKeyPattern.FindAllStringSubmatchIndex(line, -1)
	findings := make([]Finding, 0, len(matches))
	for _, match := range matches {
		if len(match) < 4 {
			continue
		}
		redacted := redactCapture(line, match[2], match[3])
		findings = append(findings, makeFinding(Finding{
			Layer:    LayerSAST,
			Tool:     "native",
			Severity: SeverityHigh,
			File:     file,
			Line:     lineNumber,
			Rule:     "native:secret",
			Category: "secret-disclosure",
			Message:  "Potential AWS access key detected.",
			Evidence: boundedEvidence(redacted),
		}))
	}
	return findings
}

func nativePermissionFinding(repoPath, file string) (Finding, bool) {
	if !isSensitivePath(file) {
		return Finding{}, false
	}
	if isTrackedSourceLikePath(file) {
		return Finding{}, false
	}
	info, err := os.Stat(filepath.Join(repoPath, filepath.FromSlash(file)))
	if err != nil || info.IsDir() {
		return Finding{}, false
	}
	mode := info.Mode().Perm()
	if mode&0o077 == 0 {
		return Finding{}, false
	}
	return makeFinding(Finding{
		Layer:    LayerSAST,
		Tool:     "native",
		Severity: SeverityMedium,
		File:     file,
		Rule:     "native:file-permission",
		Category: "shared-host-disclosure",
		Message:  "Sensitive file is readable or writable beyond owner-only permissions.",
		Evidence: fmt.Sprintf("%s mode %04o is broader than 0600", file, mode),
	}), true
}

func nativeSensitiveWriteFindings(file, text string) []Finding {
	findings := []Finding{}
	lines := splitLines(text)
	for index, line := range lines {
		if !isSensitiveText(line) {
			continue
		}
		match := sensitiveModePattern.FindStringSubmatch(line)
		if len(match) < 2 {
			continue
		}
		mode, ok := parseMode(match[1])
		if !ok || mode&0o077 == 0 {
			continue
		}
		findings = append(findings, makeFinding(Finding{
			Layer:    LayerSAST,
			Tool:     "native",
			Severity: SeverityMedium,
			File:     file,
			Line:     index + 1,
			Rule:     "native:sensitive-write",
			Category: "shared-host-disclosure",
			Message:  "Sensitive material is written with permissions broader than 0600.",
			Evidence: boundedEvidence(strings.TrimSpace(line)),
		}))
	}
	return findings
}

func shouldReadNativeFile(repoPath, file string) bool {
	info, err := os.Stat(filepath.Join(repoPath, filepath.FromSlash(file)))
	if err != nil || info.IsDir() {
		return false
	}
	if info.Size() < 0 || info.Size() > lcdefaults.HookInputMaxBytes {
		return false
	}
	return true
}

func looksBinary(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	limit := len(data)
	if limit > 4096 {
		limit = 4096
	}
	for _, b := range data[:limit] {
		if b == 0 {
			return true
		}
	}
	return false
}

func splitLines(text string) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimRight(text, "\n\r")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func redactCapture(line string, start, end int) string {
	if start < 0 || end < start || end > len(line) {
		return boundedEvidence(line)
	}
	return strings.TrimSpace(line[:start] + "[REDACTED]" + line[end:])
}

func boundedEvidence(text string) string {
	text = strings.Join(strings.Fields(strings.TrimSpace(text)), " ")
	if len(text) <= lcdefaults.ConductorHookMaxTextField {
		return text
	}
	return text[:lcdefaults.ConductorHookMaxTextField] + "...[truncated]"
}

func isSensitivePath(file string) bool {
	lower := strings.ToLower(normalizeRepoPath(file))
	base := pathBase(lower)
	if strings.HasPrefix(base, ".env") {
		return true
	}
	if strings.HasSuffix(base, ".log") || base == "log" || base == "log.txt" {
		return true
	}
	for _, component := range strings.Split(lower, "/") {
		if component == "log" || component == "logs" || component == "recovery" {
			return true
		}
	}
	for _, signal := range []string{
		"prompt",
		"schema",
		"summary",
		"recovery",
		"token",
		"secret",
		"credential",
		"password",
		"private_key",
		"private-key",
	} {
		if strings.Contains(base, signal) {
			return true
		}
	}
	return strings.HasPrefix(base, "key.") || strings.HasSuffix(base, ".key") || strings.Contains(base, "_key.") || strings.Contains(base, "-key.")
}

func isTrackedSourceLikePath(file string) bool {
	file = normalizeRepoPath(file)
	if strings.HasPrefix(file, ".loopcoder/") {
		return false
	}
	switch strings.ToLower(filepath.Ext(file)) {
	case ".go", ".md", ".yml", ".yaml", ".json", ".toml":
		return true
	default:
		return false
	}
}

func isSensitiveText(text string) bool {
	text = strings.ToLower(text)
	for _, signal := range []string{
		"prompt",
		"schema",
		"summary",
		"log",
		"recovery",
		"token",
		"key",
		"secret",
		"credential",
		"password",
	} {
		if strings.Contains(text, signal) {
			return true
		}
	}
	return false
}

func parseMode(text string) (int, bool) {
	text = strings.TrimSpace(strings.ToLower(text))
	text = strings.TrimPrefix(text, "0o")
	if strings.HasPrefix(text, "0") && len(text) > 1 {
		text = text[1:]
	}
	mode, err := strconv.ParseInt(text, 8, 32)
	if err != nil {
		return 0, false
	}
	return int(mode), true
}
