package quotapolicy

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/capclass"
)

// Rank produces an ordered soft-score breakdown for hard-eligible candidates.
// It never changes pin or hard-eligibility semantics.
func Rank(in Input) (Ranking, error) {
	pol, err := in.Policy.Normalize()
	if err != nil {
		return Ranking{}, err
	}
	if !in.TaskClass.Valid() {
		return Ranking{}, fmt.Errorf("%w: task_class", ErrInvalid)
	}
	now := in.Now.UTC()
	if now.IsZero() {
		return Ranking{}, fmt.Errorf("%w: now required (injected clock)", ErrInvalid)
	}

	scores := make([]Score, 0, len(in.Candidates))
	for _, c := range in.Candidates {
		c.Provider = strings.ToLower(strings.TrimSpace(c.Provider))
		c.Model = strings.TrimSpace(c.Model)
		if c.Provider == "" || c.Model == "" {
			return Ranking{}, fmt.Errorf("%w: candidate identity", ErrInvalid)
		}
		sc := scoreOne(c, pol, in.TaskClass, now)
		scores = append(scores, sc)
	}
	sortScores(scores)

	// Annotate ties
	for i := 1; i < len(scores); i++ {
		if !scores[i].SoftExcluded && !scores[i-1].SoftExcluded &&
			scores[i].SoftScore == scores[i-1].SoftScore {
			scores[i].Reasons = append(scores[i].Reasons, ReasonTieBroken)
		}
	}

	r := Ranking{
		Schema:        SchemaRanking,
		PolicyVersion: pol.Version,
		TaskClass:     string(in.TaskClass),
		Scores:        scores,
		Evaluated:     now,
	}
	if in.ExplicitPinActive {
		r.Reasons = append(r.Reasons, ReasonPinMode)
	}
	if len(scores) == 0 {
		r.Reasons = append(r.Reasons, "no_candidates")
	}
	r.Digest = DigestOf(r)
	return r, nil
}

