package capclass

import (
	"testing"
	"time"
)

func knownBool(v bool) (bool, EvidenceState) { return v, EvidenceKnown }

func fixtureLunaDocs() RiskEvidence {
	return RiskEvidence{
		ChangeType: "docs", ChangeTypeState: EvidenceKnown,
		OwnershipAffected: false, OwnershipAffectedState: EvidenceKnown,
		Migration: false, MigrationState: EvidenceKnown,
		Security: false, SecurityState: EvidenceKnown,
		Concurrency: false, ConcurrencyState: EvidenceKnown,
		ExternalSideEffects: false, ExternalSideEffectsState: EvidenceKnown,
		TestBreadth: "unit", TestBreadthState: EvidenceKnown,
		Reversibility: "easy", ReversibilityState: EvidenceKnown,
		Ambiguity: false, AmbiguityState: EvidenceKnown,
	}
}

func fixtureTeraCode() RiskEvidence {
	e := fixtureLunaDocs()
	e.ChangeType = "code"
	e.TestBreadth = "integration"
	return e
}

func fixtureSoulSecurity() RiskEvidence {
	e := fixtureTeraCode()
	e.Security = true
	return e
}

func fixtureSoulMigration() RiskEvidence {
	e := fixtureTeraCode()
	e.ChangeType = "migration"
	e.Migration = true
	e.Reversibility = "hard"
	return e
}

func fixtureNeedsHumanAmbiguous() RiskEvidence {
	e := fixtureTeraCode()
	e.Ambiguity = true
	return e
}

func fixtureUnknownSecurity() RiskEvidence {
	e := fixtureTeraCode()
	e.SecurityState = EvidenceUnknown
	return e
}

func allRiskInputKeys() []string {
	return []string{
		"change_type", "ownership_affected", "migration", "security",
		"concurrency", "external_side_effects", "test_breadth",
		"reversibility", "ambiguity",
	}
}

func TestClassifyFixtureSet(t *testing.T) {
	tests := []struct {
		name string
		ev   RiskEvidence
		want Class
	}{
		{"docs_luna", fixtureLunaDocs(), ClassLuna},
		{"code_tera", fixtureTeraCode(), ClassTera},
		{"security_soul", fixtureSoulSecurity(), ClassSoul},
		{"migration_soul", fixtureSoulMigration(), ClassSoul},
		{"ambiguous_needs_human", fixtureNeedsHumanAmbiguous(), ClassNeedsHuman},
		{"unknown_security_soul", fixtureUnknownSecurity(), ClassSoul},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Classify(tt.ev)
			if c.RequiredClass != tt.want {
				t.Fatalf("class=%s want=%s reasons=%+v", c.RequiredClass, tt.want, c.Reasons)
			}
			if c.Schema != SchemaClassification {
				t.Fatalf("schema %s", c.Schema)
			}
			if c.PolicyVersion != PolicyVersion {
				t.Fatalf("policy %s", c.PolicyVersion)
			}
			if c.BaseClass != tt.want {
				t.Fatalf("base %s", c.BaseClass)
			}
			if c.Digest == "" || c.Digest == "sha256:error" {
				t.Fatalf("digest %q", c.Digest)
			}
			// acceptance #1: every risk input listed
			for _, k := range allRiskInputKeys() {
				if _, ok := c.RiskInputs[k]; !ok {
					t.Fatalf("missing risk input %s", k)
				}
			}
			if len(c.Reasons) == 0 {
				t.Fatal("expected reasons")
			}
			// deterministic replay
			c2 := Classify(tt.ev)
			if c.Digest != c2.Digest || c.RequiredClass != c2.RequiredClass {
				t.Fatalf("non-deterministic: %v vs %v", c, c2)
			}
		})
	}
}

