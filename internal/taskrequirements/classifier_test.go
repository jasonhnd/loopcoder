package taskrequirements

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/delivery"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
)

func TestClassifyRepresentativeFixtures(t *testing.T) {
	base := baseInput()
	tests := []struct {
		name       string
		scope      Scope
		output     OutputContract
		wantRisk   RiskTier
		wantPerm   Permission
		wantEffect SideEffectClass
		wantRules  []string
	}{
		{
			name:       "documentation",
			scope:      Scope{Paths: []string{"docs/reference/usage.md"}, Documentation: true, AllowsRepoWrite: true},
			output:     OutputMarkdown,
			wantRisk:   RiskMedium,
			wantPerm:   PermissionWrite,
			wantEffect: SideEffectRepoWrite,
			wantRules:  []string{"scope.docs-only", "scope.repo-write"},
		},
		{
			name:       "test",
			scope:      Scope{Paths: []string{"internal/foo/foo_test.go"}, Tests: true, AllowsRepoWrite: true, RuntimeCommands: true},
			output:     OutputPatch,
			wantRisk:   RiskMedium,
			wantPerm:   PermissionWrite,
			wantEffect: SideEffectRepoWrite,
			wantRules:  []string{"scope.repo-write", "cap.cancellation"},
		},
		{
			name:       "research",
			scope:      Scope{Research: true},
			output:     OutputMarkdown,
			wantRisk:   RiskLow,
			wantPerm:   PermissionReadOnly,
			wantEffect: SideEffectLocalRead,
		},
		{
			name:       "routine implementation",
			scope:      Scope{Paths: []string{"internal/foo/foo.go"}, AllowsRepoWrite: true},
			output:     OutputPatch,
			wantRisk:   RiskMedium,
			wantPerm:   PermissionWrite,
			wantEffect: SideEffectRepoWrite,
			wantRules:  []string{"scope.repo-write"},
		},
		{
			name:       "security sensitive",
			scope:      Scope{Paths: []string{"internal/security/policy.go"}, AllowsRepoWrite: true, SecuritySensitive: true},
			output:     OutputPatch,
			wantRisk:   RiskHigh,
			wantPerm:   PermissionWrite,
			wantEffect: SideEffectRepoWrite,
			wantRules:  []string{"scope.repo-write", "quality.security-or-core"},
		},
		{
			name:       "release publish",
			scope:      Scope{ReleasePublish: true, AllowsGitHubWrite: true, AllowsGitRemoteWrite: true},
			output:     OutputPR,
			wantRisk:   RiskHigh,
			wantPerm:   PermissionWrite,
			wantEffect: SideEffectGitHubWrite,
			wantRules:  []string{"scope.repo-write", "scope.github-write", "scope.git-remote-write", "quality.security-or-core"},
		},
		{
			name:       "credential adjacent",
			scope:      Scope{CredentialAdjacent: true, ContainsSecretReference: true, AllowsRepoWrite: true},
			output:     OutputPatch,
			wantRisk:   RiskCritical,
			wantPerm:   PermissionWrite,
			wantEffect: SideEffectRepoWrite,
			wantRules:  []string{"scope.repo-write", "data.secret-reference"},
		},
		{
			name:       "destructive",
			scope:      Scope{Destructive: true, AllowsLocalRuntimeWrite: true},
			wantRisk:   RiskCritical,
			wantPerm:   PermissionWrite,
			wantEffect: SideEffectLocalWrite,
			wantRules:  []string{"scope.local-runtime-write"},
		},
		{
			name:       "cross repo",
			scope:      Scope{CrossRepo: true, AllowsGitRemoteWrite: true},
			output:     OutputBranch,
			wantRisk:   RiskCritical,
			wantPerm:   PermissionWrite,
			wantEffect: SideEffectGitRemoteWrite,
			wantRules:  []string{"scope.repo-write", "scope.git-remote-write", "quality.security-or-core"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := base
			input.Scope = tt.scope
			input.RequiredOutput = tt.output
			got, err := Classify(input)
			if err != nil {
				t.Fatalf("Classify() error = %v", err)
			}
			if got.SchemaVersion != SchemaTaskRequirement || got.TaskRequirementID == "" || got.TaskRequirementFingerprint == "" {
				t.Fatalf("identity/schema missing: %#v", got)
			}
			if got.RiskTier != tt.wantRisk || got.PermissionRequired != tt.wantPerm || got.SideEffectClass != tt.wantEffect {
				t.Fatalf("classification = (%s, %s, %s), want (%s, %s, %s)", got.RiskTier, got.PermissionRequired, got.SideEffectClass, tt.wantRisk, tt.wantPerm, tt.wantEffect)
			}
			for _, rule := range tt.wantRules {
				if !contains(got.ClassificationRules, rule) {
					t.Fatalf("classification rules = %#v, missing %s", got.ClassificationRules, rule)
				}
			}
		})
	}
}

