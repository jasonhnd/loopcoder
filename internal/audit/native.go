package audit

import (
	"context"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"

	lcdefaults "github.com/jasonhnd/loopcoder/internal/defaults"
)

const genericSecretEntropyFloor = 3.5

type nativeSecretSignature struct {
	family  string
	pattern *regexp.Regexp
}

type nativeSecretRange struct {
	start int
	end   int
}

var (
	secretAssignmentPattern = regexp.MustCompile(`(?i)\b(api[_-]?key|secret|token|password|private[_-]?key)\b\s*[:=]\s*["']?([A-Za-z0-9][A-Za-z0-9_./+=-]{15,})["']?`)
	secretSignaturePatterns = []nativeSecretSignature{
		{family: "GitHub classic token", pattern: regexp.MustCompile(`\b(ghp_[A-Za-z0-9_]{36,})\b`)},
		{family: "GitHub fine-grained token", pattern: regexp.MustCompile(`\b(github_pat_[A-Za-z0-9_]{36,})\b`)},
		{family: "Stripe live key", pattern: regexp.MustCompile(`\b(sk_live_[A-Za-z0-9]{16,})\b`)},
		{family: "AWS access key", pattern: regexp.MustCompile(`\b(AKIA[0-9A-Z]{16})\b`)},
		{family: "PEM private key block", pattern: regexp.MustCompile(`(?i)(-{5}BEGIN [A-Z0-9 ]*PRIVATE KEY-{5})`)},
		{family: "JWT-looking value", pattern: regexp.MustCompile(`\b(eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,})\b`)},
	}
	sensitiveModePattern = regexp.MustCompile(`(?i)\b(?:os\.)?(?:WriteFile|OpenFile|Chmod)\s*\([^)]*(0o[0-7]{3,4}|0[0-7]{3,4})`)
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
	var files []string
	var err error
	if repoLooksLikeGit(repoPath) {
		files, err = gitTrackedFiles(context.Background(), repoPath)
		if err != nil {
			return nil, fmt.Errorf("native audit git ls-files: %w", err)
		}
	} else {
		files, err = walkNativeAuditFiles(repoPath)
		if err != nil {
			return nil, err
		}
	}
	return filterNativeAuditFiles(files, include, exclude), nil
}

func walkNativeAuditFiles(repoPath string) ([]string, error) {
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
			if matchesAnyRepoGlob(rel+"/", defaultNativeExclude) || matchesAnyRepoGlob(rel+"/**", defaultNativeExclude) {
				return filepath.SkipDir
			}
			return nil
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("native audit scan: %w", err)
	}
	sort.Strings(files)
	return files, nil
}

func filterNativeAuditFiles(files, include, exclude []string) []string {
	if len(include) == 0 {
		include = []string{"**/*"}
	}
	exclude = mergeRepoGlobs(defaultNativeExclude, exclude)
	out := []string{}
	for _, file := range files {
		file = normalizeRepoPath(file)
		if file == "" {
			continue
		}
		if !matchesAnyRepoGlob(file, include) {
			continue
		}
		if matchesAnyRepoGlob(file, exclude) {
			continue
		}
		out = append(out, file)
	}
	sort.Strings(out)
	return out
}

func mergeRepoGlobs(lists ...[]string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, list := range lists {
		for _, pattern := range list {
			pattern = normalizeRepoPath(pattern)
			if pattern == "" || seen[pattern] {
				continue
			}
			seen[pattern] = true
			out = append(out, pattern)
		}
	}
	return out
}

func nativeSecretFindings(file, text string) []Finding {
	findings := []Finding{}
	for index, line := range splitLines(text) {
		lineNumber := index + 1
		signatureFindings, signatureRanges := secretSignatureFindings(file, lineNumber, line)
		findings = append(findings, signatureFindings...)
		findings = append(findings, secretAssignmentFindings(file, lineNumber, line, signatureRanges)...)
	}
	return findings
}

func secretSignatureFindings(file string, lineNumber int, line string) ([]Finding, []nativeSecretRange) {
	findings := []Finding{}
	ranges := []nativeSecretRange{}
	for _, signature := range secretSignaturePatterns {
		matches := signature.pattern.FindAllStringSubmatchIndex(line, -1)
		for _, match := range matches {
			if len(match) < 4 {
				continue
			}
			start, end := match[2], match[3]
			if start < 0 || end < start {
				start, end = match[0], match[1]
			}
			if start < 0 || end < start {
				continue
			}
			ranges = append(ranges, nativeSecretRange{start: start, end: end})
			findings = append(findings, makeFinding(Finding{
				Layer:    LayerSAST,
				Tool:     "native",
				Severity: SeverityHigh,
				File:     file,
				Line:     lineNumber,
				Column:   start + 1,
				Rule:     "native:secret",
				Category: "secret-disclosure",
				Message:  "High-confidence secret signature detected.",
				Evidence: boundedEvidence(signature.family + " redacted"),
				Tier:     FindingTierSignature,
				Gate:     FindingGateGate,
			}))
		}
	}
	return findings, ranges
}

