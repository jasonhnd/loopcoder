package routing

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jasonhnd/loopcoder/internal/delivery"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/taskrequirements"
)

func TestVerificationDecisionAcceptsIndependentVerifierAndReplaysStably(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, worker, verifier := persistVerificationRoutes(t, ctx, fixture, fixture.candidate("claude", "acct-c", "claude-good"), nil)
	defer store.Close()

	input := verificationInput(worker, verifier, passVerdict(verifier, "diff clean", "ev-diff"))
	first, err := DecideAndPersistVerification(ctx, store, input)
	if err != nil {
		t.Fatalf("DecideAndPersistVerification: %v", err)
	}
	if first.DecisionStatus != VerificationStatusAccepted || first.FinalAuthority != FinalAuthorityAutomatedVerifier {
		t.Fatalf("accepted decision = %#v", first)
	}
	if first.RequiredIndependence != taskrequirements.IndependenceDifferentProvider || first.ActualIndependence != taskrequirements.IndependenceDifferentProvider {
		t.Fatalf("independence = required %s actual %s", first.RequiredIndependence, first.ActualIndependence)
	}

	replayed, err := DecideAndPersistVerification(ctx, store, input)
	if err != nil {
		t.Fatalf("replay verification: %v", err)
	}
	if replayed.VerificationDecisionID != first.VerificationDecisionID || replayed.CreatedAt != first.CreatedAt || countVerificationDecisions(t, ctx, store, worker.RoutingDecisionID) != 1 {
		t.Fatalf("replay duplicated or rewrote decision: first=%#v replay=%#v count=%d", first, replayed, countVerificationDecisions(t, ctx, store, worker.RoutingDecisionID))
	}
	human := ExplainVerificationHuman(first)
	for _, want := range []string{"final_authority=automated-verifier", "required=different-provider", "actual=different-provider"} {
		if !strings.Contains(human, want) {
			t.Fatalf("human explain missing %q:\n%s", want, human)
		}
	}
	stable, err := ExplainVerificationJSON(first)
	if err != nil {
		t.Fatalf("ExplainVerificationJSON: %v", err)
	}
	for _, want := range []string{`"required_independence":"different-provider"`, `"final_authority":"automated-verifier"`, `"evidence_refs"`} {
		if !strings.Contains(string(stable), want) {
			t.Fatalf("json explain missing %q:\n%s", want, stable)
		}
	}
}

func TestVerificationDecisionRequiresHumanWhenIndependenceCannotBeEstablished(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, worker, verifier := persistVerificationRoutes(t, ctx, fixture, fixture.candidate("codex", "acct-b", "codex-verifier"), nil)
	defer store.Close()

	decision, err := DecideAndPersistVerification(ctx, store, verificationInput(worker, verifier, passVerdict(verifier, "same provider", "ev-same-provider")))
	if !errors.Is(err, taskrequirements.ErrVerifierIndependenceRequired) {
		t.Fatalf("verification error = %v, want ErrVerifierIndependenceRequired", err)
	}
	if decision.DecisionStatus != VerificationStatusNeedsHuman || decision.FinalAuthority != FinalAuthorityHuman || decision.ActualIndependence != taskrequirements.IndependenceDifferentAccount {
		t.Fatalf("needs-human independence decision = %#v", decision)
	}
}

func TestVerificationDecisionRejectsUnsafeAuthorityKeysBeforeLookup(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, worker, verifier := persistVerificationRoutes(t, ctx, fixture, fixture.candidate("claude", "acct-c", "claude-good"), nil)
	defer store.Close()

	secret := "AKIA" + strings.Repeat("A", 16)
	cases := []struct {
		name   string
		mutate func(*VerificationDecisionInput)
	}{
		{"decision secret", func(input *VerificationDecisionInput) { input.DecisionKey = "verify-" + secret }},
		{"idempotency secret", func(input *VerificationDecisionInput) { input.IdempotencyKey = "ghp_" + strings.Repeat("a", 36) }},
		{"decision unix path", func(input *VerificationDecisionInput) { input.DecisionKey = "/Users/example/private/token" }},
		{"idempotency windows path", func(input *VerificationDecisionInput) { input.IdempotencyKey = `C:\Users\example\private\secret.txt` }},
		{"oversized idempotency", func(input *VerificationDecisionInput) { input.IdempotencyKey = "key-" + strings.Repeat("a", 200) }},
		{"malformed utf8", func(input *VerificationDecisionInput) { input.DecisionKey = "verify-" + string([]byte{0xff, 0xfe}) }},
		{"leading whitespace", func(input *VerificationDecisionInput) { input.IdempotencyKey = " replay-key" }},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			input := verificationInput(worker, verifier, passVerdict(verifier, "safe keys", "ev-safe-keys"))
			tt.mutate(&input)
			decision, err := DecideAndPersistVerification(ctx, store, input)
			if !errors.Is(err, taskrequirements.ErrInvalidRecord) {
				t.Fatalf("error = %v, want ErrInvalidRecord; decision=%#v", err, decision)
			}
			if decision.VerificationDecisionID != "" {
				t.Fatalf("invalid key returned a decision: %#v", decision)
			}
			if countVerificationDecisions(t, ctx, store, worker.RoutingDecisionID) != 0 {
				t.Fatalf("invalid key persisted a decision")
			}
		})
	}
}