func TestUnknownNeverWeaker(t *testing.T) {
	// Full known low-risk Luna
	luna := Classify(fixtureLunaDocs())
	if luna.RequiredClass != ClassLuna {
		t.Fatalf("baseline luna got %s", luna.RequiredClass)
	}

	// Flip each high-impact field to unknown; floor must not drop below Tera
	// and security/migration/external/ownership/reversibility/ambiguity must
	// not yield Luna.
	cases := []struct {
		name string
		mut  func(*RiskEvidence)
		min  Class
	}{
		{"change_unknown", func(e *RiskEvidence) { e.ChangeTypeState = EvidenceUnknown }, ClassSoul},
		{"security_unknown", func(e *RiskEvidence) { e.SecurityState = EvidenceUnknown }, ClassSoul},
		{"migration_unknown", func(e *RiskEvidence) { e.MigrationState = EvidenceUnknown }, ClassSoul},
		{"external_unknown", func(e *RiskEvidence) { e.ExternalSideEffectsState = EvidenceUnknown }, ClassSoul},
		{"ownership_unknown", func(e *RiskEvidence) { e.OwnershipAffectedState = EvidenceUnknown }, ClassSoul},
		{"rev_unknown", func(e *RiskEvidence) { e.ReversibilityState = EvidenceUnknown }, ClassSoul},
		{"ambiguity_unknown", func(e *RiskEvidence) { e.AmbiguityState = EvidenceUnknown }, ClassNeedsHuman},
		{"test_unknown", func(e *RiskEvidence) { e.TestBreadthState = EvidenceUnknown }, ClassTera},
		{"concurrency_unknown", func(e *RiskEvidence) { e.ConcurrencyState = EvidenceUnknown }, ClassTera},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			e := fixtureLunaDocs()
			tt.mut(&e)
			c := Classify(e)
			if c.RequiredClass.Rank() < tt.min.Rank() {
				t.Fatalf("class %s weaker than min %s", c.RequiredClass, tt.min)
			}
			// never silently Luna when any critical evidence unknown
			if c.RequiredClass == ClassLuna {
				t.Fatalf("unknown evidence silently chose luna")
			}
			// reasons must mark unknown somewhere when we mutated to unknown
			foundUnknown := false
			for _, r := range c.Reasons {
				if r.Unknown {
					foundUnknown = true
					break
				}
			}
			if !foundUnknown {
				t.Fatalf("expected unknown reason flag: %+v", c.Reasons)
			}
		})
	}
}

func TestClassesProviderNeutral(t *testing.T) {
	// Classes are tokens independent of model IDs (acceptance #2).
	for _, cl := range []Class{ClassLuna, ClassTera, ClassSoul, ClassNeedsHuman} {
		if !cl.Valid() {
			t.Fatalf("invalid %s", cl)
		}
	}
	// Marketing-ish strings are not classes
	if Class("gpt-5").Valid() || Class("claude-opus").Valid() {
		t.Fatal("model names must not be capability classes")
	}
}

func TestMaxClass(t *testing.T) {
	if MaxClass(ClassLuna, ClassSoul) != ClassSoul {
		t.Fatal("max")
	}
	if MaxClass(ClassNeedsHuman, ClassSoul) != ClassNeedsHuman {
		t.Fatal("needs_human max")
	}
}

func TestModelMeets(t *testing.T) {
	if !ModelMeets(ClassSoul, ClassTera) {
		t.Fatal("soul meets tera")
	}
	if ModelMeets(ClassLuna, ClassTera) {
		t.Fatal("luna does not meet tera")
	}
	if ModelMeets(ClassSoul, ClassNeedsHuman) {
		t.Fatal("no model meets needs_human")
	}
}

func TestApplyOverride(t *testing.T) {
	base := Classify(fixtureTeraCode())
	if base.RequiredClass != ClassTera {
		t.Fatalf("base %s", base.RequiredClass)
	}
	ov := Override{
		ID: "cov_test1", Actor: "owner", Reason: "security review",
		Direction: OverrideRaise, TargetClass: ClassSoul,
	}
	raised, err := ApplyOverride(base, ov)
	if err != nil {
		t.Fatal(err)
	}
	if raised.RequiredClass != ClassSoul || raised.BaseClass != ClassTera {
		t.Fatalf("raised=%+v", raised)
	}
	if raised.OverrideID != "cov_test1" {
		t.Fatalf("override id %s", raised.OverrideID)
	}
	// base digest must differ from raised (immutability of values)
	if raised.Digest == base.Digest {
		t.Fatal("digest should change after override")
	}

	// invalid raise to weaker
	_, err = ApplyOverride(base, Override{
		Actor: "a", Reason: "r", Direction: OverrideRaise, TargetClass: ClassLuna,
	})
	if err == nil {
		t.Fatal("expected raise-weaker error")
	}

	// lower with reason
	lowered, err := ApplyOverride(base, Override{
		ID: "cov_low", Actor: "owner", Reason: "docs-only after re-scope",
		Direction: OverrideLower, TargetClass: ClassLuna,
	})
	if err != nil {
		t.Fatal(err)
	}
	if lowered.RequiredClass != ClassLuna {
		t.Fatalf("lowered %s", lowered.RequiredClass)
	}

	// missing actor
	_, err = ApplyOverride(base, Override{Reason: "x", Direction: OverrideRaise, TargetClass: ClassSoul})
	if err == nil {
		t.Fatal("expected actor error")
	}
}

