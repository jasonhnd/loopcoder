package taskrequirements

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/delivery"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

func TestPersistTaskRequirementReplayAndRestart(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "loopcoder.db")
	store := openStore(t, ctx, dbPath)
	seedDeliveryTask(t, ctx, store)
	input := baseInput()
	input.Scope = Scope{AllowsRepoWrite: true, RequiresJSON: true}
	req, err := Classify(input)
	if err != nil {
		t.Fatalf("Classify() error = %v", err)
	}
	got, err := PersistTaskRequirement(ctx, store, req, PersistOptions{Now: input.Now})
	if err != nil {
		t.Fatalf("PersistTaskRequirement() error = %v", err)
	}
	replayed, err := PersistTaskRequirement(ctx, store, req, PersistOptions{Now: input.Now})
	if err != nil {
		t.Fatalf("PersistTaskRequirement() replay error = %v", err)
	}
	if replayed.TaskRequirementFingerprint != got.TaskRequirementFingerprint {
		t.Fatalf("replay fingerprint = %s, want %s", replayed.TaskRequirementFingerprint, got.TaskRequirementFingerprint)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	reopened := openStore(t, ctx, dbPath)
	defer reopened.Close()
	loaded, err := LoadTaskRequirement(ctx, reopened, got.TaskRequirementID)
	if err != nil {
		t.Fatalf("LoadTaskRequirement() error = %v", err)
	}
	if loaded.TaskRequirementFingerprint != got.TaskRequirementFingerprint || loaded.RequiredOutput != OutputJSON {
		t.Fatalf("loaded requirement = %#v, want replayed JSON requirement", loaded)
	}
}

func TestPersistTaskRequirementRejectsSameIdentityDifferentPayload(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, ctx, filepath.Join(t.TempDir(), "loopcoder.db"))
	defer store.Close()
	seedDeliveryTask(t, ctx, store)
	input := baseInput()
	input.Scope = Scope{AllowsRepoWrite: true}
	req, err := Classify(input)
	if err != nil {
		t.Fatalf("Classify() error = %v", err)
	}
	if _, err := PersistTaskRequirement(ctx, store, req, PersistOptions{Now: input.Now}); err != nil {
		t.Fatalf("PersistTaskRequirement() error = %v", err)
	}
	changed := req
	changed.ProvenanceSummary = "changed"
	changed.TaskRequirementFingerprint = ""
	changed.TaskRequirementFingerprint, err = FingerprintRequirement(changed)
	if err != nil {
		t.Fatalf("fingerprint changed requirement: %v", err)
	}
	_, err = PersistTaskRequirement(ctx, store, changed, PersistOptions{Now: input.Now})
	if !errors.Is(err, ErrDuplicateRecord) {
		t.Fatalf("PersistTaskRequirement() changed payload error = %v, want ErrDuplicateRecord", err)
	}
}

func TestPersistRequirementOverrideAndReloadForLaterDecision(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, ctx, filepath.Join(t.TempDir(), "loopcoder.db"))
	defer store.Close()
	seedDeliveryTask(t, ctx, store)
	input := baseInput()
	override := RequirementOverride{
		ProjectID:     input.ProjectID,
		DeliveryRunID: input.DeliveryRunID,
		TaskKey:       input.TaskKey,
		ScopeKey:      input.TaskKey,
		Field:         "risk_tier",
		ValueJSON:     `"high"`,
		Justification: "operator correction after review",
		CreatedBy:     input.CreatedBy,
		UpdatedBy:     input.CreatedBy,
		Host:          input.Host,
	}
	stored, err := PersistRequirementOverride(ctx, store, override, PersistOptions{Now: input.Now})
	if err != nil {
		t.Fatalf("PersistRequirementOverride() error = %v", err)
	}
	overrides, err := LoadActiveOverrides(ctx, store, input.ProjectID, input.DeliveryRunID, input.TaskKey, input.PlanFingerprint)
	if err != nil {
		t.Fatalf("LoadActiveOverrides() error = %v", err)
	}
	if len(overrides) != 1 || overrides[0].RequirementOverrideID != stored.RequirementOverrideID {
		t.Fatalf("loaded overrides = %#v, want stored override", overrides)
	}
	input.Scope = Scope{AllowsRepoWrite: true}
	input.Overrides = overrides
	req, err := Classify(input)
	if err != nil {
		t.Fatalf("Classify() with persisted override error = %v", err)
	}
	if req.RiskTier != RiskHigh || !contains(req.Source.RecordIDs, stored.RequirementOverrideID) {
		t.Fatalf("requirement = %#v, want persisted override visible", req)
	}
}