func TestVerificationReplayDoesNotUseDisplaySanitizedFingerprint(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, worker, verifier := persistVerificationRoutes(t, ctx, fixture, fixture.candidate("claude", "acct-c", "claude-good"), nil)
	defer store.Close()

	firstVerdict := passVerdict(verifier, strings.Repeat("a", 300)+"x", "ev-collide")
	first, err := DecideAndPersistVerification(ctx, store, verificationInput(worker, verifier, firstVerdict))
	if err != nil {
		t.Fatalf("first verification: %v", err)
	}

	replayVerdict := passVerdict(verifier, strings.Repeat("a", 300)+"y", "ev-collide")
	replay, err := DecideAndPersistVerification(ctx, store, verificationInput(worker, verifier, replayVerdict))
	if !errors.Is(err, delivery.ErrDuplicateReplay) {
		t.Fatalf("replay error = %v, want ErrDuplicateReplay; decision=%#v", err, replay)
	}
	if replay.DecisionStatus == VerificationStatusAccepted {
		t.Fatalf("colliding display-sanitized replay returned accepted decision: %#v", replay)
	}
	if countVerificationDecisions(t, ctx, store, worker.RoutingDecisionID) != 1 {
		t.Fatalf("colliding replay duplicated verifier work; count=%d", countVerificationDecisions(t, ctx, store, worker.RoutingDecisionID))
	}
	loaded, err := LoadVerificationDecision(ctx, store, first.VerificationDecisionID)
	if err != nil {
		t.Fatalf("LoadVerificationDecision: %v", err)
	}
	if loaded.VerifierVerdicts[0].Message != strings.Repeat("a", 256) {
		t.Fatalf("stored display message changed unexpectedly: %q", loaded.VerifierVerdicts[0].Message)
	}
}

func TestVerifierIndependenceBoundariesAreOrderedAndNonWeakening(t *testing.T) {
	worker := Candidate{AdapterID: "codex", AccountProfileID: "acct-a", ModelCapabilityID: "model-a", RoutingCandidateID: "worker"}
	cases := []struct {
		name     string
		verifier Candidate
		level    taskrequirements.IndependenceLevel
		wantPass bool
		actual   taskrequirements.IndependenceLevel
	}{
		{"none", worker, taskrequirements.IndependenceNone, true, taskrequirements.IndependenceNone},
		{"different model", Candidate{AdapterID: "codex", AccountProfileID: "acct-a", ModelCapabilityID: "model-b", RoutingCandidateID: "verifier"}, taskrequirements.IndependenceDifferentModel, true, taskrequirements.IndependenceDifferentModel},
		{"different account", Candidate{AdapterID: "codex", AccountProfileID: "acct-b", ModelCapabilityID: "model-b", RoutingCandidateID: "verifier"}, taskrequirements.IndependenceDifferentAccount, true, taskrequirements.IndependenceDifferentAccount},
		{"different provider", Candidate{AdapterID: "claude", AccountProfileID: "acct-c", ModelCapabilityID: "model-c", RoutingCandidateID: "verifier"}, taskrequirements.IndependenceDifferentProvider, true, taskrequirements.IndependenceDifferentProvider},
		{"same provider fails provider boundary", Candidate{AdapterID: "codex", AccountProfileID: "acct-b", ModelCapabilityID: "model-b", RoutingCandidateID: "verifier"}, taskrequirements.IndependenceDifferentProvider, false, taskrequirements.IndependenceDifferentAccount},
		{"human cannot be automated", Candidate{AdapterID: "claude", AccountProfileID: "acct-c", ModelCapabilityID: "model-c", RoutingCandidateID: "verifier"}, taskrequirements.IndependenceHuman, false, taskrequirements.IndependenceDifferentProvider},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := independentEnough(worker, tt.verifier, tt.level); got != tt.wantPass {
				t.Fatalf("independentEnough = %t, want %t", got, tt.wantPass)
			}
			if got := actualIndependence(worker, tt.verifier); got != tt.actual {
				t.Fatalf("actualIndependence = %s, want %s", got, tt.actual)
			}
		})
	}

	req := workerRequirement("task-independence")
	req.RiskTier = taskrequirements.RiskHigh
	req.VerificationRequirements = []taskrequirements.VerificationRequirement{{
		VerificationKind:     taskrequirements.VerificationLoopReview,
		RequiredForRiskTiers: []taskrequirements.RiskTier{taskrequirements.RiskHigh},
		IndependenceLevel:    taskrequirements.IndependenceDifferentProvider,
		OutputContract:       taskrequirements.OutputVerificationVerdict,
	}}
	profile := BalancedRoutingPolicyProfile(time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC))
	profile.EligibilityPolicy.VerifierIndependence = taskrequirements.IndependenceDifferentAccount
	if got := RequiredVerifierIndependence(req, profile); got != taskrequirements.IndependenceDifferentProvider {
		t.Fatalf("required independence = %s, want fallback/profile not to weaken task requirement", got)
	}
}

func TestVerificationDecisionHumanRequiredNoEligibleAndOutputContractFailClosed(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)

	t.Run("human required", func(t *testing.T) {
		store, worker, verifier := persistVerificationRoutes(t, ctx, fixture, fixture.candidate("claude", "acct-c", "claude-good"), func(req *taskrequirements.TaskRequirement) {
			req.VerificationRequirements = []taskrequirements.VerificationRequirement{{
				VerificationKind:     taskrequirements.VerificationHumanApproval,
				RequiredForRiskTiers: []taskrequirements.RiskTier{taskrequirements.RiskHigh},
				IndependenceLevel:    taskrequirements.IndependenceHuman,
				PermissionRequired:   taskrequirements.PermissionReadOnly,
				OutputContract:       taskrequirements.OutputVerificationVerdict,
			}}
		})
		defer store.Close()
		decision, err := DecideAndPersistVerification(ctx, store, verificationInput(worker, verifier, passVerdict(verifier, "assist only", "ev-human")))
		if !errors.Is(err, taskrequirements.ErrVerifierIndependenceRequired) || decision.FinalAuthority != FinalAuthorityHuman {
			t.Fatalf("human-required decision/error = %#v / %v", decision, err)
		}
	})

	t.Run("no eligible verifier route", func(t *testing.T) {
		store, worker, verifier := persistVerificationRoutes(t, ctx, fixture, fixture.candidate("codex", "acct-a", "codex-broken"), nil)
		defer store.Close()
		if verifier.DecisionStatus != DecisionStatusNoEligible {
			t.Fatalf("fixture verifier route status = %s, want no eligible", verifier.DecisionStatus)
		}
		decision, err := DecideAndPersistVerification(ctx, store, verificationInput(worker, verifier))
		if !errors.Is(err, taskrequirements.ErrNoEligibleCandidate) || decision.DecisionStatus != VerificationStatusNeedsHuman {
			t.Fatalf("no eligible decision/error = %#v / %v", decision, err)
		}
	})

	t.Run("output contract", func(t *testing.T) {
		store, worker, verifier := persistVerificationRoutes(t, ctx, fixture, fixture.candidate("claude", "acct-c", "claude-good"), func(req *taskrequirements.TaskRequirement) {
			req.VerificationRequirements[0].OutputContract = taskrequirements.OutputFreeform
		})
		defer store.Close()
		decision, err := DecideAndPersistVerification(ctx, store, verificationInput(worker, verifier, passVerdict(verifier, "freeform", "ev-output")))
		if !errors.Is(err, taskrequirements.ErrCapabilityUnsupported) || decision.DecisionStatus != VerificationStatusNeedsHuman {
			t.Fatalf("output contract decision/error = %#v / %v", decision, err)
		}
	})
}

