//go:build windows

package storage

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRegisterAgentRejectsJunctionAliasedWriteScopeOverlap(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedNow})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	repo := filepath.Join(t.TempDir(), "repo")
	src := filepath.Join(repo, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.go"), []byte("package src\n"), 0o644); err != nil {
		t.Fatalf("write source file: %v", err)
	}
	createStorageWindowsJunction(t, filepath.Join(repo, "alias-src"), src)

	firstScope := federationAuthorityScope("project-a")
	firstScope.ReadScope = []string{"src/a.go"}
	firstScope.WriteScope = []string{"src/a.go"}
	firstScope.PathScope = []string{"src/a.go"}
	claim := createFederationClaim(t, ctx, store, "project-a", "run-root", "run-child", "child-a")
	if err := updateFederationProjectAndAuthorityScope(ctx, store, "project-a", repo, firstScope); err != nil {
		t.Fatalf("update first authority scope: %v", err)
	}
	req := federationRequest(claim)
	req.Scope = firstScope
	req.ParentScope = &firstScope
	req.OwnershipLocks[0].ResourceKey = "src/a.go"
	req.OwnershipLocks[0].LeaseExpiresAt = formatTimestamp(fixedNow().Add(time.Hour))
	if _, err := RegisterAgent(ctx, store, req); err != nil {
		t.Fatalf("RegisterAgent first: %v", err)
	}

	aliasScope := federationAuthorityScope("project-a")
	aliasScope.ReadScope = []string{"alias-src/a.go"}
	aliasScope.WriteScope = []string{"alias-src/a.go"}
	aliasScope.PathScope = []string{"alias-src/a.go"}
	claim2 := createFederationClaim(t, ctx, store, "project-a", "run-root-2", "run-child-2", "child-b")
	if err := updateFederationProjectAndAuthorityScope(ctx, store, "project-a", repo, aliasScope); err != nil {
		t.Fatalf("update alias authority scope: %v", err)
	}
	req2 := federationRequest(claim2)
	req2.Scope = aliasScope
	req2.ParentScope = &aliasScope
	req2.OwnershipLocks[0].ResourceKey = "alias-src/a.go"
	req2.OwnershipLocks[0].LeaseExpiresAt = formatTimestamp(fixedNow().Add(time.Hour))
	if _, err := RegisterAgent(ctx, store, req2); !errors.Is(err, ErrOneWriterConflict) {
		t.Fatalf("junction aliased RegisterAgent error = %v, want ErrOneWriterConflict", err)
	}
}

func createStorageWindowsJunction(t *testing.T, link, target string) {
	t.Helper()
	output, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		t.Skipf("windows junction creation unavailable: %v: %s", err, strings.TrimSpace(string(output)))
	}
}
