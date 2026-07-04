// Package relaygate manages local-only pending Worker/Verifier relay records.
package relaygate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	recordVersion = 1
	maxRecordSize = 256 * 1024
)

// Record is one unacknowledged local-only pretty attestation block.
type Record struct {
	Version  int    `json:"version"`
	Nonce    string `json:"nonce"`
	Role     string `json:"role"`
	PRNumber int    `json:"pr_number"`
	Block    string `json:"block"`
}

// WriteOptions describes the pending relay record to write.
type WriteOptions struct {
	RepoPath string
	RunID    string
	Role     string
	PRNumber int
	Block    string
}

// Nonce deterministically derives a pending relay nonce from run id, PR, and role.
func Nonce(runID string, prNumber int, role string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(runID) + "\x00" + strconv.Itoa(prNumber) + "\x00" + strings.TrimSpace(role)))
	return hex.EncodeToString(sum[:])
}

// Write atomically writes one pending relay record under .loopcoder/relay/pending.
func Write(opts WriteOptions) (string, error) {
	repoPath := strings.TrimSpace(opts.RepoPath)
	if repoPath == "" {
		return "", fmt.Errorf("repo path is required")
	}
	role := strings.TrimSpace(opts.Role)
	switch role {
	case "worker", "verifier":
	default:
		return "", fmt.Errorf("unsupported relay role %q", opts.Role)
	}
	if opts.PRNumber < 0 {
		return "", fmt.Errorf("PR number must be non-negative")
	}
	if strings.TrimSpace(opts.Block) == "" {
		return "", fmt.Errorf("pretty block is required")
	}

	nonce := Nonce(opts.RunID, opts.PRNumber, role)
	rec := Record{
		Version:  recordVersion,
		Nonce:    nonce,
		Role:     role,
		PRNumber: opts.PRNumber,
		Block:    ensureTrailingNewline(opts.Block),
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')

	dir := pendingDir(repoPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create pending relay directory: %w", err)
	}
	path := filepath.Join(dir, nonce+".json")
	if _, err := os.Stat(path); err == nil {
		return path, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("stat pending relay record: %w", err)
	}

	tmp, err := os.CreateTemp(dir, "."+nonce+".*.tmp")
	if err != nil {
		return "", fmt.Errorf("create pending relay temp file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("write pending relay temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close pending relay temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		if _, statErr := os.Stat(path); statErr == nil {
			return path, nil
		}
		return "", fmt.Errorf("rename pending relay record: %w", err)
	}
	cleanup = false
	return path, nil
}

// Check returns pending records. It fails open when the pending directory cannot
// be read. Individual bad records are skipped by readPending.
func Check(cwd string) []Record {
	records, err := readPending(cwd)
	if err != nil {
		return nil
	}
	return records
}

// List returns pending records for display. Read errors are fail-open.
func List(cwd string) []Record {
	return Check(cwd)
}

// Flush prints all pending blocks and acknowledges only records whose block was
// successfully written. It is intentionally fail-open on read errors so it can
// serve as the escape valve.
func Flush(cwd string, w io.Writer) error {
	records := Check(cwd)
	if len(records) == 0 {
		_, err := fmt.Fprintln(w, "no pending relays")
		return err
	}
	acknowledged := make([]Record, 0, len(records))
	for _, rec := range records {
		if _, err := io.WriteString(w, rec.Block); err != nil {
			_ = Ack(cwd, acknowledged)
			return fmt.Errorf("write pending relay %s: %w", rec.Nonce, err)
		}
		acknowledged = append(acknowledged, rec)
	}
	_ = Ack(cwd, acknowledged)
	return nil
}

// Ack clears the supplied pending records.
func Ack(cwd string, records []Record) error {
	var firstErr error
	for _, rec := range records {
		if strings.TrimSpace(rec.Nonce) == "" {
			continue
		}
		path := filepath.Join(pendingDir(cwd), rec.Nonce+".json")
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func readPending(cwd string) ([]Record, error) {
	dir := pendingDir(cwd)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	records := make([]Record, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.Size() > maxRecordSize {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var rec Record
		if err := json.Unmarshal(data, &rec); err != nil {
			continue
		}
		if rec.Version != recordVersion || strings.TrimSpace(rec.Nonce) == "" || strings.TrimSpace(rec.Role) == "" || strings.TrimSpace(rec.Block) == "" {
			continue
		}
		records = append(records, rec)
	}
	sort.SliceStable(records, func(i, j int) bool {
		if records[i].Role != records[j].Role {
			return records[i].Role < records[j].Role
		}
		if records[i].PRNumber != records[j].PRNumber {
			return records[i].PRNumber < records[j].PRNumber
		}
		return records[i].Nonce < records[j].Nonce
	})
	return records, nil
}

func pendingDir(cwd string) string {
	return filepath.Join(cwd, ".loopcoder", "relay", "pending")
}

func ensureTrailingNewline(s string) string {
	s = strings.TrimRight(s, "\r\n")
	return s + "\n"
}
