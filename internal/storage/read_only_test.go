package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenReadOnlyDoesNotCreateMissingDatabaseOrParent(t *testing.T) {
	ctx := context.Background()
	parent := filepath.Join(t.TempDir(), "missing")
	path := filepath.Join(parent, "loopcoder.db")

	store, err := OpenReadOnly(ctx, Options{Path: path, Now: fixedNow})
	if store != nil {
		_ = store.Close()
		t.Fatal("OpenReadOnly returned a store for a missing database")
	}
	if err == nil || !strings.Contains(err.Error(), "database does not exist") {
		t.Fatalf("OpenReadOnly error = %v, want missing database failure", err)
	}
	if _, statErr := os.Lstat(parent); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("missing parent stat error = %v, want no path creation", statErr)
	}
}

func TestOpenReadOnlyDoesNotInitializeAnEmptyDatabase(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("Secure fixture directory: %v", err)
	}
	path := filepath.Join(dir, "loopcoder.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("Create empty database fixture: %v", err)
	}

	store, err := OpenReadOnly(ctx, Options{Path: path, Now: fixedNow})
	if store != nil {
		_ = store.Close()
		t.Fatal("OpenReadOnly returned a store for an uninitialized database")
	}
	if err == nil || !strings.Contains(err.Error(), "schema version 0 is older than required") {
		t.Fatalf("OpenReadOnly error = %v, want uninitialized schema failure", err)
	}
	info, statErr := os.Lstat(path)
	if statErr != nil {
		t.Fatalf("Stat empty database after rejected open: %v", statErr)
	}
	if info.Size() != 0 {
		t.Fatalf("empty database size after rejected open = %d, want 0", info.Size())
	}
	if entries := directoryEntryNames(t, filepath.Dir(path)); len(entries) != 1 || entries[0] != filepath.Base(path) {
		t.Fatalf("directory entries after rejected empty open = %v, want only database", entries)
	}
}

