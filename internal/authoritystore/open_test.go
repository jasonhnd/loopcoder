package authoritystore_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/authoritystore"
	"github.com/jasonhnd/loopcoder/internal/store"
)

func fixedNow() time.Time {
	return time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
}

func TestOpenMachineAndProjectIndependentIdentities(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	machinePath := filepath.Join(dir, "machine.db")
	projectPath := filepath.Join(dir, "project.db")

	ms, err := authoritystore.OpenMachine(ctx, authoritystore.OpenOptions{Path: machinePath, Now: fixedNow})
	if err != nil {
		t.Fatalf("OpenMachine: %v", err)
	}
	defer ms.Close()
	ps, err := authoritystore.OpenProject(ctx, authoritystore.OpenOptions{Path: projectPath, Now: fixedNow})
	if err != nil {
		t.Fatalf("OpenProject: %v", err)
	}
	defer ps.Close()

	if ms.Role() != authoritystore.RoleMachine || ps.Role() != authoritystore.RoleProject {
		t.Fatalf("roles = %s / %s", ms.Role(), ps.Role())
	}
	if ms.FormatIdentity() != authoritystore.MachineFormatIdentity {
		t.Fatalf("machine format = %s", ms.FormatIdentity())
	}
	if ps.FormatIdentity() != authoritystore.ProjectFormatIdentity {
		t.Fatalf("project format = %s", ps.FormatIdentity())
	}
	mm, err := ms.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pm, err := ps.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if mm.FormatIdentity != authoritystore.MachineFormatIdentity {
		t.Fatalf("machine meta format = %s", mm.FormatIdentity)
	}
	if pm.FormatIdentity != authoritystore.ProjectFormatIdentity {
		t.Fatalf("project meta format = %s", pm.FormatIdentity)
	}
	if mm.StoreID == pm.StoreID {
		t.Fatal("machine and project store ids unexpectedly equal")
	}
}

func TestRoleMismatchOnSamePathFailsClosed(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "shared.db")

	ms, err := authoritystore.OpenMachine(ctx, authoritystore.OpenOptions{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("OpenMachine: %v", err)
	}
	defer ms.Close()

	_, err = authoritystore.OpenProject(ctx, authoritystore.OpenOptions{Path: path, Now: fixedNow})
	if err == nil || !errors.Is(err, authoritystore.ErrRoleMismatch) {
		t.Fatalf("OpenProject error = %v, want ErrRoleMismatch", err)
	}

	// After close, reopening as project still fails because on-disk format is machine.
	if err := ms.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = authoritystore.OpenProject(ctx, authoritystore.OpenOptions{Path: path, Now: fixedNow})
	if err == nil {
		t.Fatal("expected format identity failure reopening machine file as project")
	}
}

func TestReopenSameRoleAndIdempotentClose(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "machine.db")

	first, err := authoritystore.OpenMachine(ctx, authoritystore.OpenOptions{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	meta1, err := first.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("idempotent close: %v", err)
	}

	second, err := authoritystore.OpenMachine(ctx, authoritystore.OpenOptions{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()
	meta2, err := second.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if meta1.StoreID != meta2.StoreID {
		t.Fatalf("store id changed: %s -> %s", meta1.StoreID, meta2.StoreID)
	}
	if err := second.CheckIntegrity(ctx); err != nil {
		t.Fatalf("integrity: %v", err)
	}
}

func TestConcurrentSameRoleHandles(t *testing.T) {
	ctx := context.Background()
	// Concurrent same-role handles: open two different files (SQLite single writer
	// per file). Registry allows same-role claim once per path.
	dir := t.TempDir()
	a, err := authoritystore.OpenMachine(ctx, authoritystore.OpenOptions{Path: filepath.Join(dir, "a.db"), Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := authoritystore.OpenMachine(ctx, authoritystore.OpenOptions{Path: filepath.Join(dir, "b.db"), Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if a.Path() == b.Path() {
		t.Fatal("expected distinct paths")
	}
}

func TestOpenRequiresRoleAndPath(t *testing.T) {
	ctx := context.Background()
	_, _, _, err := authoritystore.Open(ctx, authoritystore.OpenOptions{Path: filepath.Join(t.TempDir(), "x.db"), Now: fixedNow})
	if err == nil || !errors.Is(err, authoritystore.ErrInvalidRole) {
		t.Fatalf("error = %v, want ErrInvalidRole", err)
	}
	_, err = authoritystore.OpenMachine(ctx, authoritystore.OpenOptions{Now: fixedNow})
	if err == nil {
		t.Fatal("expected path required")
	}
}

func TestCrossDBTransactionUnsupported(t *testing.T) {
	if err := authoritystore.BeginCrossDBTransaction(nil, nil); !errors.Is(err, authoritystore.ErrCrossDBTransaction) {
		t.Fatalf("error = %v", err)
	}
}

func TestGenericFoundationRejectedAsAuthorityRole(t *testing.T) {
	// A generic store.FormatIdentity file cannot be opened as machine/project.
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "generic.db")
	generic, err := store.Open(ctx, store.Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("generic open: %v", err)
	}
	if err := generic.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = authoritystore.OpenMachine(ctx, authoritystore.OpenOptions{Path: path, Now: fixedNow})
	if err == nil {
		t.Fatal("expected role/format mismatch for generic foundation file")
	}
}

func TestLegacyReadOnlyPort(t *testing.T) {
	// Legacy port requires a real v0.8 schema; opening a missing path fails closed
	// without creating files (read-only contract).
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "missing", "legacy.db")
	port, err := authoritystore.OpenLegacyReadOnly(ctx, authoritystore.LegacyOpenOptions{Path: path, Now: fixedNow})
	if err == nil {
		_ = port.Close()
		t.Fatal("expected missing legacy database failure")
	}
	if port != nil && port.Writable() {
		t.Fatal("legacy port must not be writable")
	}
}
