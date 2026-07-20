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
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create data directory %s: %w", filepath.Dir(path), err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("inspect database file %s: %w", path, err)
		}
		file, createErr := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if createErr != nil {
			if !os.IsExist(createErr) {
				return fmt.Errorf("create database file %s: %w", path, createErr)
			}
		} else if closeErr := file.Close(); closeErr != nil {
			return fmt.Errorf("create database file %s: close: %w", path, closeErr)
		}
		return nil
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("database file %s is a symlink; refusing to open", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("database file %s is not a regular file; refusing to open", path)
	}
	return nil
}

func hardenSQLiteSidecars(string) error { return nil }

func withOwnerOnlyUmask(fn func() error) error { return fn() }