func TestVerificationDecisionBindsVerdictsToSelectedVerifierAuthority(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	cases := []struct {
		name         string
		mutateInput  func(RoutingDecision, RoutingDecision, *VerificationDecisionInput)
		mutateRoutes func(*taskrequirements.TaskRequirement, *Candidate)
		wantErr      error
	}{
		{
			name: "worker candidate id",
			mutateInput: func(worker, _ RoutingDecision, input *VerificationDecisionInput) {
				input.VerifierVerdicts[0].RoutingCandidateID = worker.ChosenCandidateID
			},
			wantErr: taskrequirements.ErrVerificationDecisionConflict,
		},
		{
			name: "unrelated candidate id",
			mutateInput: func(_, _ RoutingDecision, input *VerificationDecisionInput) {
				input.VerifierVerdicts[0].RoutingCandidateID = "rcand_unrelated"
			},
			wantErr: taskrequirements.ErrVerificationDecisionConflict,
		},
		{
			name: "wrong authority fingerprint",
			mutateInput: func(_, _ RoutingDecision, input *VerificationDecisionInput) {
				input.VerifierVerdicts[0].AuthorityFingerprint = testFingerprint("wrong-verifier-authority")
			},
			wantErr: taskrequirements.ErrVerificationDecisionConflict,
		},
		{
			name: "missing authority fingerprint",
			mutateInput: func(_, _ RoutingDecision, input *VerificationDecisionInput) {
				input.VerifierVerdicts[0].AuthorityFingerprint = ""
			},
			wantErr: taskrequirements.ErrVerificationDecisionConflict,
		},
		{
			name: "forbidden authority",
			mutateInput: func(_, _ RoutingDecision, input *VerificationDecisionInput) {
				input.VerifierVerdicts[0].Authority = FinalAuthorityHuman
			},
			wantErr: taskrequirements.ErrVerificationDecisionConflict,
		},
		{
			name: "pass plus terminal error",
			mutateInput: func(_, _ RoutingDecision, input *VerificationDecisionInput) {
				input.VerifierVerdicts[0].TerminalErrorCode = taskrequirements.ErrCapabilityUnsupportedCode
			},
			wantErr: taskrequirements.ErrVerificationDecisionConflict,
		},
		{
			name: "unknown verdict",
			mutateInput: func(_, _ RoutingDecision, input *VerificationDecisionInput) {
				input.VerifierVerdicts[0].Verdict = "approved"
			},
			wantErr: taskrequirements.ErrVerificationDecisionConflict,
		},
		{
			name: "duplicate council member",
			mutateInput: func(_, verifier RoutingDecision, input *VerificationDecisionInput) {
				input.VerifierVerdicts = append(input.VerifierVerdicts, passVerdict(verifier, "second", "ev-second"))
				input.VerifierVerdicts[1].MemberID = input.VerifierVerdicts[0].MemberID
				input.CouncilLimits = validCouncilLimits(fixture.now)
				input.CouncilRoundsUsed = 1
				input.CouncilBudgetTokensUsed = 10
			},
			wantErr: taskrequirements.ErrVerificationDecisionConflict,
		},
		{
			name: "selected read-only non-verifier route",
			mutateRoutes: func(req *taskrequirements.TaskRequirement, candidate *Candidate) {
				req.RoleKey = RoleKeyVerifier
				candidate.RoleKey = RoleKeyLuna
			},
			wantErr: taskrequirements.ErrCapabilityUnsupported,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			store, worker, verifier := persistVerificationRoutesWithMutators(t, ctx, fixture, fixture.candidate("claude", "acct-c", "claude-good"), nil, tt.mutateRoutes)
			defer store.Close()
			input := verificationInput(worker, verifier, passVerdict(verifier, "bound", "ev-bound"))
			if tt.mutateInput != nil {
				tt.mutateInput(worker, verifier, &input)
			}
			decision, err := DecideAndPersistVerification(ctx, store, input)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v; decision=%#v", err, tt.wantErr, decision)
			}
			if decision.DecisionStatus == VerificationStatusAccepted || decision.FinalAuthority != FinalAuthorityHuman || decision.TerminalErrorCode == "" {
				t.Fatalf("invalid verdict was accepted or not fail-closed: %#v", decision)
			}
			if countVerificationDecisions(t, ctx, store, worker.RoutingDecisionID) != 1 {
				t.Fatalf("fail-closed decision was not persisted exactly once")
			}
		})
	}
}

