package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/registry"
	"github.com/jasonhnd/loopcoder/internal/runtimepath"
)

// TestResolveCanonicalProjectID_RegisteredDiffersFromSlug asserts registered
// runtime ProjectID is preferred over repo-slug when they differ.
func TestResolveCanonicalProjectID_RegisteredDiffersFromSlug(t *testing.T) {
	repo := t.TempDir()
	// Minimal git repo for registry.Register.
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run("init")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repo, "README"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "README")
	run("commit", "-m", "init")

	ctx := context.Background()
	registered, err := registry.Register(ctx, registry.Options{RepoPath: repo}, registry.DefaultDeps())
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	regID := strings.TrimSpace(registered.Project.ProjectID)
	if regID == "" {
		t.Fatal("empty registered project id")
	}
	slug := slugProjectFromRepo(repo)
	if regID == slug {
		// Still valid — assert resolve returns registered and matches runtimepath.
		t.Logf("registered id equals slug %q (still assert registered path)", regID)
	}
	got := resolveCanonicalProjectID(repo, repo)
	if got != regID {
		t.Fatalf("canonical project id=%q want registered=%q (slug would be %q)", got, regID, slug)
	}
	roots, err := runtimepath.Resolve(ctx, repo)
	if err != nil || !roots.Registered {
		t.Fatalf("runtimepath: %+v %v", roots, err)
	}
	if got != roots.ProjectID {
		t.Fatalf("canonical %q != runtimepath %q", got, roots.ProjectID)
	}
	// Unregistered path still falls back to slug.
	unreg := t.TempDir()
	gotSlug := resolveCanonicalProjectID(unreg, unreg)
	wantSlug := slugProjectFromRepo(unreg)
	if gotSlug != wantSlug {
		t.Fatalf("unregistered got %q want slug %q", gotSlug, wantSlug)
	}
}