func scoreOne(c Candidate, pol Policy, task capclass.Class, now time.Time) Score {
	sc := Score{
		Schema:     SchemaScore,
		Provider:   c.Provider,
		Model:      c.Model,
		AccountRef: strings.TrimSpace(c.AccountRef),
		InstallRef: strings.TrimSpace(c.InstallRef),
		WindowKind: strings.TrimSpace(c.WindowKind),
		Reasons:    []string{},
	}

	// --- soft exclude: exhausted binding window, rate limit, cooldown ---
	// Prefer exact candidate WindowKind when set; never invent five_hour over weekly.
	binding, bindRem, bindEv, bindTTR, exhausted, rateLimited, noTelemetry := bindingWindow(c.Windows, c.WindowKind)
	sc.BindingWindow = binding
	if sc.WindowKind == "" && binding != "" {
		sc.WindowKind = string(binding)
	}

	if rateLimited {
		sc.SoftExcluded = true
		sc.ExcludeReasons = append(sc.ExcludeReasons, ReasonRateLimited)
		sc.Reasons = append(sc.Reasons, ReasonRateLimited)
	}
	if exhausted {
		sc.SoftExcluded = true
		sc.ExcludeReasons = append(sc.ExcludeReasons, ReasonExhausted)
		sc.Reasons = append(sc.Reasons, ReasonExhausted)
	}
	if c.CooldownActive {
		sc.SoftExcluded = true
		sc.ExcludeReasons = append(sc.ExcludeReasons, ReasonCooldown)
		sc.Reasons = append(sc.Reasons, ReasonCooldown)
	}
	if noTelemetry {
		sc.Reasons = append(sc.Reasons, ReasonNoTelemetry)
	}

	// Remaining usable after reserve floor for higher classes.
	reserve := reserveFor(task, pol)
	rawRem := 0.0
	remKnown := false
	switch bindEv {
	case EvidenceExact, EvidenceEstimated:
		if bindRem != nil {
			rawRem = clamp01(*bindRem)
			remKnown = true
		}
	case EvidenceUnknown:
		sc.Reasons = append(sc.Reasons, ReasonUnknownEvidence)
	case EvidenceStale:
		sc.Reasons = append(sc.Reasons, ReasonStaleEvidence)
	case EvidenceMissing:
		sc.Reasons = append(sc.Reasons, ReasonNoTelemetry)
	}

	usable := rawRem
	if remKnown && reserve > 0 && task.Rank() < capclass.ClassSoul.Rank() {
		// Capacity below reserve floor is not usable for this lower-class task.
		if usable <= reserve {
			sc.SoftExcluded = true
			sc.ExcludeReasons = append(sc.ExcludeReasons, ReasonReserveBreach)
			sc.Reasons = append(sc.Reasons, ReasonReserveBreach)
			usable = 0
		} else {
			usable = (usable - reserve) / (1 - reserve)
			usable = clamp01(usable)
		}
	}
	sc.RemainingUsable = usable

	// Burn urgency: high when abundant remaining AND near reset.
	burn := 0.0
	if remKnown {
		near := 0.0
		if bindTTR != nil {
			if *bindTTR <= 0 {
				near = 1.0
			} else if *bindTTR <= pol.NearResetHorizon {
				// linear: 1 at 0, 0 at horizon
				near = 1.0 - float64(*bindTTR)/float64(pol.NearResetHorizon)
				near = clamp01(near)
			}
		}
		burn = clamp01(rawRem * (0.35 + 0.65*near))
		if near > 0.5 && rawRem > 0.4 {
			sc.Reasons = append(sc.Reasons, ReasonNearResetPrefer)
		}
		if rawRem > 0.7 {
			sc.Reasons = append(sc.Reasons, ReasonAbundant)
		}
		if binding == WindowWeekly && rawRem < 0.25 {
			sc.Reasons = append(sc.Reasons, ReasonScarceWeekly)
		}
	}
	sc.BurnUrgency = burn

	// Reliability component
	rel := 0.5 // neutral when unknown — not zero
	relNote := "neutral_unknown"
	switch c.ReliabilityEvidence {
	case EvidenceExact, EvidenceEstimated:
		if c.Reliability != nil {
			rel = clamp01(*c.Reliability)
			relNote = string(c.ReliabilityEvidence)
			if rel < 0.4 {
				sc.Reasons = append(sc.Reasons, ReasonReliabilityLow)
			}
		}
	case EvidenceStale:
		rel = 0.5 * (1 - pol.StalePenalty)
		relNote = "stale_penalty"
		sc.Reasons = append(sc.Reasons, ReasonStaleEvidence)
	case EvidenceUnknown, EvidenceMissing, "":
		rel = 0.5 * (1 - pol.UnknownPenalty*0.5)
		relNote = "unknown_neutral"
	}

	// Concurrency: prefer lower load
	conc := clamp01(1.0 - c.ConcurrencyLoad)
	if c.ConcurrencyLoad > 0.7 {
		sc.Reasons = append(sc.Reasons, ReasonConcurrency)
	}

	// Uncertainty penalty on remaining when not exact
	remComponent := usable
	if remKnown && bindEv == EvidenceEstimated {
		remComponent *= 0.9
	}
	if !remKnown {
		// do not fabricate absolute tokens; use low but non-zero soft remaining signal
		remComponent = 0.15 * (1 - pol.UnknownPenalty)
		if bindEv == EvidenceStale {
			remComponent = 0.10 * (1 - pol.StalePenalty)
		}
	}

	comps := []Component{
		{Name: "burn_urgency", Value: burn, Weight: pol.WeightBurnUrgency, Weighted: burn * pol.WeightBurnUrgency},
		{Name: "remaining", Value: remComponent, Weight: pol.WeightRemaining, Weighted: remComponent * pol.WeightRemaining, Note: string(bindEv)},
		{Name: "reliability", Value: rel, Weight: pol.WeightReliability, Weighted: rel * pol.WeightReliability, Note: relNote},
		{Name: "concurrency", Value: conc, Weight: pol.WeightConcurrency, Weighted: conc * pol.WeightConcurrency},
	}
	sc.Components = comps

	total := 0.0
	for _, c := range comps {
		total += c.Weighted
	}
	// Apply global uncertainty penalty if binding evidence bad
	switch bindEv {
	case EvidenceUnknown, EvidenceMissing:
		total *= (1 - pol.UnknownPenalty*0.5)
	case EvidenceStale:
		total *= (1 - pol.StalePenalty*0.5)
	}
	if sc.SoftExcluded {
		total = 0
		sc.Reasons = append(sc.Reasons, ReasonSoftExcluded)
	}
	sc.SoftScore = math.Round(total*1e6) / 1e6 // stable float
	if len(sc.Reasons) == 0 {
		sc.Reasons = []string{"scored"}
	}
	return sc
}

