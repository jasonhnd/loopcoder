package eligibility

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/capclass"
)

// Evaluate runs the hard eligibility ladder on a frozen snapshot.
// It never consults the network and never uses quota to repair hard failures.
func Evaluate(snap Snapshot) (Decision, error) {
	if !snap.TaskRequiredClass.Valid() {
		return Decision{}, fmt.Errorf("%w: task_required_class", ErrInvalid)
	}

	// Normalize candidates for stable order.
	cands := make([]Candidate, 0, len(snap.Candidates))
	for _, c := range snap.Candidates {
		c.Provider = strings.ToLower(strings.TrimSpace(c.Provider))
		c.Model = strings.TrimSpace(c.Model)
		if c.Provider == "" || c.Model == "" {
			return Decision{}, fmt.Errorf("%w: candidate provider/model", ErrInvalid)
		}
		cands = append(cands, c)
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].Provider != cands[j].Provider {
			return cands[i].Provider < cands[j].Provider
		}
		return cands[i].Model < cands[j].Model
	})

	d := Decision{
		Schema:        SchemaDecision,
		PolicyVersion: PolicyVersion,
		Eligible:      []CandidateView{},
		Excluded:      []Exclusion{},
		Reasons:       []string{},
	}

	// Machine capacity is a hard gate for all automatic candidates.
	machineOK, machineReasons := checkMachine(snap.Machine)

	if snap.ExplicitPin != nil {
		d.Mode = ModeExplicitPin
		pin, err := snap.ExplicitPin.Normalize()
		if err != nil {
			return Decision{}, err
		}
		// Find exact candidate match; no alias substitution.
		var match *Candidate
		for i := range cands {
			if cands[i].Provider == pin.Provider && cands[i].Model == pin.Model {
				// effort/permission: if pin specifies, require equality when candidate has value
				if pin.Effort != "" && cands[i].Effort != "" && cands[i].Effort != pin.Effort {
					continue
				}
				if pin.Permission != "" && cands[i].Permission != "" && cands[i].Permission != pin.Permission {
					continue
				}
				match = &cands[i]
				break
			}
		}
		if match == nil {
			// Pin points at unknown/missing candidate → fail closed.
			d.FailClosed = true
			d.Reasons = append(d.Reasons, ReasonPinIneligible, ReasonModelMissing)
			d.Excluded = append(d.Excluded, Exclusion{
				Schema: SchemaExclusion, Provider: pin.Provider, Model: pin.Model,
				Reasons: []string{ReasonPinMismatch, ReasonModelMissing},
			})
			d.Digest = DigestOf(d)
			return d, nil
		}
		view, reasons := assessCandidate(*match, snap, machineOK, machineReasons, true)
		view.Reasons = append([]string{ReasonPinMatch}, view.Reasons...)
		if !view.Eligible {
			d.FailClosed = true
			d.Reasons = append(d.Reasons, ReasonPinIneligible)
			d.Reasons = append(d.Reasons, reasons...)
			d.Excluded = append(d.Excluded, Exclusion{
				Schema: SchemaExclusion, Provider: match.Provider, Model: match.Model,
				Reasons: append([]string{ReasonPinIneligible}, reasons...), EvidenceID: collectEvidenceIDs(*match),
			})
			// acceptance #1: never fall back to other candidates
			d.Digest = DigestOf(d)
			return d, nil
		}
		// Eligible pin wins unchanged — no other candidates compete.
		sel := view
		d.PinSelected = &sel
		d.Eligible = []CandidateView{view}
		d.Reasons = append(d.Reasons, ReasonPinMatch, ReasonEligible)
		// Still record other candidates as excluded for pin mismatch (explain).
		for i := range cands {
			if cands[i].Provider == match.Provider && cands[i].Model == match.Model {
				continue
			}
			d.Excluded = append(d.Excluded, Exclusion{
				Schema: SchemaExclusion, Provider: cands[i].Provider, Model: cands[i].Model,
				Reasons: []string{ReasonPinMismatch}, EvidenceID: collectEvidenceIDs(cands[i]),
			})
		}
		d.Digest = DigestOf(d)
		return d, nil
	}

	// Automatic mode: all candidates compete on hard gates only.
	d.Mode = ModeAutomatic
	for _, c := range cands {
		view, reasons := assessCandidate(c, snap, machineOK, machineReasons, false)
		if view.Eligible {
			d.Eligible = append(d.Eligible, view)
		} else {
			d.Excluded = append(d.Excluded, Exclusion{
				Schema: SchemaExclusion, Provider: c.Provider, Model: c.Model,
				Reasons: reasons, EvidenceID: collectEvidenceIDs(c),
			})
		}
	}
	if len(d.Eligible) == 0 {
		d.Reasons = append(d.Reasons, "no_eligible_candidates")
	} else {
		d.Reasons = append(d.Reasons, ReasonEligible)
	}
	d.Digest = DigestOf(d)
	return d, nil
}

