package storage

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAgentRegistrationReplayEventsAndTree(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedNow})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	claim := createFederationClaim(t, ctx, store, "project-a", "run-root", "run-child", "child-a")
	req := federationRequest(claim)
	record, err := RegisterAgent(ctx, store, req)
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	replay, err := RegisterAgent(ctx, store, req)
	if err != nil {
		t.Fatalf("RegisterAgent replay: %v", err)
	}
	if replay.ChildAgentID != record.ChildAgentID || replay.RegistrationPayloadHash != record.RegistrationPayloadHash {
		t.Fatalf("replay = %#v, want same registration %#v", replay, record)
	}
	req.ProviderSessionRef = "changed-session"
	if _, err := RegisterAgent(ctx, store, req); !errors.Is(err, ErrAgentRegistrationConflict) {
		t.Fatalf("changed replay error = %v, want ErrAgentRegistrationConflict", err)
	}

	if _, err := TransitionAgentRegistration(ctx, store, record.ChildAgentID, AgentActionLaunch, claim.ExecutorID, claim.ClaimGeneration, "2026-01-01T00:00:02Z"); err != nil {
		t.Fatalf("launch transition: %v", err)
	}
	if _, err := TransitionAgentRegistration(ctx, store, record.ChildAgentID, AgentActionHeartbeat, claim.ExecutorID, claim.ClaimGeneration, "2026-01-01T00:00:03Z"); err != nil {
		t.Fatalf("heartbeat transition: %v", err)
	}
	var events []struct {
		hash     string
		previous string
		payload  string
	}
	if err := store.WithTx(ctx, func(tx Tx) error {
		rows, err := tx.Query(ctx, `SELECT event_hash, previous_event_hash, payload_hash FROM agent_events WHERE child_agent_id = ? ORDER BY created_at, id`, record.ChildAgentID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var item struct {
				hash     string
				previous string
				payload  string
			}
			if err := rows.Scan(&item.hash, &item.previous, &item.payload); err != nil {
				return err
			}
			events = append(events, item)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("load events: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("event count = %d, want register+launch+heartbeat", len(events))
	}
	if events[0].previous != "" || events[1].previous != events[0].hash || events[2].previous != events[1].hash {
		t.Fatalf("event chain = %#v", events)
	}
	for _, event := range events {
		if !strings.HasPrefix(event.hash, "sha256:") || !strings.HasPrefix(event.payload, "sha256:") {
			t.Fatalf("event hashes not written: %#v", event)
		}
	}

	tree, err := LoadAgentTree(ctx, store, "project-a", "run-root")
	if err != nil {
		t.Fatalf("LoadAgentTree: %v", err)
	}
	if len(tree.Registrations) != 1 || tree.Registrations[0].RegistrationState != AgentStateRunning {
		t.Fatalf("tree registrations = %#v", tree.Registrations)
	}
	if !strings.HasPrefix(tree.AgentFederationFingerprint, "sha256:") {
		t.Fatalf("tree fingerprint = %q", tree.AgentFederationFingerprint)
	}
}

func TestAgentRegistrationLifecycleTable(t *testing.T) {
	for _, state := range []string{
		AgentStatePlanned,
		AgentStateRegistered,
		AgentStateLaunching,
		AgentStateRunning,
		AgentStateCancelling,
		AgentStateSucceeded,
		AgentStateFailed,
		AgentStateCancelled,
		AgentStateNeedsHuman,
		AgentStateSuperseded,
	} {
		if normalizeAgentRegistrationState(state) != state {
			t.Fatalf("state %q is not recognized", state)
		}
	}
	if next, err := nextAgentRegistrationState(AgentStateRegistered, AgentActionLaunch); err != nil || next != AgentStateLaunching {
		t.Fatalf("registered launch = %q/%v, want launching", next, err)
	}
	if next, err := nextAgentRegistrationState(AgentStateLaunching, AgentActionHeartbeat); err != nil || next != AgentStateRunning {
		t.Fatalf("launching heartbeat = %q/%v, want running", next, err)
	}
	if next, err := nextAgentRegistrationState(AgentStateRunning, AgentActionCompleteSuccess); err != nil || next != AgentStateSucceeded {
		t.Fatalf("running success = %q/%v, want succeeded", next, err)
	}
	if _, err := nextAgentRegistrationState(AgentStateRegistered, AgentActionHeartbeat); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("registered heartbeat error = %v, want ErrInvalidTransition", err)
	}
	if _, err := nextAgentRegistrationState(AgentStateSucceeded, AgentActionLaunch); !errors.Is(err, ErrTerminalState) {
		t.Fatalf("succeeded launch error = %v, want ErrTerminalState", err)
	}
}

func TestProviderCapabilityFixtures(t *testing.T) {
	fixtures := []struct {
		name    string
		input   ProviderCapabilityEvidence
		wantErr bool
	}{
		{name: "claude-supported", input: ProviderCapabilityEvidence{AdapterID: "claude", NestedSubagents: true, FreshnessState: "fresh", CapabilityConfidence: "exact"}},
		{name: "codex-unsupported", input: ProviderCapabilityEvidence{AdapterID: "codex", NestedSubagents: true, FreshnessState: "fresh"}, wantErr: true},
		{name: "gemini-unsupported", input: ProviderCapabilityEvidence{AdapterID: "gemini", NestedSubagents: true, FreshnessState: "fresh"}, wantErr: true},
		{name: "antigravity-unsupported", input: ProviderCapabilityEvidence{AdapterID: "antigravity", NestedSubagents: true, FreshnessState: "fresh"}, wantErr: true},
		{name: "future-provider-supported", input: ProviderCapabilityEvidence{AdapterID: "future-provider", NestedSubagents: true, FreshnessState: "fresh", CapabilityConfidence: "exact"}},
		{name: "future-provider-stale", input: ProviderCapabilityEvidence{AdapterID: "future-provider", NestedSubagents: true, FreshnessState: "stale", CapabilityConfidence: "exact"}, wantErr: true},
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			err := ValidateProviderNativeSubagent(fixture.input)
			if fixture.wantErr && !errors.Is(err, ErrUnsupportedNativeSubAgent) {
				t.Fatalf("ValidateProviderNativeSubagent error = %v, want ErrUnsupportedNativeSubAgent", err)
			}
			if !fixture.wantErr && err != nil {
				t.Fatalf("ValidateProviderNativeSubagent error = %v", err)
			}
		})
	}
}

