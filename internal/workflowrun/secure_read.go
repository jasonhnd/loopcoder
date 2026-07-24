package workflowrun

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// cleanWorktreeRelPath returns a Clean relative path under a worktree.
// Rejects empty, absolute, scheme-like, and any ".." escape components.
// Preserves nested paths such as docs/foo.md (does NOT collapse to Base).
func cleanWorktreeRelPath(rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return "", fmt.Errorf("empty relative path")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("absolute path refused: %q", rel)
	}
	if strings.Contains(rel, "://") {
		return "", fmt.Errorf("scheme path refused: %q", rel)
	}
	// Normalize separators then Clean.
	rel = filepath.ToSlash(rel)
	if strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("absolute slash path refused: %q", rel)
	}
	cleaned := filepath.Clean(filepath.FromSlash(rel))
	if cleaned == "." || cleaned == ".." {
		return "", fmt.Errorf("invalid relative path %q", rel)
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escape refused: %q", rel)
	}
	for _, p := range strings.Split(cleaned, string(filepath.Separator)) {
		if p == "" || p == ".." || p == "." {
			return "", fmt.Errorf("invalid path component in %q", rel)
		}
	}
	return cleaned, nil
}

// requireNonSymlinkDir Lstats path and requires a non-symlink directory.
func requireNonSymlinkDir(path string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink (directory required)", path)
	}
	if !st.IsDir() {
		return fmt.Errorf("%s is not a directory (mode=%v)", path, st.Mode())
	}
	return nil
}

// resolveSecureUnderWorktree validates:
//   - worktreeAbs is a non-symlink directory (root itself cannot be a symlink)
//   - each parent component of rel is a non-symlink directory
//
// Returns the absolute path to the leaf without following any symlink.
// Leaf type is NOT validated here (caller Lstats the leaf as regular file).
func resolveSecureUnderWorktree(worktreeAbs, rel string) (full string, err error) {
	worktreeAbs, err = filepath.Abs(worktreeAbs)
	if err != nil {
		return "", err
	}
	if err := requireNonSymlinkDir(worktreeAbs); err != nil {
		return "", fmt.Errorf("worktree root: %w", err)
	}
	rel, err = cleanWorktreeRelPath(rel)
	if err != nil {
		return "", err
	}
	parts := strings.Split(rel, string(filepath.Separator))
	cur := worktreeAbs
	for i, part := range parts {
		next := filepath.Join(cur, part)
		if i < len(parts)-1 {
			if err := requireNonSymlinkDir(next); err != nil {
				return "", fmt.Errorf("parent %q: %w", part, err)
			}
			cur = next
			continue
		}
		// leaf
		if err := requirePathUnderRoot(worktreeAbs, next); err != nil {
			return "", err
		}
		return next, nil
	}
	return "", fmt.Errorf("empty path after split")
}

// readRegularFindingsFile Lstats then reads a leaf under worktree. rel may be
// nested (docs/foo.md). Rejects abs/../symlink-root/symlink-parent paths.
//
// Identity chain (fail closed on race):
//  1. secure resolve (root + parents non-symlink dirs)
//  2. preLstat leaf — must be regular non-symlink
//  3. Open path
//  4. f.Stat() regular + os.SameFile(pre, fd)
//  5. read from fd
//  6. postLstat + os.SameFile(fd, post)
func readRegularFindingsFile(worktreeAbs, rel string) ([]byte, bool) {
	return readRegularFindingsFileChecked(worktreeAbs, rel)
}

func readRegularFindingsFileChecked(worktreeAbs, rel string) ([]byte, bool) {
	raw, err := readRegularFindingsFileErr(worktreeAbs, rel)
	return raw, err == nil
}

// secureReadAfterPreLstat is nil in production. Tests may set it to swap the
// leaf between pre-Lstat and Open, asserting the SameFile identity mismatch path.
var secureReadAfterPreLstat func(fullPath string)

func readRegularFindingsFileErr(worktreeAbs, rel string) ([]byte, error) {
	full, err := resolveSecureUnderWorktree(worktreeAbs, rel)
	if err != nil {
		return nil, err
	}
	pre, err := os.Lstat(full)
	if err != nil {
		return nil, err
	}
	if pre.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a symlink", rel)
	}
	if !pre.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file (mode=%v)", rel, pre.Mode())
	}
	if secureReadAfterPreLstat != nil {
		secureReadAfterPreLstat(full)
	}
	const maxFindings = 1 << 20
	f, err := os.Open(full)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fdStat, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fdStat.Mode().IsRegular() {
		return nil, fmt.Errorf("%s fd is not a regular file", rel)
	}
	if !os.SameFile(pre, fdStat) {
		return nil, fmt.Errorf("%s identity mismatch: pre-lstat vs open fd", rel)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxFindings+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxFindings {
		return nil, fmt.Errorf("%s exceeds max findings size", rel)
	}
	post, err := os.Lstat(full)
	if err != nil {
		return nil, err
	}
	if post.Mode()&os.ModeSymlink != 0 || !post.Mode().IsRegular() {
		return nil, fmt.Errorf("%s post-read node is not a regular non-symlink file", rel)
	}
	if !os.SameFile(fdStat, post) {
		return nil, fmt.Errorf("%s identity mismatch: open fd vs post-lstat", rel)
	}
	return raw, nil
}
