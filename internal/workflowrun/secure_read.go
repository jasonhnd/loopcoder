package workflowrun

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Size bounds for secure product IO.
// Text acceptance (findings/docs/clarification) stays small; product digest
// streams without loading whole sources into memory, with a hard per-leaf cap.
const (
	maxTextProductRead  = 1 << 20   // 1 MiB — research/docs/verdict prose
	maxProductHashBytes = 256 << 20 // 256 MiB — streaming hash per product leaf
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

// secureReadAfterPreLstat is nil in production. Tests may set it to swap the
// leaf between pre-Lstat and Open, asserting the SameFile identity mismatch path.
var secureReadAfterPreLstat func(fullPath string)

// withSecureRegularProduct opens a clean relative leaf under worktreeAbs with:
//
//	secure resolve (root + parents non-symlink dirs) →
//	pre-Lstat regular non-symlink → Open → fd.Stat regular + SameFile(pre, fd) →
//	fn(fd) → post-Lstat regular non-symlink + SameFile(fd, post)
//
// Never follows root/parent/leaf symlinks. fn should read from the fd only.
func withSecureRegularProduct(worktreeAbs, rel string, fn func(f *os.File) error) error {
	full, err := resolveSecureUnderWorktree(worktreeAbs, rel)
	if err != nil {
		return err
	}
	pre, err := os.Lstat(full)
	if err != nil {
		return err
	}
	if pre.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink", rel)
	}
	if !pre.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file (mode=%v)", rel, pre.Mode())
	}
	if secureReadAfterPreLstat != nil {
		secureReadAfterPreLstat(full)
	}
	f, err := os.Open(full)
	if err != nil {
		return err
	}
	defer f.Close()
	fdStat, err := f.Stat()
	if err != nil {
		return err
	}
	if !fdStat.Mode().IsRegular() {
		return fmt.Errorf("%s fd is not a regular file", rel)
	}
	if !os.SameFile(pre, fdStat) {
		return fmt.Errorf("%s identity mismatch: pre-lstat vs open fd", rel)
	}
	if err := fn(f); err != nil {
		return err
	}
	post, err := os.Lstat(full)
	if err != nil {
		return err
	}
	if post.Mode()&os.ModeSymlink != 0 || !post.Mode().IsRegular() {
		return fmt.Errorf("%s post-read node is not a regular non-symlink file", rel)
	}
	if !os.SameFile(fdStat, post) {
		return fmt.Errorf("%s identity mismatch: open fd vs post-lstat", rel)
	}
	return nil
}

// streamSecureRegularProduct streams a secure regular leaf into w, capping at
// maxBytes (exclusive of overflow probe). Used for product digests so large
// sources are not fully buffered.
func streamSecureRegularProduct(worktreeAbs, rel string, w io.Writer, maxBytes int64) (n int64, err error) {
	if maxBytes <= 0 {
		return 0, fmt.Errorf("%s invalid maxBytes", rel)
	}
	err = withSecureRegularProduct(worktreeAbs, rel, func(f *os.File) error {
		nn, cerr := io.Copy(w, io.LimitReader(f, maxBytes+1))
		n = nn
		if cerr != nil {
			return cerr
		}
		if nn > maxBytes {
			return fmt.Errorf("%s exceeds max product size (%d bytes)", rel, maxBytes)
		}
		return nil
	})
	return n, err
}

// readSecureRegularProductBytes reads a secure regular leaf up to maxBytes.
// Used for text gates (findings/docs/clarification), not bulk source hashing.
func readSecureRegularProductBytes(worktreeAbs, rel string, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("%s invalid maxBytes", rel)
	}
	var raw []byte
	err := withSecureRegularProduct(worktreeAbs, rel, func(f *os.File) error {
		b, rerr := io.ReadAll(io.LimitReader(f, maxBytes+1))
		if rerr != nil {
			return rerr
		}
		if int64(len(b)) > maxBytes {
			return fmt.Errorf("%s exceeds max findings size", rel)
		}
		raw = b
		return nil
	})
	return raw, err
}

// proveSecureRegularProduct is true when the leaf is a secure regular file
// (full identity chain) with at least one byte of content. Zero-byte empty.go /
// empty_test.go / generic empty leaves are not useful product for accept gates.
// Research/verify/docs use stronger body checks separately.
func proveSecureRegularProduct(worktreeAbs, rel string) error {
	return withSecureRegularProduct(worktreeAbs, rel, func(f *os.File) error {
		var b [1]byte
		n, err := f.Read(b[:])
		if err != nil && err != io.EOF {
			return err
		}
		if n < 1 {
			return fmt.Errorf("%s is empty (zero-byte product refused)", rel)
		}
		return nil
	})
}

// readRegularFindingsFile Lstats then reads a text leaf under worktree. rel may
// be nested (docs/foo.md). Rejects abs/../symlink-root/symlink-parent paths.
// Caps at maxTextProductRead (1 MiB) for acceptance prose.
func readRegularFindingsFile(worktreeAbs, rel string) ([]byte, bool) {
	return readRegularFindingsFileChecked(worktreeAbs, rel)
}

func readRegularFindingsFileChecked(worktreeAbs, rel string) ([]byte, bool) {
	raw, err := readRegularFindingsFileErr(worktreeAbs, rel)
	return raw, err == nil
}

func readRegularFindingsFileErr(worktreeAbs, rel string) ([]byte, error) {
	return readSecureRegularProductBytes(worktreeAbs, rel, maxTextProductRead)
}
