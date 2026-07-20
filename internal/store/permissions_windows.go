//go:build windows

package store

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const windowsPermissionMessage = "owner-only ACL hardening is not implemented on Windows; loopcoder creates store files in the resolved user profile or LOOPCODER_HOME location but does not enforce an owner-only DACL"

// CheckPermissions documents the Windows permission guarantee without claiming
// POSIX mode protection.
func CheckPermissions(path string) (PermissionReport, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || path == "" {
		return PermissionReport{}, fmt.Errorf("store permissions: path is required")
	}
	report := PermissionReport{
		Path:      path,
		Platform:  runtime.GOOS,
		Supported: false,
		Secure:    false,
		Message:   windowsPermissionMessage,
	}
	for _, target := range []struct {
		path string
		kind string
		dir  bool
	}{
		{path: filepath.Dir(path), kind: "data directory", dir: true},
		{path: path, kind: "database file"},
		{path: path + "-wal", kind: "sqlite wal file"},
		{path: path + "-shm", kind: "sqlite shm file"},
	} {
		item := PermissionItem{Path: target.path, Kind: target.kind, Secure: true}
		info, err := os.Lstat(target.path)
		if err != nil {
			if os.IsNotExist(err) {
				item.Message = "missing"
				report.Items = append(report.Items, item)
				continue
			}
			return report, fmt.Errorf("inspect %s %s: %w", target.kind, target.path, err)
		}
		item.Exists = true
		if info.Mode()&os.ModeSymlink != 0 {
			item.Secure = false
			item.Unsafe = true
			item.Message = "refusing symlink"
		} else if target.dir && !info.IsDir() {
			item.Secure = false
			item.Unsafe = true
			item.Message = "not a directory"
		} else if !target.dir && !info.Mode().IsRegular() {
			item.Secure = false
			item.Unsafe = true
			item.Message = "not a regular file"
		} else {
			item.Message = "acl-owner-only-not-verified"
		}
		report.Items = append(report.Items, item)
	}
	return report, nil
}

func ensurePermissionsForOpen(path string) error {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || path == "" {
		return fmt.Errorf("open store: path is required")
	}
	// Foundation store requires owner-only guarantees. Until Windows DACL
	// hardening exists, fail closed rather than opening an unverified path.
	return fmt.Errorf("open store: %s", windowsPermissionMessage)
}

func hardenSQLiteSidecars(string) error { return nil }

func withOwnerOnlyUmask(fn func() error) error { return fn() }
