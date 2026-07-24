package autoroute

import (
	"fmt"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/capclass"
	"github.com/jasonhnd/loopcoder/internal/eligibility"
	"github.com/jasonhnd/loopcoder/internal/runtimecap"
)

// BoundPin is an explicit owner pin after capability proof + exact inventory bind.
// Provider/model are never overridden; account/install/window come from the exact
// hard-eligible inventory row for that pin.
type BoundPin struct {
	Provider   string
	Model      string
	Effort     string
	Permission string
	AccountRef string
	InstallRef string
	WindowKind string
	// Candidate is the exact inventory row used for capacity identity (may be nil
	// only when inventory is empty — caller must fail closed before spend).
	Candidate *eligibility.Candidate
}

// BindExplicitPinWithClass binds an explicit owner pin after exact hard eligibility.
//
// Hard eligibility is exact and shared with auto-route (eligibility.Evaluate):
// Installed, Authenticated, ModelPresent, PermissionOK, EffortOK, Healthy, and
// ResourceFit must each be KnownTrue() and !IsUnknown(); CooldownActive must be
// KnownFalse() and !IsUnknown(); machine admission applies. taskClass must be a
// valid non-needs_human class — empty/invalid/needs_human fail closed (no Tera default).
//
// The selected inventory row is rebound by full route identity: provider, model,
// effort, permission, account, install, window. No cross-row alias under the same
// account/install.
func BindExplicitPinWithClass(
	provider, model, effort, permission string,
	taskClass capclass.Class,
	inv *Inventory,
) (BoundPin, error) {
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	effort = strings.TrimSpace(effort)
	permission = strings.TrimSpace(permission)
	if permission == "" {
		permission = "default"
	}
	if err := validateExplicitPinCapability(provider, model, effort, permission, "", ""); err != nil {
		return BoundPin{}, err
	}
	if inv == nil || len(inv.Candidates) == 0 {
		return BoundPin{}, fmt.Errorf("%w: explicit pin requires fresh inventory with exact hard-eligible candidate (provider=%s model=%s)",
			ErrPinFail, provider, model)
	}
	// Fail closed: no silent ClassTera. needs_human is Valid but cannot pin-spend.
	if !taskClass.Valid() {
		return BoundPin{}, fmt.Errorf("%w: task class required and must be valid (got %q; no silent tera default)",
			ErrPinFail, taskClass)
	}
	if taskClass == capclass.ClassNeedsHuman {
		return BoundPin{}, fmt.Errorf("%w: task class needs_human cannot pin-spend", ErrPinFail)
	}

	// Capability re-check for bound dimensions (production affirm contract).
	aff := runtimecap.ExactRouteAffirm(provider)
	if !aff.ProductionEligible || !aff.Account || !aff.Install || !aff.Model || !aff.Permission {
		return BoundPin{}, fmt.Errorf("%w: pin provider %s cannot affirm exact production route dimensions", ErrPinFail, provider)
	}
	if effort != "" && !aff.Depth {
		return BoundPin{}, fmt.Errorf("%w: pin provider %s cannot affirm exact depth", ErrPinFail, provider)
	}

	// Route through the hard eligibility evaluator with ExplicitPin so pin cannot
	// bypass KnownTrue hard facts, machine admission, policy, or task class.
	pinPerm := permission
	// When pin uses default permission, do not force exact permission equality on
	// inventory rows (evaluator skips when pin permission is empty).
	if strings.EqualFold(permission, "default") {
		pinPerm = ""
	}
	snap := eligibility.Snapshot{
		TaskRequiredClass: taskClass,
		ExplicitPin: &eligibility.PinFields{
			Provider: provider, Model: model, Effort: effort, Permission: pinPerm,
		},
		Candidates: append([]eligibility.Candidate(nil), inv.Candidates...),
		Machine:    inv.Machine,
		CapturedAt: time.Now().UTC(),
	}

	dec, err := eligibility.Evaluate(snap)
	if err != nil {
		return BoundPin{}, fmt.Errorf("%w: eligibility evaluate: %v", ErrPinFail, err)
	}
	if dec.FailClosed || dec.PinSelected == nil || !dec.PinSelected.Eligible {
		reasons := strings.Join(dec.Reasons, "; ")
		if reasons == "" {
			reasons = "pin ineligible"
		}
		return BoundPin{}, fmt.Errorf("%w: no exact hard-eligible inventory candidate for pin provider=%s model=%s depth=%s permission=%s (%s)",
			ErrPinFail, provider, model, effort, permission, reasons)
	}
	sel := dec.PinSelected
	// Exact account/install/window required for capacity identity (reserve/spend).
	if strings.TrimSpace(sel.AccountRef) == "" || strings.TrimSpace(sel.InstallRef) == "" {
		return BoundPin{}, fmt.Errorf("%w: pin candidate missing account/install capacity identity (provider=%s model=%s)",
			ErrPinFail, provider, model)
	}
	if strings.TrimSpace(sel.WindowKind) == "" {
		return BoundPin{}, fmt.Errorf("%w: pin candidate missing window kind (provider=%s model=%s)",
			ErrPinFail, provider, model)
	}

	// Re-locate the exact selected inventory row by full route identity.
	// Never alias a bad requested permission/effort/window to a healthy sibling
	// under the same account/install.
	var match *eligibility.Candidate
	for i := range inv.Candidates {
		c := &inv.Candidates[i]
		if !sameExactRouteIdentity(*c, *sel, effort, permission) {
			continue
		}
		// Defense-in-depth: re-assert exact hard facts on the inventory row.
		if !exactHardEligible(*c) {
			continue
		}
		match = c
		break
	}
	if match == nil {
		return BoundPin{}, fmt.Errorf("%w: pin selected by eligibility but exact inventory row hard-ineligible or missing (provider=%s model=%s effort=%s permission=%s)",
			ErrPinFail, provider, model, effort, permission)
	}

	boundEffort := effort
	if boundEffort == "" {
		boundEffort = strings.TrimSpace(match.Effort)
	}
	boundPerm := permission
	if boundPerm == "" || strings.EqualFold(boundPerm, "default") {
		if p := strings.TrimSpace(match.Permission); p != "" {
			boundPerm = p
		}
	}
	// Owner pin identity is immutable — never take provider/model from alternates.
	// Bound permission/effort come from the exact matched row when pin left them open.
	return BoundPin{
		Provider: provider, Model: model, Effort: boundEffort, Permission: boundPerm,
		AccountRef: match.AccountRef, InstallRef: match.InstallRef, WindowKind: match.WindowKind,
		Candidate: match,
	}, nil
}

