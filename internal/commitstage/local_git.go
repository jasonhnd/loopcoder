package commitstage

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// LocalGit implements Git against a real worktree path via git CLI.
// Production only — tests inject FakeGit explicitly.
type LocalGit struct {
	// Worktree is the absolute path of the git worktree (or repo root).
	Worktree string
}

// NewLocalGit returns a fail-closed real git port. worktree must be non-empty.
func NewLocalGit(worktree string) (*LocalGit, error) {
	worktree = strings.TrimSpace(worktree)
	if worktree == "" {
		return nil, fmt.Errorf("%w: empty worktree", ErrInvalid)
	}
	abs, err := filepath.Abs(worktree)
	if err != nil {
		return nil, fmt.Errorf("%w: abs: %v", ErrInvalid, err)
	}
	return &LocalGit{Worktree: abs}, nil
}

func (g *LocalGit) run(args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", g.Worktree}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func (g *LocalGit) HEAD() (string, error) {
	return g.run("rev-parse", "HEAD")
}

func (g *LocalGit) ParentOf(commit string) (string, error) {
	return g.run("rev-parse", commit+"^")
}

func (g *LocalGit) TreeOf(commit string) (string, error) {
	return g.run("rev-parse", commit+"^{tree}")
}

func (g *LocalGit) StagePaths(owned []string, allDirty []string) error {
	own := map[string]struct{}{}
	for _, p := range owned {
		p = filepath.ToSlash(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		own[p] = struct{}{}
	}
	for _, d := range allDirty {
		d = filepath.ToSlash(strings.TrimSpace(d))
		if d == "" {
			continue
		}
		if _, ok := own[d]; !ok {
			return fmt.Errorf("%w: unowned dirty %s", ErrDrift, d)
		}
	}
	if len(owned) == 0 {
		return fmt.Errorf("%w: no owned paths", ErrEmpty)
	}
	args := []string{"add", "--"}
	args = append(args, owned...)
	_, err := g.run(args...)
	return err
}

func (g *LocalGit) Commit(message, authorPolicy string) (string, error) {
	// Ensure something is staged.
	status, err := g.run("diff", "--cached", "--name-only")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(status) == "" {
		return "", ErrEmpty
	}
	name := "loopcoder-bot"
	email := "loopcoder-bot@users.noreply.github.com"
	if strings.TrimSpace(authorPolicy) != "" && authorPolicy != "loopcoder_bot" {
		// Author policy is a label only; never inject arbitrary identities from untrusted input.
		name = "loopcoder-bot"
	}
	cmd := exec.Command("git", "-C", g.Worktree,
		"-c", "user.name="+name,
		"-c", "user.email="+email,
		"commit", "-m", message, "--no-gpg-sign",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git commit: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return g.HEAD()
}

func (g *LocalGit) IndexDirty() ([]string, error) {
	// Combined worktree + index changes (name-only, relative paths).
	out, err := g.run("status", "--porcelain", "-uall")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	var paths []string
	seen := map[string]struct{}{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if len(line) < 4 {
			continue
		}
		// porcelain: XY path  (or XY orig -> path for renames)
		rest := strings.TrimSpace(line[2:])
		if i := strings.Index(rest, " -> "); i >= 0 {
			rest = rest[i+4:]
		}
		rest = strings.Trim(rest, `"`)
		rest = filepath.ToSlash(rest)
		if rest == "" {
			continue
		}
		if _, ok := seen[rest]; ok {
			continue
		}
		seen[rest] = struct{}{}
		paths = append(paths, rest)
	}
	return paths, nil
}

// ChangedPathsSince returns paths changed between baseSHA and HEAD (inclusive of uncommitted).
func (g *LocalGit) ChangedPathsSince(baseSHA string) ([]string, error) {
	baseSHA = strings.TrimSpace(baseSHA)
	if baseSHA == "" {
		return g.IndexDirty()
	}
	out, err := g.run("diff", "--name-only", baseSHA)
	if err != nil {
		return nil, err
	}
	dirty, err := g.IndexDirty()
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		p := filepath.ToSlash(strings.TrimSpace(line))
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		paths = append(paths, p)
	}
	for _, p := range dirty {
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		paths = append(paths, p)
	}
	return paths, nil
}
