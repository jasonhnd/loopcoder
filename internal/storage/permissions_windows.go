//go:build windows

package storage

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

func preparePathForOpen(path string) error {
	dataDir := filepath.Dir(path)
	if err := os.MkdirAll(dataDir, ownerOnlyDirMode); err != nil {
		return fmt.Errorf("open storage: create data directory %s: %w", dataDir, err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("open storage: inspect SQLite database %s: %w", path, err)
		}
		file, createErr := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, ownerOnlyFileMode)
		if createErr != nil {
			return fmt.Errorf("open storage: create SQLite database %s: %w", path, createErr)
		}
		if closeErr := file.Close(); closeErr != nil {
			return fmt.Errorf("open storage: close created SQLite database %s: %w", path, closeErr)
		}
		return nil
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("open storage: SQLite database %s must not be a symlink", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("open storage: SQLite database %s is not a regular file", path)
	}
	return nil
}

func ensureSafeExistingDatabase(path string) error {
	for _, target := range []struct {
		path string
		kind string
	}{
		{path: path, kind: "SQLite database"},
		{path: path + "-wal", kind: "SQLite WAL sidecar"},
		{path: path + "-shm", kind: "SQLite SHM sidecar"},
	} {
		info, err := os.Lstat(target.path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("inspect %s %s: %w", target.kind, target.path, err)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s %s must not be a symlink", target.kind, target.path)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s %s is not a regular file", target.kind, target.path)
		}
	}
	return nil
}

func hardenSQLiteFiles(string) error {
	return nil
}

func withRestrictedFileCreation(fn func() error) error {
	return fn()
}

func checkStoragePermissions(path string, repair bool) (PermissionsReport, error) {
	path, err := cleanStoragePath(path)
	if err != nil {
		return PermissionsReport{}, err
	}
	action := "inspected"
	if repair {
		action = "unchanged"
	}
	return PermissionsReport{
		Path:        path,
		Platform:    "windows",
		OK:          false,
		Unsupported: true,
		Message:     fmt.Sprintf("%s %s; owner-only ACL hardening is not implemented on Windows in this release, so loopcoder relies on the current Windows profile or LOOPCODER_HOME directory ACLs", action, path),
	}, nil
}
