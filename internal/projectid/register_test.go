package projectid_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/acceptharness"
	"github.com/jasonhnd/loopcoder/internal/home"
	"github.com/jasonhnd/loopcoder/internal/projectid"
)

func fixedNow() time.Time {
	return time.Date(2026, 7, 21, 16, 0, 0, 0, time.UTC)
}

func TestSameShortNameDifferentOwnersGetDifferentIDs(t *testing.T) {
	ctx := context.Background()
	a := initRemoteRepo(t, "owner-a", "app")
	b := initRemoteRepo(t, "owner-b", "app")
	ida, err := projectid.Resolve(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	idb, err := projectid.Resolve(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if ida.ProjectID == idb.ProjectID {
		t.Fatalf("expected different ids for owner-a/app and owner-b/app")
	}
	if ida.Source != projectid.SourceGitHub || idb.Source != projectid.SourceGitHub {
		t.Fatalf("sources = %s / %s", ida.Source, idb.Source)
	}
}

func TestAutoRegisterAndPathMoveAlias(t *testing.T) {
	ctx := context.Background()
	homeDir := filepath.Join(t.TempDir(), "home")
	layout, err := home.EnsureMinimumLayout(homeDir, "")
	if err != nil {
		t.Fatal(err)
	}
	repo := initRemoteRepo(t, "acme", "widget")
	reg, err := projectid.AutoRegister(ctx, layout, repo, fixedNow)
	if err != nil {
		t.Fatalf("AutoRegister: %v", err)
	}
	if !reg.Created {
		t.Fatal("expected created")
	}
	// Second register is update/idempotent
	reg2, err := projectid.AutoRegister(ctx, layout, repo, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if reg2.ProjectID != reg.ProjectID {
		t.Fatalf("id changed")
	}
	// Second checkout of same remote -> same project id, new alias
	moved := filepath.Join(t.TempDir(), "moved-widget")
	if err := copyDir(repo, moved); err != nil {
		t.Fatal(err)
	}
	// Preserve synthetic GitHub remote identity after clone.
	run(t, moved, "git", "remote", "set-url", "origin", "https://github.com/acme/widget.git")
	reg3, err := projectid.AutoRegister(ctx, layout, moved, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if reg3.ProjectID != reg.ProjectID {
		t.Fatalf("moved path got new project id")
	}
	// Repo must remain free of runtime state
	if err := home.ScanRepoForRuntimeState(repo); err != nil {
		t.Fatal(err)
	}
	if err := home.ScanRepoForRuntimeState(moved); err != nil {
		t.Fatal(err)
	}
}

func TestLocalOnlyIdentity(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	run(t, dir, "git", "init", "-b", "main")
	run(t, dir, "git", "config", "user.email", "s@example.invalid")
	run(t, dir, "git", "config", "user.name", "s")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "git", "add", ".")
	run(t, dir, "git", "commit", "-m", "i")
	id, err := projectid.Resolve(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if id.Source != projectid.SourceLocalPath {
		t.Fatalf("source = %s", id.Source)
	}
	if id.ProjectID == "" {
		t.Fatal("empty id")
	}
}

func initRemoteRepo(t *testing.T, owner, name string) string {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, owner+"-"+name+".git")
	run(t, root, "git", "init", "--bare", bare)
	work := filepath.Join(root, "work-"+owner)
	run(t, root, "git", "clone", bare, work)
	run(t, work, "git", "config", "user.email", "s@example.invalid")
	run(t, work, "git", "config", "user.name", "s")
	// Fake github remote form for NormalizeURL
	run(t, work, "git", "remote", "set-url", "origin", "https://github.com/"+owner+"/"+name+".git")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte(owner+"/"+name+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, work, "git", "add", ".")
	run(t, work, "git", "commit", "-m", "init")
	return work
}

func run(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = acceptharness.CleanProcessEnv(nil)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

func copyDir(src, dst string) error {
	// simple clone via git
	cmd := exec.Command("git", "clone", src, dst)
	cmd.Env = acceptharness.CleanProcessEnv(nil)
	return cmd.Run()
}
