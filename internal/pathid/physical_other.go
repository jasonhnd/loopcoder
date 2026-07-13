//go:build !windows

package pathid

import "path/filepath"

func physicalIdentity(absPath string) (string, error) {
	if resolved, err := filepath.EvalSymlinks(absPath); err == nil {
		return filepath.Clean(resolved), nil
	} else if !isNotExist(err) {
		return "", err
	}

	ancestor := absPath
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(ancestor)
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
			return filepath.Clean(absPath), nil
		}
		suffix = append(suffix, filepath.Base(ancestor))
		ancestor = parent
	}
}
