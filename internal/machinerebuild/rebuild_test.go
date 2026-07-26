package machinerebuild_test

import (
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/machinerebuild"
)

func now() time.Time {
	return time.Date(2026, 7, 22, 16, 0, 0, 0, time.UTC)
}

func self(id, owner, name string, gen int64) *machinerebuild.ProjectSelfID {
	return &machinerebuild.ProjectSelfID{
		Schema: machinerebuild.SchemaCandidate, ProjectID: id,
		RepoOwner: owner, RepoName: name, Visibility: "private",
		RegistrationGen: gen, SchemaVersion: "v1", Owner: "alice",
	}
}

func TestMissingMachineStoreRebuildsValidProjects(t *testing.T) {
	in := machinerebuild.ScanInput{
		Home:          "/tmp/home-b",
		ExpectedOwner: "alice",
		Damaged:       machinerebuild.DamagedStore{Missing: true},
		ProjectsEntries: []machinerebuild.FSEntry{
			{Name: "app", AbsPath: "/tmp/home-b/projects/app", IsDir: true, Owner: "alice", Self: self("p1", "acme", "app", 1)},
			{Name: "lib", AbsPath: "/tmp/home-b/projects/lib", IsDir: true, Owner: "alice", Self: self("p2", "acme", "lib", 1)},
		},
		ProviderProbes: []machinerebuild.ProviderFact{
			{Provider: "codex", Available: true, Provenance: "probe"},
		},
		Now: now(),
	}
	r := machinerebuild.Rebuild(in)
	if !r.Allowed {
		t.Fatalf("denied: %v", r.Reasons)
	}
	if len(r.Store.Projects) != 2 {
		t.Fatalf("projects=%d", len(r.Store.Projects))
	}
	if r.Store.DamagedBackupPath != "" {
		t.Fatalf("missing store should not create backup path: %s", r.Store.DamagedBackupPath)
	}
	if r.Manifest == nil || len(r.Manifest.AcceptedProjectIDs) != 2 {
		t.Fatalf("manifest=%#v", r.Manifest)
	}
	// Project events / customer repos are not in the store model — only refs.
	if r.Store.Projects["p1"].RepoName != "app" {
		t.Fatalf("self id not preserved")
	}
}

func TestRejectSymlinkFileWrongOwnerPartialDuplicate(t *testing.T) {
	in := machinerebuild.ScanInput{
		Home:          "/tmp/home",
		ExpectedOwner: "alice",
		Damaged:       machinerebuild.DamagedStore{Missing: true},
		ProjectsEntries: []machinerebuild.FSEntry{
			{Name: "ok", AbsPath: "/tmp/home/projects/ok", IsDir: true, Owner: "alice", Self: self("p-ok", "acme", "ok", 1)},
			{Name: "link", AbsPath: "/tmp/home/projects/link", IsSymlink: true, Owner: "alice"},
			{Name: "file", AbsPath: "/tmp/home/projects/file", IsFile: true, Owner: "alice"},
			{Name: "bob", AbsPath: "/tmp/home/projects/bob", IsDir: true, Owner: "bob", Self: self("p-bob", "acme", "bob", 1)},
			{Name: "partial", AbsPath: "/tmp/home/projects/partial", IsDir: true, Owner: "alice"}, // no Self
			{Name: "dup", AbsPath: "/tmp/home/projects/dup", IsDir: true, Owner: "alice", Self: self("p-ok", "acme", "ok", 2)},
		},
		Now: now(),
	}
	r := machinerebuild.Rebuild(in)
	if !r.Allowed {
		t.Fatal(r.Reasons)
	}
	if len(r.Accepted) != 1 || r.Accepted[0].Self.ProjectID != "p-ok" {
		t.Fatalf("accepted=%#v", r.Accepted)
	}
	if len(r.Rejected) != 5 {
		t.Fatalf("rejected=%d diags=%v", len(r.Rejected), r.Manifest.RejectedDiagnostics)
	}
	// Rejected cannot poison authority.
	if len(r.Store.Projects) != 1 {
		t.Fatalf("store projects=%d", len(r.Store.Projects))
	}
	diags := strings.Join(r.Manifest.RejectedDiagnostics, ",")
	for _, want := range []string{"symlink_rejected", "not_directory", "wrong_owner", "partial_or_missing_self_id", "duplicate_project_id"} {
		if !strings.Contains(diags, want) {
			t.Fatalf("missing diag %s in %s", want, diags)
		}
	}
}

