package acceptharness

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// RepoKind selects the disposable repository template.
type RepoKind string

const (
	RepoDocsOnly RepoKind = "docs-only"
	RepoSmallGo  RepoKind = "small-go"
)

// FixtureRepo is a temporary git repository with synthetic identity.
type FixtureRepo struct {
	Root       string
	Kind       RepoKind
	Owner      string
	Name       string
	DefaultSHA string
	Branch     string
}

// CreateRepo builds a disposable repository under parentDir.
func CreateRepo(parentDir string, kind RepoKind) (*FixtureRepo, error) {
	if parentDir == "" {
		return nil, fmt.Errorf("acceptharness: parentDir is required")
	}
	owner := "synthetic-owner"
	name := "synthetic-" + string(kind)
	root := filepath.Join(parentDir, owner, name)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	if err := git(root, "init", "-b", "main"); err != nil {
		return nil, err
	}
	if err := git(root, "config", "user.email", "synthetic@example.invalid"); err != nil {
		return nil, err
	}
	if err := git(root, "config", "user.name", "Synthetic Fixture"); err != nil {
		return nil, err
	}
	switch kind {
	case RepoDocsOnly:
		if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("# synthetic docs fixture\n"), 0o600); err != nil {
			return nil, err
		}
	case RepoSmallGo:
		if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/synthetic\n\ngo 1.22\n"), 0o600); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o600); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("acceptharness: unknown repo kind %q", kind)
	}
	if err := git(root, "add", "."); err != nil {
		return nil, err
	}
	if err := git(root, "commit", "-m", "synthetic initial commit"); err != nil {
		return nil, err
	}
	sha, err := gitOutput(root, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	return &FixtureRepo{
		Root:       root,
		Kind:       kind,
		Owner:      owner,
		Name:       name,
		DefaultSHA: strings.TrimSpace(sha),
		Branch:     "main",
	}, nil
}

// CommitFile writes a file and creates a commit on branch, returning the new SHA.
func (r *FixtureRepo) CommitFile(branch, relPath, content, message string) (string, error) {
	if r == nil {
		return "", fmt.Errorf("acceptharness: nil repo")
	}
	if branch == "" {
		branch = "main"
	}
	if err := git(r.Root, "checkout", "-B", branch); err != nil {
		return "", err
	}
	path := filepath.Join(r.Root, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return "", err
	}
	if err := git(r.Root, "add", "--", relPath); err != nil {
		return "", err
	}
	if err := git(r.Root, "commit", "-m", message); err != nil {
		return "", err
	}
	sha, err := gitOutput(r.Root, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	r.Branch = branch
	r.DefaultSHA = strings.TrimSpace(sha)
	return r.DefaultSHA, nil
}

func git(dir string, args ...string) error {
	_, err := gitOutput(dir, args...)
	return err
}

func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = CleanProcessEnv(nil)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}