func TestVerificationCouncilBoundsTimeoutAndDisagreementAreDurable(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, worker, verifier := persistVerificationRoutes(t, ctx, fixture, fixture.candidate("claude", "acct-c", "claude-good"), nil)
	defer store.Close()
	secondVerifier := persistAdditionalVerifierRoute(t, ctx, store, fixture, worker, "route-verifier-b", fixture.candidate("claude", "acct-c", "claude-good"), nil)

	limits := CouncilLimits{
		Enabled:         true,
		MaxMembers:      2,
		MaxRounds:       1,
		MaxDurationMS:   1000,
		MaxBudgetTokens: 50,
		StartedAt:       fixture.now.Add(-2 * time.Second).Format(time.RFC3339Nano),
		DeadlineAt:      fixture.now.Add(-1 * time.Second).Format(time.RFC3339Nano),
	}
	input := verificationInput(worker, verifier,
		passVerdict(verifier, "one pass", "ev-pass"),
		VerifierVerdict{MemberID: "member-b", RoutingCandidateID: secondVerifier.ChosenCandidateID, Verdict: VerificationVerdictFail, Message: "found regression", Authority: "verifier", AuthorityFingerprint: secondVerifier.RoutingFingerprint},
	)
	input.CouncilMemberRoutingDecisionIDs = []string{verifier.RoutingDecisionID, secondVerifier.RoutingDecisionID}
	input.CouncilLimits = limits
	input.CouncilRoundsUsed = 1
	input.CouncilBudgetTokensUsed = 10
	input.Timeout = true

	decision, err := DecideAndPersistVerification(ctx, store, input)
	if !errors.Is(err, taskrequirements.ErrVerifierCouncilBoundExceeded) {
		t.Fatalf("council timeout error = %v, want ErrVerifierCouncilBoundExceeded", err)
	}
	if !decision.Timeout || decision.Council.BoundExceeded != "time" || len(decision.Disagreements) == 0 {
		t.Fatalf("council timeout decision missing durable state: %#v", decision)
	}
	loaded, err := LoadVerificationDecision(ctx, store, decision.VerificationDecisionID)
	if err != nil {
		t.Fatalf("LoadVerificationDecision: %v", err)
	}
	if !loaded.Timeout || loaded.Council.TerminalErrorCode != taskrequirements.ErrVerifierCouncilBoundExceededCode || len(loaded.VerifierVerdicts) != 2 {
		t.Fatalf("loaded council decision lost timeout/disagreement: %#v", loaded)
	}
	if len(loaded.CouncilMemberAuthorities) != 2 {
		t.Fatalf("loaded council decision lost member authorities: %#v", loaded.CouncilMemberAuthorities)
	}
}

func TestVerificationCouncilRequiresDistinctVerifierAuthorities(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)

	t.Run("one route cannot count as two member ids", func(t *testing.T) {
		store, worker, verifier := persistVerificationRoutes(t, ctx, fixture, fixture.candidate("claude", "acct-c", "claude-good"), nil)
		defer store.Close()
		second := passVerdict(verifier, "same route second member", "ev-same-route")
		second.MemberID = "member-b"
		input := verificationInput(worker, verifier, passVerdict(verifier, "same route first member", "ev-same-route-a"), second)
		input.CouncilLimits = validCouncilLimits(fixture.now)
		input.CouncilRoundsUsed = 1
		input.CouncilBudgetTokensUsed = 10
		decision, err := DecideAndPersistVerification(ctx, store, input)
		if !errors.Is(err, taskrequirements.ErrVerificationDecisionConflict) {
			t.Fatalf("error = %v, want ErrVerificationDecisionConflict; decision=%#v", err, decision)
		}
		if decision.DecisionStatus != VerificationStatusNeedsHuman || decision.Council.MemberCount != 1 || len(decision.CouncilMemberAuthorities) != 1 {
			t.Fatalf("single authority council was not fail-closed durably: %#v", decision)
		}
	})

	t.Run("two distinct routes pass and reorder deterministically", func(t *testing.T) {
		store, worker, verifierA := persistVerificationRoutes(t, ctx, fixture, fixture.candidate("claude", "acct-c", "claude-good"), nil)
		defer store.Close()
		verifierB := persistAdditionalVerifierRoute(t, ctx, store, fixture, worker, "route-verifier-b", fixture.candidate("claude", "acct-c", "claude-good"), nil)
		input := verificationInput(worker, verifierA, passVerdict(verifierA, "route a clean", "ev-route-a"), passVerdict(verifierB, "route b clean", "ev-route-b"))
		input.VerifierVerdicts[1].MemberID = "member-b"
		input.CouncilMemberRoutingDecisionIDs = []string{verifierB.RoutingDecisionID, verifierA.RoutingDecisionID}
		input.CouncilLimits = validCouncilLimits(fixture.now)
		input.CouncilRoundsUsed = 1
		input.CouncilBudgetTokensUsed = 10
		accepted, err := DecideAndPersistVerification(ctx, store, input)
		if err != nil {
			t.Fatalf("council verification: %v", err)
		}
		if accepted.DecisionStatus != VerificationStatusAccepted || accepted.Council.MemberCount != 2 || len(accepted.CouncilMemberAuthorities) != 2 {
			t.Fatalf("council decision = %#v", accepted)
		}
		replay := input
		replay.VerifierVerdicts = []VerifierVerdict{input.VerifierVerdicts[1], input.VerifierVerdicts[0]}
		replay.CouncilMemberRoutingDecisionIDs = []string{verifierA.RoutingDecisionID, verifierB.RoutingDecisionID}
		replayed, err := DecideAndPersistVerification(ctx, store, replay)
		if err != nil {
			t.Fatalf("reordered replay: %v", err)
		}
		if replayed.VerificationDecisionID != accepted.VerificationDecisionID || countVerificationDecisionMembers(t, ctx, store, accepted.VerificationDecisionID) != 2 {
			t.Fatalf("reordered council replay was not deterministic: accepted=%#v replayed=%#v member_rows=%d", accepted, replayed, countVerificationDecisionMembers(t, ctx, store, accepted.VerificationDecisionID))
		}
	})

	t.Run("missing unlisted and duplicate authorities fail closed", func(t *testing.T) {
		cases := []struct {
			name   string
			mutate func(*VerificationDecisionInput, RoutingDecision, RoutingDecision)
		}{
			{"missing verdict", func(input *VerificationDecisionInput, _, _ RoutingDecision) {
				input.VerifierVerdicts = input.VerifierVerdicts[:1]
			}},
			{"unlisted verdict", func(input *VerificationDecisionInput, _, verifierB RoutingDecision) {
				input.CouncilMemberRoutingDecisionIDs = input.CouncilMemberRoutingDecisionIDs[:1]
				input.VerifierVerdicts[1] = passVerdict(verifierB, "unlisted", "ev-unlisted")
				input.VerifierVerdicts[1].MemberID = "member-b"
			}},
			{"duplicate route", func(input *VerificationDecisionInput, verifierA, _ RoutingDecision) {
				input.CouncilMemberRoutingDecisionIDs = []string{verifierA.RoutingDecisionID, verifierA.RoutingDecisionID}
			}},
		}
		for _, tt := range cases {
			t.Run(tt.name, func(t *testing.T) {
				store, worker, verifierA := persistVerificationRoutes(t, ctx, fixture, fixture.candidate("claude", "acct-c", "claude-good"), nil)
				defer store.Close()
				verifierB := persistAdditionalVerifierRoute(t, ctx, store, fixture, worker, "route-verifier-b", fixture.candidate("claude", "acct-c", "claude-good"), nil)
				input := verificationInput(worker, verifierA, passVerdict(verifierA, "route a clean", "ev-route-a"), passVerdict(verifierB, "route b clean", "ev-route-b"))
				input.VerifierVerdicts[1].MemberID = "member-b"
				input.CouncilMemberRoutingDecisionIDs = []string{verifierA.RoutingDecisionID, verifierB.RoutingDecisionID}
				input.CouncilLimits = validCouncilLimits(fixture.now)
				input.CouncilRoundsUsed = 1
				input.CouncilBudgetTokensUsed = 10
				tt.mutate(&input, verifierA, verifierB)
				decision, err := DecideAndPersistVerification(ctx, store, input)
				if !errors.Is(err, taskrequirements.ErrVerificationDecisionConflict) {
					t.Fatalf("error = %v, want ErrVerificationDecisionConflict; decision=%#v", err, decision)
				}
				if decision.DecisionStatus != VerificationStatusNeedsHuman || decision.FinalAuthority != FinalAuthorityHuman {
					t.Fatalf("invalid council authority was not fail-closed: %#v", decision)
				}
			})
		}
	})
}

