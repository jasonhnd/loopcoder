package storage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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
	if len(events) != 4 {
		t.Fatalf("event count = %d, want register+refusal+launch+heartbeat", len(events))
	}
	if events[0].previous != "" || events[1].previous != events[0].hash || events[2].previous != events[1].hash || events[3].previous != events[2].hash {
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

func TestAgentRegistrationRequiresCallerTimestamps(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedNow})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	claim := createFederationClaim(t, ctx, store, "project-a", "run-root", "run-child", "child-a")
	req := federationRequest(claim)
	req.CreatedAt = ""
	if _, err := RegisterAgent(ctx, store, req); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("RegisterAgent empty created_at error = %v, want ErrInvalidRecord", err)
	}
	req.CreatedAt = time.Time{}.Format(time.RFC3339Nano)
	if _, err := RegisterAgent(ctx, store, req); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("RegisterAgent zero created_at error = %v, want ErrInvalidRecord", err)
	}

	req = federationRequest(claim)
	record, err := RegisterAgent(ctx, store, req)
	if err != nil {
		t.Fatalf("RegisterAgent valid: %v", err)
	}
	for _, tc := range []struct {
		name string
		at   string
	}{
		{name: "empty"},
		{name: "zero", at: time.Time{}.Format(time.RFC3339Nano)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := TransitionAgentRegistration(ctx, store, record.ChildAgentID, AgentActionLaunch, claim.ExecutorID, claim.ClaimGeneration, tc.at); !errors.Is(err, ErrInvalidRecord) {
				t.Fatalf("TransitionAgentRegistration error = %v, want ErrInvalidRecord", err)
			}
		})
	}
}

func TestAgentOwnershipLeaseRejectsOverlappingWritersAndStaleOwners(t *testing.T) {
	ctx := context.Background()
	now := fixedNow()
	store, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedNow})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	rootLease, err := AcquireAgentOwnershipLease(ctx, store, AgentOwnershipLeaseRequest{
		ProjectID:     "project-a",
		DeliveryRunID: "delivery-a",
		RunID:         "run-a",
		OwnerID:       "worker-a",
		Now:           now,
		LeaseUntil:    now.Add(time.Hour),
		Resources: []AgentOwnershipResource{
			{ResourceKind: "repo-path", ResourceKey: "."},
		},
	})
	if err != nil {
		t.Fatalf("AcquireAgentOwnershipLease root: %v", err)
	}
	if err := ValidateAgentOwnershipFence(ctx, store, rootLease); err != nil {
		t.Fatalf("ValidateAgentOwnershipFence root: %v", err)
	}
	if _, err := AcquireAgentOwnershipLease(ctx, store, AgentOwnershipLeaseRequest{
		ProjectID:     "project-a",
		DeliveryRunID: "delivery-b",
		RunID:         "run-b",
		OwnerID:       "worker-b",
		Now:           now,
		LeaseUntil:    now.Add(time.Hour),
		Resources: []AgentOwnershipResource{
			{ResourceKind: "repo-path", ResourceKey: "src/nested/file.go"},
		},
	}); !errors.Is(err, ErrOneWriterConflict) {
		t.Fatalf("overlap acquire error = %v, want ErrOneWriterConflict", err)
	}
	if err := ReleaseAgentOwnershipLease(ctx, store, rootLease, now.Add(time.Minute)); err != nil {
		t.Fatalf("ReleaseAgentOwnershipLease root: %v", err)
	}
	if err := ValidateAgentOwnershipFence(ctx, store, rootLease); !errors.Is(err, ErrOwnershipStale) {
		t.Fatalf("released owner fence error = %v, want ErrOwnershipStale", err)
	}
	nextLease, err := AcquireAgentOwnershipLease(ctx, store, AgentOwnershipLeaseRequest{
		ProjectID:     "project-a",
		DeliveryRunID: "delivery-b",
		RunID:         "run-b",
		OwnerID:       "worker-b",
		Now:           now.Add(2 * time.Minute),
		LeaseUntil:    now.Add(time.Hour),
		Resources: []AgentOwnershipResource{
			{ResourceKind: "repo-path", ResourceKey: "src/nested/file.go"},
		},
	})
	if err != nil {
		t.Fatalf("AcquireAgentOwnershipLease after release: %v", err)
	}
	if nextLease.ClaimGeneration != 1 {
		t.Fatalf("independent run generation = %d, want 1", nextLease.ClaimGeneration)
	}
	if err := ValidateAgentOwnershipFence(ctx, store, rootLease); !errors.Is(err, ErrOwnershipStale) {
		t.Fatalf("old owner after takeover fence error = %v, want ErrOwnershipStale", err)
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
	for _, tc := range []struct {
		name    string
		from    string
		action  string
		wantErr error
	}{
		{name: "planned launch requires registration", from: AgentStatePlanned, action: AgentActionLaunch, wantErr: ErrAgentRegistrationRequired},
		{name: "registered heartbeat invalid", from: AgentStateRegistered, action: AgentActionHeartbeat, wantErr: ErrInvalidTransition},
		{name: "terminal launch invalid", from: AgentStateSucceeded, action: AgentActionLaunch, wantErr: ErrTerminalState},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := nextAgentRegistrationState(tc.from, tc.action); !errors.Is(err, tc.wantErr) {
				t.Fatalf("%s/%s error = %v, want %v", tc.from, tc.action, err, tc.wantErr)
			}
		})
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