func TestOpenReadOnlyQueriesCurrentSchemaAndRejectsEveryStoreWrite(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	writable, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("Open writable seed: %v", err)
	}
	if err := writable.Close(); err != nil {
		t.Fatalf("Close writable seed: %v", err)
	}
	beforeEntries := directoryEntryNames(t, filepath.Dir(path))
	beforeInfo, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Stat writable seed: %v", err)
	}

	readOnly, err := OpenReadOnly(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	if readOnly.Path() != path || !readOnly.Now().Equal(fixedNow()) {
		t.Fatalf("read-only store identity = (%q, %s)", readOnly.Path(), readOnly.Now())
	}
	if err := readOnly.WithTx(ctx, func(tx Tx) error {
		var version, queryOnly int
		if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(version), 0) FROM migrations`).Scan(&version); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `PRAGMA query_only`).Scan(&queryOnly); err != nil {
			return err
		}
		if version != CurrentSchemaVersion || queryOnly != 1 {
			t.Fatalf("read-only connection = schema %d query_only %d, want %d and 1", version, queryOnly, CurrentSchemaVersion)
		}
		return nil
	}); err != nil {
		t.Fatalf("read-only query transaction: %v", err)
	}

	writeCallbackCalled := false
	err = readOnly.WithWriteTx(ctx, func(Tx) error {
		writeCallbackCalled = true
		return nil
	})
	if !errors.Is(err, ErrReadOnlyStore) || writeCallbackCalled {
		t.Fatalf("WithWriteTx = (called=%t, err=%v), want fail-closed read-only error", writeCallbackCalled, err)
	}
	err = readOnly.WithTx(ctx, func(tx Tx) error {
		_, execErr := tx.Exec(ctx, `DELETE FROM migrations`)
		return execErr
	})
	if !errors.Is(err, ErrReadOnlyStore) {
		t.Fatalf("read-only Tx.Exec error = %v, want ErrReadOnlyStore", err)
	}
	health, err := readOnly.Health(ctx)
	if err != nil || !health.OK || health.SchemaVersion != CurrentSchemaVersion {
		t.Fatalf("read-only Health = (%#v, %v), want current healthy schema", health, err)
	}
	if err := readOnly.Close(); err != nil {
		t.Fatalf("Close read-only store: %v", err)
	}
	afterEntries := directoryEntryNames(t, filepath.Dir(path))
	afterInfo, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Stat database after read-only open: %v", err)
	}
	if strings.Join(afterEntries, "\x00") != strings.Join(beforeEntries, "\x00") {
		t.Fatalf("directory entries changed after read-only open: before=%v after=%v", beforeEntries, afterEntries)
	}
	if afterInfo.Mode() != beforeInfo.Mode() || !afterInfo.ModTime().Equal(beforeInfo.ModTime()) || afterInfo.Size() != beforeInfo.Size() {
		t.Fatalf("database metadata changed after read-only open: before=%#v after=%#v", beforeInfo, afterInfo)
	}
}

func TestOpenReadOnlyRejectsOldNewerAndIncompleteSchemasWithoutMigration(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(context.Context, Store) error
		wantMessage string
		wantVersion int
	}{
		{
			name: "old",
			mutate: func(ctx context.Context, store Store) error {
				return store.WithWriteTx(ctx, func(tx Tx) error {
					_, err := tx.Exec(ctx, `DELETE FROM migrations WHERE version = ?`, CurrentSchemaVersion)
					return err
				})
			},
			wantMessage: "older than required",
			wantVersion: CurrentSchemaVersion - 1,
		},
		{
			name: "newer",
			mutate: func(ctx context.Context, store Store) error {
				return store.WithWriteTx(ctx, func(tx Tx) error {
					_, err := tx.Exec(ctx, `INSERT INTO migrations(version, name, applied_at) VALUES (?, ?, ?)`,
						CurrentSchemaVersion+1, "future schema", fixedNow().Format(time.RFC3339Nano))
					return err
				})
			},
			wantMessage: "unsupported storage schema version",
			wantVersion: CurrentSchemaVersion + 1,
		},
		{
			name: "missing required table",
			mutate: func(ctx context.Context, store Store) error {
				return store.WithWriteTx(ctx, func(tx Tx) error {
					_, err := tx.Exec(ctx, `DROP TABLE child_execution_requests`)
					return err
				})
			},
			wantMessage: "missing required table child_execution_requests",
			wantVersion: CurrentSchemaVersion,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "loopcoder.db")
			writable, err := Open(ctx, Options{Path: path, Now: fixedNow})
			if err != nil {
				t.Fatalf("Open writable seed: %v", err)
			}
			if err := tt.mutate(ctx, writable); err != nil {
				t.Fatalf("Mutate schema fixture: %v", err)
			}
			if err := writable.Close(); err != nil {
				t.Fatalf("Close writable seed: %v", err)
			}

			readOnly, err := OpenReadOnly(ctx, Options{Path: path, Now: fixedNow})
			if readOnly != nil {
				_ = readOnly.Close()
				t.Fatal("OpenReadOnly returned a store for an invalid schema")
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantMessage) {
				t.Fatalf("OpenReadOnly error = %v, want %q", err, tt.wantMessage)
			}

			db, openErr := openReadOnlySQLite(ctx, path)
			if openErr != nil {
				t.Fatalf("Reopen fixture without migration: %v", openErr)
			}
			gotVersion, versionErr := schemaVersion(ctx, db)
			_ = db.Close()
			if versionErr != nil || gotVersion != tt.wantVersion {
				t.Fatalf("schema after rejected open = (%d, %v), want unchanged version %d", gotVersion, versionErr, tt.wantVersion)
			}
		})
	}
}

func directoryEntryNames(t *testing.T, path string) []string {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatalf("Read directory %s: %v", path, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}
