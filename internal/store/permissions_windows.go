//go:build windows

package store

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

type permissionTarget struct {
	path string
	kind string
	dir  bool
}

// CheckPermissions inspects store path permissions using Windows owner and
// DACL state. Supported is true: this platform enforces owner-only ACLs.
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
	if err := inspectAncestorPathBoundary(path, false); err != nil {
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
	if err := inspectAncestorPathBoundary(path, true); err != nil {
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
	// Windows has no POSIX umask; owner-only ACLs are applied after create.
	return fn()
}

func storePermissionTargets(path string) []permissionTarget {
	targets := storeDirectoryTargets(path)
	targets = append(targets, permissionTarget{path: path, kind: "database file"})
	targets = append(targets, sqliteSidecarTargets(path)...)
	return targets
}

func storeDirectoryTargets(path string) []permissionTarget {
	return []permissionTarget{
		{
			path: filepath.Dir(path),
			kind: "data directory",
			dir:  true,
		},
	}
}

func sqliteSidecarTargets(path string) []permissionTarget {
	return []permissionTarget{
		{path: path + "-wal", kind: "sqlite wal file"},
		{path: path + "-shm", kind: "sqlite shm file"},
	}
}

// inspectAncestorPathBoundary walks path components from the leaf upward and
// fails closed on symlink components and foreign ownership of the database
// path or its data directory. Further ancestors stop at the first foreign
// owner (system path) after the user-owned prefix is validated.
func inspectAncestorPathBoundary(path string, _ bool) error {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "" || path == "." {
		return fmt.Errorf("path is required")
	}
	userSID, err := currentUserSID()
	if err != nil {
		return err
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
		ownerErr := requireCurrentUserOwner(current, userSID)
		if ownerErr != nil {
			if current == path || current == dataDir {
				return fmt.Errorf("ancestor %s: %w", current, ownerErr)
			}
			// Foreign-owned system parent ends the walk after user-owned prefix.
			return nil
		}
		parent := filepath.Dir(current)
		if parent == current || parent == "." {
			return nil
		}
		// Volume root (e.g. C:\) ends the walk.
		if len(parent) > 0 && parent[len(parent)-1] == filepath.Separator && filepath.Dir(parent) == parent {
			return nil
		}
		current = parent
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

	userSID, err := currentUserSID()
	if err != nil {
		return item, err
	}
	if err := requireCurrentUserOwner(target.path, userSID); err != nil {
		item.Secure = false
		item.Unsafe = true
		item.Message = err.Error()
		return item, nil
	}
	secure, detail, err := ownerOnlyACLState(target.path, userSID)
	if err != nil {
		return item, fmt.Errorf("inspect owner-only ACL for %s %s: %w", target.kind, target.path, err)
	}
	if secure {
		item.Message = "owner-only"
		return item, nil
	}
	item.Secure = false
	item.Message = detail
	if !repair {
		return item, nil
	}
	if err := applyOwnerOnlyACL(target.path, userSID, target.dir); err != nil {
		return item, fmt.Errorf("tighten %s %s: %w", target.kind, target.path, err)
	}
	secure, detail, err = ownerOnlyACLState(target.path, userSID)
	if err != nil {
		return item, fmt.Errorf("re-inspect owner-only ACL for %s %s: %w", target.kind, target.path, err)
	}
	if !secure {
		item.Message = detail
		return item, nil
	}
	item.Repaired = true
	item.Secure = true
	item.Message = "tightened to owner-only ACL"
	return item, nil
}

func ensureOwnerOnlyDir(target permissionTarget) error {
	if err := os.MkdirAll(target.path, 0o700); err != nil {
		return fmt.Errorf("create %s %s: %w", target.kind, target.path, err)
	}
	return tightenExistingTarget(target)
}

func ensureOwnerOnlyFile(path string) error {
	target := permissionTarget{path: path, kind: "database file"}
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
	if !item.Secure {
		return fmt.Errorf("%s %s is not owner-only: %s", target.kind, target.path, item.Message)
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

func currentUserSID() (*windows.SID, error) {
	token := windows.GetCurrentProcessToken()
	tu, err := token.GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("resolve current user SID: %w", err)
	}
	if tu == nil || tu.User.Sid == nil {
		return nil, fmt.Errorf("resolve current user SID: empty token user")
	}
	sid, err := tu.User.Sid.Copy()
	if err != nil {
		return nil, fmt.Errorf("copy current user SID: %w", err)
	}
	return sid, nil
}

func requireCurrentUserOwner(path string, userSID *windows.SID) error {
	sd, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION,
	)
	if err != nil {
		return fmt.Errorf("read owner SID: %w", err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return fmt.Errorf("read owner SID: %w", err)
	}
	if owner == nil || !owner.Equals(userSID) {
		ownerStr := "<nil>"
		if owner != nil {
			ownerStr = owner.String()
		}
		return fmt.Errorf("owned by %s, want current user %s", ownerStr, userSID.String())
	}
	return nil
}

func ownerOnlyACLState(path string, userSID *windows.SID) (secure bool, detail string, err error) {
	sd, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return false, "", err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return false, "", err
	}
	if owner == nil || !owner.Equals(userSID) {
		ownerStr := "<nil>"
		if owner != nil {
			ownerStr = owner.String()
		}
		return false, fmt.Sprintf("owned by %s, want current user %s", ownerStr, userSID.String()), nil
	}
	control, _, err := sd.Control()
	if err != nil {
		return false, "", err
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return false, "DACL is not protected from inherited grants", nil
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		// ERROR_OBJECT_NOT_FOUND means no DACL present (not the same as empty DACL).
		return false, fmt.Sprintf("DACL unavailable: %v", err), nil
	}
	if dacl == nil {
		// Empty/NULL DACL is fully permissive on Windows.
		return false, "DACL is empty (fully permissive)", nil
	}
	for i := uint16(0); i < dacl.AceCount; i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(i), &ace); err != nil {
			return false, "", fmt.Errorf("read ACE %d: %w", i, err)
		}
		if ace == nil {
			return false, fmt.Sprintf("ACE %d is nil", i), nil
		}
		// Inherited ACEs that only propagate are still grants if present on the
		// object without INHERIT_ONLY; reject any non-current-user grant.
		if ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			return false, "DACL contains ACCESS_DENIED ACE", nil
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return false, fmt.Sprintf("DACL contains unsupported ACE type %d", ace.Header.AceType), nil
		}
		// Skip inherit-only ACEs that do not apply to this object itself.
		const inheritOnlyACE = 0x8
		if ace.Header.AceFlags&inheritOnlyACE != 0 {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.Equals(userSID) {
			return false, fmt.Sprintf("DACL grants access to non-owner SID %s", sid.String()), nil
		}
	}
	return true, "owner-only", nil
}

func applyOwnerOnlyACL(path string, userSID *windows.SID, dir bool) error {
	var pinner runtime.Pinner
	pinner.Pin(userSID)
	defer pinner.Unpin()

	inheritance := uint32(windows.NO_INHERITANCE)
	if dir {
		inheritance = windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT
	}
	entries := []windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       inheritance,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(userSID),
		},
	}}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return fmt.Errorf("build owner-only ACL: %w", err)
	}
	// PROTECTED_DACL blocks inherited grants from parents (Everyone, Users, etc.).
	if err := windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|
			windows.DACL_SECURITY_INFORMATION|
			windows.PROTECTED_DACL_SECURITY_INFORMATION,
		userSID,
		nil,
		acl,
		nil,
	); err != nil {
		return fmt.Errorf("set owner-only ACL: %w", err)
	}
	return nil
}