func secretAssignmentFindings(file string, lineNumber int, line string, signatureRanges []nativeSecretRange) []Finding {
	if isGenericSecretSuppressedPath(file) {
		return nil
	}
	matches := secretAssignmentPattern.FindAllStringSubmatchIndex(line, -1)
	findings := make([]Finding, 0, len(matches))
	for _, match := range matches {
		if len(match) < 6 {
			continue
		}
		valueStart, valueEnd := match[4], match[5]
		if valueStart < 0 || valueEnd < valueStart || valueEnd > len(line) {
			continue
		}
		if overlapsAnyRange(valueStart, valueEnd, signatureRanges) {
			continue
		}
		value := line[valueStart:valueEnd]
		if shouldDropGenericSecret(line, value) {
			continue
		}
		entropy := shannonEntropy(value)
		if entropy < genericSecretEntropyFloor {
			continue
		}
		keyword := strings.ToLower(line[match[2]:match[3]])
		findings = append(findings, makeFinding(Finding{
			Layer:    LayerSAST,
			Tool:     "native",
			Severity: SeverityLow,
			File:     file,
			Line:     lineNumber,
			Column:   valueStart + 1,
			Rule:     "native:secret",
			Category: "secret-disclosure",
			Message:  "Potential high-entropy secret assignment detected.",
			Evidence: boundedEvidence(fmt.Sprintf("keyword %s assignment redacted; entropy=%.2f", keyword, entropy)),
			Tier:     FindingTierEntropy,
			Gate:     FindingGateWarning,
		}))
	}
	return findings
}

func shouldDropGenericSecret(line, value string) bool {
	if containsEnvRead(line) || containsEnvRead(value) {
		return true
	}
	return isPlaceholderValue(strings.TrimSpace(value))
}

func containsEnvRead(text string) bool {
	lower := strings.ToLower(text)
	for _, signal := range []string{
		"process.env",
		"os.getenv",
		"os.environ",
		"system.getenv",
	} {
		if strings.Contains(lower, signal) {
			return true
		}
	}
	return false
}

func isPlaceholderValue(value string) bool {
	value = strings.Trim(value, `"'`)
	return placeholderWrapped(value, "${", "}") || placeholderWrapped(value, "{{", "}}") || placeholderWrapped(value, "<", ">")
}

func placeholderWrapped(value, prefix, suffix string) bool {
	return strings.HasPrefix(value, prefix) && strings.HasSuffix(value, suffix) && len(value) > len(prefix)+len(suffix)
}

func overlapsAnyRange(start, end int, ranges []nativeSecretRange) bool {
	for _, candidate := range ranges {
		if start < candidate.end && end > candidate.start {
			return true
		}
	}
	return false
}

func shannonEntropy(value string) float64 {
	if value == "" {
		return 0
	}
	counts := map[rune]int{}
	total := 0
	for _, r := range value {
		counts[r]++
		total++
	}
	if total == 0 {
		return 0
	}
	entropy := 0.0
	for _, count := range counts {
		probability := float64(count) / float64(total)
		entropy -= probability * math.Log2(probability)
	}
	return entropy
}

func isGenericSecretSuppressedPath(file string) bool {
	lower := strings.ToLower(normalizeRepoPath(file))
	base := pathBase(lower)
	for _, suffix := range []string{".example", ".sample", ".template"} {
		if strings.HasSuffix(base, suffix) || strings.Contains(base, suffix+".") {
			return true
		}
	}
	for _, component := range strings.Split(lower, "/") {
		switch component {
		case "fixture", "fixtures", "__fixtures__", "testdata", "test-fixtures", "test_fixtures":
			return true
		}
	}
	return false
}

func nativePermissionFinding(repoPath, file string) (Finding, bool) {
	if !nativeModeBitPermissionChecksEnabled() {
		return Finding{}, false
	}
	if !isSensitivePath(file) || isSourceFileForPermissionScan(file) {
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

func nativeModeBitPermissionChecksEnabled() bool {
	// Windows reports synthesized Unix mode bits; skip until a real ACL signal is implemented.
	return runtime.GOOS != "windows"
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

func isSourceFileForPermissionScan(file string) bool {
	switch strings.ToLower(filepath.Ext(normalizeRepoPath(file))) {
	case ".go", ".js", ".jsx", ".ts", ".tsx", ".mjs", ".cjs", ".py", ".rb", ".rs", ".java", ".kt", ".cs", ".php", ".sh", ".bash", ".ps1", ".psm1", ".yml", ".yaml", ".json", ".toml":
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
