package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// QuarantineResult describes a non-destructive move of a failed store.
type QuarantineResult struct {
	// OriginalPath is the path that failed open (redact for logs).
	OriginalPath string
	// QuarantinePath is where the primary db file was moved.
	QuarantinePath string
	// Sidecars lists moved -wal/-shm companions (relative basenames).
	Sidecars []string
}

// QuarantineDatabase moves path and SQLite sidecars to a timestamped quarantine
// location under destDir (or beside the original when destDir is empty).
// It never deletes or overwrites the original content without a move.
func QuarantineDatabase(path string, destDir string, now time.Time) (QuarantineResult, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "" || path == "." {
		return QuarantineResult{}, fmt.Errorf("%w: empty path", ErrQuarantined)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if _, err := os.Stat(path); err != nil {
		return QuarantineResult{}, fmt.Errorf("%w: original missing: %v", ErrQuarantined, err)
	}
	if destDir == "" {
		destDir = filepath.Join(filepath.Dir(path), "quarantine")
	}
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return QuarantineResult{}, fmt.Errorf("%w: create quarantine dir: %v", ErrQuarantined, err)
	}
	stamp := now.UTC().Format("20060102T150405.000000000Z")
	base := filepath.Base(path)
	dest := filepath.Join(destDir, base+"."+stamp)
	if err := os.Rename(path, dest); err != nil {
		// Cross-device fallback: copy then remove.
		if copyErr := copyFile(path, dest); copyErr != nil {
			return QuarantineResult{}, fmt.Errorf("%w: move: %v (copy: %v)", ErrQuarantined, err, copyErr)
		}
		_ = os.Remove(path)
	}
	res := QuarantineResult{OriginalPath: path, QuarantinePath: dest}
	for _, suffix := range []string{"-wal", "-shm", ".open"} {
		src := path + suffix
		if _, err := os.Stat(src); err != nil {
			continue
		}
		d := dest + suffix
		if err := os.Rename(src, d); err != nil {
			if copyErr := copyFile(src, d); copyErr != nil {
				continue
			}
			_ = os.Remove(src)
		}
		res.Sidecars = append(res.Sidecars, filepath.Base(d))
	}
	_ = hardenPathMode(dest, 0o600)
	return res, nil
}

func copyFile(src, dst string) error {
	in, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, in, 0o600)
}

func hardenPathMode(path string, mode os.FileMode) error {
	return os.Chmod(path, mode)
}
