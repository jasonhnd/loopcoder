package registry

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

func TestNormalizeRemoteURL(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		normalized string
		owner      string
		repo       string
		ok         bool
	}{
		{
			name:       "https github",
			raw:        "https://github.com/Owner/Repo.git",
			normalized: "https://github.com/Owner/Repo",
			owner:      "owner",
			repo:       "repo",
			ok:         true,
		},
		{
			name:       "https credentials and query",
			raw:        "https://token@github.com/Owner/Repo.git?x=1#frag",
			normalized: "https://github.com/Owner/Repo",
			owner:      "owner",
			repo:       "repo",
			ok:         true,
		},
		{
			name:       "scp github",
			raw:        "git@github.com:Owner/Repo.git",
			normalized: "ssh://github.com/Owner/Repo",
			owner:      "owner",
			repo:       "repo",
			ok:         true,
		},
		{
			name:       "non github",
			raw:        "ssh://git.example.test:22/team/repo.git",
			normalized: "ssh://git.example.test/team/repo",
			ok:         true,
		},
		{
			name: "invalid",
			raw:  "not a remote",
			ok:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			normalized, owner, repo, ok := NormalizeRemoteURL(tt.raw)
			if normalized != tt.normalized || owner != tt.owner || repo != tt.repo || ok != tt.ok {
				t.Fatalf("NormalizeRemoteURL() = %q %q %q %t, want %q %q %q %t", normalized, owner, repo, ok, tt.normalized, tt.owner, tt.repo, tt.ok)
			}
		})
	}
}

func TestRegisterIsIdempotent(t *testing.T) {
	ctx := context.Background()
	deps, dbPath := testDeps(t, map[string]string{
		"remote\x00get-url\x00origin":  "https://github.com/Owner/Repo.git\n",
		"rev-parse\x00--show-toplevel": filepath.Join(t.TempDir(), "repo") + "\n",
	})
	repo := strings.TrimSpace(deps.git["rev-parse\x00--show-toplevel"])
	makeDir(t, repo)
	opts := Options{RepoPath: repo, DatabasePath: dbPath, Now: fixedRegistryNow}

	first, err := Register(ctx, opts, deps.deps())
	if err != nil {
		t.Fatalf("Register first: %v", err)
	}
	second, err := Register(ctx, opts, deps.deps())
	if err != nil {
		t.Fatalf("Register second: %v", err)
	}
	if !first.Created || first.Updated {
		t.Fatalf("first result = %#v, want created", first)
	}
	if !second.Updated || second.Created {
		t.Fatalf("second result = %#v, want updated", second)
	}
	projects, err := List(ctx, Options{DatabasePath: dbPath}, deps.deps())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("project count = %d, want 1: %#v", len(projects), projects)
	}
	if projects[0].ProjectID != first.Project.ProjectID || projects[0].GitHubOwner != "owner" || projects[0].GitHubName != "repo" {
		t.Fatalf("project = %#v, want stable github identity", projects[0])
	}
}

func TestSameFolderNameDifferentRemotesRemainDistinct(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repoA := filepath.Join(root, "a", "repo")
	repoB := filepath.Join(root, "b", "repo")
	makeDir(t, repoA)
	makeDir(t, repoB)
	_, dbPath := testDeps(t, nil)

	depsA := testRegistryDeps{
		git: map[string]string{
			"rev-parse\x00--show-toplevel": repoA + "\n",
			"remote\x00get-url\x00origin":  "https://github.com/one/repo.git\n",
		},
		dbPath: dbPath,
	}.deps()
	depsB := testRegistryDeps{
		git: map[string]string{
			"rev-parse\x00--show-toplevel": repoB + "\n",
			"remote\x00get-url\x00origin":  "https://github.com/two/repo.git\n",
		},
		dbPath: dbPath,
	}.deps()

	a, err := Register(ctx, Options{RepoPath: repoA, DatabasePath: dbPath, Now: fixedRegistryNow}, depsA)
	if err != nil {
		t.Fatalf("Register A: %v", err)
	}
	b, err := Register(ctx, Options{RepoPath: repoB, DatabasePath: dbPath, Now: fixedRegistryNow}, depsB)
	if err != nil {
		t.Fatalf("Register B: %v", err)
	}
	if a.Project.DisplayName != "repo" || b.Project.DisplayName != "repo" {
		t.Fatalf("display names = %q %q, want same repo", a.Project.DisplayName, b.Project.DisplayName)
	}
	if a.Project.ProjectID == b.Project.ProjectID {
		t.Fatalf("same project id for different remotes: %s", a.Project.ProjectID)
	}
	projects, err := List(ctx, Options{DatabasePath: dbPath}, depsA)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(projects) != 2 {
		t.Fatalf("project count = %d, want 2: %#v", len(projects), projects)
	}
}

