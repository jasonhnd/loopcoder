package capclass

import (
	"fmt"
	"strings"
)

// Classify maps risk evidence to a required capability class using
// PolicyVersion rules. Every risk input is listed; unknown high-impact
// evidence raises the floor conservatively and never silently chooses a
// weaker class (acceptance #3).
func Classify(e RiskEvidence) Classification {
	reasons := make([]Reason, 0, 16)
	floor := ClassLuna

	raise := func(code, input, detail string, to Class, unknown bool) {
		reasons = append(reasons, Reason{
			Code:    code,
			Input:   input,
			Detail:  detail,
			Raises:  to,
			Floor:   MaxClass(floor, to),
			Unknown: unknown,
		})
		floor = MaxClass(floor, to)
	}

	// --- change_type ---
	switch e.ChangeTypeState {
	case EvidenceKnown:
		switch strings.ToLower(strings.TrimSpace(e.ChangeType)) {
		case ChangeDocs:
			raise("change.docs", "change_type", "documentation-only change", ClassLuna, false)
		case ChangeCode:
			raise("change.code", "change_type", "code change requires bounded implementation class", ClassTera, false)
		case ChangeConfig:
			raise("change.config", "change_type", "config change requires bounded implementation class", ClassTera, false)
		case ChangeMigration:
			raise("change.migration", "change_type", "migration change is high-risk", ClassSoul, false)
		case ChangeArchitecture:
			raise("change.architecture", "change_type", "architecture change is high-risk", ClassSoul, false)
		case ChangeRelease:
			raise("change.release", "change_type", "release change is high-risk", ClassSoul, false)
		case ChangeUnknown, "":
			raise("change.unknown_value", "change_type", "unknown change type chooses conservative class", ClassSoul, true)
		default:
			raise("change.unrecognized", "change_type", "unrecognized change type chooses conservative class", ClassSoul, true)
		}
	case EvidenceUnknown:
		raise("change.unknown_evidence", "change_type", "unknown change evidence never selects weaker class", ClassSoul, true)
	default: // absent
		raise("change.absent", "change_type", "absent change type evidence is conservative", ClassTera, true)
	}

	// --- boolean high-impact flags ---
	boolField := func(name string, st EvidenceState, val bool, knownRaise Class, unknownRaise Class, knownCode, unknownCode, knownDetail string) {
		switch st {
		case EvidenceKnown:
			if val {
				raise(knownCode, name, knownDetail, knownRaise, false)
			} else {
				// known false: no raise; still record for explain completeness
				reasons = append(reasons, Reason{
					Code:   knownCode + ".false",
					Input:  name,
					Detail: name + " known false",
					Raises: ClassLuna,
					Floor:  floor,
				})
			}
		case EvidenceUnknown:
			raise(unknownCode, name, name+" unknown — conservative floor", unknownRaise, true)
		default:
			// absent treated as unknown for high-impact flags
			raise(name+".absent", name, name+" absent — conservative floor", unknownRaise, true)
		}
	}

	boolField("ownership_affected", e.OwnershipAffectedState, e.OwnershipAffected,
		ClassSoul, ClassSoul,
		"ownership.affected", "ownership.unknown",
		"multi-owner or exclusive boundary impact")

	boolField("migration", e.MigrationState, e.Migration,
		ClassSoul, ClassSoul,
		"migration.true", "migration.unknown",
		"storage or schema migration")

	boolField("security", e.SecurityState, e.Security,
		ClassSoul, ClassSoul,
		"security.true", "security.unknown",
		"security-sensitive change")

	boolField("concurrency", e.ConcurrencyState, e.Concurrency,
		ClassTera, ClassTera,
		"concurrency.true", "concurrency.unknown",
		"concurrency-sensitive change")

	boolField("external_side_effects", e.ExternalSideEffectsState, e.ExternalSideEffects,
		ClassSoul, ClassSoul,
		"external.true", "external.unknown",
		"external side effects (network, GitHub, publish)")

	// --- test_breadth ---
	switch e.TestBreadthState {
	case EvidenceKnown:
		switch strings.ToLower(strings.TrimSpace(e.TestBreadth)) {
		case TestNone:
			// no tests alone does not force Soul, but raises at least Tera for code-like work
			raise("test.none", "test_breadth", "no tests — at least standard class", ClassTera, false)
		case TestUnit:
			raise("test.unit", "test_breadth", "unit test breadth", ClassLuna, false)
		case TestIntegration:
			raise("test.integration", "test_breadth", "integration test breadth", ClassTera, false)
		case TestSystem:
			raise("test.system", "test_breadth", "system-level test breadth", ClassTera, false)
		case TestUnknown, "":
			raise("test.unknown_value", "test_breadth", "unknown test breadth — conservative", ClassTera, true)
		default:
			raise("test.unrecognized", "test_breadth", "unrecognized test breadth — conservative", ClassTera, true)
		}
	case EvidenceUnknown:
		raise("test.unknown_evidence", "test_breadth", "unknown test evidence — conservative", ClassTera, true)
	default:
		raise("test.absent", "test_breadth", "absent test evidence — conservative", ClassTera, true)
	}

	// --- reversibility ---
	switch e.ReversibilityState {
	case EvidenceKnown:
		switch strings.ToLower(strings.TrimSpace(e.Reversibility)) {
		case RevEasy:
			raise("rev.easy", "reversibility", "easily reversible", ClassLuna, false)
		case RevHard:
			raise("rev.hard", "reversibility", "hard to reverse", ClassTera, false)
		case RevIrreversible:
			raise("rev.irreversible", "reversibility", "irreversible change is high-risk", ClassSoul, false)
		case RevUnknown, "":
			raise("rev.unknown_value", "reversibility", "unknown reversibility — conservative", ClassSoul, true)
		default:
			raise("rev.unrecognized", "reversibility", "unrecognized reversibility — conservative", ClassSoul, true)
		}
	case EvidenceUnknown:
		raise("rev.unknown_evidence", "reversibility", "unknown reversibility evidence — never weaker", ClassSoul, true)
	default:
		raise("rev.absent", "reversibility", "absent reversibility — conservative", ClassTera, true)
	}

	// --- ambiguity ---
	switch e.AmbiguityState {
	case EvidenceKnown:
		if e.Ambiguity {
			// high ambiguity stops automatic routing
			raise("ambiguity.true", "ambiguity", "ambiguous intent requires human or Soul-class reasoning", ClassNeedsHuman, false)
		} else {
			reasons = append(reasons, Reason{
				Code:   "ambiguity.false",
				Input:  "ambiguity",
				Detail: "intent is unambiguous",
				Raises: ClassLuna,
				Floor:  floor,
			})
		}
	case EvidenceUnknown:
		raise("ambiguity.unknown", "ambiguity", "unknown ambiguity — needs human", ClassNeedsHuman, true)
	default:
		raise("ambiguity.absent", "ambiguity", "absent ambiguity evidence — needs human", ClassNeedsHuman, true)
	}

	// Ensure at least one reason if somehow empty (should not happen).
	if len(reasons) == 0 {
		raise("policy.default", "policy", "default floor", ClassLuna, false)
	}

	inputs := FormatRiskInputs(e)
	c := Classification{
		Schema:        SchemaClassification,
		PolicyVersion: PolicyVersion,
		RequiredClass: floor,
		RiskInputs:    inputs,
		Reasons:       reasons,
		BaseClass:     floor,
	}
	c.Digest = DigestOf(c)
	return c
}

