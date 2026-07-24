package capacitysnapshot

import (
	"fmt"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/autoroute"
	"github.com/jasonhnd/loopcoder/internal/capclass"
	"github.com/jasonhnd/loopcoder/internal/depthpolicy"
	"github.com/jasonhnd/loopcoder/internal/eligibility"
	"github.com/jasonhnd/loopcoder/internal/quotamode"
	"github.com/jasonhnd/loopcoder/internal/quotapolicy"
	"github.com/jasonhnd/loopcoder/internal/runtimecap"
)

// ToRouteInventory maps a Snapshot to autoroute.Inventory for production Resolve.
// Returns error when UnattendedOK is false (fail closed for auto-route).
//
// Permission-aware: each model emits separate candidates for read-only and
// bounded_write. Providers that lack read-only capability (e.g. Antigravity)
// get PermissionOK=false for read-only rows and are hard-excluded when the
// request requires read-only. Capacity remaining is shared across permission
// rows; it never invents windows.
func ToRouteInventory(s Snapshot, now time.Time) (autoroute.Inventory, error) {
	if strings.TrimSpace(s.Digest) == "" {
		return autoroute.Inventory{}, fmt.Errorf("%w: missing digest", ErrInvalidSnapshot)
	}
	if !s.UnattendedOK {
		return autoroute.Inventory{}, fmt.Errorf("%w: %s", ErrNoEligibleAccount, strings.Join(s.Reasons, "; "))
	}
	if now.IsZero() {
		now = s.CapturedAt
	}
	var cands []eligibility.Candidate
	var softs []quotapolicy.Candidate
	for _, a := range s.Accounts {
		ok, _ := unattendedEligible(a)
		if !ok {
			continue
		}
		var rem *float64
		var ttr *time.Duration
		confSoft := quotapolicy.EvidenceUnknown
		hasFreshWindow := false
		hasStaleWindow := false
		hasAnyWindow := false
		// Soft ranking receives a single window per provider. Multi-window
		// companies (e.g. Antigravity primary_5h≈98% + secondary/3p≈11%) used
		// to bind the first RemainingFraction in iteration order. When that was
		// the scarce secondary window, Luna/Tera reserve floors soft-excluded
		// the whole provider even though primary capacity was abundant — leaving
		// only codex executable for multi-provider canaries.
		// Prefer the highest remaining among fresh exact/estimated windows (and
		// exact over estimated at a tie) for the soft-score binding window.
		for _, w := range a.Windows {
			hasAnyWindow = true
			if w.Freshness == FreshnessStale || w.Freshness == FreshnessExpired {
				hasStaleWindow = true
			}
			if w.Freshness != FreshnessFresh {
				continue
			}
			hasFreshWindow = true
			f := RemainingFraction(w)
			if f == nil {
				continue
			}
			var ev quotapolicy.EvidenceClass
			switch w.Confidence {
			case ConfidenceExact:
				ev = quotapolicy.EvidenceExact
			case ConfidenceEstimated:
				ev = quotapolicy.EvidenceEstimated
			default:
				// Unknown confidence is not a soft-binding observation.
				continue
			}
			better := rem == nil || *f > *rem ||
				(*f == *rem && confSoft != quotapolicy.EvidenceExact && ev == quotapolicy.EvidenceExact)
			if !better {
				continue
			}
			rf := *f
			rem = &rf
			confSoft = ev
			ttr = nil
			if w.ResetAt != nil {
				d := w.ResetAt.Sub(now)
				if d < 0 {
					d = 0
				}
				ttr = &d
			}
		}
		staleOnly := hasStaleWindow && !hasFreshWindow
		// Stale/expired-only windows: hard-exclude via stale health evidence (no silent use).
		// Exhausted remaining (0): hard-exclude via ResourceFit false (no invent capacity).
		// Cooldown already hard-excluded via CooldownActive.
		roOK, writeOK := providerPermissionSupportFixed(a.Provider)
		for _, m := range a.Models {
			if !m.PresentInCatalog || m.ModelID == "" {
				continue
			}
			// Fail closed: static seed / adapter-declared / source-less hints
			// stay in the snapshot for display only — never production auto-route.
			if m.CatalogHintOnly {
				continue
			}
			// Emit one candidate per supported depth so per-child required depth
			// (route_requirement depth=low|medium|high) can filter and bind
			// eligibility. Never invent depths the model does not support.
			// Prefer class-hint difficulty only when no explicit child depth is set
			// (handled at Resolve); inventory must expose the full supported ladder.
			depths := supportedDepthsForModel(m)
			cl := classFromHint(m.ClassHint)
			if cl == capclass.ClassTera {
				// Prefer capclass map when model is known.
				if mapped, ok := capclass.LookupModel(capclass.DefaultModelMap(), a.Provider, m.ModelID); ok {
					cl = mapped
				}
			}
			// CooldownActive FactTrue means on cooldown (ineligible).
			cooldownFact := factBool(false, "cd-"+a.Provider)
			if a.CooldownActive {
				cooldownFact = factBool(true, "cd-"+a.Provider)
			}
			healthyFact := factBool(a.Healthy, "health-"+a.Provider)
			if staleOnly && rem == nil {
				healthyFact = eligibility.Fact{
					State: eligibility.FactTrue, EvidenceID: "health-stale-" + a.Provider,
					Freshness: eligibility.FreshStale,
				}
			}
			resFit := factBool(true, "res-"+a.Provider)
			if rem != nil && *rem <= 0 {
				resFit = factBool(false, "res-exhausted-"+a.Provider)
			}
			if hasAnyWindow && rem == nil && !staleOnly {
				// Window present but no usable remaining → unfit (not invented full).
				resFit = factBool(false, "res-unknown-remaining-"+a.Provider)
			}
			// Capacity window identity for this account (soft-bound best remaining window).
			winKind := ""
			for _, w := range a.Windows {
				if w.Freshness != FreshnessFresh {
					continue
				}
				if capacitysnapshotRemainingOK(w) {
					winKind = string(w.Kind)
					// Empty/unknown windows stay empty — never invent five_hour.
					break
				}
			}
			// Prefer the same highest-remaining window used for soft ranking when available.
			if rem != nil {
				var bestKind string
				var bestRem *float64
				for _, w := range a.Windows {
					if w.Freshness != FreshnessFresh {
						continue
					}
					f := RemainingFraction(w)
					if f == nil {
						continue
					}
					if bestRem == nil || *f > *bestRem {
						rf := *f
						bestRem = &rf
						bestKind = string(w.Kind)
					}
				}
				if bestKind != "" {
					winKind = bestKind
				}
			}
			// One canonical route window identity for hard eligibility and soft
			// ranking (RC34 #1397: fixed-week vs weekly identity split → no_route).
			// Empty stays empty — never invent five_hour/weekly.
			winKind = CanonicalRouteWindowKind(winKind)
			accRef := strings.TrimSpace(a.AccountRef)
			instRef := strings.TrimSpace(a.InstallRef)
			// Runtime hard-eligibility: capacity reserve requires exact
			// account/install/depth affirmation. Exclude providers whose runners
			// cannot affirm required dimensions before spend.
			affirm := providerRuntimeAffirm(a.Provider)
			for _, effort := range depths {
				authFact := factBool(a.Authenticated, "auth-"+a.Provider)
				effortFact := factBool(true, "effort-"+a.Provider+"-"+effort)
				// ModelPresent must reflect ExactRouteAffirm.Model (not always true).
				modelPresent := factBool(true, "model-"+a.Provider+"-"+m.ModelID)
				if !affirm.Model {
					modelPresent = factBool(false, "runtime-no-model-affirm-"+a.Provider)
				}
				// Fixture / non-production adapters are never capacity-bound winners.
				if !affirm.ProductionEligible {
					authFact = factBool(false, "runtime-not-production-eligible-"+a.Provider)
				}
				if !affirm.Account && accRef != "" {
					// Capacity-bound product path requires account affirmation.
					authFact = factBool(false, "runtime-no-account-affirm-"+a.Provider)
				}
				if !affirm.Depth {
					effortFact = factBool(false, "runtime-no-depth-affirm-"+a.Provider)
				}
				if !affirm.Install && instRef != "" {
					authFact = factBool(false, "runtime-no-install-affirm-"+a.Provider)
				}
				base := eligibility.Candidate{
					Provider: a.Provider, Model: m.ModelID, Effort: effort,
					AccountRef:     accRef,
					InstallRef:     instRef,
					WindowKind:     winKind,
					ModelClass:     cl,
					Installed:      factBool(a.Installed, "inst-"+a.Provider),
					Authenticated:  authFact,
					ModelPresent:   modelPresent,
					EffortOK:       effortFact,
					Healthy:        healthyFact,
					CooldownActive: cooldownFact,
					ResourceFit:    resFit,
					QuotaRemaining: quotaRemainingUnits(rem),
				}
				// PermissionOK must reflect ExactRouteAffirm.Permission.
				// Providers that cannot affirm permission never win/reserve.
				if writeOK {
					wc := base
					wc.Permission = "bounded_write"
					if affirm.Permission {
						wc.PermissionOK = factBool(true, "perm-write-"+a.Provider)
					} else {
						wc.PermissionOK = factBool(false, "runtime-no-permission-affirm-"+a.Provider)
					}
					cands = append(cands, wc)
				}
				if roOK {
					rc := base
					rc.Permission = "read-only"
					if affirm.Permission {
						rc.PermissionOK = factBool(true, "perm-ro-"+a.Provider)
					} else {
						rc.PermissionOK = factBool(false, "runtime-no-permission-affirm-"+a.Provider)
					}
					cands = append(cands, rc)
				} else {
					// Explicit ineligible row so reports can show denial reasons when
					// a read-only request is attempted against this company.
					rc := base
					rc.Permission = "read-only"
					rc.PermissionOK = factBool(false, "perm-ro-denied-"+a.Provider)
					cands = append(cands, rc)
				}
			}
			// One soft row per account identity. WindowKind already canonical —
			// identical to hard candidates (never remap only on soft).
			sc := quotapolicy.Candidate{
				Provider: a.Provider, Model: m.ModelID,
				AccountRef:          accRef,
				InstallRef:          instRef,
				WindowKind:          winKind,
				ReliabilityEvidence: quotapolicy.EvidenceUnknown,
			}
			if rem != nil {
				rf := *rem
				// winKind is already CanonicalRouteWindowKind; empty stays empty.
				wk := quotapolicy.WindowKind(winKind)
				win := quotapolicy.Window{
					Kind: wk, RemainingFraction: &rf,
					Evidence: confSoft,
				}
				if ttr != nil {
					d := *ttr
					win.TimeToReset = &d
				}
				if rf == 0 {
					win.Exhausted = true
				}
				sc.Windows = []quotapolicy.Window{win}
			}
			softs = append(softs, sc)
		}
	}
	if len(cands) == 0 {
		return autoroute.Inventory{}, fmt.Errorf("%w: no candidates after mapping", ErrNoEligibleAccount)
	}
	digest := s.Digest
	if len(digest) > 16 {
		digest = digest[:16]
	}
	return autoroute.Inventory{
		EvidenceDigest: "capacity-" + digest,
		Candidates:     cands,
		Soft:           softs,
		Machine: eligibility.MachineAdmission{
			CapacityOK: factBool(true, "mach"), ConcurrentSlots: 2,
		},
		// Owner product default: quality floors already applied; soft rank burns
		// usable capacity before reset (V090-CRO-007).
		Mode: quotamode.DefaultModeConfig(quotamode.ModeBurnBeforeReset),
	}, nil
}

