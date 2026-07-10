// Package registry manages the machine-local project registry.
package registry

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/gitremote"
	"github.com/jasonhnd/loopcoder/internal/home"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

type IdentitySource string

const (
	IdentityGitHub    IdentitySource = "github"
	IdentityGitRemote IdentitySource = "git-remote"
	IdentityLocalPath IdentitySource = "local-path"
)

type Project struct {
	ProjectID           string         `json:"project_id"`
	DisplayName         string         `json:"display_name"`
	LocalPath           string         `json:"local_path"`
	LocalPathCanonical  string         `json:"local_path_canonical"`
	GitRoot             string         `json:"git_root,omitempty"`
	DefaultBranch       string         `json:"default_branch,omitempty"`
	RemoteURL           string         `json:"remote_url,omitempty"`
	RemoteURLNormalized string         `json:"remote_url_normalized,omitempty"`
	GitHubOwner         string         `json:"github_owner,omitempty"`
	GitHubName          string         `json:"github_name,omitempty"`
	IdentitySource      IdentitySource `json:"identity_source"`
	CreatedAt           string         `json:"created_at,omitempty"`
	UpdatedAt           string         `json:"updated_at,omitempty"`
	DetachedAt          string         `json:"detached_at,omitempty"`
}

type RegisterResult struct {
	Project     Project `json:"project"`
	Created     bool    `json:"created"`
	Updated     bool    `json:"updated"`
	Reactivated bool    `json:"reactivated,omitempty"`
}

type ShowResult struct {
	Registered bool      `json:"registered"`
	Detached   bool      `json:"detached,omitempty"`
	Project    Project   `json:"project"`
	Conflicts  []Project `json:"conflicts,omitempty"`
}

type HistoryCounts struct {
	Runs                int64 `json:"runs"`
	RunEvents           int64 `json:"run_events"`
	RunEdges            int64 `json:"run_edges"`
	Reports             int64 `json:"reports"`
	LegacyImportRecords int64 `json:"legacy_import_records"`
	LegacyImportStatus  int64 `json:"legacy_import_status"`
}

type RemoveResult struct {
	Removed           bool          `json:"removed"`
	Detached          bool          `json:"detached"`
	ProjectDeleted    bool          `json:"project_deleted"`
	Project           Project       `json:"project"`
	DetachedAt        string        `json:"detached_at,omitempty"`
	RunHistoryDeleted bool          `json:"run_history_deleted"`
	Deleted           HistoryCounts `json:"deleted"`
	Preserved         HistoryCounts `json:"preserved"`
}

type Options struct {
	RepoPath     string
	DatabasePath string
	Now          func() time.Time
}

type Deps struct {
	Getenv      func(string) string
	UserHomeDir func() (string, error)
	RunGit      func(context.Context, string, ...string) (string, error)
	LoadConfig  func(string) (config.Config, error)
	OpenStore   func(context.Context, storage.Options) (storage.Store, error)
}

func DefaultDeps() Deps {
	return Deps{
		Getenv:      os.Getenv,
		UserHomeDir: os.UserHomeDir,
		RunGit:      runGit,
		LoadConfig:  config.Load,
		OpenStore:   storage.Open,
	}
}

func Register(ctx context.Context, opts Options, deps Deps) (RegisterResult, error) {
	deps = normalizeDeps(deps)
	store, err := openStore(ctx, opts, deps)
	if err != nil {
		return RegisterResult{}, err
	}
	defer store.Close()

	project, err := Resolve(ctx, opts, deps)
	if err != nil {
		return RegisterResult{}, err
	}
	now := formatTimestamp(normalizeNow(opts.Now)())
	var result RegisterResult
	err = store.WithTx(ctx, func(tx storage.Tx) error {
		conflicts, err := pathConflicts(ctx, tx, project)
		if err != nil {
			return err
		}
		if len(conflicts) > 0 {
			return ambiguousProjectError(project, conflicts)
		}

		existing, ok, err := getProject(ctx, tx, project.ProjectID)
		if err != nil {
			return err
		}
		if ok {
			reactivated := strings.TrimSpace(existing.DetachedAt) != ""
			project.CreatedAt = existing.CreatedAt
			project.UpdatedAt = now
			if err := updateProject(ctx, tx, project); err != nil {
				return err
			}
			result = RegisterResult{Project: project, Updated: true, Reactivated: reactivated}
			return nil
		}
		project.CreatedAt = now
		project.UpdatedAt = now
		if err := insertProject(ctx, tx, project); err != nil {
			return err
		}
		result = RegisterResult{Project: project, Created: true}
		return nil
	})
	if err != nil {
		return RegisterResult{}, err
	}
	return result, nil
}

