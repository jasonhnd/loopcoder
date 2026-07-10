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

func TestRegisterSanitizesRemoteURLBeforePersistence(t *testing.T) {
	ctx := context.Background()
	secret := "loopcoder-sentinel-secret-687"
	repo := filepath.Join(t.TempDir(), "repo")
	makeDir(t, repo)
	deps, dbPath := testDeps(t, map[string]string{
		"rev-parse\x00--show-toplevel": repo + "\n",
		"remote\x00get-url\x00origin":  "https://alice:" + secret + "@github.com/Owner/Repo.git?access_token=" + secret + "#token=" + secret + "\n",
	})

	result, err := Register(ctx, Options{RepoPath: repo, DatabasePath: dbPath, Now: fixedRegistryNow}, deps.deps())
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if result.Project.RemoteURL != "https://github.com/Owner/Repo" || result.Project.RemoteURLNormalized != "https://github.com/Owner/Repo" {
		t.Fatalf("project remote urls = %q %q, want sanitized display and normalized", result.Project.RemoteURL, result.Project.RemoteURLNormalized)
	}
	if strings.Contains(result.Project.RemoteURL, secret) || strings.Contains(result.Project.RemoteURLNormalized, secret) {
		t.Fatalf("project contains secret: %#v", result.Project)
	}

	store, err := storage.Open(ctx, storage.Options{Path: dbPath, Now: fixedRegistryNow})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	var remoteURL, normalized string
	if err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, `SELECT remote_url, remote_url_normalized FROM projects WHERE id = ?`, result.Project.ProjectID).Scan(&remoteURL, &normalized)
	}); err != nil {
		t.Fatalf("query project remote urls: %v", err)
	}
	if remoteURL != "https://github.com/Owner/Repo" || normalized != "https://github.com/Owner/Repo" {
		t.Fatalf("stored remote urls = %q %q, want sanitized", remoteURL, normalized)
	}
}

func TestCredentialRotationKeepsNormalizedIdentityStable(t *testing.T) {
	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), "repo")
	makeDir(t, repo)
	deps, dbPath := testDeps(t, map[string]string{
		"rev-parse\x00--show-toplevel": repo + "\n",
		"remote\x00get-url\x00origin":  "https://alice:first-token@github.com/Owner/Repo.git\n",
	})

	first, err := Register(ctx, Options{RepoPath: repo, DatabasePath: dbPath, Now: fixedRegistryNow}, deps.deps())
	if err != nil {
		t.Fatalf("Register first: %v", err)
	}
	deps.git["remote\x00get-url\x00origin"] = "https://alice:second-token@github.com/Owner/Repo.git\n"
	second, err := Register(ctx, Options{RepoPath: repo, DatabasePath: dbPath, Now: fixedRegistryNow}, deps.deps())
	if err != nil {
		t.Fatalf("Register second: %v", err)
	}
	if first.Project.ProjectID != second.Project.ProjectID || !second.Updated {
		t.Fatalf("identity changed across credential rotation: first=%#v second=%#v", first, second)
	}
}