func TestAmbiguousHighRiskRequiresApprovalInsteadOfDowngrade(t *testing.T) {
	input := baseInput()
	input.Scope = Scope{AllowsGitHubWrite: true, Ambiguous: true}
	got, err := Classify(input)
	if !errors.Is(err, ErrRequirementUnknown) {
		t.Fatalf("Classify() error = %v, want ErrRequirementUnknown", err)
	}
	if got.RiskTier != RiskHigh || got.TerminalErrorCode != string(ErrRequirementUnknownCode) {
		t.Fatalf("ambiguous high risk requirement = %#v", got)
	}
	if !hasVerification(got, VerificationHumanApproval) {
		t.Fatalf("verification requirements = %#v, want human approval", got.VerificationRequirements)
	}
}

func TestSecretMaterialFixtureRefusesRoute(t *testing.T) {
	input := baseInput()
	input.Scope = Scope{ContainsSecretMaterial: true, AllowsProviderLaunch: true}
	got, err := Classify(input)
	if !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("Classify() error = %v, want ErrPolicyDenied", err)
	}
	if got.RiskTier != RiskCritical || got.TerminalErrorCode != string(ErrPolicyDeniedCode) {
		t.Fatalf("secret material classification = %#v", got)
	}
}

func TestRuntimeCommandsRaiseMediumLocalWriteFloor(t *testing.T) {
	tests := []struct {
		name  string
		scope Scope
	}{
		{
			name:  "runtime command flag",
			scope: Scope{RuntimeCommands: true},
		},
		{
			name:  "populated command scope",
			scope: Scope{Commands: []string{"go test ./internal/taskrequirements -count=1"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := baseInput()
			input.Scope = tt.scope
			got, err := Classify(input)
			if err != nil {
				t.Fatalf("Classify() error = %v", err)
			}
			if got.RiskTier != RiskMedium || got.PermissionRequired != PermissionWrite || got.SideEffectClass != SideEffectLocalWrite {
				t.Fatalf("runtime classification = (%s, %s, %s), want (%s, %s, %s)", got.RiskTier, got.PermissionRequired, got.SideEffectClass, RiskMedium, PermissionWrite, SideEffectLocalWrite)
			}
			if !contains(got.ClassificationRules, "scope.local-runtime-write") || !contains(got.ClassificationRules, "cap.cancellation") {
				t.Fatalf("classification rules = %#v, want runtime floor and cancellation", got.ClassificationRules)
			}
			if !got.CancellationRequired {
				t.Fatalf("CancellationRequired = false, want true")
			}
		})
	}
}

func TestUnknownCapabilityEvidenceCannotSatisfyHardRequirement(t *testing.T) {
	req := CapabilityRequirement{
		Dimension:         CapabilityJSONOutput,
		RequiredValue:     true,
		MinimumConfidence: providerinventory.ConfidenceExact,
		FreshnessRequired: providerinventory.FreshnessFresh,
		Source:            "test",
	}
	if req.SatisfiedBy(CapabilityEvidenceSet{CapabilityJSONOutput: {Value: true, Confidence: providerinventory.ConfidenceUnknown, Freshness: providerinventory.FreshnessFresh}}) {
		t.Fatal("unknown confidence satisfied hard requirement")
	}
	if req.SatisfiedBy(CapabilityEvidenceSet{CapabilityJSONOutput: {Value: true, Confidence: providerinventory.ConfidenceExact, Freshness: providerinventory.FreshnessStale}}) {
		t.Fatal("stale evidence satisfied hard requirement")
	}
	if req.SatisfiedBy(CapabilityEvidenceSet{CapabilityJSONOutput: {Value: true, Confidence: providerinventory.ConfidenceExact, Freshness: providerinventory.FreshnessNotApplicable}}) {
		t.Fatal("not-applicable freshness satisfied fresh hard requirement")
	}
	if req.SatisfiedBy(CapabilityEvidenceSet{CapabilityJSONOutput: {Value: true, Confidence: providerinventory.ConfidenceExact}}) {
		t.Fatal("zero freshness satisfied fresh hard requirement")
	}
	if !req.SatisfiedBy(CapabilityEvidenceSet{CapabilityJSONOutput: {Value: true, Confidence: providerinventory.ConfidenceExact, Freshness: providerinventory.FreshnessFresh}}) {
		t.Fatal("fresh exact evidence did not satisfy hard requirement")
	}

	req.FreshnessRequired = ""
	if req.SatisfiedBy(CapabilityEvidenceSet{CapabilityJSONOutput: {Value: true, Confidence: providerinventory.ConfidenceExact, Freshness: providerinventory.FreshnessFresh}}) {
		t.Fatal("malformed freshness requirement was satisfied")
	}
}

func TestJSONContractFailsClosedForUnknownEnums(t *testing.T) {
	var payload struct {
		SchemaVersion string   `json:"schema_version"`
		RiskTier      RiskTier `json:"risk_tier"`
	}
	err := json.Unmarshal([]byte(`{"schema_version":"loopcoder.task_requirement.v1","risk_tier":"maybe"}`), &payload)
	if !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("json unmarshal error = %v, want ErrInvalidRecord", err)
	}
	req, classifyErr := Classify(baseInput())
	if classifyErr != nil {
		t.Fatalf("Classify() error = %v", classifyErr)
	}
	req.RequiredOutput = OutputContract("mystery")
	req.TaskRequirementFingerprint = ""
	req.TaskRequirementFingerprint, classifyErr = FingerprintRequirement(req)
	if classifyErr != nil {
		t.Fatalf("fingerprint requirement: %v", classifyErr)
	}
	if err := Validate(req); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("Validate() error = %v, want ErrInvalidRecord", err)
	}
}