func TestOneWriterActivePartialIndexAndPathOverlap(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedNow})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	claim := createFederationClaim(t, ctx, store, "project-a", "run-root", "run-child", "child-a")
	req := federationRequest(claim)
	if _, err := RegisterAgent(ctx, store, req); err != nil {
		t.Fatalf("RegisterAgent first: %v", err)
	}

	claim2 := createFederationClaim(t, ctx, store, "project-a", "run-root-2", "run-child-2", "child-b")
	req2 := federationRequest(claim2)
	req2.ChildKey = "child-b"
	req2.RunID = "run-child-2"
	req2.OwnershipLocks[0].ResourceKey = "src/a.go"
	if _, err := RegisterAgent(ctx, store, req2); !errors.Is(err, ErrOneWriterConflict) {
		t.Fatalf("same lock error = %v, want ErrOneWriterConflict", err)
	}

	req2.OwnershipLocks[0].ResourceKey = "src"
	if _, err := RegisterAgent(ctx, store, req2); !errors.Is(err, ErrOneWriterConflict) {
		t.Fatalf("ancestor lock error = %v, want ErrOneWriterConflict", err)
	}
}

func TestScopeInheritanceAndCrossProjectFailClosed(t *testing.T) {
	parent := AgentScopeGrant{Permission: PermissionWrite, ReadScope: []string{"src"}, WriteScope: []string{"src/a.go"}, PathScope: []string{"src/a.go"}, CredentialScope: []string{"none"}}
	child := AgentScopeGrant{Permission: PermissionOrchestrate, ReadScope: []string{"src"}, WriteScope: []string{"src/a.go"}, PathScope: []string{"src/a.go"}, CredentialScope: []string{"none"}}
	if err := ValidateScopeInheritance(parent, child); !errors.Is(err, ErrScopeWidening) {
		t.Fatalf("permission widening error = %v, want ErrScopeWidening", err)
	}
	child.Permission = PermissionWrite
	child.CredentialScope = []string{"ENV_TOKEN"}
	if err := ValidateScopeInheritance(parent, child); !errors.Is(err, ErrCredentialScopeDenied) {
		t.Fatalf("credential scope error = %v, want ErrCredentialScopeDenied", err)
	}
}

