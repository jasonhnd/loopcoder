package projectid

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/acceptharness"
	"github.com/jasonhnd/loopcoder/internal/gitremote"
	"github.com/jasonhnd/loopcoder/internal/home"
	"github.com/jasonhnd/loopcoder/internal/pathid"
)

// Source names how the canonical identity was derived.
type Source string

const (
	SourceGitHub    Source = "github"
	SourceGitRemote Source = "git-remote"
	SourceLocalPath Source = "local-path"
)

// Identity is a resolved project identity before registry persistence.
type Identity struct {
	ProjectID     string
	Source        Source
	DisplayName   string
	LocalPath     string
	CanonicalPath string
	RemoteURL     string // credential-free normalized remote
	GitHubOwner   string
	GitHubName    string
	IdentityKey   string // preimage used for ID derivation
}

// Resolve derives a stable identity for repoPath without writing the registry.
func Resolve(ctx context.Context, repoPath string) (Identity, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	repoPath = strings.TrimSpace(repoPath)
	if repoPath == "" {
		return Identity{}, fmt.Errorf("projectid: repo path is required")
	}
	abs, err := filepath.Abs(repoPath)
	if err != nil {
		return Identity{}, err
	}
	canon, err := pathid.Canonicalize(abs)
	if err != nil {
		return Identity{}, fmt.Errorf("projectid: canonicalize path: %w", err)
	}

	remoteRaw, _ := gitOutput(ctx, abs, "config", "--get", "remote.origin.url")
	remoteRaw = strings.TrimSpace(remoteRaw)
	if remoteRaw != "" {
		normalized, owner, name, ok := gitremote.NormalizeURL(remoteRaw)
		if !ok {
			return Identity{}, fmt.Errorf("projectid: ambiguous or unsafe remote URL")
		}
		if owner != "" && name != "" {
			key := "github.com/" + strings.ToLower(owner) + "/" + strings.ToLower(name)
			return Identity{
				ProjectID:     deriveID(key),
				Source:        SourceGitHub,
				DisplayName:   owner + "/" + name,
				LocalPath:     abs,
				CanonicalPath: canon.Identity,
				RemoteURL:     normalized,
				GitHubOwner:   owner,
				GitHubName:    name,
				IdentityKey:   key,
			}, nil
		}
		key := "remote:" + normalized
		return Identity{
			ProjectID:     deriveID(key),
			Source:        SourceGitRemote,
			DisplayName:   filepath.Base(strings.TrimSuffix(normalized, ".git")),
			LocalPath:     abs,
			CanonicalPath: canon.Identity,
			RemoteURL:     normalized,
			IdentityKey:   key,
		}, nil
	}

	key := "local:" + canon.Identity
	return Identity{
		ProjectID:     deriveID(key),
		Source:        SourceLocalPath,
		DisplayName:   filepath.Base(abs),
		LocalPath:     abs,
		CanonicalPath: canon.Identity,
		IdentityKey:   key,
	}, nil
}

func deriveID(identityKey string) string {
	sum := sha256.Sum256([]byte(identityKey))
	// Stable, filesystem-safe id (home.ValidateProjectID allows hex-like).
	return "proj_" + hex.EncodeToString(sum[:12])
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = acceptharness.CleanProcessEnv(nil)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// EnsureLayoutPath returns the project directory under the v0.9 layout.
func EnsureLayoutPath(layout home.V09Layout, projectID string) (string, error) {
	if err := layout.EnsureProject(projectID); err != nil {
		return "", err
	}
	return layout.ProjectDir(projectID)
}
