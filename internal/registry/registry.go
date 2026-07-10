// Package registry manages the machine-local project registry.
package registry

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/config"
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
}

type RegisterResult struct {
	Project Project `json:"project"`
	Created bool    `json:"created"`
	Updated bool    `json:"updated"`
}

type ShowResult struct {
	Registered bool      `json:"registered"`
	Project    Project   `json:"project"`
	Conflicts  []Project `json:"conflicts,omitempty"`
}

type RemoveResult struct {
	Removed           bool    `json:"removed"`
	Project           Project `json:"project"`
	RunHistoryDeleted bool    `json:"run_history_deleted"`
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
			project.CreatedAt = existing.CreatedAt
			project.UpdatedAt = now
			if err := updateProject(ctx, tx, project); err != nil {
				return err
			}
			result = RegisterResult{Project: project, Updated: true}
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
		rows, err := tx.Query(ctx, `SELECT id, display_name, local_path, local_path_canonical, git_root, default_branch, remote_url, remote_url_normalized, github_owner, github_name, identity_source, created_at, updated_at FROM projects ORDER BY display_name, id`)
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
			result.Registered = true
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

	result := RemoveResult{Project: project}
	err = store.WithTx(ctx, func(tx storage.Tx) error {
		existing, ok, err := getProject(ctx, tx, project.ProjectID)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		if _, err := tx.Exec(ctx, `DELETE FROM projects WHERE id = ?`, existing.ProjectID); err != nil {
			return fmt.Errorf("remove project: %w", err)
		}
		result.Project = existing
		result.Removed = true
		result.RunHistoryDeleted = false
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
		RemoteURL:           remoteURL,
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
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", "", false
	}
	if strings.HasPrefix(raw, "git@") && strings.Contains(raw, ":") && !strings.Contains(raw, "://") {
		parts := strings.SplitN(strings.TrimPrefix(raw, "git@"), ":", 2)
		if len(parts) == 2 {
			raw = "ssh://" + parts[0] + "/" + parts[1]
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", "", "", false
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", "", "", false
	}
	port := u.Port()
	if port != "" && !isDefaultPort(scheme, port) {
		host = net.JoinHostPort(host, port)
	}
	path := cleanURLPath(u.EscapedPath())
	if path == "" {
		return "", "", "", false
	}
	normalized = scheme + "://" + host + "/" + path
	if strings.EqualFold(host, "github.com") {
		parts := strings.Split(path, "/")
		if len(parts) >= 2 && parts[0] != "" && parts[1] != "" {
			githubOwner = strings.ToLower(parts[0])
			githubName = strings.ToLower(parts[1])
		}
	}
	return normalized, githubOwner, githubName, true
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

func getProject(ctx context.Context, tx storage.Tx, id string) (Project, bool, error) {
	row := tx.QueryRow(ctx, `SELECT id, display_name, local_path, local_path_canonical, git_root, default_branch, remote_url, remote_url_normalized, github_owner, github_name, identity_source, created_at, updated_at FROM projects WHERE id = ?`, id)
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
	rows, err := tx.Query(ctx, `SELECT id, display_name, local_path, local_path_canonical, git_root, default_branch, remote_url, remote_url_normalized, github_owner, github_name, identity_source, created_at, updated_at FROM projects WHERE local_path_canonical = ? AND id <> ? ORDER BY id`, project.LocalPathCanonical, project.ProjectID)
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
	_, err := tx.Exec(ctx, `INSERT INTO projects(id, display_name, local_path, local_path_canonical, git_root, default_branch, remote_url, remote_url_normalized, github_owner, github_name, identity_source, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		project.ProjectID, project.DisplayName, project.LocalPath, project.LocalPathCanonical, project.GitRoot, project.DefaultBranch, project.RemoteURL, project.RemoteURLNormalized, project.GitHubOwner, project.GitHubName, string(project.IdentitySource), project.CreatedAt, project.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insert project: %w", err)
	}
	return nil
}

func updateProject(ctx context.Context, tx storage.Tx, project Project) error {
	_, err := tx.Exec(ctx, `UPDATE projects SET display_name = ?, local_path = ?, local_path_canonical = ?, git_root = ?, default_branch = ?, remote_url = ?, remote_url_normalized = ?, github_owner = ?, github_name = ?, identity_source = ?, updated_at = ? WHERE id = ?`,
		project.DisplayName, project.LocalPath, project.LocalPathCanonical, project.GitRoot, project.DefaultBranch, project.RemoteURL, project.RemoteURLNormalized, project.GitHubOwner, project.GitHubName, string(project.IdentitySource), project.UpdatedAt, project.ProjectID)
	if err != nil {
		return fmt.Errorf("update project: %w", err)
	}
	return nil
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
	); err != nil {
		return Project{}, err
	}
	project.IdentitySource = IdentitySource(source)
	return project, nil
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

func cleanURLPath(path string) string {
	path, _ = url.PathUnescape(path)
	path = strings.ReplaceAll(path, `\`, "/")
	parts := make([]string, 0)
	for _, part := range strings.Split(path, "/") {
		if part == "" || part == "." {
			continue
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return ""
	}
	last := parts[len(parts)-1]
	if strings.HasSuffix(strings.ToLower(last), ".git") {
		parts[len(parts)-1] = last[:len(last)-4]
	}
	return strings.Join(parts, "/")
}

func isDefaultPort(scheme, port string) bool {
	switch strings.ToLower(scheme) {
	case "http":
		return port == "80"
	case "https":
		return port == "443"
	case "ssh":
		return port == "22"
	default:
		return false
	}
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