func checkMachine(m MachineAdmission) (bool, []string) {
	if m.CapacityOK.IsUnknown() {
		return false, []string{ReasonMachineUnknown}
	}
	if m.CapacityOK.KnownFalse() {
		return false, []string{ReasonMachineCapacity}
	}
	if !m.CapacityOK.KnownTrue() {
		return false, []string{ReasonMachineUnknown}
	}
	return true, nil
}

// assessCandidate applies the precedence ladder after pin resolution.
// Order: policy → install/auth → model/capability/effort/permission → task class
// → health/cooldown → resource/machine. Quota is never a compensating factor.
func assessCandidate(c Candidate, snap Snapshot, machineOK bool, machineReasons []string, isPin bool) (CandidateView, []string) {
	view := CandidateView{
		Schema:     SchemaCandidate,
		Provider:   c.Provider,
		Model:      c.Model,
		Effort:     c.Effort,
		Permission: c.Permission,
		ModelClass: c.ModelClass,
		Evidence:   evidenceMap(c),
	}
	var reasons []string
	add := func(code string) { reasons = append(reasons, code) }

	// Note quota is ignored for hard eligibility (document in reasons when present).
	if c.QuotaRemaining > 0 {
		// informational only — does not affect eligibility
		_ = ReasonQuotaIgnored
	}

	// 1) Policy allow/deny
	if inList(snap.Policy.DenyProvider, c.Provider) {
		add(ReasonPolicyDenyProvider)
	}
	if inList(snap.Policy.DenyModel, normKey(c.Provider, c.Model)) {
		add(ReasonPolicyDenyModel)
	}
	if len(snap.Policy.AllowProvider) > 0 && !inList(snap.Policy.AllowProvider, c.Provider) {
		add(ReasonPolicyNotAllowProv)
	}
	if len(snap.Policy.AllowModel) > 0 && !inList(snap.Policy.AllowModel, normKey(c.Provider, c.Model)) {
		add(ReasonPolicyNotAllowModel)
	}

	// 2) Installation
	reasons = append(reasons, factGate(c.Installed, ReasonNotInstalled, ReasonInstallUnknown, ReasonStaleEvidence)...)

	// 3) Auth
	reasons = append(reasons, factGate(c.Authenticated, ReasonAuthMissing, ReasonAuthUnknown, ReasonStaleEvidence)...)

	// 4) Model present
	reasons = append(reasons, factGate(c.ModelPresent, ReasonModelMissing, ReasonModelUnknown, ReasonStaleEvidence)...)

	// 5) Permission / effort
	reasons = append(reasons, factGate(c.PermissionOK, ReasonPermissionDenied, ReasonPermissionUnknown, ReasonStaleEvidence)...)
	reasons = append(reasons, factGate(c.EffortOK, ReasonEffortUnsupported, ReasonEffortUnknown, ReasonStaleEvidence)...)

	// 6) Task capability class
	if snap.TaskRequiredClass == capclass.ClassNeedsHuman {
		add(ReasonTaskClassNeedsHuman)
	} else if !capclass.ModelMeets(c.ModelClass, snap.TaskRequiredClass) {
		add(ReasonTaskClass)
	}

	// 7) Health / cooldown
	// CooldownActive true → ineligible; unknown cooldown → ineligible.
	switch {
	case c.CooldownActive.KnownTrue():
		add(ReasonCooldown)
	case c.CooldownActive.IsUnknown():
		add(ReasonCooldownUnknown)
	case c.CooldownActive.KnownFalse():
		// ok
	default:
		add(ReasonCooldownUnknown)
	}
	reasons = append(reasons, factGate(c.Healthy, ReasonUnhealthy, ReasonHealthUnknown, ReasonStaleEvidence)...)

	// 8) Resource fit + machine
	reasons = append(reasons, factGate(c.ResourceFit, ReasonResourceUnfit, ReasonResourceUnknown, ReasonStaleEvidence)...)
	if !machineOK {
		// Pin mode still respects machine capacity (hard gate).
		reasons = append(reasons, machineReasons...)
	}

	// Dedup reasons preserving order.
	reasons = uniqPreserve(reasons)

	if len(reasons) == 0 {
		view.Eligible = true
		view.Reasons = []string{ReasonEligible}
		if c.QuotaRemaining > 0 {
			// still record that quota was not used as a gate
			view.Reasons = append(view.Reasons, ReasonQuotaIgnored)
		}
		return view, nil
	}
	view.Eligible = false
	view.Reasons = reasons
	_ = isPin
	return view, reasons
}

func factGate(f Fact, falseCode, unknownCode, staleCode string) []string {
	if f.Freshness == FreshStale || f.Freshness == FreshExpired {
		return []string{staleCode}
	}
	if f.KnownTrue() {
		return nil
	}
	if f.KnownFalse() {
		return []string{falseCode}
	}
	return []string{unknownCode}
}

func uniqPreserve(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