func TestOverrideStoreImmutableActive(t *testing.T) {
	s := NewOverrideStore()
	ov, err := s.Put(Override{
		Actor: "owner", Reason: "pin soul for release",
		Direction: OverrideRaise, TargetClass: ClassSoul,
		AttemptID: "att_1", CreatedAt: time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ov.ID == "" || ov.Schema != SchemaOverride {
		t.Fatalf("ov %+v", ov)
	}
	if err := s.MarkActive("att_1"); err != nil {
		t.Fatal(err)
	}
	if !s.IsActive("att_1") {
		t.Fatal("expected active")
	}
	// cannot put another override for active attempt
	_, err = s.Put(Override{
		Actor: "owner", Reason: "try mutate", Direction: OverrideLower, TargetClass: ClassLuna,
		AttemptID: "att_1",
	})
	if err == nil || !isImmutable(err) {
		t.Fatalf("expected ErrImmutable, got %v", err)
	}
	// other attempt still ok
	_, err = s.Put(Override{
		Actor: "owner", Reason: "other", Direction: OverrideRaise, TargetClass: ClassSoul,
		AttemptID: "att_2",
	})
	if err != nil {
		t.Fatal(err)
	}
	ids := s.ForAttempt("att_1")
	if len(ids) != 1 || ids[0] != ov.ID {
		t.Fatalf("for attempt %v", ids)
	}
	got, err := s.Get(ov.ID)
	if err != nil || got.Actor != "owner" {
		t.Fatalf("get %v %v", got, err)
	}
}

func isImmutable(err error) bool {
	return err != nil && (err == ErrImmutable || contains(err.Error(), "immutable"))
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		(func() bool {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		})())
}

func TestDefaultModelMap(t *testing.T) {
	m := DefaultModelMap()
	if m.Schema != SchemaModelMap || m.Version == "" {
		t.Fatalf("map %+v", m)
	}
	// provider-neutral classes, separate from model IDs
	cl, ok := LookupModel(m, "grok", "grok-4.5")
	if !ok || cl != ClassSoul {
		t.Fatalf("grok-4.5 %v %v", cl, ok)
	}
	cl, ok = LookupModel(m, "claude", "claude-haiku-4-5")
	if !ok || cl != ClassLuna {
		t.Fatalf("haiku %v %v", cl, ok)
	}
	// unknown model not in map
	if _, ok := LookupModel(m, "codex", "not-a-real-model"); ok {
		t.Fatal("expected miss")
	}
	// ModelMeets integration: soul model meets tera required
	req := Classify(fixtureTeraCode()).RequiredClass
	modelClass, _ := LookupModel(m, "grok", "grok-4.5")
	if !ModelMeets(modelClass, req) {
		t.Fatal("expected meet")
	}
	// luna model does not meet soul required
	reqSoul := Classify(fixtureSoulSecurity()).RequiredClass
	luna, _ := LookupModel(m, "claude", "claude-haiku-4-5")
	if ModelMeets(luna, reqSoul) {
		t.Fatal("luna must not meet soul")
	}
}

func TestNormalizeModelMapRejectsBad(t *testing.T) {
	_, err := NormalizeModelMap(ModelMap{Version: "v", Entries: []ModelCapability{
		{Provider: "x", ModelID: "m", Class: ClassNeedsHuman},
	}})
	if err == nil {
		t.Fatal("needs_human model class rejected")
	}
	_, err = NormalizeModelMap(ModelMap{Version: "", Entries: nil})
	if err == nil {
		t.Fatal("version required")
	}
	_, err = NormalizeModelMap(ModelMap{Version: "v", Entries: []ModelCapability{
		{Provider: "a", ModelID: "m", Class: ClassLuna},
		{Provider: "a", ModelID: "m", Class: ClassTera},
	}})
	if err == nil {
		t.Fatal("duplicate rejected")
	}
}

func TestKnownBoolHelper(t *testing.T) {
	// silence unused if compiler complains — helper used for readability in future
	v, st := knownBool(true)
	if !v || st != EvidenceKnown {
		t.Fatal("helper")
	}
}
