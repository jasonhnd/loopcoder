// Package relay writes local-only attestation relay ledger files.
package relay

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jasonhnd/loopcoder/internal/attestation"
)

// Entry describes one local attestation block that a conductor must be able to
// surface verbatim. Files written by this package are gitignored local state.
type Entry struct {
	RepoPath     string
	RunID        string
	InvocationID string
	Command      string
	Role         attestation.Role
	Issue        int
	PRNumber     int
	PR           string
	CreatedAt    time.Time
	Header       string
	Pretty       string
}

// Write appends one relay entry under .loopcoder/relay/<run>/<invocation>.attest.
func Write(entry Entry) (string, error) {
	repoPath := strings.TrimSpace(entry.RepoPath)
	if repoPath == "" {
		return "", fmt.Errorf("repo path is required")
	}
	runID := sanitizePathPart(entry.RunID, "no-run")
	invocationID := sanitizePathPart(entry.InvocationID, "invocation")
	if strings.TrimSpace(entry.Header) == "" {
		return "", fmt.Errorf("attestation header is required")
	}
	if strings.TrimSpace(entry.Pretty) == "" {
		return "", fmt.Errorf("attestation pretty block is required")
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now().UTC()
	}

	dir := filepath.Join(repoPath, ".loopcoder", "relay", runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create relay directory: %w", err)
	}
	path := filepath.Join(dir, invocationID+".attest")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return "", fmt.Errorf("open relay ledger: %w", err)
	}
	defer file.Close()

	if info, statErr := file.Stat(); statErr == nil && info.Size() > 0 {
		if _, err := file.WriteString("\n"); err != nil {
			return path, fmt.Errorf("append relay separator: %w", err)
		}
	}
	if _, err := file.WriteString(render(entry)); err != nil {
		return path, fmt.Errorf("write relay ledger: %w", err)
	}
	return path, nil
}

func render(entry Entry) string {
	var b strings.Builder
	b.WriteString("# loopcoder relay attestation\n")
	writeMeta(&b, "command", entry.Command)
	writeMeta(&b, "role", string(entry.Role))
	writeMeta(&b, "run_id", entry.RunID)
	if entry.Issue > 0 {
		writeMeta(&b, "issue", strconv.Itoa(entry.Issue))
	}
	if entry.PRNumber > 0 {
		writeMeta(&b, "pr_number", strconv.Itoa(entry.PRNumber))
	}
	writeMeta(&b, "pr", entry.PR)
	writeMeta(&b, "created_at", entry.CreatedAt.UTC().Format(time.RFC3339Nano))
	b.WriteString(strings.TrimRight(entry.Header, "\r\n"))
	b.WriteByte('\n')
	b.WriteString(strings.TrimRight(entry.Pretty, "\r\n"))
	b.WriteByte('\n')
	return b.String()
}

func writeMeta(b *strings.Builder, key, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	b.WriteString("# ")
	b.WriteString(key)
	b.WriteByte('=')
	b.WriteString(value)
	b.WriteByte('\n')
}

func sanitizePathPart(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		keep := unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '_' || r == '-'
		if keep {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" {
		return fallback
	}
	return out
}