func TestCorruptStorePreservedAsBackup(t *testing.T) {
	in := machinerebuild.ScanInput{
		Home: "/tmp/home",
		Damaged: machinerebuild.DamagedStore{
			Path: "/tmp/home/machine.db", Digest: "deadbeefcafebabe", Corrupt: true,
		},
		ProjectsEntries: []machinerebuild.FSEntry{
			{Name: "ok", AbsPath: "/tmp/home/projects/ok", IsDir: true, Self: self("p1", "acme", "app", 1)},
		},
		Now: now(),
	}
	r := machinerebuild.Rebuild(in)
	if r.Store.DamagedBackupPath == "" || r.Store.DamagedBackupPath == in.Damaged.Path {
		t.Fatalf("backup path=%q must be beside damaged, not overwrite", r.Store.DamagedBackupPath)
	}
	if r.Store.DamagedBackupDigest != "deadbeefcafebabe" {
		t.Fatalf("digest=%s", r.Store.DamagedBackupDigest)
	}
	if !strings.Contains(r.Store.DamagedBackupPath, "backups") {
		t.Fatalf("backup path should be under backups: %s", r.Store.DamagedBackupPath)
	}
}

func TestProviderStaleIgnored(t *testing.T) {
	in := machinerebuild.ScanInput{
		Home:    "/tmp/home",
		Damaged: machinerebuild.DamagedStore{Missing: true},
		ProjectsEntries: []machinerebuild.FSEntry{
			{Name: "ok", AbsPath: "/tmp/home/projects/ok", IsDir: true, Self: self("p1", "acme", "app", 1)},
		},
		ProviderProbes: []machinerebuild.ProviderFact{
			{Provider: "codex", Available: true, Provenance: "probe"},
			{Provider: "claude", Available: true, Provenance: "stale_ignored"},
			{Provider: "ghost", Available: false, Provenance: ""},
		},
		Now: now(),
	}
	r := machinerebuild.Rebuild(in)
	if len(r.Store.Providers) != 1 || r.Store.Providers[0].Provider != "codex" {
		t.Fatalf("providers=%#v", r.Store.Providers)
	}
	// No credential fields exist on ProviderFact — structural guarantee.
}

