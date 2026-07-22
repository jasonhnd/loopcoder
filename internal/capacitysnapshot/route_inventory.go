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
		roOK, writeOK := providerPermissionSupportFixed(a.Provider)
		for _, m := range a.Models {
			if !m.PresentInCatalog || m.ModelID == "" {
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
			for _, effort := range depths {
				base := eligibility.Candidate{
					Provider: a.Provider, Model: m.ModelID, Effort: effort,
					ModelClass:     cl,
					Installed:      factBool(a.Installed, "inst-"+a.Provider),
					Authenticated:  factBool(a.Authenticated, "auth-"+a.Provider),
					ModelPresent:   factBool(true, "model-"+a.Provider+"-"+m.ModelID),
					EffortOK:       factBool(true, "effort-"+a.Provider+"-"+effort),
					Healthy:        factBool(a.Healthy, "health-"+a.Provider),
					CooldownActive: cooldownFact,
					ResourceFit:    factBool(true, "res-"+a.Provider),
					QuotaRemaining: quotaRemainingUnits(rem),
				}
				// Emit permission-specific candidates. PermissionOK is a hard gate.
				if writeOK {
					wc := base
					wc.Permission = "bounded_write"
					wc.PermissionOK = factBool(true, "perm-write-"+a.Provider)
					cands = append(cands, wc)
				}
				if roOK {
					rc := base
					rc.Permission = "read-only"
					rc.PermissionOK = factBool(true, "perm-ro-"+a.Provider)
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
		// Owner product default: quality floors already applied; soft rank burns
		// usable capacity before reset (V090-CRO-007).
		Mode: quotamode.DefaultModeConfig(quotamode.ModeBurnBeforeReset),
	}, nil
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