// bindingWindow picks the route-bound window. When preferKind is set, only that
// kind is considered (exact selected window identity). Otherwise pick most scarce.
// Rate-limit windows with RateLimited force exclusion signal.
func bindingWindow(windows []Window, preferKind string) (kind WindowKind, rem *float64, ev EvidenceClass, ttr *time.Duration, exhausted, rateLimited, noTelemetry bool) {
	if len(windows) == 0 {
		return "", nil, EvidenceMissing, nil, false, false, true
	}
	preferKind = strings.TrimSpace(preferKind)
	noTelemetry = true
	// Prefer known finite windows; pick lowest remaining fraction among exact/estimated.
	bestScore := 2.0 // remaining fraction; lower = more scarce
	found := false
	for _, w := range windows {
		if preferKind != "" && string(w.Kind) != preferKind && !windowKindAliasEqual(string(w.Kind), preferKind) {
			// Still detect rate-limit on sibling windows.
			if w.RateLimited {
				rateLimited = true
			}
			continue
		}
		if w.RateLimited || w.Kind == WindowRateLimit && w.RateLimited {
			rateLimited = true
		}
		if w.RateLimited {
			rateLimited = true
		}
		if w.Exhausted && (w.Evidence == EvidenceExact || w.Evidence == EvidenceEstimated) {
			// exhausted window binds hard
			exhausted = true
			kind = w.Kind
			ev = w.Evidence
			z := 0.0
			rem = &z
			ttr = w.TimeToReset
			found = true
			noTelemetry = false
			continue
		}
		switch w.Evidence {
		case EvidenceExact, EvidenceEstimated:
			noTelemetry = false
			if w.RemainingFraction == nil {
				continue
			}
			r := clamp01(*w.RemainingFraction)
			// Scarcity score: remaining; rate-limit kind treated as binding when low
			score := r
			if !found || score < bestScore {
				bestScore = score
				kind = w.Kind
				rr := r
				rem = &rr
				ev = w.Evidence
				ttr = w.TimeToReset
				found = true
				if r == 0 {
					exhausted = true
				}
			}
		case EvidenceStale, EvidenceUnknown, EvidenceMissing:
			if !found {
				// keep worst uncertainty as binding if nothing known yet
				kind = w.Kind
				ev = w.Evidence
				ttr = w.TimeToReset
				if w.Evidence != EvidenceMissing {
					noTelemetry = false
				}
			}
		}
	}
	if !found && ev == "" {
		ev = EvidenceMissing
		noTelemetry = true
	}
	return
}

func windowKindAliasEqual(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == b {
		return true
	}
	norm := func(k string) string {
		switch strings.ToLower(k) {
		case "weekly", "fixed-week", "fixed_week":
			return "weekly"
		case "credit":
			return "credit"
		case "five_hour", "fixed_hour", "fixed-hour", "5h":
			return "five_hour"
		default:
			return strings.ToLower(k)
		}
	}
	return norm(a) == norm(b)
}

func reserveFor(task capclass.Class, pol Policy) float64 {
	// Higher-class reserves apply when the task is lower class.
	switch task {
	case capclass.ClassLuna:
		// Hold both tera and soul reserves (max of configured floors).
		return math.Max(pol.TeraReserveFraction, pol.SoulReserveFraction)
	case capclass.ClassTera:
		return pol.SoulReserveFraction
	default:
		return 0
	}
}