func capacitysnapshotRemainingOK(w Window) bool {
	return RemainingFraction(w) != nil
}

// CanonicalRouteWindowKind maps an observed capacity window kind onto one
// route-inventory identity token shared by hard eligibility candidates and
// soft ranking candidates. Known aliases converge; empty stays empty (never
// invent five_hour/weekly). Other nonempty kinds remain a normalized raw
// token so hard and soft stay identical without inventing a fixed window.
func CanonicalRouteWindowKind(raw string) string {
	k := strings.ToLower(strings.TrimSpace(raw))
	switch k {
	case "":
		return ""
	case "weekly", "fixed-week", "fixed_week":
		return "weekly"
	case "five_hour", "fixed_hour", "fixed-hour", "5h":
		return "five_hour"
	case "credit":
		return "credit"
	case "daily":
		// Preserve prior soft mapping (WindowOther) for daily buckets.
		return "other"
	default:
		return k
	}
}

// providerRuntimeAffirm is a thin alias of the single authoritative
// runtimecap.ExactRouteAffirm contract (auto-route and explicit pin share it).
// Fixture is never production-eligible.
func providerRuntimeAffirm(provider string) runtimecap.ExactRouteAffirmation {
	return runtimecap.ExactRouteAffirm(provider)
}

// providerPermissionSupportFixed returns (readOnlyOK, writeOK) from the
// authoritative runtimecap contract.
//
// Antigravity declares neither ReadOnly nor BoundedWrite but product write path
// uses `agy -p` workspace write — treat as write-only.
// Codex/Claude declare both ReadOnly and BoundedWrite.
func providerPermissionSupportFixed(provider string) (readOnlyOK, writeOK bool) {
	p, ok := runtimecap.LookupProvider(provider)
	if !ok {
		// Unknown: do not invent read-only.
		return false, true
	}
	readOnlyOK = p.ReadOnly
	writeOK = p.BoundedWrite
	if !p.ReadOnly && !p.BoundedWrite {
		// Write-only adapters (antigravity).
		writeOK = true
	}
	return readOnlyOK, writeOK
}

