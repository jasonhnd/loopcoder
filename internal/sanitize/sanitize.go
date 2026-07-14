package sanitize

import (
	"regexp"
	"strings"
	"unicode"
)

const (
	RedactedSecret = "[REDACTED_SECRET]"
	RedactedToken  = "[REDACTED_TOKEN]"
	RedactedAPIKey = "[REDACTED_API_KEY]"
	RedactedGitHub = "[REDACTED_GITHUB_TOKEN]"
	RedactedAWS    = "[REDACTED_AWS_ACCESS_KEY]"
	RedactedPath   = "[REDACTED_PATH]"
)

var (
	githubTokenPattern  = regexp.MustCompile(`(?i)(ghp|github_pat|gho|ghu|ghs|ghr)_[A-Za-z0-9_]+`)
	awsAccessKeyPattern = regexp.MustCompile(`AKIA[0-9A-Z]{16}`)
	apiKeyPattern       = regexp.MustCompile(`(?i)(sk-ant-|sk-|xox[baprs]-)[A-Za-z0-9._~+/=-]{12,}`)
	bearerPattern       = regexp.MustCompile(`(?i)(Bearer\s+)[A-Za-z0-9._~+/=-]{3,}`)
	assignmentPattern   = regexp.MustCompile(`(?i)\b((?:callback[_-]?token|token|password|secret|api[_-]?key|credential)\s*[=:]\s*["']?)([^\s"',}]{3,})["']?`)
	unixPathPattern     = regexp.MustCompile(`(^|[\s"'=:,(])(/(?:Users|var|private/var|tmp|home|opt|Volumes)/[^\s"',)]+)`)
	windowsPathPattern  = regexp.MustCompile(`(?i)(^|[\s"'=:,(])([A-Z]:\\[^\s"',)]+)`)
)

// Text redacts secrets, local paths, and control characters before text is
// persisted or rendered.
func Text(value string) string {
	value = strings.ToValidUTF8(value, "")
	value = stripControls(value)
	value = githubTokenPattern.ReplaceAllString(value, RedactedGitHub)
	value = awsAccessKeyPattern.ReplaceAllString(value, RedactedAWS)
	value = apiKeyPattern.ReplaceAllString(value, RedactedAPIKey)
	value = bearerPattern.ReplaceAllString(value, "${1}"+RedactedToken)
	value = assignmentPattern.ReplaceAllString(value, "${1}"+RedactedSecret)
	value = unixPathPattern.ReplaceAllString(value, "${1}"+RedactedPath)
	value = windowsPathPattern.ReplaceAllString(value, "${1}"+RedactedPath)
	return value
}

func stripControls(value string) string {
	var b strings.Builder
	changed := false
	for _, r := range value {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			changed = true
			continue
		}
		b.WriteRune(r)
	}
	if !changed {
		return value
	}
	return b.String()
}