func TestShowWorksWhenUnregistered(t *testing.T) {
	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), "repo")
	makeDir(t, repo)
	deps, dbPath := testDeps(t, map[string]string{
		"rev-parse\x00--show-toplevel": repo + "\n",
		"remote\x00get-url\x00origin":  "https://github.com/owner/repo.git\n",
	})

	result, err := Show(ctx, Options{RepoPath: repo, DatabasePath: dbPath}, deps.deps())
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if result.Registered {
		t.Fatalf("Registered = true, want false")
	}
	if result.Project.ProjectID == "" || result.Project.IdentitySource != IdentityGitHub {
		t.Fatalf("candidate = %#v, want github identity", result.Project)
	}
}

func TestRemoveLeavesRunHistory(t *testing.T) {
	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), "repo")
	makeDir(t, repo)
	deps, dbPath := testDeps(t, map[string]string{
		"rev-parse\x00--show-toplevel": repo + "\n",
		"remote\x00get-url\x00origin":  "https://github.com/owner/repo.git\n",
	})
	registered, err := Register(ctx, Options{RepoPath: repo, DatabasePath: dbPath, Now: fixedRegistryNow}, deps.deps())
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	store, err := storage.Open(ctx, storage.Options{Path: dbPath, Now: fixedRegistryNow})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	err = store.WithTx(ctx, func(tx storage.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO runs(id, project_id, status, updated_at) VALUES (?, ?, ?, ?)`, "run-1", registered.Project.ProjectID, "done", "2026-01-01T00:00:00Z")
		return err
	})
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	store.Close()

	removed, err := Remove(ctx, Options{RepoPath: repo, DatabasePath: dbPath}, deps.deps())
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !removed.Removed || removed.RunHistoryDeleted {
		t.Fatalf("Remove result = %#v, want removed without history deletion", removed)
	}
	store, err = storage.Open(ctx, storage.Options{Path: dbPath, Now: fixedRegistryNow})
	if err != nil {
		t.Fatalf("Open after remove: %v", err)
	}
	defer store.Close()
	var projectID string
	if err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, `SELECT COALESCE(project_id, '') FROM runs WHERE id = ?`, "run-1").Scan(&projectID)
	}); err != nil {
		t.Fatalf("query run: %v", err)
	}
	if projectID != "" {
		t.Fatalf("run project_id = %q, want null/empty after project delete", projectID)
	}
}

func TestRegisterFailsOnPathAmbiguity(t *testing.T) {
	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), "repo")
	makeDir(t, repo)
	deps, dbPath := testDeps(t, map[string]string{
		"rev-parse\x00--show-toplevel": repo + "\n",
		"remote\x00get-url\x00origin":  "https://github.com/owner/one.git\n",
	})
	if _, err := Register(ctx, Options{RepoPath: repo, DatabasePath: dbPath, Now: fixedRegistryNow}, deps.deps()); err != nil {
		t.Fatalf("Register first: %v", err)
	}
	deps.git["remote\x00get-url\x00origin"] = "https://github.com/owner/two.git\n"
	_, err := Register(ctx, Options{RepoPath: repo, DatabasePath: dbPath, Now: fixedRegistryNow}, deps.deps())
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("Register changed remote error = %v, want ambiguity", err)
	}
	show, err := Show(ctx, Options{RepoPath: repo, DatabasePath: dbPath}, deps.deps())
	if err != nil {
		t.Fatalf("Show after ambiguity: %v", err)
	}
	if len(show.Conflicts) != 1 {
		t.Fatalf("conflicts = %#v, want one", show.Conflicts)
	}
}

func testDeps(t *testing.T, git map[string]string) (testRegistryDeps, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "data", "loopcoder.db")
	return testRegistryDeps{git: git, dbPath: dbPath}, dbPath
}

type testRegistryDeps struct {
	git    map[string]string
	dbPath string
}

func (d testRegistryDeps) deps() Deps {
	return Deps{
		RunGit: func(_ context.Context, _ string, args ...string) (string, error) {
			key := strings.Join(args, "\x00")
			if value, ok := d.git[key]; ok {
				return value, nil
			}
			return "", errors.New("git fixture not found: " + key)
		},
		LoadConfig: func(string) (config.Config, error) {
			return config.Config{}, errors.New("no config")
		},
		OpenStore: storage.Open,
	}
}

func makeDir(t *testing.T, path string) {
	t.Helper()
	if err := osMkdirAll(path); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

var osMkdirAll = func(path string) error {
	return os.MkdirAll(path, 0o755)
}

func fixedRegistryNow() time.Time {
	return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
}

func TestProjectJSONFieldsAreStable(t *testing.T) {
	project := Project{ProjectID: "proj_1", IdentitySource: IdentityLocalPath}
	got := reflect.TypeOf(project).Field(0).Tag.Get("json")
	if got != "project_id" {
		t.Fatalf("first Project JSON tag = %q, want project_id", got)
	}
}