func TestOverlappingReadOnlyRegistrationsDoNotRequireWriterLocks(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedNow})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	claim := createFederationClaim(t, ctx, store, "project-a", "run-root", "run-child", "child-a")
	readScope := federationReadOnlyScope("project-a")
	if err := updateFederationAuthority(ctx, store, "project-a", readScope, PermissionReadOnly, SideEffectLocalRead); err != nil {
		t.Fatalf("update read-only authority: %v", err)
	}
	req := federationRequest(claim)
	req.Scope = readScope
	req.ParentScope = &readScope
	req.Permission = PermissionReadOnly
	req.SideEffectClass = SideEffectLocalRead
	req.OwnershipLocks = nil
	if _, err := RegisterAgent(ctx, store, req); err != nil {
		t.Fatalf("RegisterAgent first read-only: %v", err)
	}

	claim2 := createFederationClaim(t, ctx, store, "project-a", "run-root-2", "run-child-2", "child-b")
	if err := updateFederationAuthority(ctx, store, "project-a", readScope, PermissionReadOnly, SideEffectLocalRead); err != nil {
		t.Fatalf("update second read-only authority: %v", err)
	}
	req2 := federationRequest(claim2)
	req2.ChildKey = "child-b"
	req2.RunID = "run-child-2"
	req2.Scope = readScope
	req2.ParentScope = &readScope
	req2.Permission = PermissionReadOnly
	req2.SideEffectClass = SideEffectLocalRead
	req2.OwnershipLocks = nil
	if _, err := RegisterAgent(ctx, store, req2); err != nil {
		t.Fatalf("RegisterAgent overlapping read-only: %v", err)
	}
}

func TestValidateNativeChildLaunchRequiresLiveBudgetReservation(t *testing.T) {
	for _, reservationState := range []string{"inactive", "committed", "released", "expired"} {
		t.Run(reservationState, func(t *testing.T) {
			ctx := context.Background()
			store, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedNow})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer store.Close()

			claim := createFederationClaim(t, ctx, store, "project-a", "run-root", "run-child", "child-a")
			req := federationRequest(claim)
			if err := store.WithWriteTx(ctx, func(tx Tx) error {
				state := reservationState
				lease := "2026-01-01T01:00:00Z"
				if reservationState == "inactive" {
					state = "refused"
				}
				if reservationState == "expired" {
					state = BudgetReservationStateActive
					lease = "2025-12-31T23:00:00Z"
				}
				_, err := tx.Exec(ctx, `UPDATE budget_reservations SET state = ?, lease_expires_at = ? WHERE budget_reservation_id = ?`,
					state, lease, req.BudgetBindings[0].BudgetReservationID)
				return err
			}); err != nil {
				t.Fatalf("mutate authoritative reservation: %v", err)
			}
			req.OwnershipLocks[0].LeaseExpiresAt = formatTimestamp(fixedNow().Add(time.Hour))
			if _, err := RegisterAgent(ctx, store, req); !errors.Is(err, ErrChildBudgetRequired) {
				t.Fatalf("RegisterAgent error = %v, want ErrChildBudgetRequired", err)
			}
		})
	}

	t.Run("active", func(t *testing.T) {
		ctx := context.Background()
		store, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedNow})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer store.Close()

		claim := createFederationClaim(t, ctx, store, "project-a", "run-root", "run-child", "child-a")
		req := federationRequest(claim)
		req.OwnershipLocks[0].LeaseExpiresAt = formatTimestamp(fixedNow().Add(time.Hour))
		if _, err := RegisterAgent(ctx, store, req); err != nil {
			t.Fatalf("RegisterAgent: %v", err)
		}
		if _, err := ValidateNativeChildLaunch(ctx, store, claim.RunID, claim.ExecutorID, claim.ClaimGeneration); err != nil {
			t.Fatalf("ValidateNativeChildLaunch active: %v", err)
		}
	})
}

func TestValidateNativeChildLaunchRequiresLiveOwnershipLocks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(context.Context, Store, *AgentRegistrationRequest, federationClaim)
	}{
		{
			name: "expired lease",
			mutate: func(_ context.Context, _ Store, req *AgentRegistrationRequest, _ federationClaim) {
				req.OwnershipLocks[0].LeaseExpiresAt = formatTimestamp(fixedNow().Add(-time.Second))
			},
		},
		{
			name: "stale claim generation",
			mutate: func(ctx context.Context, store Store, _ *AgentRegistrationRequest, claim federationClaim) {
				if err := store.WithWriteTx(ctx, func(tx Tx) error {
					_, err := tx.Exec(ctx, `UPDATE agent_ownership_locks SET claim_generation = ? WHERE run_id = ?`,
						claim.ClaimGeneration+1, claim.RunID)
					return err
				}); err != nil {
					t.Fatalf("stale lock generation update: %v", err)
				}
			},
		},
		{
			name: "uncovered write scope",
			mutate: func(_ context.Context, _ Store, req *AgentRegistrationRequest, _ federationClaim) {
				req.OwnershipLocks[0].LeaseExpiresAt = formatTimestamp(fixedNow().Add(time.Hour))
				req.OwnershipLocks[0].ResourceKey = "src/other.go"
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedNow})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer store.Close()

			claim := createFederationClaim(t, ctx, store, "project-a", "run-root", "run-child", "child-a")
			req := federationRequest(claim)
			req.OwnershipLocks[0].LeaseExpiresAt = formatTimestamp(fixedNow().Add(time.Hour))
			if tc.name != "stale claim generation" {
				tc.mutate(ctx, store, &req, claim)
			}
			if _, err := RegisterAgent(ctx, store, req); err != nil {
				if errors.Is(err, ErrOwnershipRequired) {
					return
				}
				t.Fatalf("RegisterAgent: %v", err)
			}
			if tc.name == "stale claim generation" {
				tc.mutate(ctx, store, &req, claim)
			}
			if _, err := ValidateNativeChildLaunch(ctx, store, claim.RunID, claim.ExecutorID, claim.ClaimGeneration); !errors.Is(err, ErrOwnershipRequired) {
				t.Fatalf("ValidateNativeChildLaunch error = %v, want ErrOwnershipRequired", err)
			}
		})
	}

	t.Run("held current covering lock", func(t *testing.T) {
		ctx := context.Background()
		store, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedNow})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer store.Close()

		claim := createFederationClaim(t, ctx, store, "project-a", "run-root", "run-child", "child-a")
		req := federationRequest(claim)
		req.OwnershipLocks[0].LeaseExpiresAt = formatTimestamp(fixedNow().Add(time.Hour))
		req.OwnershipLocks[0].ResourceKey = "src"
		if _, err := RegisterAgent(ctx, store, req); err != nil {
			t.Fatalf("RegisterAgent: %v", err)
		}
		if _, err := ValidateNativeChildLaunch(ctx, store, claim.RunID, claim.ExecutorID, claim.ClaimGeneration); err != nil {
			t.Fatalf("ValidateNativeChildLaunch covering lock: %v", err)
		}
	})
}

