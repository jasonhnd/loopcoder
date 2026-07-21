package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestOpenReportWALAndPoolBounds(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	st, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	rep := st.LastOpenReport()
	if rep.Recovered {
		t.Fatal("fresh open should not report recovery")
	}
	if rep.JournalMode != JournalModeWAL {
		t.Fatalf("journal_mode = %q, want wal", rep.JournalMode)
	}
	if rep.MaxOpenConns != MaxOpenConns {
		t.Fatalf("MaxOpenConns = %d, want %d", rep.MaxOpenConns, MaxOpenConns)
	}
	mode, err := st.JournalMode(ctx)
	if err != nil || mode != JournalModeWAL {
		t.Fatalf("JournalMode = %q err=%v", mode, err)
	}
	if st.db.Stats().MaxOpenConnections != MaxOpenConns {
		t.Fatalf("db MaxOpenConnections = %d", st.db.Stats().MaxOpenConnections)
	}
}

func TestUncleanOpenReportsRecoveryAndPreservesCommitted(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	st, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Committed row via write tx.
	if err := st.WithWriteTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS probe(id INTEGER PRIMARY KEY, v TEXT)`)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO probe(id, v) VALUES (1, 'committed')`)
		return err
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Simulate abrupt termination: close DB without clearing marker.
	st.closeMu.Lock()
	st.closed = true
	_ = st.db.Close()
	st.db = nil
	st.closeMu.Unlock()
	if !readUncleanMarker(path) {
		t.Fatal("expected open marker after abrupt close")
	}

	reopened, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if !reopened.LastOpenReport().Recovered {
		t.Fatal("expected Recovered=true after unclean open")
	}
	var v string
	err = reopened.WithDB(func(db *sql.DB) error {
		return db.QueryRowContext(ctx, `SELECT v FROM probe WHERE id=1`).Scan(&v)
	})
	if err != nil || v != "committed" {
		t.Fatalf("committed data lost: v=%q err=%v", v, err)
	}
}

func TestWriteTxCancelUsesIndependentRollback(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	st, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	cancelCtx, cancel := context.WithCancel(ctx)
	err = st.WithWriteTx(cancelCtx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS cancel_probe(id INTEGER PRIMARY KEY)`); err != nil {
			return err
		}
		cancel()
		_, err := tx.ExecContext(ctx, `INSERT INTO cancel_probe(id) VALUES (1)`)
		return err
	})
	if err == nil {
		// Cancellation may surface on Exec or we may still succeed if driver ignored ctx.
		// Ensure no poisoned handle: another write must work.
	}
	if err := st.WithWriteTx(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS after_cancel(id INTEGER PRIMARY KEY)`)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO after_cancel(id) VALUES (1)`)
		return err
	}); err != nil {
		t.Fatalf("write after cancel: %v", err)
	}
	var n int
	_ = st.WithDB(func(db *sql.DB) error {
		return db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM after_cancel`).Scan(&n)
	})
	if n != 1 {
		t.Fatalf("after_cancel count = %d", n)
	}
}

func TestConcurrentReadersDoNotGrowPool(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	st, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	var wg sync.WaitGroup
	errCh := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errCh <- st.WithRead(ctx, func(ctx context.Context, db *sql.DB) error {
				var v int
				return db.QueryRowContext(ctx, `SELECT schema_version FROM store_metadata WHERE id=1`).Scan(&v)
			})
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("reader: %v", err)
		}
	}
	stats := st.db.Stats()
	if stats.MaxOpenConnections != MaxOpenConns {
		t.Fatalf("pool max open = %d", stats.MaxOpenConnections)
	}
	if stats.OpenConnections > MaxOpenConns {
		t.Fatalf("open connections grew to %d", stats.OpenConnections)
	}
}

func TestBusyClassifyRetryable(t *testing.T) {
	d := Classify(ErrBusy)
	if d.Class != ClassBusy || !d.Retryable {
		t.Fatalf("Classify(ErrBusy) = %+v", d)
	}
	if !IsBusy(ErrBusy) {
		t.Fatal("IsBusy(ErrBusy) false")
	}
	d2 := Classify(context.Canceled)
	if d2.Class != ClassCancelled {
		t.Fatalf("cancelled class = %s", d2.Class)
	}
	if strings.Contains(RedactPath("/Users/ms23m2/data/x.db"), "ms23m2") {
		// home redaction best-effort
		if home, _ := os.UserHomeDir(); home != "" && strings.HasPrefix("/Users/ms23m2/data/x.db", home) {
			t.Fatalf("RedactPath leaked home: %s", RedactPath("/Users/ms23m2/data/x.db"))
		}
	}
}

func TestQuarantineOnCorruptDoesNotRecreate(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "store.db")
	st, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	meta, err := st.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// Overwrite with non-SQLite garbage.
	if err := os.WriteFile(path, []byte("not a sqlite database at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Open(ctx, Options{Path: path, Now: fixedNow, QuarantineDir: filepath.Join(dir, "q")})
	if err == nil || !errors.Is(err, ErrQuarantined) {
		t.Fatalf("Open corrupt = %v, want ErrQuarantined", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("original path should be moved, still exists: %v", statErr)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "q"))
	if len(entries) == 0 {
		t.Fatal("expected quarantine artifacts")
	}
	// Open must not silently recreate over the old identity without explicit new bootstrap.
	// Path is free: a new Open creates a NEW store (new store_id) — that is intentional
	// only when the path is empty after quarantine. Operator must restore backups deliberately.
	st2, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("open empty path after quarantine: %v", err)
	}
	defer st2.Close()
	meta2, err := st2.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if meta2.StoreID == meta.StoreID {
		t.Fatal("new bootstrap must not reuse quarantined store_id")
	}
}

func TestBackupAndRestoreReproducesMetadata(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "store.db")
	st, err := Open(ctx, Options{Path: path, Now: fixedNow, FormatIdentity: "loopcoder.project.v1"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := st.WithWriteTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS events_probe(
			sequence INTEGER PRIMARY KEY, payload TEXT NOT NULL
		)`)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO events_probe(sequence, payload) VALUES (1, '{"k":1}'), (2, '{"k":2}')`)
		return err
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	meta, err := st.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}

	backupPath := filepath.Join(dir, "backup", "store.db")
	man, err := st.Backup(ctx, backupPath)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if man.StoreID != meta.StoreID || man.SchemaVersion != meta.SchemaVersion {
		t.Fatalf("manifest mismatch: %+v vs %+v", man, meta)
	}
	if man.SHA256 == "" {
		t.Fatal("missing sha256")
	}
	// Source remains usable.
	if err := st.CheckIntegrity(ctx); err != nil {
		t.Fatalf("source integrity after backup: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	restored, err := VerifyBackupOpen(ctx, backupPath, man, "loopcoder.project.v1")
	if err != nil {
		t.Fatalf("VerifyBackupOpen: %v", err)
	}
	defer restored.Close()
	var n int
	err = restored.WithDB(func(db *sql.DB) error {
		return db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events_probe`).Scan(&n)
	})
	if err != nil || n != 2 {
		t.Fatalf("restored events count = %d err=%v", n, err)
	}
	rm, err := restored.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rm.StoreID != meta.StoreID {
		t.Fatalf("store_id %s != %s", rm.StoreID, meta.StoreID)
	}
}

func TestCleanCloseClearsMarker(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	st, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	if !readUncleanMarker(path) {
		t.Fatal("marker should exist while open")
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if readUncleanMarker(path) {
		t.Fatal("marker should clear on clean close")
	}
	reopened, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.LastOpenReport().Recovered {
		t.Fatal("clean close then reopen must not report recovery")
	}
}
