package artifactqual

import (
	"fmt"
	"strings"
)

// BinaryIdentity is the version/commit pair parsed from exact binary --version output.
type BinaryIdentity struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

// ParseBinaryIdentity parses a single --version line into Version and Commit.
// Requires exactly one nonempty version= token and exactly one commit= token.
// Rejects version=dev, commit=unknown, and non-40-hex commit. Errors never include raw output.
func ParseBinaryIdentity(output string) (BinaryIdentity, error) {
	var zero BinaryIdentity
	tokens := strings.Fields(output)
	var versions, commits []string
	for _, tok := range tokens {
		switch {
		case strings.HasPrefix(tok, "version="):
			versions = append(versions, strings.TrimPrefix(tok, "version="))
		case strings.HasPrefix(tok, "commit="):
			commits = append(commits, strings.TrimPrefix(tok, "commit="))
		}
	}
	if len(versions) == 0 {
		return zero, fmt.Errorf("artifactqual: binary version field missing")
	}
	if len(versions) > 1 {
		return zero, fmt.Errorf("artifactqual: binary version field duplicate")
	}
	if len(commits) == 0 {
		return zero, fmt.Errorf("artifactqual: binary commit field missing")
	}
	if len(commits) > 1 {
		return zero, fmt.Errorf("artifactqual: binary commit field duplicate")
	}
	ver := strings.TrimSpace(versions[0])
	if ver == "" {
		return zero, fmt.Errorf("artifactqual: binary version field empty")
	}
	if ver == "dev" {
		return zero, fmt.Errorf("artifactqual: binary version is dev")
	}
	commit := strings.ToLower(strings.TrimSpace(commits[0]))
	if commit == "" {
		return zero, fmt.Errorf("artifactqual: binary commit field empty")
	}
	if commit == "unknown" {
		return zero, fmt.Errorf("artifactqual: binary commit is unknown")
	}
	if !isExact40Hex(commit) {
		return zero, fmt.Errorf("artifactqual: binary commit not 40 hex")
	}
	return BinaryIdentity{Version: ver, Commit: commit}, nil
}

// ValidateBinaryIdentity parses --version output and requires commit EqualFold expectedSHA.
// expectedSHA must itself be exactly 40 hex. Errors are stable/sanitized (no raw output).
func ValidateBinaryIdentity(output, expectedSHA string) (BinaryIdentity, error) {
	var zero BinaryIdentity
	want := strings.ToLower(strings.TrimSpace(expectedSHA))
	if !isExact40Hex(want) {
		return zero, fmt.Errorf("artifactqual: expected sha not 40 hex")
	}
	id, err := ParseBinaryIdentity(output)
	if err != nil {
		return zero, err
	}
	if !strings.EqualFold(id.Commit, want) {
		return zero, fmt.Errorf("artifactqual: binary commit mismatch expected sha")
	}
	return id, nil
}

func isExact40Hex(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}
