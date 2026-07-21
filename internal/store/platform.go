package store

import (
	"errors"
	"fmt"
	"runtime"
)

// ErrUnsupportedPlatform is returned when the compact store is opened or
// inspected outside the v0.9 product platform (darwin/arm64).
//
// Callers must treat this as a hard product boundary, not a recoverable
// permission or IO failure.
var ErrUnsupportedPlatform = errors.New("unsupported platform: loopcoder compact store requires darwin/arm64")

// SupportedPlatform reports whether the current process may use the compact store.
func SupportedPlatform() bool {
	return runtime.GOOS == "darwin" && runtime.GOARCH == "arm64"
}

// PlatformTuple returns the runtime GOOS/GOARCH pair for diagnostics that must
// not include host paths or usernames.
func PlatformTuple() string {
	return runtime.GOOS + "/" + runtime.GOARCH
}

func requireSupportedPlatform() error {
	if SupportedPlatform() {
		return nil
	}
	return fmt.Errorf("%w (got %s)", ErrUnsupportedPlatform, PlatformTuple())
}
