//go:build !windows

package upgrade

import "errors"

func scheduleReplace(string, string) error {
	return errors.New("deferred replacement is supported on Windows only")
}
