package capacitysnapshot

import (
	"fmt"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/autoroute"
	"github.com/jasonhnd/loopcoder/internal/capclass"
	"github.com/jasonhnd/loopcoder/internal/eligibility"
	"github.com/jasonhnd/loopcoder/internal/quotamode"
	"github.com/jasonhnd/loopcoder/internal/quotapolicy"
)

// ToRouteInventory maps a Snapshot to autoroute.Inventory for production Resolve.
// Returns error when UnattendedOK is false (fail closed for auto-route).
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
		for _, w := range a.Windows {
			if w.Freshness != FreshnessFresh {
				continue
			}
			if f := RemainingFraction(w); f != nil {
				rem = f
				switch w.Confidence {
				case ConfidenceExact:
					confSoft = quotapolicy.EvidenceExact
				case ConfidenceEstimated:
					confSoft = quotapolicy.EvidenceEstimated
				}
				if w.ResetAt != nil {
					d := w.ResetAt.Sub(now)
					if d < 0 {
						d = 0
					}
					ttr = &d
				}
				break
			}
		}
		for _, m := range a.Models {
			if !m.PresentInCatalog || m.ModelID == "" {
				continue
			}
			effort := m.DefaultDepth
			if effort == "" && len(m.SupportedDepths) > 0 {
				effort = pickDefaultDepth(m.SupportedDepths)
			}
			if effort == "" {
				effort = "medium"
			}
			cl := classFromHint(m.ClassHint)
			// CooldownActive FactTrue means on cooldown (ineligible).
			cooldownFact := factBool(false, "cd-"+a.Provider)
			if a.CooldownActive {
				cooldownFact = factBool(true, "cd-"+a.Provider)
			}
			cands = append(cands, eligibility.Candidate{
				Provider: a.Provider, Model: m.ModelID, Effort: effort,
				Permission: "bounded_write", ModelClass: cl,
				Installed:      factBool(a.Installed, "inst-"+a.Provider),
				Authenticated:  factBool(a.Authenticated, "auth-"+a.Provider),
				ModelPresent:   factBool(true, "model-"+a.Provider+"-"+m.ModelID),
				PermissionOK:   factBool(true, "perm-"+a.Provider),
				EffortOK:       factBool(true, "effort-"+a.Provider),
				Healthy:        factBool(a.Healthy, "health-"+a.Provider),
				CooldownActive: cooldownFact,
				ResourceFit:    factBool(true, "res-"+a.Provider),
				QuotaRemaining: quotaRemainingUnits(rem),
			})
			sc := quotapolicy.Candidate{
				Provider: a.Provider, Model: m.ModelID,
				ReliabilityEvidence: quotapolicy.EvidenceUnknown,
			}
			if rem != nil {
				rf := *rem
				win := quotapolicy.Window{
					Kind: quotapolicy.WindowFiveHour, RemainingFraction: &rf,
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
		Mode: quotamode.DefaultModeConfig(quotamode.ModeBalanced),
	}, nil
}

func pickDefaultDepth(depths []string) string {
	for _, pref := range []string{"medium", "low", "high", "xhigh"} {
		for _, d := range depths {
			if d == pref {
				return pref
			}
		}
	}
	return depths[0]
}

func classFromHint(h string) capclass.Class {
	switch capclass.Class(strings.TrimSpace(h)) {
	case capclass.ClassLuna, capclass.ClassTera, capclass.ClassSoul, capclass.ClassNeedsHuman:
		return capclass.Class(h)
	default:
		return capclass.ClassTera
	}
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
