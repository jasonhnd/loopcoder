// Package pathid defines path representations used for identity and display.
package pathid

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Path describes one input path in both user-facing and identity forms.
type Path struct {
	// Display is the absolute, lexical path kept stable for user output.
	Display string
	// Identity is the physical canonical path used for equality and IDs.
	Identity string
}

// Canonicalize resolves path into display and physical identity forms.
//
// Existing paths are resolved through filepath.EvalSymlinks. For paths whose
// leaf does not exist yet, the longest existing ancestor is resolved and the
// missing suffix is appended lexically. Windows identity additionally folds the
// volume name and path case because Go does not expose a stable filesystem file
// identifier here; callers must still treat the result as path identity, not a
// security permission proof.
func Canonicalize(path string) (Path, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		path = "."
	}
	display, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return Path{}, err
	}
	display = filepath.Clean(display)
	identity, err := physicalIdentity(display)
	if err != nil {
		return Path{}, err
	}
	return Path{Display: display, Identity: foldIdentity(identity)}, nil
}

// Identity returns the physical identity path for path.
func Identity(path string) (string, error) {
	canonical, err := Canonicalize(path)
	if err != nil {
		return "", err
	}
	return canonical.Identity, nil
}

// Display returns the absolute lexical display path for path.
func Display(path string) (string, error) {
	canonical, err := Canonicalize(path)
	if err != nil {
		return "", err
	}
	return canonical.Display, nil
}

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

func foldIdentity(path string) string {
	path = filepath.Clean(path)
	if runtime.GOOS != "windows" {
		return path
	}
	volume := filepath.VolumeName(path)
	rest := strings.TrimPrefix(path, volume)
	return strings.ToUpper(volume) + strings.ToLower(rest)
}

func isNotExist(err error) bool {
	return errors.Is(err, os.ErrNotExist)
}