func TestChildOutputBoundedRedactedAndClosedStatus(t *testing.T) {
	secret := "AK" + "IA" + strings.Repeat("A", 16)
	envelope, err := NormalizeChildOutput("prefix "+secret+" suffix", "success", 12)
	if err != nil {
		t.Fatalf("NormalizeChildOutput: %v", err)
	}
	if !envelope.Truncated || envelope.Classification != "provider-output-untrusted" || envelope.Accepted {
		t.Fatalf("envelope flags = %#v", envelope)
	}
	envelope, err = NormalizeChildOutput("prefix "+secret+" suffix", "failed", 1024)
	if err != nil {
		t.Fatalf("NormalizeChildOutput redaction: %v", err)
	}
	if !envelope.Redacted || strings.Contains(envelope.Output, secret) {
		t.Fatalf("redacted output = %#v", envelope)
	}
	if _, err := NormalizeChildOutput("ok", "mystery", 1024); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("unknown status error = %v, want ErrInvalidRecord", err)
	}
}

func TestChildOutputRedactsBeforeTruncatingSecretPrefix(t *testing.T) {
	secret := "AK" + "IA" + strings.Repeat("B", 16)
	envelope, err := NormalizeChildOutput(secret+" suffix", "failed", 8)
	if err != nil {
		t.Fatalf("NormalizeChildOutput: %v", err)
	}
	if !envelope.Redacted || !envelope.Truncated {
		t.Fatalf("envelope flags = %#v, want redacted and truncated", envelope)
	}
	if strings.Contains(envelope.Output, "AKIA") || strings.Contains(envelope.Output, secret[:8]) {
		t.Fatalf("output leaked credential prefix after truncation: %#v", envelope)
	}
}

func TestValidateNativeChildLaunchRequiresActiveBudgetReservation(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		name string
		edit string
	}{
		{name: "inactive", edit: `UPDATE agent_budget_bindings SET reservation_state = 'cancelled'`},
		{name: "empty quantities", edit: `UPDATE agent_budget_bindings SET reserved_quantities_json = '{}'`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedNow})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer store.Close()
			claim := createFederationClaim(t, ctx, store, "project-a", "run-root", "run-child", "child-a")
			record, err := RegisterAgent(ctx, store, federationRequest(claim))
			if err != nil {
				t.Fatalf("RegisterAgent: %v", err)
			}
			if err := store.WithWriteTx(ctx, func(tx Tx) error {
				_, err := tx.Exec(ctx, tt.edit+` WHERE child_agent_id = ?`, record.ChildAgentID)
				return err
			}); err != nil {
				t.Fatalf("mutate budget binding: %v", err)
			}
			if _, err := ValidateNativeChildLaunch(ctx, store, claim.RunID, claim.ExecutorID, claim.ClaimGeneration); !errors.Is(err, ErrChildBudgetRequired) {
				t.Fatalf("ValidateNativeChildLaunch error = %v, want ErrChildBudgetRequired", err)
			}
		})
	}
}