func TestValidateNativeChildLaunchRejectsStaleParentAuthority(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedNow})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	claim := createFederationClaim(t, ctx, store, "project-a", "run-root", "run-child", "child-a")
	req := federationRequest(claim)
	req.OwnershipLocks[0].LeaseExpiresAt = formatTimestamp(fixedNow().Add(time.Hour))
	if _, err := RegisterAgent(ctx, store, req); err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	if err := store.WithWriteTx(ctx, func(tx Tx) error {
		_, err := tx.Exec(ctx, `UPDATE delivery_runs SET policy_fingerprint = ? WHERE delivery_run_id = ?`, "sha256:policy-new", req.DeliveryRunID)
		return err
	}); err != nil {
		t.Fatalf("mutate parent authority: %v", err)
	}
	if _, err := ValidateNativeChildLaunch(ctx, store, claim.RunID, claim.ExecutorID, claim.ClaimGeneration); !errors.Is(err, ErrAgentFingerprintMismatch) {
		t.Fatalf("ValidateNativeChildLaunch error = %v, want ErrAgentFingerprintMismatch", err)
	}
	assertRefusalEvidence(t, ctx, store, ErrAgentFingerprintMismatchCode, "fingerprint")
	var state string
	if err := store.WithTx(ctx, func(tx Tx) error {
		return tx.QueryRow(ctx, `SELECT registration_state FROM agent_registrations WHERE child_run_id = ?`, claim.RunID).Scan(&state)
	}); err != nil {
		t.Fatalf("load registration state: %v", err)
	}
	if state != AgentStateRegistered {
		t.Fatalf("registration state = %q, want registered", state)
	}
}

func TestRegisterAgentBindsAcceptedChildIdentity(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*AgentRegistrationRequest)
	}{
		{name: "plan id", edit: func(req *AgentRegistrationRequest) { req.PlanID = "plan-other" }},
		{name: "child key", edit: func(req *AgentRegistrationRequest) { req.ChildKey = "child-other" }},
		{name: "depth", edit: func(req *AgentRegistrationRequest) { req.Depth = 2 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedNow})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer store.Close()

			claim := createFederationClaim(t, ctx, store, "project-a", "run-root", "run-child", "child-a")
			req := federationRequest(claim)
			req.OwnershipLocks[0].LeaseExpiresAt = formatTimestamp(fixedNow().Add(time.Hour))
			tc.edit(&req)
			if _, err := RegisterAgent(ctx, store, req); !errors.Is(err, ErrAgentRegistrationConflict) {
				t.Fatalf("RegisterAgent error = %v, want ErrAgentRegistrationConflict", err)
			}
			assertNoCommittedAgentAuthority(t, ctx, store)
			assertRefusalEvidence(t, ctx, store, ErrAgentRegistrationConflictCode, "registration-identity")
		})
	}
}

func TestRegisterAgentRejectsUnknownParentAuthorityEnums(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*AgentScopeGrant)
	}{
		{name: "permission", edit: func(scope *AgentScopeGrant) { scope.Permission = "future-admin" }},
		{name: "side effect", edit: func(scope *AgentScopeGrant) { scope.SideEffectClass = "future-write" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedNow})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer store.Close()

			claim := createFederationClaim(t, ctx, store, "project-a", "run-root", "run-child", "child-a")
			req := federationRequest(claim)
			req.OwnershipLocks[0].LeaseExpiresAt = formatTimestamp(fixedNow().Add(time.Hour))
			parentScope := *req.ParentScope
			tc.edit(&parentScope)
			req.ParentScope = &parentScope
			if _, err := RegisterAgent(ctx, store, req); !errors.Is(err, ErrScopeUnknown) {
				t.Fatalf("RegisterAgent error = %v, want ErrScopeUnknown", err)
			}
			assertNoCommittedAgentAuthority(t, ctx, store)
			assertRefusalEvidence(t, ctx, store, ErrScopeUnknownCode, "scope-unknown")
		})
	}
}

func TestRegisterAgentRejectsPhysicalScopeEscapeAndLeavesOnlyRefusalEvidence(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not reliable in unprivileged Windows test runs")
	}
	ctx := context.Background()
	store, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedNow})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outside, "escape.go"), []byte("package outside\n"), 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, "linked")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	claim := createFederationClaim(t, ctx, store, "project-a", "run-root", "run-child", "child-a")
	scope := federationAuthorityScope("project-a")
	scope.ReadScope = []string{"linked/escape.go"}
	scope.WriteScope = []string{"linked/escape.go"}
	scope.PathScope = []string{"linked/escape.go"}
	if err := updateFederationProjectAndAuthorityScope(ctx, store, "project-a", repo, scope); err != nil {
		t.Fatalf("update authority scope: %v", err)
	}
	req := federationRequest(claim)
	req.Scope = scope
	req.ParentScope = &scope
	req.OwnershipLocks[0].ResourceKey = "linked/escape.go"
	req.OwnershipLocks[0].LeaseExpiresAt = formatTimestamp(fixedNow().Add(time.Hour))
	if _, err := RegisterAgent(ctx, store, req); !errors.Is(err, ErrScopeWidening) {
		t.Fatalf("RegisterAgent error = %v, want ErrScopeWidening", err)
	}
	assertNoCommittedAgentAuthority(t, ctx, store)
	assertRefusalEvidence(t, ctx, store, ErrScopeWideningCode, "scope")
}

func TestRegisterAgentRejectsAlternateRepositoryScope(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedNow})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	claim := createFederationClaim(t, ctx, store, "project-a", "run-root", "run-child", "child-a")
	scope := federationAuthorityScope("project-a")
	scope.RepositoryScope = []string{"project-b"}
	if err := updateFederationProjectAndAuthorityScope(ctx, store, "project-a", t.TempDir(), scope); err != nil {
		t.Fatalf("update authority scope: %v", err)
	}
	req := federationRequest(claim)
	req.Scope = scope
	req.ParentScope = &scope
	req.OwnershipLocks[0].LeaseExpiresAt = formatTimestamp(fixedNow().Add(time.Hour))
	if _, err := RegisterAgent(ctx, store, req); !errors.Is(err, ErrCrossProjectReference) {
		t.Fatalf("RegisterAgent error = %v, want ErrCrossProjectReference", err)
	}
	assertNoCommittedAgentAuthority(t, ctx, store)
	assertRefusalEvidence(t, ctx, store, ErrCrossProjectReferenceCode, "project")
}

