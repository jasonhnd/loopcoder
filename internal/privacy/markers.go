package privacy

import "strings"

// Synthetic canary markers used only in tests and conformance fixtures.
// These strings must never appear in machine-global DB, default global status,
// unrelated project stores, host diagnostics, CI artifacts, or release
// manifests after redaction. Values are intentionally unique and non-secret
// for real systems; they stand in for private issue/code/prompt/path/account
// content during automated scans.
const (
	// MarkerIssue stands for private issue body / title content.
	MarkerIssue = "SYN_PRIVATE_ISSUE_TOKEN_v090067_AAAA"

	// MarkerCode stands for private source snippet content.
	MarkerCode = "SYN_PRIVATE_CODE_TOKEN_v090067_BBBB"

	// MarkerPrompt stands for provider prompt content.
	MarkerPrompt = "SYN_PRIVATE_PROMPT_TOKEN_v090067_CCCC"

	// MarkerPath stands for a private absolute path.
	MarkerPath = "/Users/syn-private/v090067/SECRET_PATH_DDDD"

	// MarkerAccount stands for a quota/account identifier.
	MarkerAccount = "SYN_PRIVATE_ACCOUNT_TOKEN_v090067_EEEE"

	// MarkerCredential stands for a credential / token value.
	MarkerCredential = "SYN_PRIVATE_CREDENTIAL_TOKEN_v090067_FFFF"

	// MarkerOutput stands for model/provider raw output.
	MarkerOutput = "SYN_PRIVATE_OUTPUT_TOKEN_v090067_GGGG"
)

// AllMarkers returns every synthetic private marker used by canaries.
func AllMarkers() []string {
	return []string{
		MarkerIssue,
		MarkerCode,
		MarkerPrompt,
		MarkerPath,
		MarkerAccount,
		MarkerCredential,
		MarkerOutput,
	}
}

// MarkerLabel maps a marker constant to a short field label for scan findings
// (never the marker value itself in failure messages for credential class).
func MarkerLabel(marker string) string {
	switch marker {
	case MarkerIssue:
		return "private_issue"
	case MarkerCode:
		return "private_code"
	case MarkerPrompt:
		return "private_prompt"
	case MarkerPath:
		return "private_path"
	case MarkerAccount:
		return "private_account"
	case MarkerCredential:
		return "private_credential"
	case MarkerOutput:
		return "private_output"
	default:
		return "private_unknown"
	}
}

// ContainsAnyMarker reports whether text still holds any synthetic marker.
func ContainsAnyMarker(text string) bool {
	for _, m := range AllMarkers() {
		if strings.Contains(text, m) {
			return true
		}
	}
	return false
}

// ContainedMarkers returns which markers appear in text (values returned only
// for test helpers that assert on the set; scanners use labels instead).
func ContainedMarkers(text string) []string {
	var found []string
	for _, m := range AllMarkers() {
		if strings.Contains(text, m) {
			found = append(found, m)
		}
	}
	return found
}
