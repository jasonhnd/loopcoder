//go:build !windows

package storage

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

const (
	ownerOnlyDirMode    fs.FileMode = 0o700
	ownerOnlyFileMode   fs.FileMode = 0o600
	defaultDataDirName              = "data"
	defaultDatabaseName             = "loopcoder.db"
)

type permissionTarget struct {
	path string
	kind string
	mode fs.FileMode
	dir  bool
}

// CheckPermissions inspects storage permissions without creating paths or
// changing modes.
func CheckPermissions(path string) (PermissionReport, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || path == "" {
		return PermissionReport{}, fmt.Errorf("storage permissions: path is required")
	}
	report := PermissionReport{
		Path:      path,
		Platform:  runtime.GOOS,
		Supported: true,
		Secure:    true,
	}
	for _, target := range storagePermissionTargets(path) {
		item, err := inspectPermissionTarget(target, false)
		if err != nil {
			return report, err
		}
		report.Items = append(report.Items, item)
		if !item.Secure {
			report.Secure = false
		}
	}
	report.Message = permissionReportMessage(report)
	return report, nil
}

// RepairPermissions safely tightens existing storage paths. Missing paths are
// left missing; this repair path never deletes or recreates database contents.
func RepairPermissions(path string) (PermissionReport, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || path == "" {
		return PermissionReport{}, fmt.Errorf("storage permissions: path is required")
	}
	report := PermissionReport{
		Path:      path,
		Platform:  runtime.GOOS,
		Supported: true,
		Secure:    true,
	}
	for _, target := range storagePermissionTargets(path) {
		item, err := inspectPermissionTarget(target, true)
		if err != nil {
			return report, err
		}
		report.Items = append(report.Items, item)
		if item.Unsafe {
			report.Secure = false
			return report, fmt.Errorf("%s %s is unsafe: %s", target.kind, target.path, item.Message)
		}
		if item.Repaired {
			report.Repaired = true
		}
		if !item.Secure {
			report.Secure = false
		}
	}
	report.Message = permissionReportMessage(report)
	return report, nil
}

func ensurePermissionsForOpen(path string) error {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || path == "" {
		return fmt.Errorf("open storage: path is required")
	}

	for _, target := range storageDirectoryTargets(path) {
		if err := ensureOwnerOnlyDir(target); err != nil {
			return err
		}
	}
	if err := ensureOwnerOnlyFile(path); err != nil {
		return err
	}
	for _, sidecar := range sqliteSidecarTargets(path) {
		if err := tightenExistingTarget(sidecar); err != nil {
			return err
		}
	}
	return nil
}

func hardenSQLiteSidecars(path string) error {
	for _, sidecar := range sqliteSidecarTargets(path) {
		if err := tightenExistingTarget(sidecar); err != nil {
			return err
		}
	}
	return nil
}

func withOwnerOnlyUmask(fn func() error) error {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	return fn()
}

func storagePermissionTargets(path string) []permissionTarget {
	targets := storageDirectoryTargets(path)
	targets = append(targets, permissionTarget{path: path, kind: "database file", mode: ownerOnlyFileMode})
	targets = append(targets, sqliteSidecarTargets(path)...)
	return targets
}

func storageDirectoryTargets(path string) []permissionTarget {
	dataDir := filepath.Dir(path)
	targets := []permissionTarget{}
	if filepath.Base(path) == defaultDatabaseName && filepath.Base(dataDir) == defaultDataDirName {
		targets = append(targets, permissionTarget{
			path: filepath.Dir(dataDir),
			kind: "home directory",
			mode: ownerOnlyDirMode,
			dir:  true,
		})
	}
	targets = append(targets, permissionTarget{
		path: dataDir,
		kind: "data directory",
		mode: ownerOnlyDirMode,
		dir:  true,
	})
	return targets
}

func sqliteSidecarTargets(path string) []permissionTarget {
	return []permissionTarget{
		{path: path + "-wal", kind: "sqlite wal file", mode: ownerOnlyFileMode},
		{path: path + "-shm", kind: "sqlite shm file", mode: ownerOnlyFileMode},
	}
}