func TestRegisterAgentRejectsDriveQualifiedScopeEscape(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedNow})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	claim := createFederationClaim(t, ctx, store, "project-a", "run-root", "run-child", "child-a")
	req := federationRequest(claim)
	req.Scope.ReadScope = []string{"C:/outside.go"}
	req.Scope.WriteScope = []string{"C:/outside.go"}
	req.Scope.PathScope = []string{"C:/outside.go"}
	req.ParentScope = &req.Scope
	req.OwnershipLocks[0].ResourceKey = "C:/outside.go"
	req.OwnershipLocks[0].LeaseExpiresAt = formatTimestamp(fixedNow().Add(time.Hour))
	if _, err := RegisterAgent(ctx, store, req); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("RegisterAgent error = %v, want ErrInvalidRecord", err)
	}
	assertNoCommittedAgentAuthority(t, ctx, store)
	assertRefusalEvidence(t, ctx, store, ErrInvalidRecordCode, "record")
}

func TestTransitionAgentRegistrationReleasesHeldLocksAndAllowsNewWriter(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedNow})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	claim := createFederationClaim(t, ctx, store, "project-a", "run-root", "run-child", "child-a")
	req := federationRequest(claim)
	req.OwnershipLocks[0].LeaseExpiresAt = formatTimestamp(fixedNow().Add(time.Hour))
	record, err := RegisterAgent(ctx, store, req)
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	if _, err := TransitionAgentRegistration(ctx, store, record.ChildAgentID, AgentActionLaunch, claim.ExecutorID, claim.ClaimGeneration, "2026-01-01T00:00:02Z"); err != nil {
		t.Fatalf("launch transition: %v", err)
	}
	if _, err := TransitionAgentRegistration(ctx, store, record.ChildAgentID, AgentActionHeartbeat, claim.ExecutorID, claim.ClaimGeneration, "2026-01-01T00:00:03Z"); err != nil {
		t.Fatalf("heartbeat transition: %v", err)
	}
	if _, err := TransitionAgentRegistration(ctx, store, record.ChildAgentID, AgentActionCompleteSuccess, claim.ExecutorID, claim.ClaimGeneration, "2026-01-01T00:00:04Z"); err != nil {
		t.Fatalf("success transition: %v", err)
	}
	var lockState string
	if err := store.WithTx(ctx, func(tx Tx) error {
		return tx.QueryRow(ctx, `SELECT state FROM agent_ownership_locks WHERE child_agent_id = ?`, record.ChildAgentID).Scan(&lockState)
	}); err != nil {
		t.Fatalf("query lock state: %v", err)
	}
	if lockState != OwnershipStateReleased {
		t.Fatalf("lock state = %q, want released", lockState)
	}

	claim2 := createFederationClaim(t, ctx, store, "project-a", "run-root-2", "run-child-2", "child-b")
	req2 := federationRequest(claim2)
	req2.OwnershipLocks[0].LeaseExpiresAt = formatTimestamp(fixedNow().Add(time.Hour))
	if _, err := RegisterAgent(ctx, store, req2); err != nil {
		t.Fatalf("RegisterAgent second writer after release: %v", err)
	}
}

func TestTerminalCompletionRequiresLiveOwnershipFence(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedNow})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	claim := createFederationClaim(t, ctx, store, "project-a", "run-root", "run-child", "child-a")
	req := federationRequest(claim)
	req.OwnershipLocks[0].LeaseExpiresAt = formatTimestamp(fixedNow().Add(time.Hour))
	record, err := RegisterAgent(ctx, store, req)
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	if _, err := TransitionAgentRegistration(ctx, store, record.ChildAgentID, AgentActionLaunch, claim.ExecutorID, claim.ClaimGeneration, "2026-01-01T00:00:02Z"); err != nil {
		t.Fatalf("launch transition: %v", err)
	}
	if _, err := TransitionAgentRegistration(ctx, store, record.ChildAgentID, AgentActionHeartbeat, claim.ExecutorID, claim.ClaimGeneration, "2026-01-01T00:00:03Z"); err != nil {
		t.Fatalf("heartbeat transition: %v", err)
	}
	if err := store.WithWriteTx(ctx, func(tx Tx) error {
		_, err := tx.Exec(ctx, `UPDATE agent_ownership_locks SET lease_expires_at = ? WHERE child_agent_id = ?`,
			"2026-01-01T00:00:03Z", record.ChildAgentID)
		return err
	}); err != nil {
		t.Fatalf("expire ownership lock: %v", err)
	}

	err = CompleteClaimedChildRun(ctx, store, claim.ParentRunID, claim.RunID, claim.ExecutorID, claim.ClaimGeneration, "succeeded", "2026-01-01T00:00:04Z", "terminal", "")
	if !errors.Is(err, ErrOwnershipStale) {
		t.Fatalf("CompleteClaimedChildRun error = %v, want ErrOwnershipStale", err)
	}
	var state string
	if err := store.WithTx(ctx, func(tx Tx) error {
		return tx.QueryRow(ctx, `SELECT registration_state FROM agent_registrations WHERE id = ?`, record.ChildAgentID).Scan(&state)
	}); err != nil {
		t.Fatalf("load registration state: %v", err)
	}
	if state != AgentStateRunning {
		t.Fatalf("registration state = %q, want rollback to running", state)
	}
}

