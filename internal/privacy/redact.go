package privacy

import (
	"path"
	"path/filepath"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/sanitize"
)

// Redaction tokens written in place of private content. Distinct from
// sanitize package tokens so privacy conformance can assert class-level
// redaction without coupling to credential-shape regexes alone.
const (
	RedactedPrivateIssue      = "[REDACTED_PRIVATE_ISSUE]"
	RedactedPrivateCode       = "[REDACTED_PRIVATE_CODE]"
	RedactedPrivatePrompt     = "[REDACTED_PRIVATE_PROMPT]"
	RedactedPrivatePath       = "[REDACTED_PRIVATE_PATH]"
	RedactedPrivateAccount    = "[REDACTED_PRIVATE_ACCOUNT]"
	RedactedPrivateCredential = "[REDACTED_PRIVATE_CREDENTIAL]"
	RedactedPrivateOutput     = "[REDACTED_PRIVATE_OUTPUT]"
)

// RedactFor applies destination policy to text:
//  1. Always run credential/path sanitize first.
//  2. Strip every synthetic private marker (class code/prompt/output/account).
//  3. For global/host/CI/release/unrelated destinations, also strip any residual
//     long absolute-looking path segments that may have slipped past sanitize.
func RedactFor(dest Destination, text string) string {
	if text == "" {
		return text
	}
	// Credentials and common secret shapes first.
	out := sanitize.Text(text)
	// Synthetic markers always removed regardless of destination: even
	// project-local logs must not retain raw canary credential markers.
	out = strings.ReplaceAll(out, MarkerCredential, RedactedPrivateCredential)
	out = strings.ReplaceAll(out, MarkerIssue, RedactedPrivateIssue)
	out = strings.ReplaceAll(out, MarkerCode, RedactedPrivateCode)
	out = strings.ReplaceAll(out, MarkerPrompt, RedactedPrivatePrompt)
	out = strings.ReplaceAll(out, MarkerPath, RedactedPrivatePath)
	out = strings.ReplaceAll(out, MarkerAccount, RedactedPrivateAccount)
	out = strings.ReplaceAll(out, MarkerOutput, RedactedPrivateOutput)

	// Global / cross-project / host / CI / release surfaces: public identity only.
	if !Allowed(ClassCodePromptOutput, dest) {
		// Extra belt: collapse remaining absolute path-like tokens.
		out = collapseAbsolutePaths(out)
	}
	return out
}

// RedactField redacts a single named field for a destination. Empty fields stay empty.
func RedactField(dest Destination, field, value string) string {
	_ = field // field name reserved for future field-policy tables
	return RedactFor(dest, value)
}

// PathBasename returns only the final path element for machine-summary style
// public facts. Empty or "." paths become empty.
func PathBasename(full string) string {
	full = strings.TrimSpace(full)
	if full == "" {
		return ""
	}
	base := filepath.Base(filepath.Clean(full))
	if base == "." || base == string(filepath.Separator) {
		base = path.Base(full)
	}
	if base == "." || base == "/" {
		return ""
	}
	return base
}

// PublicProjectFact is the only project-shaped content allowed on machine-global
// and host diagnostic surfaces.
type PublicProjectFact struct {
	ProjectID    string `json:"project_id"`
	ShortName    string `json:"short_name"`
	Owner        string `json:"owner"`
	PathBasename string `json:"path_basename"`
	// Never present: issue text, full paths, prompts, outputs, credentials.
}

// ToPublicFact converts raw project identity into a public fact, redacting path.
func ToPublicFact(projectID, shortName, owner, localPath string) PublicProjectFact {
	return PublicProjectFact{
		ProjectID:    strings.TrimSpace(projectID),
		ShortName:    strings.TrimSpace(shortName),
		Owner:        strings.TrimSpace(owner),
		PathBasename: PathBasename(localPath),
	}
}

func collapseAbsolutePaths(text string) string {
	// After sanitize.Text, most paths are already replaced. This is a
	// conservative second pass for synthetic-style absolute segments that
	// do not match the sanitize unix path regex prefix list.
	const marker = RedactedPrivatePath
	var b strings.Builder
	b.Grow(len(text))
	i := 0
	for i < len(text) {
		if text[i] == '/' && i+1 < len(text) && isPathStart(text, i) {
			// consume until whitespace or common delimiters
			j := i + 1
			for j < len(text) && !isPathEnd(text[j]) {
				j++
			}
			// only rewrite if looks like multi-segment absolute path
			seg := text[i:j]
			if strings.Count(seg, "/") >= 2 && len(seg) > 8 {
				b.WriteString(marker)
				i = j
				continue
			}
		}
		b.WriteByte(text[i])
		i++
	}
	return b.String()
}

func isPathStart(text string, i int) bool {
	if i == 0 {
		return true
	}
	prev := text[i-1]
	switch prev {
	case ' ', '\t', '\n', '"', '\'', '=', ':', ',', '(', '{', '[':
		return true
	default:
		return false
	}
}

func isPathEnd(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '"', '\'', ',', ')', '}', ']', ';':
		return true
	default:
		return false
	}
}
