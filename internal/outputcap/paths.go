package outputcap

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ResolveLogPath ensures attempt logs live under payloadRoot (project payload).
// Returns an absolute path inside root, creating owner-only directories.
func ResolveLogPath(payloadRoot, projectRel string) (string, error) {
	root := filepath.Clean(strings.TrimSpace(payloadRoot))
	if root == "" || root == "." {
		return "", ErrInvalidRoot
	}
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("%w: root must be absolute", ErrInvalidRoot)
	}
	rel := filepath.Clean(strings.TrimSpace(projectRel))
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", ErrOutsidePayloadRoot
	}
	if filepath.IsAbs(rel) {
		return "", ErrOutsidePayloadRoot
	}
	full := filepath.Join(root, rel)
	// Ensure full is still under root.
	relCheck, err := filepath.Rel(root, full)
	if err != nil || strings.HasPrefix(relCheck, "..") {
		return "", ErrOutsidePayloadRoot
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return "", fmt.Errorf("%w: mkdir: %v", ErrLogWrite, err)
	}
	return full, nil
}

// ValidateUnderRoot returns error if path is outside root.
func ValidateUnderRoot(payloadRoot, path string) error {
	root := filepath.Clean(payloadRoot)
	path = filepath.Clean(path)
	rel, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(rel, "..") || rel == ".." {
		return ErrOutsidePayloadRoot
	}
	return nil
}