func TestPersistTaskRequirementMissingTaskFailsClosed(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, ctx, filepath.Join(t.TempDir(), "loopcoder.db"))
	defer store.Close()
	seedProjectAndRun(t, ctx, store)
	input := baseInput()
	input.Scope = Scope{AllowsRepoWrite: true}
	req, err := Classify(input)
	if err != nil {
		t.Fatalf("Classify() error = %v", err)
	}
	_, err = PersistTaskRequirement(ctx, store, req, PersistOptions{Now: input.Now})
	if !errors.Is(err, ErrMissingReference) {
		t.Fatalf("PersistTaskRequirement() error = %v, want ErrMissingReference", err)
	}
}

func openStore(t *testing.T, ctx context.Context, path string) storage.Store {
	t.Helper()
	now := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	store, err := storage.Open(ctx, storage.Options{Path: path, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	return store
}

func seedDeliveryTask(t *testing.T, ctx context.Context, store storage.Store) {
	t.Helper()
	seedProjectAndRun(t, ctx, store)
	input := baseInput()
	task := delivery.Task{
		SchemaVersion:    delivery.SchemaTask,
		RecordVersion:    1,
		TaskID:           input.TaskID,
		TaskKey:          input.TaskKey,
		DeliveryRunID:    input.DeliveryRunID,
		ProjectID:        input.ProjectID,
		State:            "pending",
		Title:            input.Title,
		RequirementsJSON: "{}",
		ScopeJSON:        "{}",
		Permission:       "write",
		SideEffectClass:  "repo-write",
		PolicyVersion:    input.PolicyVersion,
		PlanFingerprint:  input.PlanFingerprint,
		CreatedBy:        input.CreatedBy,
		UpdatedBy:        input.CreatedBy,
		Host:             input.Host,
	}
	if _, err := delivery.PersistTask(ctx, store, task, delivery.PersistOptions{Now: input.Now}); err != nil {
		t.Fatalf("persist task: %v", err)
	}
}

func seedProjectAndRun(t *testing.T, ctx context.Context, store storage.Store) {
	t.Helper()
	input := baseInput()
	err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		_, err := tx.Exec(ctx, `INSERT OR IGNORE INTO projects(id, local_path, created_at, updated_at) VALUES (?, ?, ?, ?)`, input.ProjectID, t.TempDir(), input.Now.Format(time.RFC3339), input.Now.Format(time.RFC3339))
		return err
	})
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}
	run := delivery.DeliveryRun{
		SchemaVersion:      delivery.SchemaDeliveryRun,
		RecordVersion:      1,
		DeliveryRunID:      input.DeliveryRunID,
		RunID:              input.DeliveryRunID,
		ProjectID:          input.ProjectID,
		RootRunID:          input.DeliveryRunID,
		State:              "draft",
		IntentSummary:      input.IntentSummary,
		InputFingerprint:   "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		PolicyFingerprint:  "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		PlanFingerprint:    input.PlanFingerprint,
		PolicyVersion:      input.PolicyVersion,
		MaxSideEffectClass: "repo-write",
		ApprovalStatus:     "not-required",
		OverrideStatus:     "none",
		CreatedBy:          input.CreatedBy,
		UpdatedBy:          input.CreatedBy,
		Host:               input.Host,
	}
	if _, err := delivery.PersistDeliveryRun(ctx, store, run, delivery.PersistOptions{Now: input.Now}); err != nil {
		t.Fatalf("persist delivery run: %v", err)
	}
}