// ApplyOverride returns a new classification with an owner override applied.
// Overrides may raise or lower relative to base, but:
//   - actor and reason are required
//   - the override is recorded by ID; classification is a new value object
//   - callers must not mutate an active attempt route (see OverrideStore)
func ApplyOverride(base Classification, ov Override) (Classification, error) {
	if strings.TrimSpace(ov.Actor) == "" || strings.TrimSpace(ov.Reason) == "" {
		return Classification{}, fmt.Errorf("%w: override actor and reason required", ErrInvalid)
	}
	if !ov.TargetClass.Valid() {
		return Classification{}, fmt.Errorf("%w: override target class", ErrInvalid)
	}
	if ov.Direction != OverrideRaise && ov.Direction != OverrideLower {
		return Classification{}, fmt.Errorf("%w: override direction", ErrInvalid)
	}
	// Direction must match rank relation when both known.
	if ov.Direction == OverrideRaise && ov.TargetClass.Rank() < base.BaseClass.Rank() {
		return Classification{}, fmt.Errorf("%w: raise target weaker than base", ErrInvalid)
	}
	if ov.Direction == OverrideLower && ov.TargetClass.Rank() > base.BaseClass.Rank() {
		return Classification{}, fmt.Errorf("%w: lower target stronger than base", ErrInvalid)
	}
	out := base
	out.RequiredClass = ov.TargetClass
	out.OverrideID = ov.ID
	out.Reasons = append(append([]Reason{}, base.Reasons...), Reason{
		Code:   "policy.owner_override",
		Input:  "owner_override",
		Detail: fmt.Sprintf("actor=%s direction=%s reason=%s", ov.Actor, ov.Direction, ov.Reason),
		Raises: ov.TargetClass,
		Floor:  ov.TargetClass,
	})
	out.Digest = DigestOf(out)
	return out, nil
}

// ModelMeets reports whether a model class satisfies a required class.
// needs_human is never satisfied by any model class.
func ModelMeets(modelClass, required Class) bool {
	if required == ClassNeedsHuman {
		return false
	}
	if !modelClass.Valid() || !required.Valid() {
		return false
	}
	return modelClass.Rank() >= required.Rank()
}