func TestProviderReceiptCompletionRequiresProviderReceiptOwnership(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedNow})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	claim := createFederationClaim(t, ctx, store, "project-a", "run-root", "run-child", "child-a")
	req := federationRequest(claim)
	req.OwnershipLocks[0].LeaseExpiresAt = formatTimestamp(fixedNow().Add(time.Hour))
	record, err := RegisterAgent(ctx, store, req)
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	if _, err := TransitionAgentRegistration(ctx, store, record.ChildAgentID, AgentActionLaunch, claim.ExecutorID, claim.ClaimGeneration, "2026-01-01T00:00:02Z"); err != nil {
		t.Fatalf("launch transition: %v", err)
	}
	if _, err := TransitionAgentRegistration(ctx, store, record.ChildAgentID, AgentActionHeartbeat, claim.ExecutorID, claim.ClaimGeneration, "2026-01-01T00:00:03Z"); err != nil {
		t.Fatalf("heartbeat transition: %v", err)
	}

	err = CompleteClaimedChildRun(ctx, store, claim.ParentRunID, claim.RunID, claim.ExecutorID, claim.ClaimGeneration, "succeeded", "2026-01-01T00:00:04Z", "terminal", "provider-receipt-1")
	if !errors.Is(err, ErrOwnershipRequired) {
		t.Fatalf("CompleteClaimedChildRun error = %v, want ErrOwnershipRequired", err)
	}
}

func TestOwnershipFenceRejectsStaleGenerationAndNonOwner(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedNow})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	claim := createFederationClaim(t, ctx, store, "project-a", "run-root", "run-child", "child-a")
	req := federationRequest(claim)
	req.OwnershipLocks[0].LeaseExpiresAt = formatTimestamp(fixedNow().Add(time.Hour))
	record, err := RegisterAgent(ctx, store, req)
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	lockID := record.OwnershipLockIDs[0]
	if err := ValidateAgentOwnershipFence(ctx, store, AgentOwnershipFence{
		ChildAgentID:    record.ChildAgentID,
		RunID:           record.RunID,
		ExecutorID:      claim.ExecutorID,
		ClaimGeneration: claim.ClaimGeneration,
		LockID:          lockID,
		LockGeneration:  1,
		ResourceKind:    "repo-path",
		ResourceKey:     "src/a.go",
		At:              "2026-01-01T00:00:02Z",
	}); err != nil {
		t.Fatalf("ValidateAgentOwnershipFence current: %v", err)
	}
	if err := store.WithWriteTx(ctx, func(tx Tx) error {
		_, err := tx.Exec(ctx, `UPDATE agent_ownership_locks SET lock_generation = lock_generation + 1 WHERE id = ?`, lockID)
		return err
	}); err != nil {
		t.Fatalf("bump lock generation: %v", err)
	}
	err = ValidateAgentOwnershipFence(ctx, store, AgentOwnershipFence{
		ChildAgentID:    record.ChildAgentID,
		RunID:           record.RunID,
		ExecutorID:      claim.ExecutorID,
		ClaimGeneration: claim.ClaimGeneration,
		LockID:          lockID,
		LockGeneration:  1,
		ResourceKind:    "repo-path",
		ResourceKey:     "src/a.go",
		At:              "2026-01-01T00:00:03Z",
	})
	if !errors.Is(err, ErrOwnershipStale) {
		t.Fatalf("stale lock generation error = %v, want ErrOwnershipStale", err)
	}
	err = ValidateAgentOwnershipFence(ctx, store, AgentOwnershipFence{
		ChildAgentID:    record.ChildAgentID,
		RunID:           record.RunID,
		ExecutorID:      "other-executor",
		ClaimGeneration: claim.ClaimGeneration,
		LockID:          lockID,
		LockGeneration:  2,
		ResourceKind:    "repo-path",
		ResourceKey:     "src/a.go",
		At:              "2026-01-01T00:00:04Z",
	})
	if !errors.Is(err, ErrOwnershipStale) {
		t.Fatalf("non-owner error = %v, want ErrOwnershipStale", err)
	}
}

func TestPreLaunchRecoveryTakeoverAdvancesRegistrationGeneration(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedNow})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	claim := createFederationClaim(t, ctx, store, "project-a", "run-root", "run-child", "child-a")
	req := federationRequest(claim)
	req.OwnershipLocks[0].LeaseExpiresAt = "2026-01-01T00:30:01Z"
	record, err := RegisterAgent(ctx, store, req)
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}

	now := time.Date(2026, 1, 1, 0, 31, 0, 0, time.UTC)
	replayReq := federationRequest(claim)
	replayReq.CreatedAt = ""
	replayReq.OwnershipLocks[0].LeaseExpiresAt = formatTimestamp(fixedNow().Add(time.Hour))
	takeover, replay, err := ClaimAndRegisterNativeChild(ctx, store, claim.ParentRunID, claim.RunID, "executor-recovery", now, fixedNow().Add(time.Hour), replayReq)
	if err != nil {
		t.Fatalf("ClaimAndRegisterNativeChild takeover: %v", err)
	}
	if takeover.Outcome != ClaimOutcomeStaleClaim || replay.ChildAgentID != record.ChildAgentID || replay.ClaimGeneration != 2 || replay.ExecutorID != "executor-recovery" {
		t.Fatalf("takeover=%#v replay=%#v, want stale takeover on same agent generation 2", takeover, replay)
	}
	if _, err := ValidateNativeChildLaunch(ctx, store, claim.RunID, "executor-recovery", 2); err != nil {
		t.Fatalf("ValidateNativeChildLaunch recovered owner: %v", err)
	}
	if _, err := ValidateNativeChildLaunch(ctx, store, claim.RunID, claim.ExecutorID, 1); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("ValidateNativeChildLaunch stale owner error = %v, want ErrStaleClaim", err)
	}
}