func TestValidateNativeChildLaunchRequiresActiveClaimFencedLockCoverage(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		edit string
	}{
		{name: "expired", edit: `UPDATE agent_ownership_locks SET lease_expires_at = '2026-01-01T00:00:00Z'`},
		{name: "released", edit: `UPDATE agent_ownership_locks SET state = 'released'`},
		{name: "stale claim", edit: `UPDATE agent_ownership_locks SET claim_generation = claim_generation + 1`},
		{name: "uncovered write scope", edit: `UPDATE agent_ownership_locks SET resource_key = 'src/other.go'`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedNow})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer store.Close()
			claim := createFederationClaim(t, ctx, store, "project-a", "run-root", "run-child", "child-a")
			record, err := RegisterAgent(ctx, store, federationRequest(claim))
			if err != nil {
				t.Fatalf("RegisterAgent: %v", err)
			}
			if err := store.WithWriteTx(ctx, func(tx Tx) error {
				_, err := tx.Exec(ctx, tt.edit+` WHERE child_agent_id = ?`, record.ChildAgentID)
				return err
			}); err != nil {
				t.Fatalf("mutate ownership lock: %v", err)
			}
			if _, err := ValidateNativeChildLaunch(ctx, store, claim.RunID, claim.ExecutorID, claim.ClaimGeneration); !errors.Is(err, ErrOwnershipRequired) {
				t.Fatalf("ValidateNativeChildLaunch error = %v, want ErrOwnershipRequired", err)
			}
		})
	}
}

func TestTransitionAgentRegistrationTerminalReleasesHeldLocks(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name    string
		actions []string
		want    string
	}{
		{name: "succeeded", actions: []string{AgentActionLaunch, AgentActionHeartbeat, AgentActionCompleteSuccess}, want: AgentStateSucceeded},
		{name: "failed", actions: []string{AgentActionLaunch, AgentActionHeartbeat, AgentActionCompleteFailure}, want: AgentStateFailed},
		{name: "cancelled", actions: []string{AgentActionLaunch, AgentActionHeartbeat, AgentActionCancel, AgentActionCompleteFailure}, want: AgentStateCancelled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			timestamps := []string{"2026-01-02T03:04:06Z", "2026-01-02T03:04:07Z", "2026-01-02T03:04:08Z", "2026-01-02T03:04:09Z"}
			store, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedNow})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer store.Close()
			claim := createFederationClaim(t, ctx, store, "project-a", "run-root", "run-child", "child-a")
			record, err := RegisterAgent(ctx, store, federationRequest(claim))
			if err != nil {
				t.Fatalf("RegisterAgent: %v", err)
			}
			for i, action := range tt.actions {
				record, err = TransitionAgentRegistration(ctx, store, record.ChildAgentID, action, claim.ExecutorID, claim.ClaimGeneration, timestamps[i])
				if err != nil {
					t.Fatalf("TransitionAgentRegistration %s: %v", action, err)
				}
			}
			if record.RegistrationState != tt.want {
				t.Fatalf("registration state = %s, want %s", record.RegistrationState, tt.want)
			}
			var states []string
			if err := store.WithTx(ctx, func(tx Tx) error {
				rows, err := tx.Query(ctx, `SELECT state FROM agent_ownership_locks WHERE child_agent_id = ? ORDER BY id`, record.ChildAgentID)
				if err != nil {
					return err
				}
				defer rows.Close()
				for rows.Next() {
					var state string
					if err := rows.Scan(&state); err != nil {
						return err
					}
					states = append(states, state)
				}
				return rows.Err()
			}); err != nil {
				t.Fatalf("query ownership locks: %v", err)
			}
			if len(states) != 1 || states[0] != OwnershipStateReleased {
				t.Fatalf("lock states = %#v, want released", states)
			}
		})
	}
}

type federationClaim struct {
	ParentRunID     string
	RunID           string
	RootRunID       string
	ChildKey        string
	ExecutorID      string
	ClaimGeneration int64
	ProviderKey     string
}

