// Package runtimepath resolves machine-local runtime payload locations.
package runtimepath

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/jasonhnd/loopcoder/internal/home"
	"github.com/jasonhnd/loopcoder/internal/pathid"
	"github.com/jasonhnd/loopcoder/internal/registry"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

var rootsCache sync.Map

// Roots describes the active runtime payload roots for one invocation.
type Roots struct {
	RepoPath           string
	HomeDir            string
	DatabasePath       string
	Registered         bool
	FallbackMode       string
	ProjectID          string
	ProjectRoot        string
	RunsRoot           string
	RelayRoot          string
	RecoveryRoot       string
	AuditRoot          string
	LogsRoot           string
	TmpRoot            string
	LegacyRoot         string
	LegacyRunsRoot     string
	LegacyRelayRoot    string
	LegacyRecoveryRoot string
}

// Resolve returns global project payload roots when repoPath is registered.
// Unregistered projects deliberately keep v0.6 repo-local payloads so the
// fallback mode is explicit instead of silently weakening registered isolation.
func Resolve(ctx context.Context, repoPath string) (Roots, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	repoPath = strings.TrimSpace(repoPath)
	if repoPath == "" {
		repoPath = "."
	}
	absRepo, err := filepath.Abs(repoPath)
	if err != nil {
		return Roots{}, fmt.Errorf("resolve runtime repo path: %w", err)
	}
	absRepo = filepath.Clean(absRepo)

	layout, err := home.Resolve(home.DefaultDeps())
	if err != nil {
		return Roots{}, err
	}
	roots := legacyRoots(absRepo)
	roots.HomeDir = layout.HomeDir
	roots.DatabasePath = layout.DatabasePath()
	roots.LogsRoot = layout.LogsDir()
	roots.TmpRoot = layout.TmpDir()

	cacheKey := absRepo + "\x00" + roots.DatabasePath
	dbStamp := databaseStamp(roots.DatabasePath)
	if cached, ok := rootsCache.Load(cacheKey); ok {
		entry := cachedRoots(cached)
		if entry.dbStamp == dbStamp {
			return entry.roots, nil
		}
	}

	project, ok := registeredProject(ctx, absRepo, roots.DatabasePath)
	if !ok {
		roots.FallbackMode = "unregistered-repo-local"
		rootsCache.Store(cacheKey, cacheEntry{dbStamp: dbStamp, roots: roots})
		return roots, nil
	}

	roots.Registered = true
	roots.FallbackMode = "registered-global"
	roots.ProjectID = project.ProjectID
	roots.ProjectRoot = layout.ProjectDir(project.ProjectID)
	roots.RunsRoot = filepath.Join(roots.ProjectRoot, "runs")
	roots.RelayRoot = filepath.Join(roots.ProjectRoot, "relay")
	roots.RecoveryRoot = filepath.Join(roots.ProjectRoot, "recovery")
	roots.AuditRoot = filepath.Join(roots.ProjectRoot, "audit")
	rootsCache.Store(cacheKey, cacheEntry{dbStamp: dbStamp, roots: roots})
	return roots, nil
}

type cacheEntry struct {
	dbStamp string
	roots   Roots
}

func cachedRoots(value any) cacheEntry {
	if entry, ok := value.(cacheEntry); ok {
		return entry
	}
	return cacheEntry{}
}

func databaseStamp(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return "missing"
	}
	return fmt.Sprintf("%d:%d", info.ModTime().UnixNano(), info.Size())
}

func legacyRoots(repoPath string) Roots {
	legacyRoot := filepath.Join(repoPath, ".loopcoder")
	return Roots{
		RepoPath:           repoPath,
		FallbackMode:       "unregistered-repo-local",
		RunsRoot:           filepath.Join(legacyRoot, "runs"),
		RelayRoot:          filepath.Join(legacyRoot, "relay"),
		RecoveryRoot:       filepath.Join(legacyRoot, "recovery"),
		AuditRoot:          filepath.Join(legacyRoot, "audit"),
		LegacyRoot:         legacyRoot,
		LegacyRunsRoot:     filepath.Join(legacyRoot, "runs"),
		LegacyRelayRoot:    filepath.Join(legacyRoot, "relay"),
		LegacyRecoveryRoot: filepath.Join(legacyRoot, "runs"),
	}
}

func registeredProject(ctx context.Context, repoPath, dbPath string) (registry.Project, bool) {
	if _, err := os.Stat(dbPath); err != nil {
		return registry.Project{}, false
	}
	project, ok := registeredProjectByPath(ctx, repoPath, dbPath)
	if ok {
		return project, true
	}
	show, err := registry.Show(ctx, registry.Options{RepoPath: repoPath, DatabasePath: dbPath}, registry.DefaultDeps())
	if err == nil && show.Registered {
		return show.Project, true
	}
	return registry.Project{}, false
}

func registeredProjectByPath(ctx context.Context, repoPath, dbPath string) (registry.Project, bool) {
	if _, err := os.Stat(dbPath); err != nil {
		return registry.Project{}, false
	}
	store, err := storage.Open(ctx, storage.Options{Path: dbPath})
	if err != nil {
		return registry.Project{}, false
	}
	defer store.Close()

	candidates := canonicalCandidates(repoPath)
	var project registry.Project
	found := false
	_ = store.WithTx(ctx, func(tx storage.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, display_name, local_path, local_path_canonical, git_root, default_branch, remote_url, remote_url_normalized, github_owner, github_name, identity_source, created_at, updated_at, detached_at FROM projects WHERE detached_at = ''`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var source string
			var candidate registry.Project
			if err := rows.Scan(
				&candidate.ProjectID,
				&candidate.DisplayName,
				&candidate.LocalPath,
				&candidate.LocalPathCanonical,
				&candidate.GitRoot,
				&candidate.DefaultBranch,
				&candidate.RemoteURL,
				&candidate.RemoteURLNormalized,
				&candidate.GitHubOwner,
				&candidate.GitHubName,
				&source,
				&candidate.CreatedAt,
				&candidate.UpdatedAt,
				&candidate.DetachedAt,
			); err != nil {
				return err
			}
			candidate.IdentitySource = registry.IdentitySource(source)
			if projectMatchesPath(candidate, candidates) {
				project = candidate
				found = true
				return nil
			}
		}
		if err := rows.Err(); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		return nil
	})
	return project, found
}

func canonicalCandidates(path string) map[string]bool {
	out := map[string]bool{}
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value != "" {
			out[value] = true
			out[filepath.Clean(value)] = true
		}
	}
	add(path)
	if display, err := pathid.Display(path); err == nil {
		add(display)
	}
	if identity, err := pathid.Identity(path); err == nil {
		add(identity)
	}
	return out
}

func projectMatchesPath(project registry.Project, candidates map[string]bool) bool {
	for _, value := range []string{project.LocalPath, project.LocalPathCanonical, project.GitRoot} {
		if candidates[strings.TrimSpace(value)] || candidates[filepath.Clean(strings.TrimSpace(value))] {
			return true
		}
	}
	return false
}
