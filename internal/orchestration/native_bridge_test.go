package orchestration

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/runtimecap"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

func TestStartProviderNativeChildRegistersBeforeAdapterStart(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 10, 0, 0, 1, 0, time.UTC)
	store, parent, child, req := nativeBridgeFixture(t, ctx, "claude", now)
	defer store.Close()

	var sawRegistration bool
	adapter := nativeAdapterFunc(func(ctx context.Context, registration storage.AgentRegistrationRecord, child ChildRunPlan) (NativeChildOutput, error) {
		sawRegistration = true
		if registration.ChildAgentID == "" || registration.RegistrationState != storage.AgentRegistrationStateActive {
			t.Fatalf("registration passed to adapter = %#v", registration)
		}
		if _, err := storage.RequireActiveAgentRegistrationForLaunch(ctx, store, child.RunID, registration.ExecutorID, registration.ClaimGeneration); err != nil {
			t.Fatalf("registration was not active before adapter start: %v", err)
		}
		return NativeChildOutput{Status: NestedStatusSucceeded, Summary: "native child completed", ProviderReceipt: "receipt-fixture"}, nil
	})

	result, err := StartProviderNativeChild(ctx, NativeChildStartOptions{
		Store:        store,
		ParentRunID:  parent.RunID,
		Child:        child,
		Registration: req,
		Adapter:      adapter,
		Clock:        func() time.Time { return now },
		Lease:        time.Minute,
	})
	if err != nil {
		t.Fatalf("StartProviderNativeChild returned error: %v", err)
	}
	if !sawRegistration {
		t.Fatal("adapter did not start")
	}
	if result.Status != NestedStatusSucceeded || result.ChildAgentID == "" || result.ScopeGrantID == "" || len(result.BudgetBindingIDs) != 1 || len(result.OwnershipLockIDs) != 1 {
		t.Fatalf("native child result missing registration refs: %#v", result)
	}
}

func TestStartProviderNativeChildRefusesUnsupportedProviders(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 10, 0, 0, 1, 0, time.UTC)
	for _, provider := range []string{"codex", "gemini", "antigravity"} {
		t.Run(provider, func(t *testing.T) {
			store, parent, child, req := nativeBridgeFixture(t, ctx, provider, now)
			defer store.Close()
			started := false
			_, err := StartProviderNativeChild(ctx, NativeChildStartOptions{
				Store:        store,
				ParentRunID:  parent.RunID,
				Child:        child,
				Registration: req,
				Adapter: nativeAdapterFunc(func(context.Context, storage.AgentRegistrationRecord, ChildRunPlan) (NativeChildOutput, error) {
					started = true
					return NativeChildOutput{Status: NestedStatusSucceeded}, nil
				}),
				Clock: func() time.Time { return now },
				Lease: time.Minute,
			})
			if !errors.Is(err, storage.ErrUnsupportedNativeSubAgent) {
				t.Fatalf("error = %v, want ErrUnsupportedNativeSubAgent", err)
			}
			if started {
				t.Fatal("unsupported provider adapter was started")
			}
		})
	}
}

func TestStartProviderNativeChildAcceptsFutureProviderThroughSameContract(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 10, 0, 0, 1, 0, time.UTC)
	store, parent, child, req := nativeBridgeFixture(t, ctx, "future-provider", now)
	defer store.Close()
	contract := runtimecap.Contract{
		Providers: []runtimecap.ProviderRuntime{{
			Name:                  "future-provider",
			Executable:            "future-provider",
			NestedSubagents:       true,
			Cancellation:          true,
			AuthUnsupportedReason: "future provider fixture",
		}},
		Hosts: runtimecap.DefaultContract().Hosts,
	}

	result, err := StartProviderNativeChild(ctx, NativeChildStartOptions{
		Store:        store,
		Contract:     contract,
		ParentRunID:  parent.RunID,
		Child:        child,
		Registration: req,
		Adapter: nativeAdapterFunc(func(context.Context, storage.AgentRegistrationRecord, ChildRunPlan) (NativeChildOutput, error) {
			return NativeChildOutput{Status: NestedStatusSucceeded, Summary: "future provider completed", ProviderReceipt: "future-receipt"}, nil
		}),
		Clock: func() time.Time { return now },
		Lease: time.Minute,
	})
	if err != nil {
		t.Fatalf("future provider StartProviderNativeChild returned error: %v", err)
	}
	if result.Status != NestedStatusSucceeded || result.ChildAgentID == "" {
		t.Fatalf("future provider result = %#v", result)
	}
}

func TestNormalizeNativeChildOutputBoundsAndRedactsUntrustedRawOutput(t *testing.T) {
	raw := "prefix Authorization: suffix\n" + strings.Repeat("x", maxNativeChildRawOutputBytes+64)
	envelope, err := NormalizeNativeChildOutput(NativeChildOutput{
		Status:    NestedStatusSucceeded,
		Summary:   strings.Repeat("s", 1024),
		RawOutput: raw,
	})
	if err != nil {
		t.Fatalf("NormalizeNativeChildOutput returned error: %v", err)
	}
	if envelope.SchemaVersion != NativeChildResultEnvelopeSchemaV1 || envelope.Trusted || envelope.Accepted {
		t.Fatalf("envelope trust/schema fields = %#v", envelope)
	}
	if !envelope.Truncated || len(envelope.RawOutput) <= maxNativeChildRawOutputBytes {
		t.Fatalf("raw output was not bounded: len=%d truncated=%v", len(envelope.RawOutput), envelope.Truncated)
	}
	if strings.Contains(envelope.RawOutput, "Authorization: suffix") {
		t.Fatalf("raw output was not redacted: %q", envelope.RawOutput[:80])
	}
}