func TestAmbiguousExpiredExecutingRecoveryMarksRegistrationNeedsHuman(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedNow})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	claim := createFederationClaim(t, ctx, store, "project-a", "run-root", "run-child", "child-a")
	req := federationRequest(claim)
	req.OwnershipLocks[0].LeaseExpiresAt = formatTimestamp(fixedNow().Add(time.Hour))
	record, err := RegisterAgent(ctx, store, req)
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	if _, err := TransitionAgentRegistration(ctx, store, record.ChildAgentID, AgentActionLaunch, claim.ExecutorID, claim.ClaimGeneration, "2026-01-01T00:00:02Z"); err != nil {
		t.Fatalf("launch transition: %v", err)
	}
	if err := UpdateChildRunClaimPhase(ctx, store, claim.ParentRunID, claim.RunID, claim.ExecutorID, claim.ClaimGeneration, ClaimPhaseExecuting, "2026-01-01T00:00:03Z", ""); err != nil {
		t.Fatalf("UpdateChildRunClaimPhase executing: %v", err)
	}

	blocked, err := ClaimChildRunExecution(ctx, store, claim.ParentRunID, claim.RunID, "executor-recovery", fixedNow().Add(2*time.Hour), fixedNow().Add(3*time.Hour))
	if err != nil {
		t.Fatalf("ClaimChildRunExecution ambiguous: %v", err)
	}
	if blocked.Outcome != ClaimOutcomeBlocked {
		t.Fatalf("blocked claim = %#v, want blocked", blocked)
	}
	var state string
	if err := store.WithTx(ctx, func(tx Tx) error {
		return tx.QueryRow(ctx, `SELECT registration_state FROM agent_registrations WHERE id = ?`, record.ChildAgentID).Scan(&state)
	}); err != nil {
		t.Fatalf("load registration state: %v", err)
	}
	if state != AgentStateNeedsHuman {
		t.Fatalf("registration state = %q, want needs-human", state)
	}
}

func TestScopeInheritanceAndCrossProjectFailClosed(t *testing.T) {
	parent := AgentScopeGrant{Permission: PermissionWrite, SideEffectClass: SideEffectRepoWrite, ReadScope: []string{"src"}, WriteScope: []string{"src/a.go"}, PathScope: []string{"src/a.go"}, CredentialScope: []string{"none"}}
	child := AgentScopeGrant{Permission: PermissionOrchestrate, SideEffectClass: SideEffectRepoWrite, ReadScope: []string{"src"}, WriteScope: []string{"src/a.go"}, PathScope: []string{"src/a.go"}, CredentialScope: []string{"none"}}
	if err := ValidateScopeInheritance(parent, child); !errors.Is(err, ErrScopeWidening) {
		t.Fatalf("permission widening error = %v, want ErrScopeWidening", err)
	}
	child.Permission = PermissionWrite
	child.SideEffectClass = SideEffectExternalWrite
	parent.SideEffectClass = SideEffectRepoWrite
	if err := ValidateScopeInheritance(parent, child); !errors.Is(err, ErrScopeWidening) {
		t.Fatalf("side-effect widening error = %v, want ErrScopeWidening", err)
	}
	child.SideEffectClass = SideEffectRepoWrite
	child.CredentialScope = []string{"ENV_TOKEN"}
	if err := ValidateScopeInheritance(parent, child); !errors.Is(err, ErrCredentialScopeDenied) {
		t.Fatalf("credential scope error = %v, want ErrCredentialScopeDenied", err)
	}
}

func TestScopeInheritanceRejectsUnknownParentEnums(t *testing.T) {
	parent := AgentScopeGrant{Permission: "future-admin", SideEffectClass: SideEffectRepoWrite, ReadScope: []string{"src"}, WriteScope: []string{"src/a.go"}, PathScope: []string{"src/a.go"}, CredentialScope: []string{"none"}}
	child := AgentScopeGrant{Permission: PermissionReadOnly, SideEffectClass: SideEffectNone, ReadScope: []string{"src"}, CredentialScope: []string{"none"}}
	if err := ValidateScopeInheritance(parent, child); !errors.Is(err, ErrScopeUnknown) {
		t.Fatalf("unknown parent permission error = %v, want ErrScopeUnknown", err)
	}
	parent.Permission = PermissionOrchestrate
	parent.SideEffectClass = "future-write"
	if err := ValidateScopeInheritance(parent, child); !errors.Is(err, ErrScopeUnknown) {
		t.Fatalf("unknown parent side effect error = %v, want ErrScopeUnknown", err)
	}
}

