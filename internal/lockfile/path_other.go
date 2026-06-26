//go:build !windows

package lockfile

func normalizeCanonicalPath(path string) string {
	return path
}