func TestVerificationCouncilBoundsAreMandatory(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, worker, verifier := persistVerificationRoutes(t, ctx, fixture, fixture.candidate("claude", "acct-c", "claude-good"), nil)
	defer store.Close()

	clean := verificationInput(worker, verifier, passVerdict(verifier, "clean", "ev-clean"))
	clean.CouncilLimits = validCouncilLimits(fixture.now)
	clean.CouncilRoundsUsed = 1
	clean.CouncilBudgetTokensUsed = 10
	accepted, err := DecideAndPersistVerification(ctx, store, clean)
	if err != nil || accepted.DecisionStatus != VerificationStatusAccepted || accepted.Council.MemberCount != 1 {
		t.Fatalf("bounded council acceptance = %#v / %v", accepted, err)
	}

	cases := []struct {
		name   string
		mutate func(*VerificationDecisionInput)
	}{
		{"missing start", func(input *VerificationDecisionInput) { input.CouncilLimits.StartedAt = "" }},
		{"invalid start", func(input *VerificationDecisionInput) { input.CouncilLimits.StartedAt = "not-time" }},
		{"future start", func(input *VerificationDecisionInput) {
			input.CouncilLimits.StartedAt = fixture.now.Add(time.Second).Format(time.RFC3339Nano)
		}},
		{"missing deadline", func(input *VerificationDecisionInput) { input.CouncilLimits.DeadlineAt = "" }},
		{"deadline beyond cap", func(input *VerificationDecisionInput) {
			input.CouncilLimits.DeadlineAt = fixture.now.Add(2 * time.Second).Format(time.RFC3339Nano)
		}},
		{"expired boundary", func(input *VerificationDecisionInput) {
			input.CouncilLimits.DeadlineAt = fixture.now.Format(time.RFC3339Nano)
		}},
		{"negative budget", func(input *VerificationDecisionInput) { input.CouncilBudgetTokensUsed = -1 }},
		{"zero members", func(input *VerificationDecisionInput) { input.VerifierVerdicts = nil }},
		{"duplicate member", func(input *VerificationDecisionInput) {
			input.VerifierVerdicts = append(input.VerifierVerdicts, input.VerifierVerdicts[0])
		}},
		{"zero rounds", func(input *VerificationDecisionInput) { input.CouncilRoundsUsed = 0 }},
		{"too many rounds", func(input *VerificationDecisionInput) { input.CouncilRoundsUsed = 2 }},
		{"too many members", func(input *VerificationDecisionInput) {
			input.CouncilLimits.MaxMembers = 1
			input.VerifierVerdicts = append(input.VerifierVerdicts, passVerdict(verifier, "second", "ev-second"))
			input.VerifierVerdicts[1].MemberID = "member-b"
		}},
		{"over budget", func(input *VerificationDecisionInput) { input.CouncilBudgetTokensUsed = 51 }},
		{"zero duration", func(input *VerificationDecisionInput) { input.CouncilLimits.MaxDurationMS = 0 }},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			store, worker, verifier := persistVerificationRoutes(t, ctx, fixture, fixture.candidate("claude", "acct-c", "claude-good"), nil)
			defer store.Close()
			input := verificationInput(worker, verifier, passVerdict(verifier, "bounded", "ev-"+strings.ReplaceAll(tt.name, " ", "-")))
			input.CouncilLimits = validCouncilLimits(fixture.now)
			input.CouncilRoundsUsed = 1
			input.CouncilBudgetTokensUsed = 10
			tt.mutate(&input)
			decision, err := DecideAndPersistVerification(ctx, store, input)
			if !errors.Is(err, taskrequirements.ErrVerifierCouncilBoundExceeded) && tt.name != "duplicate member" && tt.name != "too many members" && tt.name != "zero members" {
				t.Fatalf("error = %v, want ErrVerifierCouncilBoundExceeded; decision=%#v", err, decision)
			}
			if (tt.name == "duplicate member" || tt.name == "too many members" || tt.name == "zero members") && !errors.Is(err, taskrequirements.ErrVerificationDecisionConflict) {
				t.Fatalf("%s error = %v, want ErrVerificationDecisionConflict; decision=%#v", tt.name, err, decision)
			}
			if decision.DecisionStatus != VerificationStatusNeedsHuman || decision.TerminalErrorCode == "" {
				t.Fatalf("council bound was not fail-closed: %#v", decision)
			}
		})
	}
}

