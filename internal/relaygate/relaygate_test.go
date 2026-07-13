package relaygate

import (
	"bytes"
	"errors"
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
	if records[0].Nonce != nonce || records[0].RunID != "run-test" || records[0].Role != "worker" || records[0].PRNumber != 101 {
		t.Fatalf("record = %#v, want deterministic worker PR record", records[0])
	}
	if records[0].Block != testPrettyBlock {
		t.Fatalf("block = %q, want verbatim block %q", records[0].Block, testPrettyBlock)
	}
}

func TestWriteNormalizesConductorToWorkerRelayRole(t *testing.T) {
	repo := t.TempDir()

	path, err := Write(WriteOptions{
		RepoPath: repo,
		RunID:    "run-test",
		Role:     "conductor",
		PRNumber: 101,
		Block:    "TEST RELAY BLOCK\nrole=conductor\n",
	})
	if err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	nonce := Nonce("run-test", 101, "worker")
	if filepath.Base(path) != nonce+".json" {
		t.Fatalf("path = %s, want normalized nonce filename %s.json", path, nonce)
	}

	records := Check(repo)
	if len(records) != 1 {
		t.Fatalf("Check returned %d records, want 1", len(records))
	}
	if records[0].Role != "worker" || records[0].Nonce != nonce {
		t.Fatalf("record = %#v, want normalized worker relay record", records[0])
	}
	if !strings.Contains(records[0].Block, "role=conductor") {
		t.Fatalf("block = %q, want original conductor block preserved", records[0].Block)
	}
}

func TestWriteRejectsUnknownRelayRole(t *testing.T) {
	_, err := Write(WriteOptions{
		RepoPath: t.TempDir(),
		RunID:    "run-test",
		Role:     "publisher",
		PRNumber: 101,
		Block:    testPrettyBlock,
	})
	if err == nil || !strings.Contains(err.Error(), `unsupported relay role "publisher"`) {
		t.Fatalf("Write error = %v, want unsupported role", err)
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

func TestCheckSkipsBadFilesWithoutBypassingValidRecords(t *testing.T) {
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
	dir := filepath.Dir(path)
	if err := os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{not-json"), 0o600); err != nil {
		t.Fatalf("write corrupt pending: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "oversized.json"), bytes.Repeat([]byte("x"), maxRecordSize+1), 0o600); err != nil {
		t.Fatalf("write oversized pending: %v", err)
	}

	records := Check(repo)
	if len(records) != 1 {
		t.Fatalf("Check returned %d records, want valid record only", len(records))
	}
	if records[0].Nonce != Nonce("run-test", 101, "worker") {
		t.Fatalf("Check returned record %#v, want valid pending record", records[0])
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
	if err := Flush(repo, &stdout); err != nil {
		t.Fatalf("Flush returned error: %v", err)
	}
	if stdout.String() != testPrettyBlock {
		t.Fatalf("Flush stdout = %q, want %q", stdout.String(), testPrettyBlock)
	}
	if records := Check(repo); len(records) != 0 {
		t.Fatalf("Check after Flush returned %d records, want 0", len(records))
	}
}

func TestFlushReturnsAckErrorAndLeavesPendingRecord(t *testing.T) {
	repo := t.TempDir()
	if _, err := Write(WriteOptions{
		RepoPath: repo,
		RunID:    "run-test",
		Role:     "worker",
		PRNumber: 101,
		Block:    testPrettyBlock,
	}); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}

	removeErr := errors.New("remove denied")
	oldRemove := removePendingRecord
	removePendingRecord = func(string) error { return removeErr }
	t.Cleanup(func() { removePendingRecord = oldRemove })

	var stdout bytes.Buffer
	err := Flush(repo, &stdout)
	if err == nil {
		t.Fatal("Flush returned nil error, want Ack error")
	}
	if !strings.Contains(err.Error(), "acknowledge pending relays") || !strings.Contains(err.Error(), removeErr.Error()) {
		t.Fatalf("Flush error = %q, want acknowledgement failure", err.Error())
	}
	if stdout.String() != testPrettyBlock {
		t.Fatalf("Flush stdout = %q, want surfaced block %q", stdout.String(), testPrettyBlock)
	}
	if records := Check(repo); len(records) != 1 {
		t.Fatalf("Check after failed Ack returned %d records, want record still pending", len(records))
	}
}

func TestFlushEmptyPrintsConfirmation(t *testing.T) {
	repo := t.TempDir()

	var stdout bytes.Buffer
	if err := Flush(repo, &stdout); err != nil {
		t.Fatalf("Flush returned error: %v", err)
	}
	if stdout.String() != "no pending relays\n" {
		t.Fatalf("Flush stdout = %q, want no-pending confirmation", stdout.String())
	}
}

func TestFlushWriteErrorDoesNotAckUnsurfacedRecord(t *testing.T) {
	repo := t.TempDir()
	if _, err := Write(WriteOptions{
		RepoPath: repo,
		RunID:    "run-test",
		Role:     "worker",
		PRNumber: 101,
		Block:    testPrettyBlock,
	}); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}

	writer := failingWriter{err: errors.New("stdout closed")}
	if err := Flush(repo, writer); err == nil {
		t.Fatal("Flush returned nil error, want write error")
	}
	if records := Check(repo); len(records) != 1 {
		t.Fatalf("Check after failed Flush returned %d records, want unsurfaced record pending", len(records))
	}
}

func TestFlushWriteErrorSurfacesAckErrorForAlreadySurfacedRecords(t *testing.T) {
	repo := t.TempDir()
	for _, pr := range []int{101, 102} {
		if _, err := Write(WriteOptions{
			RepoPath: repo,
			RunID:    "run-test",
			Role:     "worker",
			PRNumber: pr,
			Block:    testPrettyBlock,
		}); err != nil {
			t.Fatalf("Write returned error: %v", err)
		}
	}

	removeErr := errors.New("remove denied")
	oldRemove := removePendingRecord
	removePendingRecord = func(string) error { return removeErr }
	t.Cleanup(func() { removePendingRecord = oldRemove })

	writer := failOnWriteN{failAt: 2, err: errors.New("stdout closed")}
	err := Flush(repo, &writer)
	if err == nil {
		t.Fatal("Flush returned nil error, want write and Ack errors")
	}
	for _, want := range []string{"write pending relay", "stdout closed", "acknowledge surfaced pending relays", "remove denied"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Flush error missing %q: %v", want, err)
		}
	}
	if records := Check(repo); len(records) != 2 {
		t.Fatalf("Check after failed Flush returned %d records, want both records still pending", len(records))
	}
}

type failingWriter struct {
	err error
}

func (w failingWriter) Write([]byte) (int, error) {
	return 0, w.err
}

type failOnWriteN struct {
	writes int
	failAt int
	err    error
}

func (w *failOnWriteN) Write(p []byte) (int, error) {
	w.writes++
	if w.writes == w.failAt {
		return 0, w.err
	}
	return len(p), nil
}
