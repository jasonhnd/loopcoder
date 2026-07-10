package audit

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/home"
	"github.com/jasonhnd/loopcoder/internal/registry"
	"github.com/jasonhnd/loopcoder/internal/state"
)

func TestPrepareAuditReviewLogPathRegisteredUsesGlobalAuditRoot(t *testing.T) {
	repo := t.TempDir()
	t.Setenv(home.EnvHome, filepath.Join(t.TempDir(), "home"))
	if _, err := registry.Register(context.Background(), registry.Options{
		RepoPath: repo,
		Now: func() time.Time {
			return time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
		},
	}, registry.DefaultDeps()); err != nil {
		t.Fatalf("registry.Register: %v", err)
	}
	layout, err := state.ResolveRuntimeLayout(repo)
	if err != nil {
		t.Fatalf("ResolveRuntimeLayout: %v", err)
	}

	path, err := prepareAuditReviewLogPath(repo)
	if err != nil {
		t.Fatalf("prepareAuditReviewLogPath: %v", err)
	}
	if filepath.Dir(path) != layout.AuditRoot {
		t.Fatalf("audit log path = %s, want under %s", path, layout.AuditRoot)
	}
	if _, err := os.Stat(filepath.Join(repo, ".loopcoder")); !os.IsNotExist(err) {
		t.Fatalf("registered audit path created repo-local .loopcoder: %v", err)
	}
}