func classFromHint(h string) capclass.Class {
	switch capclass.Class(strings.TrimSpace(h)) {
	case capclass.ClassLuna, capclass.ClassTera, capclass.ClassSoul, capclass.ClassNeedsHuman:
		return capclass.Class(h)
	default:
		return capclass.ClassTera
	}
}

// supportedDepthsForModel returns normalized unique depths the model may run.
// Empty catalog support falls back to medium only (never invent high/low).
func supportedDepthsForModel(m ModelEntry) []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range m.SupportedDepths {
		n := depthpolicy.NormalizeDepth(d)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	if len(out) == 0 {
		if def := depthpolicy.NormalizeDepth(m.DefaultDepth); def != "" {
			return []string{def}
		}
		return []string{"medium"}
	}
	return out
}

func factBool(ok bool, id string) eligibility.Fact {
	if ok {
		return eligibility.Fact{State: eligibility.FactTrue, EvidenceID: id, Freshness: eligibility.FreshFresh}
	}
	return eligibility.Fact{State: eligibility.FactFalse, EvidenceID: id, Freshness: eligibility.FreshFresh}
}

func quotaRemainingUnits(rem *float64) int64 {
	if rem == nil {
		// Unknown must not look like a huge remaining budget.
		return 0
	}
	return int64((*rem) * 1000)
}
