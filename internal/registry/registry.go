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
	"sort"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/gitremote"
	"github.com/jasonhnd/loopcoder/internal/home"
	"github.com/jasonhnd/loopcoder/internal/pathid"
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
		if project.IdentitySource == IdentityLocalPath {
			if err := repairPhysicalIdentityForCanonical(ctx, tx, project.LocalPathCanonical, project.ProjectID); err != nil {
				return err
			}
		}
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
	if err := ensurePayloadDirs(result.Project.ProjectID, opts, deps); err != nil {
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
	canonical, err := pathid.Canonicalize(repoPath)
	if err != nil {
		return Project{}, fmt.Errorf("resolve project path: %w", err)
	}
	absPath := canonical.Display
	info, err := os.Stat(absPath)
	if err != nil {
		return Project{}, fmt.Errorf("resolve project path: %w", err)
	}
	if !info.IsDir() {
		return Project{}, fmt.Errorf("project path is not a directory: %s", absPath)
	}

	gitRoot, _ := gitOutput(ctx, deps, absPath, "rev-parse", "--show-toplevel")
	gitRoot = strings.TrimSpace(gitRoot)
	if gitRoot != "" {
		if root, err := pathid.Display(gitRoot); err == nil {
			gitRoot = root
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
	identityCanonical := canonicalPath(identityPath)
	identityKey := "local-path:" + identityCanonical
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
		LocalPathCanonical:  identityCanonical,
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

func ensurePayloadDirs(projectID string, opts Options, deps Deps) error {
	if strings.TrimSpace(projectID) == "" {
		return nil
	}
	layout, err := layoutForPayloadDirs(opts, deps)
	if err != nil {
		return err
	}
	for _, dir := range []string{
		filepath.Join(layout.ProjectDir(projectID), "runs"),
		filepath.Join(layout.ProjectDir(projectID), "relay"),
		filepath.Join(layout.ProjectDir(projectID), "recovery"),
		filepath.Join(layout.ProjectDir(projectID), "audit"),
		layout.LogsDir(),
		layout.TmpDir(),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create project payload directory %s: %w", dir, err)
		}
	}
	return nil
}

func layoutForPayloadDirs(opts Options, deps Deps) (home.Layout, error) {
	if dbPath := strings.TrimSpace(opts.DatabasePath); dbPath != "" {
		dbPath = filepath.Clean(dbPath)
		dataDir := filepath.Dir(dbPath)
		if filepath.Base(dbPath) == "loopcoder.db" && filepath.Base(dataDir) == "data" {
			return home.New(filepath.Dir(dataDir)), nil
		}
	}
	return home.Resolve(home.Deps{Getenv: deps.Getenv, UserHomeDir: deps.UserHomeDir})
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
	identity, err := pathid.Identity(path)
	if err != nil {
		clean := filepath.Clean(path)
		if abs, absErr := filepath.Abs(clean); absErr == nil {
			clean = filepath.Clean(abs)
		}
		return clean
	}
	return identity
}

// DuplicatePhysicalIdentity is a set of registry rows that resolve to one
// physical local path identity.
type DuplicatePhysicalIdentity struct {
	Canonical string    `json:"canonical"`
	Projects  []Project `json:"projects"`
}

// DuplicatePhysicalIdentities detects duplicate physical project identities.
func DuplicatePhysicalIdentities(ctx context.Context, opts Options, deps Deps) ([]DuplicatePhysicalIdentity, error) {
	deps = normalizeDeps(deps)
	store, err := openStore(ctx, opts, deps)
	if err != nil {
		return nil, err
	}
	defer store.Close()

	var groups []DuplicatePhysicalIdentity
	err = store.WithTx(ctx, func(tx storage.Tx) error {
		projects, err := allProjects(ctx, tx)
		if err != nil {
			return err
		}
		byCanonical := groupProjectsByPhysicalIdentity(projects)
		for canonical, projects := range byCanonical {
			if len(projects) < 2 {
				continue
			}
			groups = append(groups, DuplicatePhysicalIdentity{Canonical: canonical, Projects: projects})
		}
		sort.Slice(groups, func(i, j int) bool {
			return groups[i].Canonical < groups[j].Canonical
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return groups, nil
}

// RepairDuplicatePhysicalIdentities reconciles duplicate physical identities
// without deleting run, report, or import history.
func RepairDuplicatePhysicalIdentities(ctx context.Context, opts Options, deps Deps) ([]DuplicatePhysicalIdentity, error) {
	deps = normalizeDeps(deps)
	store, err := openStore(ctx, opts, deps)
	if err != nil {
		return nil, err
	}
	defer store.Close()

	var repaired []DuplicatePhysicalIdentity
	err = store.WithTx(ctx, func(tx storage.Tx) error {
		projects, err := allProjects(ctx, tx)
		if err != nil {
			return err
		}
		byCanonical := groupProjectsByPhysicalIdentity(projects)
		for canonical, projects := range byCanonical {
			if len(projects) < 2 {
				continue
			}
			repaired = append(repaired, DuplicatePhysicalIdentity{Canonical: canonical, Projects: projects})
			if err := repairProjectGroup(ctx, tx, canonical, projects, ""); err != nil {
				return err
			}
		}
		sort.Slice(repaired, func(i, j int) bool {
			return repaired[i].Canonical < repaired[j].Canonical
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return repaired, nil
}

func repairPhysicalIdentityForCanonical(ctx context.Context, tx storage.Tx, canonical, preferredID string) error {
	canonical = strings.TrimSpace(canonical)
	if canonical == "" {
		return nil
	}
	projects, err := allProjects(ctx, tx)
	if err != nil {
		return err
	}
	byCanonical := groupProjectsByPhysicalIdentity(projects)
	group := byCanonical[canonical]
	if len(group) == 0 {
		return nil
	}
	needsRepair := len(group) > 1
	for _, project := range group {
		if project.LocalPathCanonical != canonical || (preferredID != "" && project.ProjectID != preferredID) {
			needsRepair = true
			break
		}
	}
	if !needsRepair {
		return nil
	}
	return repairProjectGroup(ctx, tx, canonical, group, preferredID)
}

func allProjects(ctx context.Context, tx storage.Tx) ([]Project, error) {
	rows, err := tx.Query(ctx, `SELECT `+projectSelectColumns+` FROM projects ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list registry projects for physical identity repair: %w", err)
	}
	defer rows.Close()
	var projects []Project
	for rows.Next() {
		project, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		projects = append(projects, project)
	}
	return projects, rows.Err()
}

func groupProjectsByPhysicalIdentity(projects []Project) map[string][]Project {
	byCanonical := map[string][]Project{}
	for _, project := range projects {
		candidate := firstNonEmpty(project.GitRoot, project.LocalPathCanonical, project.LocalPath)
		canonical := canonicalPath(candidate)
		if strings.TrimSpace(canonical) == "" {
			continue
		}
		project.LocalPathCanonical = canonical
		byCanonical[canonical] = append(byCanonical[canonical], project)
	}
	for canonical := range byCanonical {
		sortProjectsForRepair(byCanonical[canonical])
	}
	return byCanonical
}

func repairProjectGroup(ctx context.Context, tx storage.Tx, canonical string, projects []Project, preferredID string) error {
	if len(projects) == 0 {
		return nil
	}
	sortProjectsForRepair(projects)
	survivor := projects[0]
	if preferredID != "" {
		for _, project := range projects {
			if project.ProjectID == preferredID {
				survivor = project
				break
			}
		}
	}
	if preferredID != "" && survivor.ProjectID != preferredID {
		if err := cloneProject(ctx, tx, survivor.ProjectID, preferredID); err != nil {
			return err
		}
		oldID := survivor.ProjectID
		survivor.ProjectID = preferredID
		if err := mergeProjectHistory(ctx, tx, oldID, preferredID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE projects SET local_path_canonical = ?, detached_at = CASE WHEN id = ? THEN '' ELSE detached_at END WHERE id = ?`, canonical, survivor.ProjectID, survivor.ProjectID); err != nil {
		return fmt.Errorf("update survivor project identity: %w", err)
	}
	for _, project := range projects {
		if project.ProjectID == survivor.ProjectID {
			continue
		}
		if err := mergeProjectHistory(ctx, tx, project.ProjectID, survivor.ProjectID); err != nil {
			return err
		}
	}
	return nil
}

func sortProjectsForRepair(projects []Project) {
	sort.Slice(projects, func(i, j int) bool {
		leftDetached := strings.TrimSpace(projects[i].DetachedAt) != ""
		rightDetached := strings.TrimSpace(projects[j].DetachedAt) != ""
		if leftDetached != rightDetached {
			return !leftDetached
		}
		if projects[i].CreatedAt != projects[j].CreatedAt {
			return projects[i].CreatedAt < projects[j].CreatedAt
		}
		return projects[i].ProjectID < projects[j].ProjectID
	})
}

func cloneProject(ctx context.Context, tx storage.Tx, fromID, toID string) error {
	if fromID == toID {
		return nil
	}
	if _, err := tx.Exec(ctx, `INSERT OR IGNORE INTO projects(id, display_name, local_path, local_path_canonical, git_root, default_branch, remote_url, remote_url_normalized, github_owner, github_name, identity_source, created_at, updated_at, detached_at)
		SELECT ?, display_name, local_path, local_path_canonical, git_root, default_branch, remote_url, remote_url_normalized, github_owner, github_name, identity_source, created_at, updated_at, detached_at FROM projects WHERE id = ?`, toID, fromID); err != nil {
		return fmt.Errorf("clone project identity %s to %s: %w", fromID, toID, err)
	}
	return nil
}

func mergeProjectHistory(ctx context.Context, tx storage.Tx, fromID, toID string) error {
	if fromID == toID {
		return nil
	}
	for _, statement := range []string{
		`UPDATE runs SET project_id = ? WHERE project_id = ?`,
		`UPDATE reports SET project_id = ? WHERE project_id = ?`,
		`UPDATE OR IGNORE legacy_import_records SET project_id = ? WHERE project_id = ?`,
		`DELETE FROM legacy_import_records WHERE project_id = ?`,
	} {
		args := []any{toID, fromID}
		if strings.HasPrefix(statement, `DELETE`) {
			args = []any{fromID}
		}
		if _, err := tx.Exec(ctx, statement, args...); err != nil {
			return fmt.Errorf("merge project history %s into %s: %w", fromID, toID, err)
		}
	}
	if err := mergeLegacyImportStatus(ctx, tx, fromID, toID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM projects WHERE id = ?`, fromID); err != nil {
		return fmt.Errorf("remove duplicate project %s after history merge: %w", fromID, err)
	}
	return nil
}

func mergeLegacyImportStatus(ctx context.Context, tx storage.Tx, fromID, toID string) error {
	var survivorCount int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM legacy_import_status WHERE project_id = ?`, toID).Scan(&survivorCount); err != nil {
		return err
	}
	if survivorCount == 0 {
		if _, err := tx.Exec(ctx, `UPDATE legacy_import_status SET project_id = ? WHERE project_id = ?`, toID, fromID); err != nil {
			return fmt.Errorf("reassign legacy import status %s into %s: %w", fromID, toID, err)
		}
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE legacy_import_status SET
		scanned_count = scanned_count + COALESCE((SELECT scanned_count FROM legacy_import_status WHERE project_id = ?), 0),
		imported_count = imported_count + COALESCE((SELECT imported_count FROM legacy_import_status WHERE project_id = ?), 0),
		skipped_count = skipped_count + COALESCE((SELECT skipped_count FROM legacy_import_status WHERE project_id = ?), 0),
		malformed_count = malformed_count + COALESCE((SELECT malformed_count FROM legacy_import_status WHERE project_id = ?), 0),
		message = CASE WHEN message = '' THEN 'reconciled duplicate physical project identity' ELSE message || '; reconciled duplicate physical project identity' END
		WHERE project_id = ?`, fromID, fromID, fromID, fromID, toID); err != nil {
		return fmt.Errorf("merge legacy import status %s into %s: %w", fromID, toID, err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM legacy_import_status WHERE project_id = ?`, fromID); err != nil {
		return fmt.Errorf("remove duplicate legacy import status %s: %w", fromID, err)
	}
	return nil
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

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
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