func TestVerificationDecisionConcurrentDuplicateAndAuthorityChanges(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, worker, verifier := persistVerificationRoutes(t, ctx, fixture, fixture.candidate("claude", "acct-c", "claude-good"), nil)
	defer store.Close()
	input := verificationInput(worker, verifier, passVerdict(verifier, "concurrent", "ev-concurrent"))

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	ids := make(chan string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			decision, err := DecideAndPersistVerification(ctx, store, input)
			if err != nil {
				errs <- err
				return
			}
			ids <- decision.VerificationDecisionID
		}()
	}
	wg.Wait()
	close(errs)
	close(ids)
	for err := range errs {
		t.Fatalf("concurrent verification: %v", err)
	}
	first := ""
	for id := range ids {
		if first == "" {
			first = id
			continue
		}
		if id != first {
			t.Fatalf("concurrent duplicate returned different ids: %s vs %s", first, id)
		}
	}
	if countVerificationDecisions(t, ctx, store, worker.RoutingDecisionID) != 1 {
		t.Fatalf("concurrent duplicate count = %d, want 1", countVerificationDecisions(t, ctx, store, worker.RoutingDecisionID))
	}

	changed := input
	changed.IdempotencyKey = "changed-authority"
	changed.AuthorityFingerprint = testFingerprint("changed-authority")
	second, err := DecideAndPersistVerification(ctx, store, changed)
	if err != nil {
		t.Fatalf("changed authority verification: %v", err)
	}
	if second.VerificationDecisionID == first || countVerificationDecisions(t, ctx, store, worker.RoutingDecisionID) != 2 {
		t.Fatalf("changed authority did not create distinct history: second=%s first=%s count=%d", second.VerificationDecisionID, first, countVerificationDecisions(t, ctx, store, worker.RoutingDecisionID))
	}
}

func TestVerificationDecisionRefusesOverwriteOfAcceptedAuthority(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, worker, verifier := persistVerificationRoutes(t, ctx, fixture, fixture.candidate("claude", "acct-c", "claude-good"), nil)
	defer store.Close()

	first, err := DecideAndPersistVerification(ctx, store, verificationInput(worker, verifier, passVerdict(verifier, "accepted", "ev-accepted")))
	if err != nil {
		t.Fatalf("first verification: %v", err)
	}
	overwrite := verificationInput(worker, verifier, VerifierVerdict{
		MemberID:             "member-a",
		RoutingCandidateID:   verifier.ChosenCandidateID,
		Verdict:              VerificationVerdictFail,
		Message:              "later contradictory verdict",
		Authority:            "verifier",
		AuthorityFingerprint: verifier.RoutingFingerprint,
		EvidenceRefs:         []EvidenceRef{{RecordKind: "check", RecordID: "ev-overwrite", Summary: "later contradictory verdict"}},
	})
	overwrite.IdempotencyKey = "overwrite-attempt"
	_, err = DecideAndPersistVerification(ctx, store, overwrite)
	if !errors.Is(err, taskrequirements.ErrVerificationDecisionConflict) {
		t.Fatalf("overwrite error = %v, want ErrVerificationDecisionConflict", err)
	}
	loaded, err := LoadVerificationDecision(ctx, store, first.VerificationDecisionID)
	if err != nil {
		t.Fatalf("LoadVerificationDecision: %v", err)
	}
	if loaded.Verdict != VerificationVerdictPass || countVerificationDecisions(t, ctx, store, worker.RoutingDecisionID) != 1 {
		t.Fatalf("accepted verdict was overwritten or duplicated: %#v count=%d", loaded, countVerificationDecisions(t, ctx, store, worker.RoutingDecisionID))
	}
}

func TestVerificationExplanationRedactsBeforeTruncation(t *testing.T) {
	secret := "AKIA" + strings.Repeat("A", 16)
	windowsPath := `C:\Users\example\private\secret\file.txt`
	unixPath := "/Users/example/private/path/with/token"
	multibyte := strings.Repeat("検証", 120)
	decision := VerificationDecision{
		SchemaVersion:           VerificationDecisionSchema,
		RecordVersion:           1,
		VerificationDecisionID:  "vdec_test",
		ProjectID:               "proj",
		DeliveryRunID:           "run",
		TaskID:                  "task",
		TaskRequirementID:       "treq",
		WorkerRoutingDecisionID: "rdec_worker",
		DecisionKey:             "verify",
		IdempotencyKey:          "verify",
		DecisionStatus:          VerificationStatusNeedsHuman,
		Verdict:                 VerificationVerdictNeedsHuman,
		RequiredIndependence:    taskrequirements.IndependenceDifferentProvider,
		ActualIndependence:      taskrequirements.IndependenceNone,
		WorkerCandidateID:       "worker-" + secret,
		VerifierCandidateID:     "verifier-" + windowsPath,
		VerifierVerdicts: []VerifierVerdict{{
			MemberID:             "member-" + secret,
			RoutingCandidateID:   "candidate-" + unixPath,
			Verdict:              VerificationVerdictNeedsHuman,
			Message:              multibyte + secret,
			Authority:            "verifier token=" + secret,
			AuthorityFingerprint: "sha256:" + secret,
			EvidenceRefs: []EvidenceRef{
				{RecordKind: "log-" + secret, RecordID: windowsPath, Summary: unixPath + " secret=" + secret},
				{RecordKind: "log-" + secret, RecordID: windowsPath, Summary: unixPath + " secret=" + secret},
			},
		}},
		Disagreements: []string{strings.Repeat("y", 300) + " token=" + secret},
		EligibilityExclusions: []RejectionReason{{
			Code:              RejectEvidenceStale,
			Message:           windowsPath + " bearer " + strings.Repeat("a", 16),
			EvidenceRecordIDs: []string{"evidence-" + unixPath, "evidence-" + unixPath},
			SnapshotIDs:       []string{"snapshot-" + secret},
		}},
		EvidenceRefs:              []EvidenceRef{{RecordKind: unixPath, RecordID: "ev-" + secret, Summary: strings.Repeat("z", 300) + secret}},
		FinalAuthority:            FinalAuthorityHuman,
		FinalAuthorityFingerprint: testFingerprint("auth"),
		VerificationFingerprint:   testFingerprint("verification"),
		PolicyFingerprint:         testFingerprint("policy"),
		PlanFingerprint:           testFingerprint("plan"),
		CreatedAt:                 "2026-07-13T00:00:00Z",
		UpdatedAt:                 "2026-07-13T00:00:00Z",
		DecidedBy:                 schedulerActor(),
		Host:                      routingHost(),
		TerminalErrorCode:         taskrequirements.ErrVerifierIndependenceRequiredCode,
	}
	out, err := ExplainVerificationJSON(decision)
	if err != nil {
		t.Fatalf("ExplainVerificationJSON: %v", err)
	}
	human := ExplainVerificationHuman(decision)
	for _, surface := range []string{string(out), human} {
		if !utf8.ValidString(surface) {
			t.Fatalf("explanation emitted invalid utf-8: %q", surface)
		}
		for _, raw := range []string{secret, "/Users/example", `C:\Users\example`, "token=" + secret} {
			if strings.Contains(surface, raw) {
				t.Fatalf("explanation leaked raw material %q: %s", raw, surface)
			}
		}
	}
	if strings.Count(string(out), `"record_kind"`) > 2 {
		t.Fatalf("explanation did not dedupe evidence refs: %s", out)
	}
	if strings.Contains(string(out), secret) || strings.Contains(string(out), "/Users/example") {
		t.Fatalf("explanation leaked secret/local path after truncation: %s", out)
	}
	if !strings.Contains(string(out), "[REDACTED]") || !strings.Contains(human, "[REDACTED]") {
		t.Fatalf("explanation did not include redaction marker: json=%s human=%s", out, human)
	}
}

