//go:build !darwin

package store

import "runtime"

// CheckPermissions fails closed on non-Darwin hosts. There is no weaker
// permission path for unsupported platforms.
func CheckPermissions(path string) (PermissionReport, error) {
	report := PermissionReport{
		Path:      path,
		Platform:  runtime.GOOS,
		Supported: false,
		Secure:    false,
		Message:   ErrUnsupportedPlatform.Error(),
	}
	return report, requireSupportedPlatform()
}

func ensurePermissionsForOpen(path string) error {
	_ = path
	return requireSupportedPlatform()
}

func hardenSQLiteSidecars(path string) error {
	_ = path
	return requireSupportedPlatform()
}

func withOwnerOnlyUmask(fn func() error) error {
	if err := requireSupportedPlatform(); err != nil {
		return err
	}
	// Unreachable on the !darwin build; kept so the signature matches Darwin.
	return fn()
}
