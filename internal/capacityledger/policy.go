package capacityledger

import (
	"strings"

	"github.com/jasonhnd/loopcoder/internal/quotamode"
)

// PolicyName is the owner-facing routing policy selector.
type PolicyName string

const (
	// PolicyUseBeforeReset is the default for the owner's product goal:
	// after quality floors, burn usable paid capacity before reset.
	PolicyUseBeforeReset PolicyName = "use-before-reset"
	// PolicyBalanced spreads soft preference across providers/windows.
	PolicyBalanced PolicyName = "balanced"
	// PolicyQualityFirst preserves premium capacity (preserve_premium mode).
	PolicyQualityFirst PolicyName = "quality-first"
)

// ParsePolicy maps user/config strings onto a policy. Empty → use-before-reset.
func ParsePolicy(s string) PolicyName {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "use-before-reset", "burn_before_reset", "burn-before-reset", "use_before_reset":
		return PolicyUseBeforeReset
	case "balanced":
		return PolicyBalanced
	case "quality-first", "quality_first", "preserve_premium", "preserve-premium":
		return PolicyQualityFirst
	default:
		return PolicyUseBeforeReset
	}
}

// ModeConfig returns the quotamode config for a policy.
func ModeConfig(p PolicyName) quotamode.ModeConfig {
	switch ParsePolicy(string(p)) {
	case PolicyBalanced:
		return quotamode.DefaultModeConfig(quotamode.ModeBalanced)
	case PolicyQualityFirst:
		return quotamode.DefaultModeConfig(quotamode.ModePreservePremium)
	default:
		return quotamode.DefaultModeConfig(quotamode.ModeBurnBeforeReset)
	}
}

// DefaultPolicy is the owner-goal default.
func DefaultPolicy() PolicyName { return PolicyUseBeforeReset }