func persistVerificationRoutes(t *testing.T, ctx context.Context, fixture hardFixture, verifierCandidate Candidate, mutateWorkerReq func(*taskrequirements.TaskRequirement)) (storage.Store, RoutingDecision, RoutingDecision) {
	return persistVerificationRoutesWithMutators(t, ctx, fixture, verifierCandidate, mutateWorkerReq, nil)
}

func persistVerificationRoutesWithMutators(t *testing.T, ctx context.Context, fixture hardFixture, verifierCandidate Candidate, mutateWorkerReq func(*taskrequirements.TaskRequirement), mutateVerifierRoute func(*taskrequirements.TaskRequirement, *Candidate)) (storage.Store, RoutingDecision, RoutingDecision) {
	t.Helper()
	store := openRoutingStore(t, ctx, fixture.now)
	workerInput := replayDecisionInput(fixture)
	workerReq := workerInput.Inputs.Requirement
	workerReq.RiskTier = taskrequirements.RiskHigh
	workerReq.VerificationRequirements = []taskrequirements.VerificationRequirement{{
		VerificationKind:     taskrequirements.VerificationLoopReview,
		RequiredForRiskTiers: []taskrequirements.RiskTier{taskrequirements.RiskHigh, taskrequirements.RiskCritical},
		IndependenceLevel:    taskrequirements.IndependenceDifferentProvider,
		PermissionRequired:   taskrequirements.PermissionReadOnly,
		OutputContract:       taskrequirements.OutputVerificationVerdict,
	}}
	if mutateWorkerReq != nil {
		mutateWorkerReq(&workerReq)
	}
	workerInput.Inputs.Requirement = persistFallbackTaskRequirement(t, ctx, store, workerReq, fixture.now)
	workerInput.TaskRequirementID = workerInput.Inputs.Requirement.TaskRequirementID
	profile := BalancedRoutingPolicyProfile(fixture.now)
	workerInput.RoutingPolicyProfile = profile
	workerInput.RoutingPolicyProfileID = profile.RoutingPolicyProfileID
	workerInput.PolicyFingerprint = profile.PolicyFingerprint
	workerInput.Inputs.Policy = profile.EligibilityPolicy
	for i := range workerInput.Inputs.Inventory.QuotaSnapshots {
		workerInput.Inputs.Inventory.QuotaSnapshots[i].Confidence = providerinventory.ConfidenceExact
	}
	for i := range workerInput.Inputs.Availability {
		workerInput.Inputs.Availability[i].ScoreConfidence = providerinventory.ConfidenceExact
		if workerInput.Inputs.Availability[i].Scope.AdapterID == "codex" {
			workerInput.Inputs.Availability[i].Score = 99
		} else {
			workerInput.Inputs.Availability[i].Score = 70
		}
	}
	worker, err := BuildRoutingDecision(workerInput)
	if err != nil {
		t.Fatalf("BuildRoutingDecision worker: %v", err)
	}
	if err := PersistRoutingDecision(ctx, store, worker); err != nil {
		t.Fatalf("PersistRoutingDecision worker: %v", err)
	}

	verifierReq := decisionRequirement(t, fixture, verifierRequirement("task-verifier-route"), "treq-verifier-route", worker.PlanFingerprint)
	verifierReq.RequiredOutput = taskrequirements.OutputVerificationVerdict
	verifierReq.TaskID = "task-verifier-route"
	verifierReq.TaskKey = verifierReq.TaskID
	verifierCandidate.TaskID = verifierReq.TaskID
	verifierCandidate.RoleKey = verifierReq.RoleKey
	verifierCandidate.Permission = verifierReq.PermissionRequired
	verifierCandidate.LaunchSideEffectClass = taskrequirements.SideEffectProviderLaunch
	if mutateVerifierRoute != nil {
		mutateVerifierRoute(&verifierReq, &verifierCandidate)
	}
	verifierReq = persistFallbackTaskRequirement(t, ctx, store, verifierReq, fixture.now)
	verifierCandidate.RoutingCandidateID = candidateID(verifierCandidate)
	verifierCandidate.CandidateFingerprint = candidateFingerprint(verifierCandidate)
	verifierInput := workerInput
	verifierInput.DecisionKey = "route-verifier"
	verifierInput.TaskRequirementID = verifierReq.TaskRequirementID
	verifierInput.RoleDefinitionID = "role-verifier"
	verifierInput.Inputs.Requirement = verifierReq
	verifierInput.Inputs.Candidates = []Candidate{verifierCandidate}
	verifierInput.Inputs.WorkerRoute = nil
	verifier, err := BuildRoutingDecision(verifierInput)
	if err != nil {
		t.Fatalf("BuildRoutingDecision verifier: %v", err)
	}
	if err := PersistRoutingDecision(ctx, store, verifier); err != nil {
		t.Fatalf("PersistRoutingDecision verifier: %v", err)
	}
	return store, worker, verifier
}