func TestResolveMalformedRemoteFailsClosedForDisplay(t *testing.T) {
	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), "repo")
	makeDir(t, repo)
	deps, _ := testDeps(t, map[string]string{
		"rev-parse\x00--show-toplevel": repo + "\n",
		"remote\x00get-url\x00origin":  "https://github.com/Owner/%zz.git\n",
	})

	project, err := Resolve(ctx, Options{RepoPath: repo}, deps.deps())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if project.RemoteURL != "" || project.RemoteURLNormalized != "" || project.IdentitySource != IdentityLocalPath {
		t.Fatalf("project = %#v, want malformed remote omitted and local-path identity", project)
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

func TestRemoveDetachesProjectAndPreservesHistory(t *testing.T) {
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
		statements := []struct {
			query string
			args  []any
		}{
			{`INSERT INTO runs(id, project_id, status, updated_at) VALUES ('run-1', ?, 'done', '2026-01-01T00:00:00Z')`, []any{registered.Project.ProjectID}},
			{`INSERT INTO runs(id, project_id, status, updated_at) VALUES ('run-2', ?, 'done', '2026-01-01T00:00:00Z')`, []any{registered.Project.ProjectID}},
			{`INSERT INTO run_events(id, run_id, sequence, ts, event_type) VALUES ('event-1', 'run-1', 1, '2026-01-01T00:00:00Z', 'started')`, nil},
			{`INSERT INTO run_edges(parent_run_id, child_run_id, edge_type, created_at) VALUES ('run-1', 'run-2', 'child', '2026-01-01T00:00:00Z')`, nil},
			{`INSERT INTO reports(id, project_id, run_id, role, provider, model, payload_json, created_at) VALUES ('report-1', ?, 'run-1', 'worker', 'codex', 'gpt-test', '{}', '2026-01-01T00:00:00Z')`, []any{registered.Project.ProjectID}},
			{`INSERT INTO legacy_import_records(id, project_id, run_id, record_type, source_path, source_hash, imported_at) VALUES ('legacy-1', ?, 'run-1', 'event', '.loopcoder/runs/run-1/events.jsonl', 'hash', '2026-01-01T00:00:00Z')`, []any{registered.Project.ProjectID}},
			{`INSERT INTO legacy_import_status(project_id, repo_path, started_at, completed_at, status) VALUES (?, ?, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', 'completed')`, []any{registered.Project.ProjectID, repo}},
		}
		for _, statement := range statements {
			if _, err := tx.Exec(ctx, statement.query, statement.args...); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed history: %v", err)
	}
	store.Close()

	removed, err := Remove(ctx, Options{RepoPath: repo, DatabasePath: dbPath}, deps.deps())
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !removed.Removed || !removed.Detached || removed.ProjectDeleted || removed.RunHistoryDeleted {
		t.Fatalf("Remove result = %#v, want detached without deletion", removed)
	}
	wantPreserved := HistoryCounts{Runs: 2, RunEvents: 1, RunEdges: 1, Reports: 1, LegacyImportRecords: 1, LegacyImportStatus: 1}
	if removed.Preserved != wantPreserved || removed.Deleted != (HistoryCounts{}) {
		t.Fatalf("history counts = preserved:%#v deleted:%#v, want preserved:%#v deleted zero", removed.Preserved, removed.Deleted, wantPreserved)
	}
	if strings.TrimSpace(removed.Project.DetachedAt) == "" {
		t.Fatalf("DetachedAt empty in removed project: %#v", removed.Project)
	}
	projects, err := List(ctx, Options{DatabasePath: dbPath}, deps.deps())
	if err != nil {
		t.Fatalf("List after remove: %v", err)
	}
	if len(projects) != 0 {
		t.Fatalf("active project list after remove = %#v, want empty", projects)
	}
	show, err := Show(ctx, Options{RepoPath: repo, DatabasePath: dbPath}, deps.deps())
	if err != nil {
		t.Fatalf("Show after remove: %v", err)
	}
	if show.Registered || !show.Detached || show.Project.ProjectID != registered.Project.ProjectID {
		t.Fatalf("Show after remove = %#v, want detached preserved identity", show)
	}
	store, err = storage.Open(ctx, storage.Options{Path: dbPath, Now: fixedRegistryNow})
	if err != nil {
		t.Fatalf("Open after remove: %v", err)
	}
	assertHistory := func(label string) {
		t.Helper()
		expected := map[string]int{
			"projects":              1,
			"runs":                  2,
			"run_events":            1,
			"run_edges":             1,
			"reports":               1,
			"legacy_import_records": 1,
			"legacy_import_status":  1,
		}
		for table, want := range expected {
			got := registryTestCount(t, store, `SELECT COUNT(*) FROM `+table)
			if got != want {
				t.Fatalf("%s: %s count = %d, want %d", label, table, got, want)
			}
		}
		for _, query := range []string{
			`SELECT COUNT(*) FROM runs WHERE project_id = ?`,
			`SELECT COUNT(*) FROM reports WHERE project_id = ?`,
			`SELECT COUNT(*) FROM legacy_import_records WHERE project_id = ?`,
			`SELECT COUNT(*) FROM legacy_import_status WHERE project_id = ?`,
		} {
			if got := registryTestCount(t, store, query, registered.Project.ProjectID); got == 0 {
				t.Fatalf("%s: query %q lost project links", label, query)
			}
		}
	}
	assertHistory("after remove")
	store.Close()

	reactivated, err := Register(ctx, Options{RepoPath: repo, DatabasePath: dbPath, Now: fixedRegistryNow}, deps.deps())
	if err != nil {
		t.Fatalf("Register after remove: %v", err)
	}
	if !reactivated.Updated || !reactivated.Reactivated || reactivated.Project.ProjectID != registered.Project.ProjectID {
		t.Fatalf("Register after remove = %#v, want reactivated same identity", reactivated)
	}
	store, err = storage.Open(ctx, storage.Options{Path: dbPath, Now: fixedRegistryNow})
	if err != nil {
		t.Fatalf("Open after re-register: %v", err)
	}
	defer store.Close()
	assertHistory("after re-register")
	var detachedAt string
	if err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, `SELECT detached_at FROM projects WHERE id = ?`, registered.Project.ProjectID).Scan(&detachedAt)
	}); err != nil {
		t.Fatalf("query project detached_at: %v", err)
	}
	if detachedAt != "" {
		t.Fatalf("detached_at after re-register = %q, want empty", detachedAt)
	}
}

func TestRemoveRollsBackWhenDetachFails(t *testing.T) {
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
	if err := store.WithTx(ctx, func(tx storage.Tx) error {
		_, err := tx.Exec(ctx, `CREATE TRIGGER fail_project_detach BEFORE UPDATE OF detached_at ON projects BEGIN SELECT RAISE(FAIL, 'detach blocked'); END`)
		return err
	}); err != nil {
		t.Fatalf("create failing trigger: %v", err)
	}
	store.Close()

	if _, err := Remove(ctx, Options{RepoPath: repo, DatabasePath: dbPath}, deps.deps()); err == nil || !strings.Contains(err.Error(), "detach blocked") {
		t.Fatalf("Remove error = %v, want trigger failure", err)
	}
	store, err = storage.Open(ctx, storage.Options{Path: dbPath, Now: fixedRegistryNow})
	if err != nil {
		t.Fatalf("Open after failed remove: %v", err)
	}
	defer store.Close()
	var detachedAt string
	if err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, `SELECT detached_at FROM projects WHERE id = ?`, registered.Project.ProjectID).Scan(&detachedAt)
	}); err != nil {
		t.Fatalf("query project: %v", err)
	}
	if detachedAt != "" {
		t.Fatalf("detached_at after failed remove = %q, want rollback to active", detachedAt)
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

func registryTestCount(t *testing.T, store storage.Store, query string, args ...any) int {
	t.Helper()
	var count int
	if err := store.WithTx(context.Background(), func(tx storage.Tx) error {
		return tx.QueryRow(context.Background(), query, args...).Scan(&count)
	}); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return count
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