func TestAgentAuthorityRefusalEvidenceIsBoundedAndRedacted(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedNow})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	secret := "sk-" + strings.Repeat("runtime", 8)
	localPath := filepath.Join(t.TempDir(), "repo", "secret", "prompt.txt")
	longMultiByte := strings.Repeat("界", AgentAuthorityRefusalMessageRunes*2)
	req := AgentRegistrationRequest{
		ProjectID:       "project-a",
		DeliveryRunID:   "drun-a",
		ParentRunID:     "run-root",
		RunID:           "run-child",
		TaskID:          "task-a",
		AttemptID:       "attempt-a",
		ChildKey:        "child-a",
		PlanFingerprint: "sha256:plan",
		CreatedAt:       "2026-01-01T00:00:01Z",
		Classification:  "local-diagnostic",
	}
	cause := federationError(ErrScopeWideningCode, "path %s leaked %s %s", localPath, secret, longMultiByte)
	persistAgentAuthorityRefusal(ctx, store, req, cause)

	event := loadLatestRefusalEvent(t, ctx, store)
	if event.TerminalErrorCode != ErrScopeWideningCode || event.Boundary != "scope" {
		t.Fatalf("refusal event code/boundary = %s/%s", event.TerminalErrorCode, event.Boundary)
	}
	if !utf8.ValidString(event.Message) {
		t.Fatalf("refusal message is invalid UTF-8: %q", event.Message)
	}
	if len([]rune(event.Message)) > AgentAuthorityRefusalMessageRunes {
		t.Fatalf("refusal message rune length = %d, want <= %d", len([]rune(event.Message)), AgentAuthorityRefusalMessageRunes)
	}
	if strings.Contains(event.Message, secret) || strings.Contains(event.Message, localPath) || strings.Contains(event.Message, "prompt.txt") {
		t.Fatalf("refusal message leaked sensitive material: %q", event.Message)
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
	if strings.Contains(envelope.Output, secret[:8]) || strings.Contains(envelope.Output, "AKIA") {
		t.Fatalf("truncated redacted output leaked credential prefix: %#v", envelope)
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
		if err != nil {
			return err
		}
		return seedFederationAuthorityTx(ctx, tx, projectID, rootRunID, childRunID, childKey, at)
	}); err != nil {
		t.Fatalf("insert project authority: %v", err)
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

func federationAuthorityScope(projectID string) AgentScopeGrant {
	return AgentScopeGrant{
		Permission:      PermissionWrite,
		SideEffectClass: SideEffectRepoWrite,
		ReadScope:       []string{"src", "src/a.go"},
		WriteScope:      []string{"src/a.go"},
		PathScope:       []string{"src/a.go"},
		RepositoryScope: []string{projectID},
		WorktreeScope:   []string{"worktree-a"},
		CommandScope:    []string{"go-test"},
		NetworkScope:    []string{"none"},
		CredentialScope: []string{"none"},
		SideEffectScope: []string{SideEffectRepoWrite},
		ApprovalScope:   []string{"auth-a"},
	}
}

func federationReadOnlyScope(projectID string) AgentScopeGrant {
	return AgentScopeGrant{
		Permission:      PermissionReadOnly,
		SideEffectClass: SideEffectLocalRead,
		ReadScope:       []string{"src/a.go"},
		PathScope:       []string{"src/a.go"},
		RepositoryScope: []string{projectID},
		WorktreeScope:   []string{"worktree-a"},
		CommandScope:    []string{"go-test"},
		NetworkScope:    []string{"none"},
		CredentialScope: []string{"none"},
		SideEffectScope: []string{SideEffectLocalRead},
		ApprovalScope:   []string{"auth-a"},
	}
}

func seedFederationAuthorityTx(ctx context.Context, tx Tx, projectID, rootRunID, childRunID, childKey, at string) error {
	childAgentID := stableID("agent_", projectID, "drun-a", rootRunID, "task-a", "attempt-a", childKey, "sha256:plan")
	scopeJSON, err := json.Marshal(federationAuthorityScope(projectID))
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO delivery_runs(
		delivery_run_id, run_id, schema_version, record_version, project_id, root_run_id, parent_run_id,
		state, intent_summary, input_fingerprint, policy_fingerprint, plan_fingerprint, authorization_fingerprint,
		policy_version, max_side_effect_class, approval_status, override_status, created_at, updated_at,
		created_by_json, updated_by_json, host_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '{}', '{}', '{}')
		ON CONFLICT(delivery_run_id) DO NOTHING`,
		"drun-a", "delivery-run-"+childKey, "loopcoder.delivery_run.v1", 1, projectID, rootRunID, "",
		"approved", "test federation run", "sha256:input", "sha256:policy", "sha256:plan", "sha256:auth",
		"0805.agent_federation.v1", "repo-write", "approved", "none", at, at); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO delivery_tasks(
		task_id, schema_version, record_version, project_id, delivery_run_id, task_key, state, title,
		requirements_json, scope_json, permission, side_effect_class, policy_version, plan_fingerprint,
		authorization_fingerprint, created_at, updated_at, created_by_json, updated_by_json, host_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, '{}', ?, ?, ?, ?, ?, ?, ?, ?, '{}', '{}', '{}')
		ON CONFLICT(task_id) DO NOTHING`,
		"task-a", "loopcoder.delivery_task.v1", 1, projectID, "drun-a", "task-a", "approved", "task a",
		string(scopeJSON), PermissionWrite, SideEffectRepoWrite, "0805.agent_federation.v1", "sha256:plan", "sha256:auth", at, at); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO task_requirements(
		task_requirement_id, schema_version, record_version, task_requirement_fingerprint, project_id, delivery_run_id,
		task_id, task_key, role_key, risk_tier, permission_required, side_effect_class, required_output, scope_json,
		data_classification, network_required, nested_allowed, cancellation_required, quality_floor,
		provenance_summary, policy_version, plan_fingerprint, created_at, updated_at, created_by_json,
		updated_by_json, host_json, classification, confidence, heuristic, payload_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '{}', '{}', '{}', ?, ?, ?, '{}')
		ON CONFLICT(task_requirement_id) DO NOTHING`,
		"treq-a", "loopcoder.task_requirement.v1", 1, "sha256:req", projectID, "drun-a",
		"task-a", "task-a", "worker", "high", PermissionWrite, SideEffectRepoWrite, "json",
		string(scopeJSON), "internal", "none", 1, 1, "standard", "test", "0805.agent_federation.v1", "sha256:plan",
		at, at, "local-diagnostic", "high", 0); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO provider_installations(
		provider_installation_id, schema_version, record_version, scope, project_id, adapter_id, adapter_declaration_id,
		provider_display_name, executable_name, executable_identity_json, canonical_path_redacted, discovery_source,
		discovery_order, platform, version_confidence, installation_state, usable_for_invocation, created_at,
		updated_at, captured_at, freshness_state, confidence, side_effect_class, classification, payload_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '{}', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '{}')
		ON CONFLICT(provider_installation_id) DO NOTHING`,
		"pinst-a", "loopcoder.provider_installation.v1", 1, "project", projectID, "claude", "adecl-a",
		"Claude", "claude", "claude", "test", 1, "test", "high", "active", "yes", at, at, at,
		"fresh", "high", SideEffectLocalRead, "local-diagnostic"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO account_profiles(account_profile_id, adapter_id, provider_installation_id, payload_json)
		VALUES (?, ?, ?, '{}') ON CONFLICT(account_profile_id) DO NOTHING`, "acct-a", "claude", "pinst-a"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO model_catalog_snapshots(model_catalog_snapshot_id, adapter_id, provider_installation_id, payload_json)
		VALUES (?, ?, ?, '{}') ON CONFLICT(model_catalog_snapshot_id) DO NOTHING`, "snap-a", "claude", "pinst-a"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO model_capabilities(model_capability_id, model_catalog_snapshot_id, adapter_id, payload_json)
		VALUES (?, ?, ?, '{}') ON CONFLICT(model_capability_id) DO NOTHING`, "mcap-a", "snap-a", "claude"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO budget_policies(
		budget_policy_id, project_id, delivery_run_id, task_id, sub_agent_id, adapter_id, account_profile_id,
		model_capability_id, scope_kind, scope_key, quantity_kind, unit, window_kind, policy_mode,
		ceiling_value, active, policy_version, payload_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '{}')
		ON CONFLICT(budget_policy_id) DO NOTHING`,
		"bpol-a", projectID, "drun-a", "task-a", childAgentID, "claude", "acct-a", "mcap-a",
		"sub-agent", "sub-agent:"+childAgentID, "local-policy", "unit", "run", "hard", 1000, 1, "0805.agent_federation.v1"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO budget_aggregates(budget_policy_id, reserved_value, committed_value, updated_at)
		VALUES (?, ?, ?, ?) ON CONFLICT(budget_policy_id) DO UPDATE SET reserved_value = excluded.reserved_value, committed_value = excluded.committed_value, updated_at = excluded.updated_at`,
		"bpol-a", 100, 0, at); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO budget_reservations(
		budget_reservation_id, idempotency_key, request_fingerprint, requester_id, authorization_fingerprint,
		project_id, delivery_run_id, task_id, sub_agent_id, adapter_id, account_profile_id, model_capability_id,
		quantity_kind, unit, requested_value, reserved_value, state, generation, lease_expires_at, scope_key,
		policy_ids_json, payload_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(budget_reservation_id) DO NOTHING`,
		"bres-a-"+childKey, "idem-"+childKey, "sha256:budget", "test", "sha256:auth",
		projectID, "drun-a", "task-a", childAgentID, "claude", "acct-a", "mcap-a",
		"local-policy", "unit", 100, 100, BudgetReservationStateActive, 1, "2026-01-03T01:00:00Z", "sub-agent:"+childAgentID,
		`["bpol-a"]`, `{"reserved_value":100,"committed_value":0,"released_value":0,"state":"active","generation":1,"updated_at":"`+at+`"}`, at, at); err != nil {
		return err
	}
	return nil
}

