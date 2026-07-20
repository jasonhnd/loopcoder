//go:build !windows

package store

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
	ownerOnlyDirMode  fs.FileMode = 0o700
	ownerOnlyFileMode fs.FileMode = 0o600
)

type permissionTarget struct {
	path string
	kind string
	mode fs.FileMode
	dir  bool
}

// CheckPermissions inspects store path permissions without creating paths or
// changing modes.
func CheckPermissions(path string) (PermissionReport, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || path == "" {
		return PermissionReport{}, fmt.Errorf("store permissions: path is required")
	}
	report := PermissionReport{
		Path:      path,
		Platform:  runtime.GOOS,
		Supported: true,
		Secure:    true,
	}
	if err := inspectAncestorPathBoundary(path); err != nil {
		report.Secure = false
		report.Message = err.Error()
		report.Items = append(report.Items, PermissionItem{
			Path:    path,
			Kind:    "path boundary",
			Exists:  true,
			Secure:  false,
			Unsafe:  true,
			Message: err.Error(),
		})
		return report, nil
	}
	for _, target := range storePermissionTargets(path) {
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

func ensurePermissionsForOpen(path string) error {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || path == "" {
		return fmt.Errorf("open store: path is required")
	}
	if err := inspectAncestorPathBoundary(path); err != nil {
		return fmt.Errorf("open store: insecure path boundary for %s: %w", path, err)
	}
	for _, target := range storeDirectoryTargets(path) {
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

func storePermissionTargets(path string) []permissionTarget {
	targets := storeDirectoryTargets(path)
	targets = append(targets, permissionTarget{path: path, kind: "database file", mode: ownerOnlyFileMode})
	targets = append(targets, sqliteSidecarTargets(path)...)
	return targets
}

func storeDirectoryTargets(path string) []permissionTarget {
	// Immediate data directory must be owner-only (0700).
	// Ancestor directories are checked separately for symlink escape and
	// foreign ownership without requiring every parent to be 0700 (e.g. $HOME).
	return []permissionTarget{
		{
			path: filepath.Dir(path),
			kind: "data directory",
			mode: ownerOnlyDirMode,
			dir:  true,
		},
	}
}

// inspectAncestorPathBoundary walks path components from the leaf upward and
// fails closed on:
//   - any symlink component
//   - foreign ownership of the database path or its data directory
//   - foreign ownership of further ancestors while still inside a current-user
//     owned prefix (so a user-owned intermediate cannot redirect via symlink
//     into another principal's tree)
//
// System ancestors owned by another uid (for example /var or /Users) stop the
// walk without error once the user-owned prefix has been validated. Mode 0700
// is enforced only on the data directory and database file targets.
func inspectAncestorPathBoundary(path string) error {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "" || path == "." {
		return fmt.Errorf("path is required")
	}
	dataDir := filepath.Dir(path)
	current := path
	seen := map[string]bool{}
	for {
		if seen[current] {
			return fmt.Errorf("path cycle while inspecting ancestors of %s", path)
		}
		seen[current] = true
		info, err := os.Lstat(current)
		if err != nil {
			if os.IsNotExist(err) {
				parent := filepath.Dir(current)
				if parent == current {
					return nil
				}
				current = parent
				continue
			}
			return fmt.Errorf("inspect ancestor %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("ancestor path component is a symlink: %s", current)
		}
		ownerErr := requireCurrentUserOwner(info)
		if ownerErr != nil {
			// Database file and data directory must always be owned by us.
			if current == path || current == dataDir {
				return fmt.Errorf("ancestor %s: %w", current, ownerErr)
			}
			// Foreign-owned system parent ends the walk after user-owned prefix.
			return nil
		}
		parent := filepath.Dir(current)
		if parent == current || parent == string(filepath.Separator) || parent == "." {
			return nil
		}
		current = parent
	}
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

	// Fail closed when the path is not owned by the current process user.
	// Mode bits alone are insufficient: a world-readable path owned by
	// another uid is still an integrity violation for local store state.
	if err := requireCurrentUserOwner(info); err != nil {
		item.Secure = false
		item.Unsafe = true
		item.Message = err.Error()
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

func requireCurrentUserOwner(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return fmt.Errorf("unable to read posix owner identity")
	}
	uid := uint32(os.Getuid())
	if stat.Uid != uid {
		return fmt.Errorf("owned by uid %d, want current uid %d", stat.Uid, uid)
	}
	return nil
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
		return fmt.Sprintf("owner-only store permissions enforced; repaired %d path(s)", repairs)
	}
	if existing == 0 {
		return "store paths do not exist yet; new store will be created owner-only"
	}
	return "store permissions are owner-only"
}
