package relaygate

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testPrettyBlock = "TEST RELAY BLOCK\nrole=worker\n"

func TestWriteUsesDeterministicNonceAndCheckReturnsPending(t *testing.T) {
	repo := t.TempDir()

	path, err := Write(WriteOptions{
		RepoPath: repo,
		RunID:    "run-test",
		Role:     "worker",
		PRNumber: 101,
		Block:    testPrettyBlock,
	})
	if err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	nonce := Nonce("run-test", 101, "worker")
	if filepath.Base(path) != nonce+".json" {
		t.Fatalf("path = %s, want nonce filename %s.json", path, nonce)
	}

	records := Check(repo)
	if len(records) != 1 {
		t.Fatalf("Check returned %d records, want 1", len(records))
	}
	if records[0].Nonce != nonce || records[0].Role != "worker" || records[0].PRNumber != 101 {
		t.Fatalf("record = %#v, want deterministic worker PR record", records[0])
	}
	if records[0].Block != testPrettyBlock {
		t.Fatalf("block = %q, want verbatim block %q", records[0].Block, testPrettyBlock)
	}
}

func TestCheckFailsOpenOnMissingOrCorruptState(t *testing.T) {
	repo := t.TempDir()
	if records := Check(repo); len(records) != 0 {
		t.Fatalf("missing state Check returned %d records, want 0", len(records))
	}

	dir := filepath.Join(repo, ".loopcoder", "relay", "pending")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir pending: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{not-json"), 0o600); err != nil {
		t.Fatalf("write corrupt pending: %v", err)
	}
	if records := Check(repo); len(records) != 0 {
		t.Fatalf("corrupt state Check returned %d records, want fail-open 0", len(records))
	}
}

func TestFlushPrintsVerbatimAndClears(t *testing.T) {
	repo := t.TempDir()
	if _, err := Write(WriteOptions{
		RepoPath: repo,
		RunID:    "run-test",
		Role:     "worker",
		PRNumber: 101,
		Block:    strings.TrimRight(testPrettyBlock, "\n"),
	}); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}

	var stdout bytes.Buffer
	Flush(repo, &stdout)
	if stdout.String() != testPrettyBlock {
		t.Fatalf("Flush stdout = %q, want %q", stdout.String(), testPrettyBlock)
	}
	if records := Check(repo); len(records) != 0 {
		t.Fatalf("Check after Flush returned %d records, want 0", len(records))
	}
}
