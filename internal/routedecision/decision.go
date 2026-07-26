package routedecision

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jasonhnd/loopcoder/internal/capclass"
	"github.com/jasonhnd/loopcoder/internal/eligibility"
	"github.com/jasonhnd/loopcoder/internal/quotamode"
	"github.com/jasonhnd/loopcoder/internal/quotapolicy"
)

const (
	SchemaDecision  = "loopcoder.route.decision.v1"
	SchemaExplain   = "loopcoder.route.explain.v1"
	PolicyVersion   = "route-decision-v1"
	OutcomeSelected = "selected"
	OutcomeNoRoute  = "no_route"
	OutcomePinFail  = "pin_fail_closed"
)

// Request freezes everything needed to decide a route (no live probes).
type Request struct {
	// DecisionKey is a stable attempt key (idempotent decide).
	DecisionKey string `json:"decision_key"`
	ProjectID   string `json:"project_id"`
	// EvidenceDigest binds the observation snapshot used for this decide.
	EvidenceDigest string               `json:"evidence_digest"`
	TaskClass      capclass.Class       `json:"task_class"`
	Eligibility    eligibility.Snapshot `json:"eligibility"`
	// SoftCandidates feed quotapolicy/quotamode (hard-eligible subset may be filtered).
	SoftCandidates []quotapolicy.Candidate `json:"soft_candidates"`
	Mode           quotamode.ModeConfig    `json:"mode"`
	// SoftStore optional for reservation pressure in ranking.
	// Not serialized; use Remaining instead for pure replay.
	Remaining []quotamode.SnapshotRemaining `json:"remaining,omitempty"`
	Now       time.Time                     `json:"now"`
}

// Decision is the immutable persisted route choice.
type Decision struct {
	Schema         string `json:"schema"`
	PolicyVersion  string `json:"policy_version"`
	DecisionID     string `json:"decision_id"`
	DecisionKey    string `json:"decision_key"`
	ProjectID      string `json:"project_id"`
	EvidenceDigest string `json:"evidence_digest"`
	Outcome        string `json:"outcome"`
	// Digests for replay after observations change.
	EligibilityDigest string `json:"eligibility_digest"`
	SoftRankingDigest string `json:"soft_ranking_digest,omitempty"`
	ModeRankingDigest string `json:"mode_ranking_digest,omitempty"`
	// Winner when selected.
	Winner *Winner `json:"winner,omitempty"`
	// Ordered candidates after hard+soft (eligible first).
	Candidates []CandidateView `json:"candidates"`
	// Hard exclusions from eligibility.
	HardExcluded []eligibility.Exclusion `json:"hard_excluded"`
	// Soft excluded after ranking.
	SoftExcluded []string `json:"soft_excluded,omitempty"`
	// Reasons top-level.
	Reasons   []string  `json:"reasons"`
	Digest    string    `json:"digest"`
	DecidedAt time.Time `json:"decided_at"`
	// PinFailClosed when explicit pin was ineligible.
	PinFailClosed bool `json:"pin_fail_closed,omitempty"`
}

// Winner is the selected route identity including capacity account/install/window.
type Winner struct {
	Provider   string   `json:"provider"`
	Model      string   `json:"model"`
	Effort     string   `json:"effort,omitempty"`
	Permission string   `json:"permission,omitempty"`
	AccountRef string   `json:"account_ref,omitempty"`
	InstallRef string   `json:"install_ref,omitempty"`
	WindowKind string   `json:"window_kind,omitempty"`
	SoftScore  float64  `json:"soft_score,omitempty"`
	TieBreak   string   `json:"tie_break,omitempty"`
	Reasons    []string `json:"reasons"`
}

