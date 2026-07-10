package storage

import (
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
)

const (
	ownerOnlyDirMode  fs.FileMode = 0o700
	ownerOnlyFileMode fs.FileMode = 0o600
)

// PermissionsReport describes the local filesystem protection for SQLite state.
type PermissionsReport struct {
	Path        string
	Platform    string
	OK          bool
	Unsupported bool
	Changed     bool
	Repairable  bool
	Unsafe      bool
	Message     string
	Issues      []PermissionIssue
	Repairs     []PermissionRepair
}

// PermissionIssue is a single read-only diagnostic for a storage path.
type PermissionIssue struct {
	Path    string
	Kind    string
	Mode    fs.FileMode
	Message string
}

// PermissionRepair records a chmod-style permission tightening.
type PermissionRepair struct {
	Path   string
	Kind   string
	Before fs.FileMode
	After  fs.FileMode
}

// CheckPermissions inspects storage modes without mutating the filesystem.
func CheckPermissions(path string) (PermissionsReport, error) {
	return checkStoragePermissions(path, false)
}

// RepairPermissions tightens storage modes in place where the platform supports it.
func RepairPermissions(path string) (PermissionsReport, error) {
	return checkStoragePermissions(path, true)
}

func cleanStoragePath(path string) (string, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "" || path == "." {
		return "", errors.New("storage permissions: path is required")
	}
	return path, nil
}