func inspectPermissionTarget(target permissionTarget, repair bool) (PermissionItem, error) {
	item := PermissionItem{Path: target.path, Kind: target.kind, Secure: true}
	info, err := os.Lstat(target.path)
	if err != nil {
		if os.IsNotExist(err) {
			item.Exists = false
			item.Message = "missing"
			return item, nil
		}
		return item, fmt.Errorf("inspect %s %s: %w", target.kind, target.path, err)
	}
	item.Exists = true
	item.BeforeMode = info.Mode().Perm()
	item.AfterMode = item.BeforeMode

	if info.Mode()&os.ModeSymlink != 0 {
		item.Secure = false
		item.Unsafe = true
		item.Message = "refusing symlink"
		return item, nil
	}
	if target.dir {
		if !info.IsDir() {
			item.Secure = false
			item.Unsafe = true
			item.Message = "not a directory"
			return item, nil
		}
	} else if !info.Mode().IsRegular() {
		item.Secure = false
		item.Unsafe = true
		item.Message = "not a regular file"
		return item, nil
	}

	if item.BeforeMode&^target.mode == 0 {
		item.Message = "owner-only"
		return item, nil
	}
	item.Secure = false
	item.Message = fmt.Sprintf("mode %04o is broader than %04o", item.BeforeMode, target.mode)
	if !repair {
		return item, nil
	}
	next := item.BeforeMode & target.mode
	if err := os.Chmod(target.path, next); err != nil {
		return item, fmt.Errorf("tighten %s %s from %04o to %04o: %w", target.kind, target.path, item.BeforeMode, next, err)
	}
	item.AfterMode = next
	item.Repaired = true
	item.Secure = true
	item.Message = fmt.Sprintf("tightened from %04o to %04o", item.BeforeMode, item.AfterMode)
	return item, nil
}

func ensureOwnerOnlyDir(target permissionTarget) error {
	if err := os.MkdirAll(target.path, ownerOnlyDirMode); err != nil {
		return fmt.Errorf("create %s %s: %w", target.kind, target.path, err)
	}
	return tightenExistingTarget(target)
}

func ensureOwnerOnlyFile(path string) error {
	target := permissionTarget{path: path, kind: "database file", mode: ownerOnlyFileMode}
	info, err := os.Lstat(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("inspect database file %s: %w", path, err)
		}
		file, createErr := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, ownerOnlyFileMode)
		if createErr != nil {
			if !os.IsExist(createErr) {
				return fmt.Errorf("create database file %s: %w", path, createErr)
			}
		} else if closeErr := file.Close(); closeErr != nil {
			return fmt.Errorf("create database file %s: close: %w", path, closeErr)
		}
		return tightenExistingTarget(target)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("database file %s is a symlink; refusing to open", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("database file %s is not a regular file; refusing to open", path)
	}
	return tightenExistingTarget(target)
}

func tightenExistingTarget(target permissionTarget) error {
	item, err := inspectPermissionTarget(target, true)
	if err != nil {
		return err
	}
	if !item.Exists {
		return nil
	}
	if item.Unsafe {
		return fmt.Errorf("%s %s is unsafe: %s", target.kind, target.path, item.Message)
	}
	return nil
}

func permissionReportMessage(report PermissionReport) string {
	if !report.Supported {
		return report.Message
	}
	var insecure []string
	repairs := 0
	existing := 0
	for _, item := range report.Items {
		if item.Exists {
			existing++
		}
		if item.Repaired {
			repairs++
		}
		if !item.Secure {
			insecure = append(insecure, fmt.Sprintf("%s %s: %s", item.Kind, item.Path, item.Message))
		}
	}
	if len(insecure) > 0 {
		return strings.Join(insecure, "; ")
	}
	if repairs > 0 {
		return fmt.Sprintf("owner-only storage permissions enforced; repaired %d path(s)", repairs)
	}
	if existing == 0 {
		return "storage paths do not exist yet; new storage will be created owner-only"
	}
	return "storage permissions are owner-only"
}