// CandidateView is one ordered candidate in the decision.
// Effort and Permission are the observed eligibility fields for this row —
// never synthesized from the request. Distinct depth/permission lanes must
// not collapse into a single provider/model identity.
// AccountRef/InstallRef/WindowKind are first-class capacity identity.
type CandidateView struct {
	Provider      string   `json:"provider"`
	Model         string   `json:"model"`
	Effort        string   `json:"effort,omitempty"`
	Permission    string   `json:"permission,omitempty"`
	AccountRef    string   `json:"account_ref,omitempty"`
	InstallRef    string   `json:"install_ref,omitempty"`
	WindowKind    string   `json:"window_kind,omitempty"`
	HardEligible  bool     `json:"hard_eligible"`
	SoftExcluded  bool     `json:"soft_excluded"`
	SoftScore     float64  `json:"soft_score,omitempty"`
	AdjustedScore float64  `json:"adjusted_score,omitempty"`
	Reasons       []string `json:"reasons,omitempty"`
}

// ExplainResult is human + JSON explain output (redacted).
type ExplainResult struct {
	Schema     string   `json:"schema"`
	DecisionID string   `json:"decision_id"`
	Outcome    string   `json:"outcome"`
	Digest     string   `json:"digest"`
	Human      string   `json:"human"`
	WinnerLine string   `json:"winner_line,omitempty"`
	Sections   []string `json:"sections"`
	// JSONView is the decision with any residual secrets already absent by construction.
	JSONView Decision `json:"json_view"`
}

var (
	ErrInvalid   = errors.New("routedecision: invalid")
	ErrNotFound  = errors.New("routedecision: not found")
	ErrConflict  = errors.New("routedecision: decision key conflict")
	ErrNoRoute   = errors.New("routedecision: no route")
	ErrPinFailed = errors.New("routedecision: pin fail closed")
)

// Store holds immutable decisions keyed by decision_key.
type Store struct {
	mu    sync.Mutex
	byKey map[string]*Decision
	byID  map[string]*Decision
	seq   int64
}

// NewStore creates an empty decision store.
func NewStore() *Store {
	return &Store{byKey: map[string]*Decision{}, byID: map[string]*Decision{}}
}