func TestReservationReconcile(t *testing.T) {
	in := machinerebuild.ScanInput{
		Home:    "/tmp/home",
		Damaged: machinerebuild.DamagedStore{Missing: true},
		ProjectsEntries: []machinerebuild.FSEntry{
			{Name: "ok", AbsPath: "/tmp/home/projects/ok", IsDir: true, Self: self("p1", "acme", "app", 1)},
		},
		PriorReservations: []machinerebuild.Reservation{
			{ID: "r-live", ProjectID: "p1", Kind: "worker"},
			{ID: "r-dead", ProjectID: "p1", Kind: "test"},
			{ID: "r-orphan", ProjectID: "gone", Kind: "worker"},
		},
		Live: []machinerebuild.ProcessEvidence{
			{PID: 101, ProjectID: "p1", Kind: "worker", Alive: true},
			{PID: 202, ProjectID: "", Kind: "worker", Alive: true}, // unknown
			{PID: 303, ProjectID: "p1", Kind: "test", Alive: false},
		},
		Now: now(),
	}
	r := machinerebuild.Rebuild(in)
	byID := map[string]machinerebuild.Reservation{}
	for _, res := range r.Store.Reservations {
		byID[res.ID] = res
	}
	if byID["r-live"].Status != machinerebuild.ResLiveOwned {
		t.Fatalf("live=%#v", byID["r-live"])
	}
	if byID["r-dead"].Status != machinerebuild.ResReleased {
		t.Fatalf("dead=%#v", byID["r-dead"])
	}
	if byID["r-orphan"].Status != machinerebuild.ResAttention || !byID["r-orphan"].Attention {
		t.Fatalf("orphan=%#v", byID["r-orphan"])
	}
	// Unknown live → attention
	foundUnknown := false
	for _, res := range r.Store.Reservations {
		if res.Status == machinerebuild.ResAttention && strings.Contains(res.Reason, "unknown_live") {
			foundUnknown = true
		}
	}
	if !foundUnknown {
		t.Fatalf("missing unknown live attention: %#v", r.Store.Reservations)
	}
	// Never double-admit: attention count >= 1 and released/live are distinct.
	sum := r.Manifest.ReservationSummary
	if sum[string(machinerebuild.ResLiveOwned)] < 1 || sum[string(machinerebuild.ResReleased)] < 1 {
		t.Fatalf("summary=%v", sum)
	}
}

func TestIdempotentRebuild(t *testing.T) {
	in := machinerebuild.ScanInput{
		Home:    "/tmp/home",
		Damaged: machinerebuild.DamagedStore{Path: "/tmp/home/machine.db", Digest: "abc", Corrupt: true},
		ProjectsEntries: []machinerebuild.FSEntry{
			{Name: "ok", AbsPath: "/tmp/home/projects/ok", IsDir: true, Self: self("p1", "acme", "app", 1)},
		},
		Now: now(),
	}
	r1 := machinerebuild.Rebuild(in)
	r2 := machinerebuild.RebuildIdempotent(in, r1.Manifest.EvidenceFingerprint)
	if !r2.Manifest.IdempotentReplay {
		t.Fatalf("expected idempotent: fp1=%s fp2=%s", r1.Manifest.EvidenceFingerprint, r2.Manifest.EvidenceFingerprint)
	}
	if r1.Manifest.EvidenceFingerprint != r2.Manifest.EvidenceFingerprint {
		t.Fatal("fingerprint drift")
	}
	// One redacted manifest + backup path/digest preserved.
	if r2.Manifest.DamagedBackupPath == "" || r2.Manifest.DamagedBackupDigest == "" {
		t.Fatalf("manifest backup missing: %#v", r2.Manifest)
	}
}

func TestRegistrationGenIndependentOfPath(t *testing.T) {
	s := self("p1", "acme", "app", 7)
	s.LocalPath = "/old/path"
	in := machinerebuild.ScanInput{
		Home:    "/tmp/home",
		Damaged: machinerebuild.DamagedStore{Missing: true},
		ProjectsEntries: []machinerebuild.FSEntry{
			{Name: "moved", AbsPath: "/tmp/home/projects/moved", IsDir: true, Self: s},
		},
		Now: now(),
	}
	r := machinerebuild.Rebuild(in)
	got := r.Store.Projects["p1"]
	if got.RegistrationGen != 7 {
		t.Fatalf("gen=%d", got.RegistrationGen)
	}
	if got.LocalPath != "/tmp/home/projects/moved" {
		t.Fatalf("path not updated to scan path: %s", got.LocalPath)
	}
}

func TestNoSilentOverwriteDamaged(t *testing.T) {
	damaged := "/tmp/home/machine.db"
	in := machinerebuild.ScanInput{
		Home:            "/tmp/home",
		Damaged:         machinerebuild.DamagedStore{Path: damaged, Digest: "ff", Corrupt: true},
		ProjectsEntries: nil,
		Now:             now(),
	}
	r := machinerebuild.Rebuild(in)
	if r.Store.DamagedBackupPath == damaged {
		t.Fatal("must not write rebuilt authority over damaged path")
	}
}