func updateFederationProjectAndAuthorityScope(ctx context.Context, store Store, projectID, repoPath string, scope AgentScopeGrant) error {
	scopeJSON, err := json.Marshal(scope)
	if err != nil {
		return err
	}
	return store.WithWriteTx(ctx, func(tx Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE projects SET local_path = ?, local_path_canonical = ?, git_root = ? WHERE id = ?`,
			repoPath, repoPath, repoPath, projectID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE delivery_tasks SET scope_json = ? WHERE project_id = ? AND delivery_run_id = ? AND task_id = ?`,
			string(scopeJSON), projectID, "drun-a", "task-a"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE task_requirements SET scope_json = ? WHERE project_id = ? AND delivery_run_id = ? AND task_id = ?`,
			string(scopeJSON), projectID, "drun-a", "task-a")
		return err
	})
}

func updateFederationAuthority(ctx context.Context, store Store, projectID string, scope AgentScopeGrant, permission, sideEffectClass string) error {
	scopeJSON, err := json.Marshal(scope)
	if err != nil {
		return err
	}
	return store.WithWriteTx(ctx, func(tx Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE delivery_tasks SET scope_json = ?, permission = ?, side_effect_class = ? WHERE project_id = ? AND delivery_run_id = ? AND task_id = ?`,
			string(scopeJSON), permission, sideEffectClass, projectID, "drun-a", "task-a"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE task_requirements SET scope_json = ?, permission_required = ?, side_effect_class = ? WHERE project_id = ? AND delivery_run_id = ? AND task_id = ?`,
			string(scopeJSON), permission, sideEffectClass, projectID, "drun-a", "task-a"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE run_edges SET permission = ?, scope_json = ? WHERE root_run_id IN ('run-root', 'run-root-2')`,
			permission, string(scopeJSON))
		return err
	})
}

func assertNoCommittedAgentAuthority(t *testing.T, ctx context.Context, store Store) {
	t.Helper()
	if err := store.WithTx(ctx, func(tx Tx) error {
		for _, table := range []string{"agent_registrations", "agent_scope_grants", "agent_budget_bindings", "agent_ownership_locks"} {
			var count int
			if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				t.Fatalf("%s rows = %d, want 0", table, count)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("assert no committed authority: %v", err)
	}
}

func assertRefusalEvidence(t *testing.T, ctx context.Context, store Store, code FederationErrorCode, boundary string) {
	t.Helper()
	event := loadLatestRefusalEvent(t, ctx, store)
	if event.TerminalErrorCode != code || event.Boundary != boundary || strings.TrimSpace(event.Message) == "" {
		t.Fatalf("refusal event = %#v, want code %s boundary %s with message", event, code, boundary)
	}
}

func loadLatestRefusalEvent(t *testing.T, ctx context.Context, store Store) struct {
	TerminalErrorCode FederationErrorCode `json:"terminal_error_code"`
	Boundary          string              `json:"boundary"`
	Message           string              `json:"message"`
} {
	t.Helper()
	var payload string
	if err := store.WithTx(ctx, func(tx Tx) error {
		return tx.QueryRow(ctx, `SELECT payload_json FROM agent_events WHERE event_kind = ? ORDER BY created_at DESC, id DESC LIMIT 1`, "registration.refused").Scan(&payload)
	}); err != nil {
		t.Fatalf("load refusal event: %v", err)
	}
	var event struct {
		TerminalErrorCode FederationErrorCode `json:"terminal_error_code"`
		Boundary          string              `json:"boundary"`
		Message           string              `json:"message"`
	}
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		t.Fatalf("unmarshal refusal event: %v", err)
	}
	return event
}

func federationRequest(claim federationClaim) AgentRegistrationRequest {
	parentScope := federationAuthorityScope("project-a")
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
			ReservedQuantitiesJSON: "{}",
			AncestorBudgetRefsJSON: "[]",
			ReservationState:       "active",
		}},
		OwnershipLocks: []AgentOwnershipLock{{
			ResourceKind: "repo-path",
			ResourceKey:  "src/a.go",
			State:        OwnershipStateHeld,
		}},
		ClaimGeneration:          claim.ClaimGeneration,
		ExecutorID:               claim.ExecutorID,
		ProviderIDempotencyKey:   claim.ProviderKey,
		CancellationChannel:      "local-cancel",
		ExpectedOutputsJSON:      "{}",
		PlanFingerprint:          "sha256:plan",
		PolicyFingerprint:        "sha256:policy",
		AuthorizationFingerprint: "sha256:auth",
		CreatedAt:                "2026-01-01T00:00:01Z",
	}
}