func createFederationClaim(t *testing.T, ctx context.Context, store Store, projectID, rootRunID, childRunID, childKey string) federationClaim {
	t.Helper()
	at := "2026-01-01T00:00:00Z"
	if err := store.WithWriteTx(ctx, func(tx Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO projects(id, local_path, created_at, updated_at) VALUES (?, ?, ?, ?) ON CONFLICT(id) DO NOTHING`,
			projectID, "/tmp/"+projectID, at, at)
		return err
	}); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	err := PersistChildPlanGraph(ctx, store,
		RunNode{RunID: rootRunID, ProjectID: projectID, RootRunID: rootRunID, Depth: 0, Origin: "test", Status: "running", CreatedAt: at, UpdatedAt: at},
		[]RunNode{{RunID: childRunID, ProjectID: projectID, ParentRunID: rootRunID, RootRunID: rootRunID, Depth: 1, Origin: "test-child", Status: "planned", CreatedAt: at, UpdatedAt: at}},
		ChildPlanRecord{PlanID: "plan-" + childKey, ParentRunID: rootRunID, RootRunID: rootRunID, SchemaVersion: "loopcoder.child_plan.v1", MaxDepth: 2, MaxConcurrency: 1, PlanJSON: "{}", CreatedAt: at},
		[]RunEdgeRecord{{ParentRunID: rootRunID, ChildRunID: childRunID, RootRunID: rootRunID, PlanID: "plan-" + childKey, ChildKey: childKey, Depth: 1, Ordinal: 0, ScopeJSON: "{}", Permission: PermissionWrite, AggregationJSON: "{}", Status: "planned", CreatedAt: at, UpdatedAt: at}},
	)
	if err != nil {
		t.Fatalf("PersistChildPlanGraph: %v", err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)
	claim, err := ClaimChildRunExecution(ctx, store, rootRunID, childRunID, "executor-"+childKey, now, now.Add(30*time.Minute))
	if err != nil {
		t.Fatalf("ClaimChildRunExecution: %v", err)
	}
	return federationClaim{ParentRunID: rootRunID, RunID: childRunID, RootRunID: rootRunID, ChildKey: childKey, ExecutorID: claim.ExecutorID, ClaimGeneration: claim.ClaimGeneration, ProviderKey: claim.ProviderKey}
}

func federationRequest(claim federationClaim) AgentRegistrationRequest {
	parentScope := AgentScopeGrant{
		Permission:      PermissionWrite,
		ReadScope:       []string{"src", "src/a.go"},
		WriteScope:      []string{"src/a.go"},
		PathScope:       []string{"src/a.go"},
		RepositoryScope: []string{"project-a"},
		WorktreeScope:   []string{"worktree-a"},
		CommandScope:    []string{"go-test"},
		NetworkScope:    []string{"none"},
		CredentialScope: []string{"none"},
		SideEffectScope: []string{"repo-write"},
		ApprovalScope:   []string{"auth-a"},
	}
	return AgentRegistrationRequest{
		ProjectID:              "project-a",
		DeliveryRunID:          "drun-a",
		RootRunID:              claim.RootRunID,
		ParentRunID:            claim.ParentRunID,
		RunID:                  claim.RunID,
		Depth:                  1,
		TaskID:                 "task-a",
		AttemptID:              "attempt-a",
		PlanID:                 "plan-" + claim.ChildKey,
		ChildKey:               claim.ChildKey,
		AdapterID:              "claude",
		ProviderInstallationID: "pinst-a",
		AccountProfileID:       "acct-a",
		ModelCapabilityID:      "mcap-a",
		RoutingDecisionID:      "route-a",
		ProviderSessionRef:     "session-ref-a",
		Scope:                  parentScope,
		ParentScope:            &parentScope,
		Permission:             PermissionWrite,
		SideEffectClass:        "repo-write",
		BudgetBindings: []AgentBudgetBinding{{
			BudgetPolicyID:         "bpol-a",
			BudgetReservationID:    "bres-a-" + claim.ChildKey,
			ReservedQuantitiesJSON: `{"attempts":1}`,
			AncestorBudgetRefsJSON: "[]",
			ReservationState:       "active",
		}},
		OwnershipLocks: []AgentOwnershipLock{{
			ResourceKind:   "repo-path",
			ResourceKey:    "src/a.go",
			State:          OwnershipStateHeld,
			LeaseExpiresAt: "2026-01-03T00:00:00Z",
		}},
		ClaimGeneration:        claim.ClaimGeneration,
		ExecutorID:             claim.ExecutorID,
		ProviderIDempotencyKey: claim.ProviderKey,
		CancellationChannel:    "local-cancel",
		ExpectedOutputsJSON:    "{}",
		PlanFingerprint:        "sha256:plan",
		PolicyFingerprint:      "sha256:policy",
		CreatedAt:              "2026-01-01T00:00:01Z",
	}
}