func List(ctx context.Context, opts Options, deps Deps) ([]Project, error) {
	deps = normalizeDeps(deps)
	store, err := openStore(ctx, opts, deps)
	if err != nil {
		return nil, err
	}
	defer store.Close()

	var projects []Project
	err = store.WithTx(ctx, func(tx storage.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+projectSelectColumns+` FROM projects WHERE detached_at = '' ORDER BY display_name, id`)
		if err != nil {
			return fmt.Errorf("list projects: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			project, err := scanProject(rows)
			if err != nil {
				return err
			}
			projects = append(projects, project)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return projects, nil
}

func Show(ctx context.Context, opts Options, deps Deps) (ShowResult, error) {
	deps = normalizeDeps(deps)
	project, err := Resolve(ctx, opts, deps)
	if err != nil {
		return ShowResult{}, err
	}
	store, err := openStore(ctx, opts, deps)
	if err != nil {
		return ShowResult{}, err
	}
	defer store.Close()

	result := ShowResult{Project: project}
	err = store.WithTx(ctx, func(tx storage.Tx) error {
		conflicts, err := pathConflicts(ctx, tx, project)
		if err != nil {
			return err
		}
		result.Conflicts = conflicts
		existing, ok, err := getProject(ctx, tx, project.ProjectID)
		if err != nil {
			return err
		}
		if ok {
			result.Registered = strings.TrimSpace(existing.DetachedAt) == ""
			result.Detached = strings.TrimSpace(existing.DetachedAt) != ""
			result.Project = existing
		}
		return nil
	})
	if err != nil {
		return ShowResult{}, err
	}
	return result, nil
}

func Remove(ctx context.Context, opts Options, deps Deps) (RemoveResult, error) {
	deps = normalizeDeps(deps)
	project, err := Resolve(ctx, opts, deps)
	if err != nil {
		return RemoveResult{}, err
	}
	store, err := openStore(ctx, opts, deps)
	if err != nil {
		return RemoveResult{}, err
	}
	defer store.Close()

	result := RemoveResult{Project: project, RunHistoryDeleted: false}
	err = store.WithTx(ctx, func(tx storage.Tx) error {
		existing, ok, err := getProject(ctx, tx, project.ProjectID)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		result.Project = existing
		if strings.TrimSpace(existing.DetachedAt) != "" {
			result.Detached = true
			result.DetachedAt = existing.DetachedAt
			return nil
		}
		counts, err := historyCounts(ctx, tx, existing.ProjectID)
		if err != nil {
			return err
		}
		detachedAt := formatTimestamp(normalizeNow(opts.Now)())
		if _, err := tx.Exec(ctx, `UPDATE projects SET detached_at = ?, updated_at = ? WHERE id = ?`, detachedAt, detachedAt, existing.ProjectID); err != nil {
			return fmt.Errorf("remove project: %w", err)
		}
		existing.DetachedAt = detachedAt
		existing.UpdatedAt = detachedAt
		result.Project = existing
		result.Removed = true
		result.Detached = true
		result.ProjectDeleted = false
		result.DetachedAt = detachedAt
		result.RunHistoryDeleted = false
		result.Preserved = counts
		return nil
	})
	if err != nil {
		return RemoveResult{}, err
	}
	return result, nil
}

func Resolve(ctx context.Context, opts Options, deps Deps) (Project, error) {
	deps = normalizeDeps(deps)
	repoPath := strings.TrimSpace(opts.RepoPath)
	if repoPath == "" {
		repoPath = "."
	}
	absPath, err := filepath.Abs(repoPath)
	if err != nil {
		return Project{}, fmt.Errorf("resolve project path: %w", err)
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return Project{}, fmt.Errorf("resolve project path: %w", err)
	}
	if !info.IsDir() {
		return Project{}, fmt.Errorf("project path is not a directory: %s", absPath)
	}
	absPath = filepath.Clean(absPath)

	gitRoot, _ := gitOutput(ctx, deps, absPath, "rev-parse", "--show-toplevel")
	gitRoot = strings.TrimSpace(gitRoot)
	if gitRoot != "" {
		if root, err := filepath.Abs(gitRoot); err == nil {
			gitRoot = filepath.Clean(root)
		}
	}
	identityPath := absPath
	if gitRoot != "" {
		identityPath = gitRoot
	}

	remoteURL, _ := gitOutput(ctx, deps, identityPath, "remote", "get-url", "origin")
	remoteURL = strings.TrimSpace(remoteURL)
	normalized, owner, name, _ := NormalizeRemoteURL(remoteURL)
	remoteURLDisplay, _ := gitremote.SanitizeDisplayURL(remoteURL)

	displayName := filepath.Base(identityPath)
	source := IdentityLocalPath
	identityKey := "local-path:" + canonicalPath(identityPath)
	if normalized != "" {
		source = IdentityGitRemote
		identityKey = "git-remote:" + normalized
		displayName = remoteDisplayName(normalized, displayName)
	}
	if owner != "" && name != "" {
		source = IdentityGitHub
		identityKey = "github:" + strings.ToLower(owner) + "/" + strings.ToLower(name)
		displayName = name
	}

	project := Project{
		ProjectID:           projectID(identityKey),
		DisplayName:         displayName,
		LocalPath:           absPath,
		LocalPathCanonical:  canonicalPath(identityPath),
		GitRoot:             gitRoot,
		DefaultBranch:       defaultBranch(ctx, deps, identityPath),
		RemoteURL:           remoteURLDisplay,
		RemoteURLNormalized: normalized,
		GitHubOwner:         owner,
		GitHubName:          name,
		IdentitySource:      source,
	}
	if strings.TrimSpace(project.DisplayName) == "" || project.DisplayName == "." || project.DisplayName == string(filepath.Separator) {
		project.DisplayName = "project"
	}
	return project, nil
}

func NormalizeRemoteURL(raw string) (normalized string, githubOwner string, githubName string, ok bool) {
	return gitremote.NormalizeURL(raw)
}

func openStore(ctx context.Context, opts Options, deps Deps) (storage.Store, error) {
	path, err := databasePath(opts, deps)
	if err != nil {
		return nil, err
	}
	return deps.OpenStore(ctx, storage.Options{Path: path, Now: opts.Now})
}

func databasePath(opts Options, deps Deps) (string, error) {
	if path := strings.TrimSpace(opts.DatabasePath); path != "" {
		return filepath.Clean(path), nil
	}
	layout, err := home.Resolve(home.Deps{Getenv: deps.Getenv, UserHomeDir: deps.UserHomeDir})
	if err != nil {
		return "", err
	}
	return layout.DatabasePath(), nil
}

const projectSelectColumns = `id, display_name, local_path, local_path_canonical, git_root, default_branch, remote_url, remote_url_normalized, github_owner, github_name, identity_source, created_at, updated_at, detached_at`

func getProject(ctx context.Context, tx storage.Tx, id string) (Project, bool, error) {
	row := tx.QueryRow(ctx, `SELECT `+projectSelectColumns+` FROM projects WHERE id = ?`, id)
	project, err := scanProject(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, false, nil
	}
	if err != nil {
		return Project{}, false, err
	}
	return project, true, nil
}

func pathConflicts(ctx context.Context, tx storage.Tx, project Project) ([]Project, error) {
	if strings.TrimSpace(project.LocalPathCanonical) == "" {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `SELECT `+projectSelectColumns+` FROM projects WHERE local_path_canonical = ? AND id <> ? ORDER BY id`, project.LocalPathCanonical, project.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("inspect project conflicts: %w", err)
	}
	defer rows.Close()
	var conflicts []Project
	for rows.Next() {
		conflict, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		conflicts = append(conflicts, conflict)
	}
	return conflicts, rows.Err()
}

func insertProject(ctx context.Context, tx storage.Tx, project Project) error {
	project = sanitizeProject(project)
	_, err := tx.Exec(ctx, `INSERT INTO projects(id, display_name, local_path, local_path_canonical, git_root, default_branch, remote_url, remote_url_normalized, github_owner, github_name, identity_source, created_at, updated_at, detached_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		project.ProjectID, project.DisplayName, project.LocalPath, project.LocalPathCanonical, project.GitRoot, project.DefaultBranch, project.RemoteURL, project.RemoteURLNormalized, project.GitHubOwner, project.GitHubName, string(project.IdentitySource), project.CreatedAt, project.UpdatedAt, project.DetachedAt)
	if err != nil {
		return fmt.Errorf("insert project: %w", err)
	}
	return nil
}

func updateProject(ctx context.Context, tx storage.Tx, project Project) error {
	project = sanitizeProject(project)
	_, err := tx.Exec(ctx, `UPDATE projects SET display_name = ?, local_path = ?, local_path_canonical = ?, git_root = ?, default_branch = ?, remote_url = ?, remote_url_normalized = ?, github_owner = ?, github_name = ?, identity_source = ?, updated_at = ?, detached_at = ? WHERE id = ?`,
		project.DisplayName, project.LocalPath, project.LocalPathCanonical, project.GitRoot, project.DefaultBranch, project.RemoteURL, project.RemoteURLNormalized, project.GitHubOwner, project.GitHubName, string(project.IdentitySource), project.UpdatedAt, project.DetachedAt, project.ProjectID)
	if err != nil {
		return fmt.Errorf("update project: %w", err)
	}
	return nil
}

func historyCounts(ctx context.Context, tx storage.Tx, projectID string) (HistoryCounts, error) {
	count := func(query string, args ...any) (int64, error) {
		var value int64
		if err := tx.QueryRow(ctx, query, args...).Scan(&value); err != nil {
			return 0, err
		}
		return value, nil
	}
	var counts HistoryCounts
	var err error
	if counts.Runs, err = count(`SELECT COUNT(*) FROM runs WHERE project_id = ?`, projectID); err != nil {
		return HistoryCounts{}, fmt.Errorf("count preserved runs: %w", err)
	}
	if counts.RunEvents, err = count(`SELECT COUNT(*) FROM run_events WHERE run_id IN (SELECT id FROM runs WHERE project_id = ?)`, projectID); err != nil {
		return HistoryCounts{}, fmt.Errorf("count preserved run events: %w", err)
	}
	if counts.RunEdges, err = count(`SELECT COUNT(*) FROM run_edges WHERE parent_run_id IN (SELECT id FROM runs WHERE project_id = ?) OR child_run_id IN (SELECT id FROM runs WHERE project_id = ?)`, projectID, projectID); err != nil {
		return HistoryCounts{}, fmt.Errorf("count preserved run edges: %w", err)
	}
	if counts.Reports, err = count(`SELECT COUNT(DISTINCT id) FROM reports WHERE project_id = ? OR run_id IN (SELECT id FROM runs WHERE project_id = ?)`, projectID, projectID); err != nil {
		return HistoryCounts{}, fmt.Errorf("count preserved reports: %w", err)
	}
	if counts.LegacyImportRecords, err = count(`SELECT COUNT(*) FROM legacy_import_records WHERE project_id = ?`, projectID); err != nil {
		return HistoryCounts{}, fmt.Errorf("count preserved legacy import records: %w", err)
	}
	if counts.LegacyImportStatus, err = count(`SELECT COUNT(*) FROM legacy_import_status WHERE project_id = ?`, projectID); err != nil {
		return HistoryCounts{}, fmt.Errorf("count preserved legacy import status: %w", err)
	}
	return counts, nil
}

type projectScanner interface {
	Scan(...any) error
}

func scanProject(scanner projectScanner) (Project, error) {
	var project Project
	var source string
	if err := scanner.Scan(
		&project.ProjectID,
		&project.DisplayName,
		&project.LocalPath,
		&project.LocalPathCanonical,
		&project.GitRoot,
		&project.DefaultBranch,
		&project.RemoteURL,
		&project.RemoteURLNormalized,
		&project.GitHubOwner,
		&project.GitHubName,
		&source,
		&project.CreatedAt,
		&project.UpdatedAt,
		&project.DetachedAt,
	); err != nil {
		return Project{}, err
	}
	project.IdentitySource = IdentitySource(source)
	project = sanitizeProject(project)
	return project, nil
}

func sanitizeProject(project Project) Project {
	if display, ok := gitremote.SanitizeDisplayURL(project.RemoteURL); ok {
		project.RemoteURL = display
	} else {
		project.RemoteURL = ""
	}
	if normalized, owner, name, ok := gitremote.NormalizeURL(project.RemoteURLNormalized); ok {
		project.RemoteURLNormalized = normalized
		if owner != "" && name != "" {
			project.GitHubOwner = owner
			project.GitHubName = name
		}
	} else if normalized, owner, name, ok := gitremote.NormalizeURL(project.RemoteURL); ok {
		project.RemoteURLNormalized = normalized
		if owner != "" && name != "" {
			project.GitHubOwner = owner
			project.GitHubName = name
		}
	} else {
		project.RemoteURLNormalized = ""
	}
	return project
}

func defaultBranch(ctx context.Context, deps Deps, repoPath string) string {
	if cfg, err := deps.LoadConfig(filepath.Join(repoPath, ".delivery.yml")); err == nil {
		if branch := strings.TrimSpace(cfg.Worker.BaseBranch); branch != "" {
			return branch
		}
	}
	for _, args := range [][]string{
		{"symbolic-ref", "--short", "refs/remotes/origin/HEAD"},
		{"rev-parse", "--abbrev-ref", "HEAD"},
	} {
		out, err := gitOutput(ctx, deps, repoPath, args...)
		if err != nil {
			continue
		}
		branch := strings.TrimSpace(out)
		branch = strings.TrimPrefix(branch, "origin/")
		if branch != "" && branch != "HEAD" {
			return branch
		}
	}
	return ""
}

func remoteDisplayName(normalized, fallback string) string {
	u, err := url.Parse(normalized)
	if err != nil {
		return fallback
	}
	path := strings.Trim(u.Path, "/")
	if path == "" {
		return fallback
	}
	parts := strings.Split(path, "/")
	name := parts[len(parts)-1]
	if name != "" {
		return name
	}
	return fallback
}

func projectID(identityKey string) string {
	sum := sha256.Sum256([]byte(identityKey))
	return "proj_" + hex.EncodeToString(sum[:])[:16]
}

func canonicalPath(path string) string {
	clean := filepath.Clean(path)
	if abs, err := filepath.Abs(clean); err == nil {
		clean = filepath.Clean(abs)
	}
	if runtime.GOOS == "windows" {
		clean = strings.ToLower(clean)
	}
	return clean
}

func gitOutput(ctx context.Context, deps Deps, repoPath string, args ...string) (string, error) {
	if strings.TrimSpace(repoPath) == "" {
		repoPath = "."
	}
	return deps.RunGit(ctx, repoPath, args...)
}

func runGit(ctx context.Context, repoPath string, args ...string) (string, error) {
	cmdArgs := append([]string{"-C", repoPath}, args...)
	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func ambiguousProjectError(project Project, conflicts []Project) error {
	ids := make([]string, 0, len(conflicts))
	for _, conflict := range conflicts {
		ids = append(ids, conflict.ProjectID)
	}
	sort.Strings(ids)
	return fmt.Errorf("project identity is ambiguous: local path %s is already registered to project(s) %s; remove or inspect the conflicting registry entry before registering %s", project.LocalPathCanonical, strings.Join(ids, ", "), project.ProjectID)
}

func normalizeDeps(deps Deps) Deps {
	defaults := DefaultDeps()
	if deps.Getenv == nil {
		deps.Getenv = defaults.Getenv
	}
	if deps.UserHomeDir == nil {
		deps.UserHomeDir = defaults.UserHomeDir
	}
	if deps.RunGit == nil {
		deps.RunGit = defaults.RunGit
	}
	if deps.LoadConfig == nil {
		deps.LoadConfig = defaults.LoadConfig
	}
	if deps.OpenStore == nil {
		deps.OpenStore = defaults.OpenStore
	}
	return deps
}

func normalizeNow(now func() time.Time) func() time.Time {
	if now == nil {
		return time.Now
	}
	return now
}

func formatTimestamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}
