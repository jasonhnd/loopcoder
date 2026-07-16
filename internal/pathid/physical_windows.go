//go:build windows

package pathid

import (
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func physicalIdentity(absPath string) (string, error) {
	if resolved, err := finalPathForExisting(absPath); err == nil {
		return filepath.Clean(resolved), nil
	} else if !isNotExist(err) {
		return "", err
	}

	ancestor := absPath
	var suffix []string
	for {
		resolved, err := finalPathForExisting(ancestor)
		if err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !isNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", fmt.Errorf("resolve physical path %q: no existing ancestor", absPath)
		}
		suffix = append(suffix, filepath.Base(ancestor))
		ancestor = parent
	}
}

func finalPathForExisting(path string) (string, error) {
	ptr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	handle, err := windows.CreateFile(
		ptr,
		0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return "", err
	}
	resolved, queryErr := finalPathFromHandle(handle)
	closeErr := windows.CloseHandle(handle)
	if queryErr != nil {
		return "", queryErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	return resolved, nil
}

func finalPathFromHandle(handle windows.Handle) (string, error) {
	buffer := make([]uint16, windows.MAX_PATH)
	for {
		n, err := windows.GetFinalPathNameByHandle(handle, &buffer[0], uint32(len(buffer)), 0)
		if err != nil {
			return "", err
		}
		if n == 0 {
			return "", fmt.Errorf("GetFinalPathNameByHandle returned an empty path")
		}
		if n < uint32(len(buffer)) {
			return normalizeWindowsFinalPath(windows.UTF16ToString(buffer[:n])), nil
		}
		buffer = make([]uint16, n+1)
	}
}

func normalizeWindowsFinalPath(path string) string {
	path = strings.ReplaceAll(path, "/", `\`)
	for _, prefix := range []string{`\\?\UNC\`, `\??\UNC\`} {
		if strings.HasPrefix(path, prefix) {
			return filepath.Clean(`\\` + strings.TrimPrefix(path, prefix))
		}
	}
	for _, prefix := range []string{`\\?\`, `\??\`} {
		if strings.HasPrefix(path, prefix) {
			return filepath.Clean(strings.TrimPrefix(path, prefix))
		}
	}
	return filepath.Clean(path)
}
