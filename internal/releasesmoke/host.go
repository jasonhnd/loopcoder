package releasesmoke

import (
	"fmt"
	"runtime"
)

// RequireDarwinARM64 fails unless the process host is darwin/arm64.
// Call before creating temporary state or mutating the filesystem.
func RequireDarwinARM64() error {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return fmt.Errorf("releasesmoke requires darwin/arm64 host; got %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	return nil
}

// HostTuple returns the supported host tuple string.
func HostTuple() string {
	return "darwin/arm64"
}