func persistAdditionalVerifierRoute(t *testing.T, ctx context.Context, store storage.Store, fixture hardFixture, worker RoutingDecision, decisionKey string, verifierCandidate Candidate, mutateVerifierRoute func(*taskrequirements.TaskRequirement, *Candidate)) RoutingDecision {
	t.Helper()
	verifierReq := decisionRequirement(t, fixture, verifierRequirement("task-"+decisionKey), "treq-"+decisionKey, worker.PlanFingerprint)
	verifierReq.ProjectID = worker.ProjectID
	verifierReq.DeliveryRunID = worker.DeliveryRunID
	verifierReq.RequiredOutput = taskrequirements.OutputVerificationVerdict
	verifierReq.TaskID = "task-" + decisionKey
	verifierReq.TaskKey = verifierReq.TaskID
	verifierCandidate.TaskID = verifierReq.TaskID
	verifierCandidate.RoleKey = verifierReq.RoleKey
	verifierCandidate.Permission = verifierReq.PermissionRequired
	verifierCandidate.LaunchSideEffectClass = taskrequirements.SideEffectProviderLaunch
	if mutateVerifierRoute != nil {
		mutateVerifierRoute(&verifierReq, &verifierCandidate)
	}
	verifierReq = persistFallbackTaskRequirement(t, ctx, store, verifierReq, fixture.now)
	verifierCandidate.RoutingCandidateID = candidateID(verifierCandidate)
	verifierCandidate.CandidateFingerprint = candidateFingerprint(verifierCandidate)
	input := replayDecisionInput(fixture)
	input.ProjectID = worker.ProjectID
	input.DeliveryRunID = worker.DeliveryRunID
	input.DecisionKey = decisionKey
	input.TaskRequirementID = verifierReq.TaskRequirementID
	input.RoleDefinitionID = "role-verifier"
	input.PlanFingerprint = worker.PlanFingerprint
	profile := BalancedRoutingPolicyProfile(fixture.now)
	input.RoutingPolicyProfile = profile
	input.RoutingPolicyProfileID = profile.RoutingPolicyProfileID
	input.PolicyFingerprint = profile.PolicyFingerprint
	input.Inputs.Policy = profile.EligibilityPolicy
	input.Inputs.Requirement = verifierReq
	input.Inputs.Candidates = []Candidate{verifierCandidate}
	input.Inputs.WorkerRoute = nil
	for i := range input.Inputs.Inventory.QuotaSnapshots {
		input.Inputs.Inventory.QuotaSnapshots[i].Confidence = providerinventory.ConfidenceExact
	}
	for i := range input.Inputs.Availability {
		input.Inputs.Availability[i].ScoreConfidence = providerinventory.ConfidenceExact
		if input.Inputs.Availability[i].Scope.AdapterID == verifierCandidate.AdapterID {
			input.Inputs.Availability[i].Score = 99
		}
	}
	verifier, err := BuildRoutingDecision(input)
	if err != nil {
		t.Fatalf("BuildRoutingDecision additional verifier: %v", err)
	}
	if err := PersistRoutingDecision(ctx, store, verifier); err != nil {
		t.Fatalf("PersistRoutingDecision additional verifier: %v", err)
	}
	return verifier
}

func verificationInput(worker, verifier RoutingDecision, verdicts ...VerifierVerdict) VerificationDecisionInput {
	authorityFingerprint := verifier.RoutingFingerprint
	if authorityFingerprint == "" {
		authorityFingerprint = worker.RoutingFingerprint
	}
	return VerificationDecisionInput{
		WorkerRoutingDecisionID:   worker.RoutingDecisionID,
		VerifierRoutingDecisionID: verifier.RoutingDecisionID,
		DecisionKey:               "verify-worker",
		IdempotencyKey:            "verify-worker-key",
		VerifierVerdicts:          verdicts,
		AuthorityFingerprint:      authorityFingerprint,
		DecidedBy:                 schedulerActor(),
		Host:                      routingHost(),
	}
}

func passVerdict(verifier RoutingDecision, message, evidenceID string) VerifierVerdict {
	return VerifierVerdict{
		MemberID:             "member-a",
		RoutingCandidateID:   verifier.ChosenCandidateID,
		Verdict:              VerificationVerdictPass,
		Message:              message,
		Authority:            "verifier",
		AuthorityFingerprint: verifier.RoutingFingerprint,
		EvidenceRefs:         []EvidenceRef{{RecordKind: "check", RecordID: evidenceID, Summary: message}},
	}
}

func validCouncilLimits(now time.Time) CouncilLimits {
	return CouncilLimits{
		Enabled:         true,
		MaxMembers:      2,
		MaxRounds:       1,
		MaxDurationMS:   1000,
		MaxBudgetTokens: 50,
		StartedAt:       now.Add(-500 * time.Millisecond).Format(time.RFC3339Nano),
		DeadlineAt:      now.Add(500 * time.Millisecond).Format(time.RFC3339Nano),
	}
}

func countVerificationDecisions(t *testing.T, ctx context.Context, store storage.Store, workerRoutingDecisionID string) int {
	t.Helper()
	var count int
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, `SELECT COUNT(*) FROM verification_decisions WHERE worker_routing_decision_id = ?`, workerRoutingDecisionID).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count verification decisions: %v", err)
	}
	return count
}

func countVerificationDecisionMembers(t *testing.T, ctx context.Context, store storage.Store, verificationDecisionID string) int {
	t.Helper()
	var count int
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, `SELECT COUNT(*) FROM verification_decision_members WHERE verification_decision_id = ?`, verificationDecisionID).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count verification decision members: %v", err)
	}
	return count
}