// Decide evaluates and persists a decision. Replaying the same decision_key with
// identical digest returns the prior decision; a conflicting payload fails.
func (s *Store) Decide(req Request) (Decision, error) {
	if s == nil {
		return Decision{}, fmt.Errorf("%w: nil store", ErrInvalid)
	}
	d, err := Evaluate(req)
	if err != nil && d.DecisionID == "" && d.Digest == "" {
		// pure evaluation hard failure
		return Decision{}, err
	}
	// Evaluate always returns a Decision value even for no_route / pin fail;
	// only invalid input is a hard error without decision.
	if err != nil && !errors.Is(err, ErrNoRoute) && !errors.Is(err, ErrPinFailed) {
		return Decision{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	key := strings.TrimSpace(req.DecisionKey)
	if prev, ok := s.byKey[key]; ok {
		if prev.Digest == d.Digest {
			return *prev, nil // idempotent replay
		}
		return Decision{}, fmt.Errorf("%w: key %s prior digest %s new %s", ErrConflict, key, prev.Digest, d.Digest)
	}
	s.seq++
	d.DecisionID = fmt.Sprintf("rdec_%d", s.seq)
	// Recompute digest including DecisionID for stable storage identity.
	// Keep evaluation digest as content identity in a field... actually
	// acceptance wants same inputs → same digest. DecisionID is store-local.
	// So digest must NOT include DecisionID.
	cp := d
	s.byKey[key] = &cp
	s.byID[cp.DecisionID] = &cp
	return cp, nil
}

// GetByKey returns a persisted decision.
func (s *Store) GetByKey(key string) (Decision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.byKey[strings.TrimSpace(key)]
	if !ok {
		return Decision{}, ErrNotFound
	}
	return *d, nil
}

// GetByID returns a decision by id.
func (s *Store) GetByID(id string) (Decision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.byID[strings.TrimSpace(id)]
	if !ok {
		return Decision{}, ErrNotFound
	}
	return *d, nil
}

// Evaluate runs the pure decision pipeline without persistence.
func Evaluate(req Request) (Decision, error) {
	if strings.TrimSpace(req.DecisionKey) == "" || strings.TrimSpace(req.ProjectID) == "" {
		return Decision{}, fmt.Errorf("%w: decision_key and project_id required", ErrInvalid)
	}
	if strings.TrimSpace(req.EvidenceDigest) == "" {
		return Decision{}, fmt.Errorf("%w: evidence_digest required", ErrInvalid)
	}
	if !req.TaskClass.Valid() {
		return Decision{}, fmt.Errorf("%w: task_class", ErrInvalid)
	}
	now := req.Now.UTC()
	if now.IsZero() {
		return Decision{}, fmt.Errorf("%w: now required", ErrInvalid)
	}
	// Align eligibility task class with request.
	eligSnap := req.Eligibility
	eligSnap.TaskRequiredClass = req.TaskClass
	eligSnap.CapturedAt = now

	hard, err := eligibility.Evaluate(eligSnap)
	if err != nil {
		return Decision{}, err
	}

	d := Decision{
		Schema:            SchemaDecision,
		PolicyVersion:     PolicyVersion,
		DecisionKey:       strings.TrimSpace(req.DecisionKey),
		ProjectID:         strings.TrimSpace(req.ProjectID),
		EvidenceDigest:    strings.TrimSpace(req.EvidenceDigest),
		EligibilityDigest: hard.Digest,
		HardExcluded:      hard.Excluded,
		DecidedAt:         now,
		Reasons:           append([]string{}, hard.Reasons...),
	}

	if hard.FailClosed {
		d.Outcome = OutcomePinFail
		d.PinFailClosed = true
		d.Candidates = candidateViewsFromHard(hard, nil, nil)
		d.Digest = digestDecision(d)
		return d, ErrPinFailed
	}

	// Soft rank only hard-eligible candidates (filter SoftCandidates by hard set).
	// Identity includes effort+permission+account+window so accounts never collapse.
	eligibleKeys := map[string]eligibility.CandidateView{}
	eligiblePM := map[string]bool{}  // provider/model — soft filter
	eligibleAcc := map[string]bool{} // provider/model/account for exact soft bind
	for _, e := range hard.Eligible {
		eligibleKeys[candIdentity(e.Provider, e.Model, e.Effort, e.Permission, e.AccountRef, e.InstallRef, e.WindowKind)] = e
		eligiblePM[norm(e.Provider, e.Model)] = true
		if e.AccountRef != "" {
			eligibleAcc[norm(e.Provider, e.Model)+"|"+strings.TrimSpace(e.AccountRef)] = true
		}
	}
	softIn := make([]quotapolicy.Candidate, 0, len(req.SoftCandidates))
	for _, c := range req.SoftCandidates {
		if !eligiblePM[norm(c.Provider, c.Model)] {
			continue
		}
		// When soft row has account, require that account is hard-eligible.
		if c.AccountRef != "" && len(eligibleAcc) > 0 {
			if !eligibleAcc[norm(c.Provider, c.Model)+"|"+strings.TrimSpace(c.AccountRef)] {
				continue
			}
		}
		softIn = append(softIn, c)
	}
	// If no soft candidates provided, synthesize neutral ones from hard eligible
	// (one soft row per unique provider/model/account/window).
	// Empty WindowKind stays empty — never invent five_hour.
	// Unknown/provider-defined may be reported but is not a reservable fixed window.
	if len(softIn) == 0 {
		seenID := map[string]bool{}
		for _, e := range hard.Eligible {
			k := candIdentity(e.Provider, e.Model, e.Effort, e.Permission, e.AccountRef, e.InstallRef, e.WindowKind)
			if seenID[k] {
				continue
			}
			seenID[k] = true
			var wk quotapolicy.WindowKind
			if e.WindowKind != "" {
				wk = quotapolicy.WindowKind(e.WindowKind)
			}
			softIn = append(softIn, quotapolicy.Candidate{
				Provider: e.Provider, Model: e.Model,
				AccountRef: e.AccountRef, WindowKind: e.WindowKind,
				Windows: []quotapolicy.Window{{
					Kind: wk, Evidence: quotapolicy.EvidenceMissing,
				}},
				ReliabilityEvidence: quotapolicy.EvidenceMissing,
			})
		}
	}

	mode := req.Mode
	if !mode.Mode.Valid() {
		mode = quotamode.DefaultModeConfig(quotamode.ModeBalanced)
	}
	var pin *struct{ Provider, Model string }
	if hard.Mode == eligibility.ModeExplicitPin && hard.PinSelected != nil {
		pin = &struct{ Provider, Model string }{hard.PinSelected.Provider, hard.PinSelected.Model}
	}

	modeRank, err := quotamode.Rank(quotamode.RankInput{
		Mode: mode, TaskClass: req.TaskClass, Candidates: softIn,
		Remaining: req.Remaining, Now: now, ExplicitPin: pin,
	})
	if err != nil {
		return Decision{}, err
	}
	d.SoftRankingDigest = modeRank.Base.Digest
	d.ModeRankingDigest = modeRank.Digest

	// Build candidate views ordered by mode ranking.
	d.Candidates = candidateViewsFromHard(hard, &modeRank, eligibleKeys)

	// Winner selection: first non-soft-excluded adjusted score whose AccountRef
	// and WindowKind are first-class on the score row (no post-hoc fill from
	// first provider/model CandidateView — that is the forbidden PM fallback).
	var winner *Winner
	for _, sc := range modeRank.Scores {
		if sc.SoftExcluded {
			d.SoftExcluded = append(d.SoftExcluded, norm(sc.Provider, sc.Model))
			continue
		}
		if !eligiblePM[norm(sc.Provider, sc.Model)] {
			continue
		}
		// Production auto-route requires exact account+window on the scored row.
		// Empty account/window cannot qualify (never fill from another bound row).
		if strings.TrimSpace(sc.AccountRef) == "" || strings.TrimSpace(sc.WindowKind) == "" {
			d.SoftExcluded = append(d.SoftExcluded, norm(sc.Provider, sc.Model)+"|missing_score_identity")
			continue
		}
		// Match effort/permission/install only from exact account/window candidate rows.
		var effort, perm, install string
		matched := false
		for _, cv := range d.Candidates {
			if !cv.HardEligible || cv.SoftExcluded {
				continue
			}
			if strings.TrimSpace(cv.Provider) != strings.TrimSpace(sc.Provider) ||
				strings.TrimSpace(cv.Model) != strings.TrimSpace(sc.Model) {
				continue
			}
			if strings.TrimSpace(cv.AccountRef) != strings.TrimSpace(sc.AccountRef) {
				continue
			}
			if strings.TrimSpace(cv.InstallRef) != strings.TrimSpace(sc.InstallRef) {
				continue
			}
			if strings.TrimSpace(cv.WindowKind) != strings.TrimSpace(sc.WindowKind) {
				continue
			}
			effort = cv.Effort
			perm = cv.Permission
			install = cv.InstallRef
			matched = true
			break
		}
		if !matched {
			for _, e := range hard.Eligible {
				if strings.TrimSpace(e.Provider) != strings.TrimSpace(sc.Provider) || e.Model != sc.Model {
					continue
				}
				if strings.TrimSpace(e.AccountRef) != strings.TrimSpace(sc.AccountRef) {
					continue
				}
				if strings.TrimSpace(e.InstallRef) != strings.TrimSpace(sc.InstallRef) {
					continue
				}
				if strings.TrimSpace(e.WindowKind) != strings.TrimSpace(sc.WindowKind) {
					continue
				}
				effort = e.Effort
				perm = e.Permission
				install = e.InstallRef
				matched = true
				break
			}
		}
		if !matched {
			// Score has account/window but no hard-eligible exact row → skip.
			continue
		}
		winner = &Winner{
			Provider: sc.Provider, Model: sc.Model,
			AccountRef: sc.AccountRef, InstallRef: install, WindowKind: sc.WindowKind,
			Effort: effort, Permission: perm,
			SoftScore: sc.AdjustedScore,
			Reasons:   append([]string{"winner"}, sc.Reasons...),
		}
		break
	}
	// Tie-break annotation
	if winner != nil {
		// check if next has same score
		for i, sc := range modeRank.Scores {
			if sc.Provider == winner.Provider && sc.Model == winner.Model {
				if i+1 < len(modeRank.Scores) && !modeRank.Scores[i+1].SoftExcluded &&
					modeRank.Scores[i+1].AdjustedScore == sc.AdjustedScore {
					winner.TieBreak = "provider_model_lexicographic"
					winner.Reasons = append(winner.Reasons, "tie_break.provider_model")
				}
				break
			}
		}
		d.Winner = winner
		d.Outcome = OutcomeSelected
		d.Reasons = append(d.Reasons, "selected")
		d.Digest = digestDecision(d)
		return d, nil
	}

	d.Outcome = OutcomeNoRoute
	d.Reasons = append(d.Reasons, "no_route")
	d.Digest = digestDecision(d)
	return d, ErrNoRoute
}

// Explain builds a redacted human+JSON explanation from a decision.
func Explain(d Decision) ExplainResult {
	var b strings.Builder
	fmt.Fprintf(&b, "Route decision %s\n", d.DecisionID)
	fmt.Fprintf(&b, "Outcome: %s\n", d.Outcome)
	fmt.Fprintf(&b, "Digest: %s\n", d.Digest)
	fmt.Fprintf(&b, "Evidence: %s\n", d.EvidenceDigest)
	fmt.Fprintf(&b, "Eligibility digest: %s\n", d.EligibilityDigest)
	if d.SoftRankingDigest != "" {
		fmt.Fprintf(&b, "Soft ranking digest: %s\n", d.SoftRankingDigest)
	}
	if d.ModeRankingDigest != "" {
		fmt.Fprintf(&b, "Mode ranking digest: %s\n", d.ModeRankingDigest)
	}
	sections := []string{"header"}
	winnerLine := ""
	if d.Winner != nil {
		winnerLine = fmt.Sprintf("Winner: %s/%s score=%.4f", d.Winner.Provider, d.Winner.Model, d.Winner.SoftScore)
		fmt.Fprintln(&b, winnerLine)
		if d.Winner.TieBreak != "" {
			fmt.Fprintf(&b, "Tie-break: %s\n", d.Winner.TieBreak)
		}
		sections = append(sections, "winner")
	}
	if d.PinFailClosed {
		fmt.Fprintln(&b, "Explicit pin failed closed (no automatic fallback).")
		sections = append(sections, "pin_fail_closed")
	}
	if len(d.HardExcluded) > 0 {
		fmt.Fprintln(&b, "Hard exclusions:")
		for _, ex := range d.HardExcluded {
			fmt.Fprintf(&b, "  - %s/%s: %s\n", ex.Provider, ex.Model, strings.Join(ex.Reasons, ", "))
		}
		sections = append(sections, "hard_exclusions")
	}
	if len(d.Candidates) > 0 {
		fmt.Fprintln(&b, "Candidates (ordered):")
		for i, c := range d.Candidates {
			fmt.Fprintf(&b, "  %d. %s/%s hard=%v soft_excluded=%v adj=%.4f\n",
				i+1, c.Provider, c.Model, c.HardEligible, c.SoftExcluded, c.AdjustedScore)
		}
		sections = append(sections, "candidates")
	}
	if len(d.Reasons) > 0 {
		fmt.Fprintf(&b, "Reasons: %s\n", strings.Join(d.Reasons, ", "))
		sections = append(sections, "reasons")
	}
	// Redaction note: credentials and raw quota payloads are never in Decision.
	fmt.Fprintln(&b, "Redaction: no credentials or raw quota payloads included.")
	sections = append(sections, "redaction")

	return ExplainResult{
		Schema:     SchemaExplain,
		DecisionID: d.DecisionID,
		Outcome:    d.Outcome,
		Digest:     d.Digest,
		Human:      b.String(),
		WinnerLine: winnerLine,
		Sections:   sections,
		JSONView:   d,
	}
}

// ExplainJSON returns compact JSON for CLI --format json.
func ExplainJSON(d Decision) ([]byte, error) {
	ex := Explain(d)
	return json.MarshalIndent(ex, "", "  ")
}

func candidateViewsFromHard(hard eligibility.Decision, mode *quotamode.ModeRanking, eligible map[string]eligibility.CandidateView) []CandidateView {
	// Index scores by exact account when present, else provider/model.
	scoreByID := map[string]quotamode.AdjustedScore{}
	scoreByPM := map[string]quotamode.AdjustedScore{}
	if mode != nil {
		for _, sc := range mode.Scores {
			scoreByPM[norm(sc.Provider, sc.Model)] = sc
			if sc.AccountRef != "" {
				scoreByID[norm(sc.Provider, sc.Model)+"|"+strings.TrimSpace(sc.AccountRef)+"|"+strings.TrimSpace(sc.InstallRef)+"|"+strings.TrimSpace(sc.WindowKind)] = sc
			}
		}
	}
	var views []CandidateView
	// One view per hard-eligible identity (provider+model+effort+permission+account+window).
	seenID := map[string]struct{}{}
	for _, e := range hard.Eligible {
		id := candIdentity(e.Provider, e.Model, e.Effort, e.Permission, e.AccountRef, e.InstallRef, e.WindowKind)
		if _, ok := seenID[id]; ok {
			continue
		}
		seenID[id] = struct{}{}
		v := CandidateView{
			Provider: e.Provider, Model: e.Model,
			Effort: e.Effort, Permission: e.Permission,
			AccountRef: e.AccountRef, InstallRef: e.InstallRef, WindowKind: e.WindowKind,
			HardEligible: true,
		}
		// Account/window-bound hard rows MUST use exact score identity only.
		// Never borrow another account's provider/model score (cross-wire).
		// PM fallback only for explicitly unbound legacy rows (empty AccountRef
		// AND empty WindowKind) — never qualifies production auto-route identity.
		bound := strings.TrimSpace(e.AccountRef) != "" || strings.TrimSpace(e.WindowKind) != ""
		if sc, ok := scoreByID[norm(e.Provider, e.Model)+"|"+strings.TrimSpace(e.AccountRef)+"|"+strings.TrimSpace(e.InstallRef)+"|"+strings.TrimSpace(e.WindowKind)]; ok {
			v.SoftExcluded = sc.SoftExcluded
			v.SoftScore = sc.BaseSoftScore
			v.AdjustedScore = sc.AdjustedScore
			v.Reasons = append([]string(nil), sc.Reasons...)
		} else if bound {
			// Missing exact score for a bound row → soft-exclude (fail closed).
			v.SoftExcluded = true
			v.Reasons = append(v.Reasons, "missing_exact_account_window_score")
		} else if sc, ok := scoreByPM[norm(e.Provider, e.Model)]; ok {
			// Legacy unbound only.
			v.SoftExcluded = sc.SoftExcluded
			v.SoftScore = sc.BaseSoftScore
			v.AdjustedScore = sc.AdjustedScore
			v.Reasons = append([]string(nil), sc.Reasons...)
			v.Reasons = append(v.Reasons, "legacy_unbound_pm_score")
		}
		// Prefer eligibility map when provided (same identity).
		if eligible != nil {
			if hv, ok := eligible[id]; ok {
				v.Effort = hv.Effort
				v.Permission = hv.Permission
				v.AccountRef = firstNonEmptyStr(hv.AccountRef, v.AccountRef)
				v.InstallRef = firstNonEmptyStr(hv.InstallRef, v.InstallRef)
				v.WindowKind = firstNonEmptyStr(hv.WindowKind, v.WindowKind)
			}
		}
		views = append(views, v)
	}
	// Hard excluded rows (no effort on Exclusion — provider/model only).
	for _, ex := range hard.Excluded {
		k := norm(ex.Provider, ex.Model)
		hasEligiblePM := false
		for _, v := range views {
			if norm(v.Provider, v.Model) == k && v.HardEligible {
				hasEligiblePM = true
				break
			}
		}
		if hasEligiblePM {
			continue
		}
		id := "ex|" + k
		if _, ok := seenID[id]; ok {
			continue
		}
		seenID[id] = struct{}{}
		views = append(views, CandidateView{
			Provider: ex.Provider, Model: ex.Model,
			HardEligible: false, Reasons: append([]string(nil), ex.Reasons...),
		})
	}
	// stable sort: hard eligible & not soft excluded first, then by adjusted score,
	// then by full identity (effort/permission) so depths never collapse.
	sort.SliceStable(views, func(i, j int) bool {
		iOK := views[i].HardEligible && !views[i].SoftExcluded
		jOK := views[j].HardEligible && !views[j].SoftExcluded
		if iOK != jOK {
			return iOK && !jOK
		}
		if views[i].AdjustedScore != views[j].AdjustedScore {
			return views[i].AdjustedScore > views[j].AdjustedScore
		}
		if views[i].Provider != views[j].Provider {
			return views[i].Provider < views[j].Provider
		}
		if views[i].Model != views[j].Model {
			return views[i].Model < views[j].Model
		}
		if views[i].Effort != views[j].Effort {
			return views[i].Effort < views[j].Effort
		}
		return views[i].Permission < views[j].Permission
	})
	return views
}

func norm(provider, model string) string {
	return strings.ToLower(strings.TrimSpace(provider)) + "/" + strings.TrimSpace(model)
}

// candIdentity is the durable decision-set identity including observed depth,
// permission, account, install, and window. Two installs of the same account
// must never collapse.
func candIdentity(provider, model, effort, permission, accountRef, installRef, windowKind string) string {
	return strings.ToLower(strings.TrimSpace(provider)) + "/" + strings.TrimSpace(model) +
		"|" + strings.ToLower(strings.TrimSpace(effort)) +
		"|" + strings.ToLower(strings.TrimSpace(permission)) +
		"|" + strings.TrimSpace(accountRef) +
		"|" + strings.TrimSpace(installRef) +
		"|" + strings.TrimSpace(windowKind)
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func digestDecision(d Decision) string {
	// Stable content digest: exclude DecisionID (store-local).
	type wire struct {
		Schema            string                  `json:"schema"`
		PolicyVersion     string                  `json:"policy_version"`
		DecisionKey       string                  `json:"decision_key"`
		ProjectID         string                  `json:"project_id"`
		EvidenceDigest    string                  `json:"evidence_digest"`
		Outcome           string                  `json:"outcome"`
		EligibilityDigest string                  `json:"eligibility_digest"`
		SoftRankingDigest string                  `json:"soft_ranking_digest,omitempty"`
		ModeRankingDigest string                  `json:"mode_ranking_digest,omitempty"`
		Winner            *Winner                 `json:"winner,omitempty"`
		Candidates        []CandidateView         `json:"candidates"`
		HardExcluded      []eligibility.Exclusion `json:"hard_excluded"`
		SoftExcluded      []string                `json:"soft_excluded,omitempty"`
		Reasons           []string                `json:"reasons"`
		PinFailClosed     bool                    `json:"pin_fail_closed,omitempty"`
		DecidedAt         time.Time               `json:"decided_at"`
	}
	w := wire{
		Schema: d.Schema, PolicyVersion: d.PolicyVersion,
		DecisionKey: d.DecisionKey, ProjectID: d.ProjectID,
		EvidenceDigest: d.EvidenceDigest, Outcome: d.Outcome,
		EligibilityDigest: d.EligibilityDigest,
		SoftRankingDigest: d.SoftRankingDigest, ModeRankingDigest: d.ModeRankingDigest,
		Winner: d.Winner, Candidates: d.Candidates, HardExcluded: d.HardExcluded,
		SoftExcluded: d.SoftExcluded, Reasons: d.Reasons,
		PinFailClosed: d.PinFailClosed, DecidedAt: d.DecidedAt,
	}
	b, err := json.Marshal(w)
	if err != nil {
		return "sha256:error"
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])[:24]
}
