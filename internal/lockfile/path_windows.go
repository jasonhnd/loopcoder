//go:build windows

package lockfile

import "strings"

func normalizeCanonicalPath(path string) string {
	return strings.ToLower(path)
}