type nativeAdapterFunc func(context.Context, storage.AgentRegistrationRecord, ChildRunPlan) (NativeChildOutput, error)

func (fn nativeAdapterFunc) StartNativeChild(ctx context.Context, registration storage.AgentRegistrationRecord, child ChildRunPlan) (NativeChildOutput, error) {
	return fn(ctx, registration, child)
}

func nativeBridgeFixture(t *testing.T, ctx context.Context, adapterID string, now time.Time) (storage.Store, storage.RunNode, ChildRunPlan, storage.AgentRegistrationRequest) {
	t.Helper()
	store, err := storage.Open(ctx, storage.Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	parent := storage.RunNode{RunID: "run-parent", RootRunID: "run-parent", Depth: 0, Origin: "nested_parent", Status: "running", CreatedAt: "2026-07-10T00:00:00Z", UpdatedAt: "2026-07-10T00:00:00Z"}
	childRun := storage.RunNode{RunID: "run-child", ParentRunID: parent.RunID, RootRunID: parent.RootRunID, Depth: 1, Origin: "sub_agent", Status: NestedStatusQueued, CreatedAt: parent.CreatedAt, UpdatedAt: parent.UpdatedAt}
	plan := storage.ChildPlanRecord{PlanID: "plan-run-parent", ParentRunID: parent.RunID, RootRunID: parent.RootRunID, SchemaVersion: ChildPlanSchemaVersionV1, MaxDepth: 2, MaxConcurrency: 1, PlanJSON: `{"schema_version":"loopcoder.child_plan.v1"}`, CreatedAt: parent.CreatedAt}
	edge := storage.RunEdgeRecord{ParentRunID: parent.RunID, ChildRunID: childRun.RunID, RootRunID: parent.RootRunID, PlanID: plan.PlanID, ChildKey: "native-child", Depth: 1, Ordinal: 0, ScopeJSON: `{"repo":".","paths":["internal/orchestration"]}`, Permission: "write", AggregationJSON: `{"mode":"collect","required":true,"include_report":true}`, Status: NestedStatusQueued, CreatedAt: parent.CreatedAt, UpdatedAt: parent.UpdatedAt}
	if err := storage.PersistChildPlanGraph(ctx, store, parent, []storage.RunNode{childRun}, plan, []storage.RunEdgeRecord{edge}); err != nil {
		t.Fatalf("PersistChildPlanGraph: %v", err)
	}
	child := ChildRunPlan{
		ID:          edge.ChildKey,
		ChildKey:    edge.ChildKey,
		Title:       "Native child",
		Role:        "native-subagent",
		RunID:       childRun.RunID,
		AdapterID:   adapterID,
		NativeChild: true,
		Scope:       ChildScope{Repo: ".", Paths: []string{"internal/orchestration"}},
		Permission:  edge.Permission,
		Aggregation: ChildAggregation{Mode: ChildAggregationCollect, Required: true, IncludeReport: true},
		Required:    true,
		Depth:       1,
	}
	req := storage.AgentRegistrationRequest{
		ProjectID:              "project-federation",
		DeliveryRunID:          "delivery-run-federation",
		RootRunID:              parent.RootRunID,
		ParentRunID:            parent.RunID,
		ChildRunID:             childRun.RunID,
		TaskID:                 "task-federation",
		AttemptID:              "attempt-federation",
		PlanID:                 plan.PlanID,
		ChildKey:               edge.ChildKey,
		AdapterID:              adapterID,
		ProviderInstallationID: "pinst-" + adapterID,
		AccountProfileID:       "acct-fixture",
		ModelCapabilityID:      "mcap-fixture",
		RoutingDecisionID:      "route-fixture",
		ProviderSessionRef:     "provider-session-ref-fixture",
		ScopeJSON:              edge.ScopeJSON,
		Permission:             edge.Permission,
		SideEffectClass:        "repo-write",
		BudgetBindings: []storage.AgentBudgetBindingInput{{
			BudgetPolicyID:      "budget-policy-fixture",
			BudgetReservationID: "budget-reservation-fixture",
			ReservationScope:    "child",
			ReservedQuantities:  `{"total_tokens":1000}`,
			AncestorBudgetRefs:  `["root-budget-fixture"]`,
			ReservationState:    "active",
		}},
		OwnershipLocks: []storage.AgentOwnershipLockInput{{
			ResourceKind:      "repo-path",
			ResourceKey:       "internal/orchestration",
			LockMode:          "write",
			State:             "held",
			LeaseExpiresAt:    "2026-07-10T00:30:00Z",
			HeartbeatAt:       "2026-07-10T00:00:01Z",
			ConflictsWithJSON: `[]`,
		}},
		CancellationChannel: "local-context",
		ExpectedOutputsJSON: `{"kind":"summary"}`,
		PolicyVersion:       storage.AgentFederationPolicyVersionV1,
		PlanFingerprint:     "sha256:" + strings.Repeat("a", 64),
		PolicyFingerprint:   "sha256:" + strings.Repeat("1", 64),
		Classification:      "internal",
		Now:                 now,
	}
	return store, parent, child, req
}
