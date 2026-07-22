package quotamode

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/capclass"
	"github.com/jasonhnd/loopcoder/internal/quotapolicy"
)

// RankInput freezes mode ranking inputs.
type RankInput struct {
	Mode       ModeConfig
	TaskClass  capclass.Class
	Candidates []quotapolicy.Candidate
	// Remaining optional remaining snapshots for reservation adjustment.
	Remaining []SnapshotRemaining
	Store     *Store
	Now       time.Time
	// ExplicitPin when set: modes must not substitute; pin stays selected.
	ExplicitPin *struct {
		Provider string
		Model    string
	}
}

// ModeRanking is a mode-adjusted ordered ranking.
type ModeRanking struct {
	Schema        string              `json:"schema"`
	PolicyVersion string              `json:"policy_version"`
	Mode          Mode                `json:"mode"`
	Base          quotapolicy.Ranking `json:"base"`
	Scores        []AdjustedScore     `json:"scores"`
	Reasons       []string            `json:"reasons"`
	Digest        string              `json:"digest"`
}

// AdjustedScore is one candidate after mode + reservation soft adjustment.
type AdjustedScore struct {
	Provider         string   `json:"provider"`
	Model            string   `json:"model"`
	BaseSoftScore    float64  `json:"base_soft_score"`
	AdjustedScore    float64  `json:"adjusted_score"`
	ReservedFraction float64  `json:"reserved_fraction"`
	SoftExcluded     bool     `json:"soft_excluded"`
	Reasons          []string `json:"reasons"`
}

// Rank applies mode weights and active reservations on top of quotapolicy.Rank.
// Explicit pins are never substituted by mode preference.
func Rank(in RankInput) (ModeRanking, error) {
	if !in.Mode.Mode.Valid() {
		return ModeRanking{}, fmt.Errorf("%w: mode", ErrInvalid)
	}
	if !in.TaskClass.Valid() {
		return ModeRanking{}, fmt.Errorf("%w: task_class", ErrInvalid)
	}
	now := in.Now.UTC()
	if now.IsZero() {
		return ModeRanking{}, fmt.Errorf("%w: now", ErrInvalid)
	}

	pol := quotapolicy.DefaultPolicy()
	switch in.Mode.Mode {
	case ModeBurnBeforeReset:
		pol.WeightBurnUrgency *= in.Mode.BurnBoost
	case ModePreservePremium:
		pol.WeightBurnUrgency *= in.Mode.BurnBoost
		pol.SoulReserveFraction = clamp01(pol.SoulReserveFraction + in.Mode.PreservePremiumFraction*0.5)
	case ModeBalanced:
		// defaults
	}

	base, err := quotapolicy.Rank(quotapolicy.Input{
		Policy:            pol,
		TaskClass:         in.TaskClass,
		Candidates:        in.Candidates,
		Now:               now,
		ExplicitPinActive: in.ExplicitPin != nil,
	})
	if err != nil {
		return ModeRanking{}, err
	}

	out := ModeRanking{
		Schema:        SchemaModeRank,
		PolicyVersion: PolicyVersion,
		Mode:          in.Mode.Mode,
		Base:          base,
	}

	if in.ExplicitPin != nil {
		out.Reasons = append(out.Reasons, "pin.active_mode_cannot_substitute")
		p := strings.ToLower(strings.TrimSpace(in.ExplicitPin.Provider))
		m := strings.TrimSpace(in.ExplicitPin.Model)
		for _, sc := range base.Scores {
			adj := AdjustedScore{
				Provider: sc.Provider, Model: sc.Model,
				BaseSoftScore: sc.SoftScore, AdjustedScore: sc.SoftScore,
				SoftExcluded: sc.SoftExcluded, Reasons: append([]string{}, sc.Reasons...),
			}
			if sc.Provider == p && sc.Model == m {
				adj.Reasons = append(adj.Reasons, "pin.selected")
				adj.AdjustedScore = 1.0
				adj.SoftExcluded = sc.SoftExcluded
			} else {
				adj.SoftExcluded = true
				adj.AdjustedScore = 0
				adj.Reasons = append(adj.Reasons, "pin.not_selected")
			}
			out.Scores = append(out.Scores, adj)
		}
		out.Digest = digestJSON(struct {
			Mode    Mode
			Scores  []AdjustedScore
			BaseDig string
			Reasons []string
		}{out.Mode, out.Scores, base.Digest, out.Reasons})
		return out, nil
	}

	for _, sc := range base.Scores {
		adj := AdjustedScore{
			Provider: sc.Provider, Model: sc.Model,
			BaseSoftScore: sc.SoftScore, AdjustedScore: sc.SoftScore,
			SoftExcluded: sc.SoftExcluded,
			Reasons:      append([]string{}, sc.Reasons...),
		}
		wk := WindowKey{Provider: sc.Provider, Model: sc.Model, Window: sc.BindingWindow}
		if wk.Window == "" {
			wk.Window = quotapolicy.WindowFiveHour
		}
		var reserved float64
		if in.Store != nil {
			reserved = in.Store.ActiveFraction(wk)
		}
		adj.ReservedFraction = reserved
		if reserved > 0 {
			penalty := clamp01(reserved)
			adj.AdjustedScore = clamp01(sc.SoftScore * (1 - 0.6*penalty))
			adj.Reasons = append(adj.Reasons, "reservation.active_pressure")
		}
		if in.Mode.Mode == ModePreservePremium && in.TaskClass.Rank() < capclass.ClassSoul.Rank() {
			if sc.BurnUrgency > 0.6 {
				adj.AdjustedScore *= 0.85
				adj.Reasons = append(adj.Reasons, "mode.preserve_premium")
			}
		}
		if in.Mode.Mode == ModeBurnBeforeReset && sc.BurnUrgency > 0.5 && !sc.SoftExcluded {
			adj.AdjustedScore = clamp01(adj.AdjustedScore * 1.05)
			adj.Reasons = append(adj.Reasons, "mode.burn_before_reset")
		}
		if in.Mode.Mode == ModeBalanced {
			adj.Reasons = append(adj.Reasons, "mode.balanced")
		}
		out.Scores = append(out.Scores, adj)
	}

	sort.SliceStable(out.Scores, func(i, j int) bool {
		if out.Scores[i].SoftExcluded != out.Scores[j].SoftExcluded {
			return !out.Scores[i].SoftExcluded && out.Scores[j].SoftExcluded
		}
		if out.Scores[i].AdjustedScore != out.Scores[j].AdjustedScore {
			return out.Scores[i].AdjustedScore > out.Scores[j].AdjustedScore
		}
		if out.Scores[i].Provider != out.Scores[j].Provider {
			return out.Scores[i].Provider < out.Scores[j].Provider
		}
		return out.Scores[i].Model < out.Scores[j].Model
	})

	out.Digest = digestJSON(struct {
		Mode    Mode
		Scores  []AdjustedScore
		BaseDig string
	}{out.Mode, out.Scores, base.Digest})
	return out, nil
}
