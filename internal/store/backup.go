package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// BackupManifest describes a consistent online backup of a compact store.
type BackupManifest struct {
	SourcePath     string
	BackupPath     string
	SHA256         string
	StoreID        string
	FormatIdentity string
	SchemaVersion  int
	CreatedAt      time.Time
	// JournalMode of the source at backup time.
	JournalMode string
}

// Backup creates a consistent snapshot at destPath using VACUUM INTO.
// The destination must not already exist. Permissions are owner-only (0600).
func (s *Store) Backup(ctx context.Context, destPath string) (BackupManifest, error) {
	db, src, err := s.openHandle()
	if err != nil {
		return BackupManifest{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	destPath = filepath.Clean(strings.TrimSpace(destPath))
	if destPath == "" || destPath == "." {
		return BackupManifest{}, fmt.Errorf("backup: destination path required")
	}
	if _, err := os.Stat(destPath); err == nil {
		return BackupManifest{}, fmt.Errorf("backup: destination already exists: %s", RedactPath(destPath))
	} else if !os.IsNotExist(err) {
		return BackupManifest{}, fmt.Errorf("backup: stat destination: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o700); err != nil {
		return BackupManifest{}, fmt.Errorf("backup: create parent: %w", err)
	}

	// Checkpoint WAL so VACUUM INTO sees committed pages.
	if _, err := db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return BackupManifest{}, fmt.Errorf("backup: wal checkpoint: %w", err)
	}

	// Escape single quotes for SQLite string literal.
	escaped := strings.ReplaceAll(destPath, "'", "''")
	if _, err := db.ExecContext(ctx, "VACUUM INTO '"+escaped+"'"); err != nil {
		_ = os.Remove(destPath)
		return BackupManifest{}, fmt.Errorf("backup: vacuum into: %w", err)
	}
	if err := os.Chmod(destPath, 0o600); err != nil {
		return BackupManifest{}, fmt.Errorf("backup: chmod: %w", err)
	}

	sum, err := fileSHA256(destPath)
	if err != nil {
		return BackupManifest{}, err
	}
	meta, err := s.Metadata(ctx)
	if err != nil {
		return BackupManifest{}, err
	}
	var journal string
	_ = db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journal)

	return BackupManifest{
		SourcePath:     src,
		BackupPath:     destPath,
		SHA256:         sum,
		StoreID:        meta.StoreID,
		FormatIdentity: meta.FormatIdentity,
		SchemaVersion:  meta.SchemaVersion,
		CreatedAt:      s.now().UTC(),
		JournalMode:    strings.ToLower(journal),
	}, nil
}

// VerifyBackupOpen opens destPath with the same format identity and checks
// store_id / schema match the expected manifest. SHA-256 is compared before
// open (open may create WAL/sidecars). Caller must Close the returned store.
func VerifyBackupOpen(ctx context.Context, destPath string, expected BackupManifest, formatIdentity string) (*Store, error) {
	if expected.SHA256 != "" {
		sum, err := fileSHA256(destPath)
		if err != nil {
			return nil, err
		}
		if sum != expected.SHA256 {
			return nil, fmt.Errorf("verify backup: sha256 mismatch before open")
		}
	}
	opts := Options{
		Path:           destPath,
		FormatIdentity: formatIdentity,
	}
	if formatIdentity == "" {
		opts.FormatIdentity = expected.FormatIdentity
	}
	st, err := Open(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("verify backup open: %w", err)
	}
	meta, err := st.Metadata(ctx)
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	if expected.StoreID != "" && meta.StoreID != expected.StoreID {
		_ = st.Close()
		return nil, fmt.Errorf("verify backup: store_id mismatch")
	}
	if expected.SchemaVersion != 0 && meta.SchemaVersion != expected.SchemaVersion {
		_ = st.Close()
		return nil, fmt.Errorf("verify backup: schema version mismatch")
	}
	if expected.FormatIdentity != "" && meta.FormatIdentity != expected.FormatIdentity {
		_ = st.Close()
		return nil, fmt.Errorf("verify backup: format identity mismatch")
	}
	return st, nil
}

func fileSHA256(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("backup: hash: %w", err)
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