func TestOverrideCannotLowerDeterministicFloor(t *testing.T) {
	input := baseInput()
	input.Scope = Scope{AllowsRepoWrite: true}
	input.Overrides = []RequirementOverride{{
		SchemaVersion: SchemaTaskRequirementOverride,
		ProjectID:     input.ProjectID,
		DeliveryRunID: input.DeliveryRunID,
		TaskKey:       input.TaskKey,
		ScopeKey:      input.TaskKey,
		Field:         "risk_tier",
		ValueJSON:     `"low"`,
		Justification: "operator correction",
		Status:        "active",
	}}
	got, err := Classify(input)
	if !errors.Is(err, ErrRequirementConfidenceInsufficient) {
		t.Fatalf("Classify() error = %v, want ErrRequirementConfidenceInsufficient", err)
	}
	if got.RiskTier != RiskMedium || !contains(got.GapReasons, "invalid-user-correction") {
		t.Fatalf("requirement after rejected override = %#v", got)
	}
}

func TestOverrideCanRaiseAndIsVisibleInDecisionSource(t *testing.T) {
	input := baseInput()
	input.Scope = Scope{AllowsRepoWrite: true}
	input.Overrides = []RequirementOverride{{
		SchemaVersion:         SchemaTaskRequirementOverride,
		RequirementOverrideID: "treqovr_raise",
		ProjectID:             input.ProjectID,
		DeliveryRunID:         input.DeliveryRunID,
		TaskKey:               input.TaskKey,
		ScopeKey:              input.TaskKey,
		Field:                 "risk_tier",
		ValueJSON:             `"high"`,
		Justification:         "operator correction",
		Status:                "active",
	}}
	got, err := Classify(input)
	if err != nil {
		t.Fatalf("Classify() error = %v", err)
	}
	if got.RiskTier != RiskHigh || !contains(got.ClassificationRules, "policy.user-correction") || !contains(got.Source.RecordIDs, "treqovr_raise") {
		t.Fatalf("requirement after override = %#v", got)
	}
}

func baseInput() ClassificationInput {
	now := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	return ClassificationInput{
		ProjectID:       "proj_1",
		DeliveryRunID:   "drun_1",
		TaskID:          "task_1",
		TaskKey:         "task-key",
		Title:           "Implement task",
		IntentSummary:   "Implement task",
		RoleKey:         "worker",
		PlanFingerprint: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		CreatedBy:       delivery.Actor{ActorKind: "user", ActorID: "u1", DecisionAuthority: "operator", Source: "test"},
		Host:            delivery.Host{HostKind: "codex", HostID: "h1", SessionID: "s1", LoopcoderVersion: "test", Platform: "test"},
		Now:             now,
	}
}

func hasVerification(req TaskRequirement, kind VerificationKind) bool {
	for _, verification := range req.VerificationRequirements {
		if verification.VerificationKind == kind {
			return true
		}
	}
	return false
}