// sameExactRouteIdentity requires provider/model/account/install/window plus
// effort and permission equality when the pin (or selected view) specifies them.
// Prevents cross-row alias under the same account/install.
func sameExactRouteIdentity(c eligibility.Candidate, sel eligibility.CandidateView, pinEffort, pinPerm string) bool {
	if !strings.EqualFold(c.Provider, sel.Provider) {
		return false
	}
	if strings.TrimSpace(c.Model) != strings.TrimSpace(sel.Model) {
		return false
	}
	if strings.TrimSpace(c.AccountRef) != strings.TrimSpace(sel.AccountRef) {
		return false
	}
	if strings.TrimSpace(c.InstallRef) != strings.TrimSpace(sel.InstallRef) {
		return false
	}
	if strings.TrimSpace(c.WindowKind) != strings.TrimSpace(sel.WindowKind) {
		return false
	}
	// Effort: when pin or selection specifies, require exact match on inventory row.
	wantEffort := strings.TrimSpace(pinEffort)
	if wantEffort == "" {
		wantEffort = strings.TrimSpace(sel.Effort)
	}
	if wantEffort != "" && normalizeEffort(c.Effort) != normalizeEffort(wantEffort) {
		return false
	}
	// Permission: when pin is non-default or selection has permission, exact match.
	wantPerm := strings.TrimSpace(pinPerm)
	if wantPerm == "" || strings.EqualFold(wantPerm, "default") {
		wantPerm = strings.TrimSpace(sel.Permission)
	}
	if wantPerm != "" && !strings.EqualFold(wantPerm, "default") {
		if !strings.EqualFold(normalizePermission(c.Permission), normalizePermission(wantPerm)) {
			return false
		}
	}
	// Selected view permission must also match inventory when present.
	if p := strings.TrimSpace(sel.Permission); p != "" {
		if !strings.EqualFold(normalizePermission(c.Permission), normalizePermission(p)) {
			return false
		}
	}
	if e := strings.TrimSpace(sel.Effort); e != "" {
		if normalizeEffort(c.Effort) != normalizeEffort(e) {
			return false
		}
	}
	return true
}

// usableTrue requires KnownTrue and rejects any IsUnknown freshness, including
// State=true + FreshUnknown (KnownTrue alone still allows that hole).
func usableTrue(f eligibility.Fact) bool {
	return f.KnownTrue() && !f.IsUnknown()
}

// exactHardEligible requires KnownTrue with usable freshness for every positive
// hard fact and CooldownActive exactly known false (not unknown/stale/expired).
// FreshUnknown is never usable even when State is true.
func exactHardEligible(c eligibility.Candidate) bool {
	if !usableTrue(c.Installed) || !usableTrue(c.Authenticated) ||
		!usableTrue(c.ModelPresent) || !usableTrue(c.PermissionOK) ||
		!usableTrue(c.EffortOK) || !usableTrue(c.Healthy) ||
		!usableTrue(c.ResourceFit) {
		return false
	}
	// CooldownActive: exactly known false and not IsUnknown.
	if !c.CooldownActive.KnownFalse() || c.CooldownActive.IsUnknown() {
		return false
	}
	return true
}
