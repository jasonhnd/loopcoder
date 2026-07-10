//go:build !windows

package storage

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

type permissionTarget struct {
	path        string
	kind        string
	wantRegular bool
	wantDir     bool
	maxMode     fs.FileMode
}

func sqlitePermissionTargets(path string) []permissionTarget {
	dataDir := filepath.Dir(path)
	targets := []permissionTarget{}
	if filepath.Base(dataDir) == "data" {
		targets = append(targets, permissionTarget{
			path:    filepath.Dir(dataDir),
			kind:    "loopcoder home directory",
			wantDir: true,
			maxMode: ownerOnlyDirMode,
		})
	}
	targets = append(targets,
		permissionTarget{
			path:    dataDir,
			kind:    "storage data directory",
			wantDir: true,
			maxMode: ownerOnlyDirMode,
		},
		permissionTarget{
			path:        path,
			kind:        "SQLite database",
			wantRegular: true,
			maxMode:     ownerOnlyFileMode,
		},
		permissionTarget{
			path:        path + "-wal",
			kind:        "SQLite WAL sidecar",
			wantRegular: true,
			maxMode:     ownerOnlyFileMode,
		},
		permissionTarget{
			path:        path + "-shm",
			kind:        "SQLite SHM sidecar",
			wantRegular: true,
			maxMode:     ownerOnlyFileMode,
		},
	)
	return targets
}

func preparePathForOpen(path string) error {
	dataDir := filepath.Dir(path)
	if filepath.Base(dataDir) == "data" {
		if err := ensureStorageDir(filepath.Dir(dataDir), "loopcoder home directory"); err != nil {
			return err
		}
	}
	if err := ensureStorageDir(dataDir, "storage data directory"); err != nil {
		return err
	}
	if err := ensureDatabaseFile(path); err != nil {
		return err
	}
	if err := hardenSQLiteFiles(path); err != nil {
		return err
	}
	return nil
}

func modeSummary(mode fs.FileMode) string {
	return fmt.Sprintf("%04o", mode.Perm())
}

func tightenedMode(mode fs.FileMode) fs.FileMode {
	return mode &^ 0o077
}

func ensureStorageDir(path, kind string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("open storage: inspect %s %s: %w", kind, path, err)
		}
		if err := os.MkdirAll(path, ownerOnlyDirMode); err != nil {
			return fmt.Errorf("open storage: create %s %s: %w", kind, path, err)
		}
		info, err = os.Lstat(path)
		if err != nil {
			return fmt.Errorf("open storage: inspect created %s %s: %w", kind, path, err)
		}
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("open storage: %s %s must not be a symlink", kind, path)
	}
	if !info.IsDir() {
		return fmt.Errorf("open storage: %s %s is not a directory", kind, path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		next := tightenedMode(info.Mode())
		if err := os.Chmod(path, next.Perm()); err != nil {
			return fmt.Errorf("open storage: tighten %s %s from %s to %s: %w", kind, path, modeSummary(info.Mode()), modeSummary(next), err)
		}
	}
	return nil
}

func ensureDatabaseFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
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
	if info.Mode().Perm()&0o077 != 0 {
		next := tightenedMode(info.Mode())
		if err := os.Chmod(path, next.Perm()); err != nil {
			return fmt.Errorf("open storage: tighten SQLite database %s from %s to %s: %w", path, modeSummary(info.Mode()), modeSummary(next), err)
		}
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
			if errors.Is(err, fs.ErrNotExist) {
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

func hardenSQLiteFiles(path string) error {
	report, err := RepairPermissions(path)
	if err != nil {
		return fmt.Errorf("open storage: repair storage permissions for %s: %w", path, err)
	}
	if report.Unsafe {
		return fmt.Errorf("open storage: unsafe storage path for %s: %s", path, report.Message)
	}
	return nil
}

func withRestrictedFileCreation(fn func() error) error {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	return fn()
}

func checkStoragePermissions(path string, repair bool) (PermissionsReport, error) {
	path, err := cleanStoragePath(path)
	if err != nil {
		return PermissionsReport{}, err
	}
	report := PermissionsReport{
		Path:     path,
		Platform: "unix",
		OK:       true,
	}
	for _, target := range sqlitePermissionTargets(path) {
		changed, unsafe, err := inspectPermissionTarget(&report, target, repair)
		if err != nil {
			return report, err
		}
		report.Changed = report.Changed || changed
		report.Unsafe = report.Unsafe || unsafe
	}
	report.OK = len(report.Issues) == 0
	switch {
	case report.OK && report.Changed:
		report.Message = fmt.Sprintf("storage permissions repaired for %s", path)
	case report.OK:
		report.Message = fmt.Sprintf("storage permissions are owner-only for %s", path)
	case report.Unsafe:
		report.Message = fmt.Sprintf("storage contains unsafe path types under %s", path)
	default:
		report.Message = fmt.Sprintf("storage permissions are broader than owner-only for %s", path)
		report.Repairable = true
	}
	return report, nil
}

func inspectPermissionTarget(report *PermissionsReport, target permissionTarget, repair bool) (changed bool, unsafe bool, err error) {
	info, err := os.Lstat(target.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("inspect %s %s: %w", target.kind, target.path, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		report.Issues = append(report.Issues, PermissionIssue{
			Path:    target.path,
			Kind:    target.kind,
			Mode:    info.Mode(),
			Message: "path is a symlink and will not be chmodded",
		})
		return false, true, nil
	}
	if target.wantDir && !info.IsDir() {
		report.Issues = append(report.Issues, PermissionIssue{
			Path:    target.path,
			Kind:    target.kind,
			Mode:    info.Mode(),
			Message: "path is not a directory",
		})
		return false, true, nil
	}
	if target.wantRegular && !info.Mode().IsRegular() {
		report.Issues = append(report.Issues, PermissionIssue{
			Path:    target.path,
			Kind:    target.kind,
			Mode:    info.Mode(),
			Message: "path is not a regular file",
		})
		return false, true, nil
	}
	if info.Mode().Perm()&0o077 == 0 {
		return false, false, nil
	}
	next := tightenedMode(info.Mode())
	if repair {
		if err := os.Chmod(target.path, next.Perm()); err != nil {
			return false, false, fmt.Errorf("tighten %s %s from %s to %s: %w", target.kind, target.path, modeSummary(info.Mode()), modeSummary(next), err)
		}
		report.Repairs = append(report.Repairs, PermissionRepair{
			Path:   target.path,
			Kind:   target.kind,
			Before: info.Mode().Perm(),
			After:  next.Perm(),
		})
		return true, false, nil
	}
	report.Issues = append(report.Issues, PermissionIssue{
		Path:    target.path,
		Kind:    target.kind,
		Mode:    info.Mode().Perm(),
		Message: fmt.Sprintf("mode %s is broader than owner-only %s", modeSummary(info.Mode()), modeSummary(target.maxMode)),
	})
	report.Repairable = true
	return false, false, nil
}
