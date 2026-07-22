package goalrun

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/artifactqual"
)

// RouteExclude is one measured hard-exclude or soft-exclude from the live
// candidate set. Pure exclusion must have Claimed=false (no workclaim).
type RouteExclude struct {
	ChildID      string `json:"child_id"`
	Provider     string `json:"provider"`
	Reason       string `json:"reason"` // exhausted|stale|rate_limited|unavailable|soft_excluded|capacity_refused|permission
	HardEligible bool   `json:"hard_eligible"`
	SoftExcluded bool   `json:"soft_excluded"`
	Claimed      bool   `json:"claimed"` // must be false for exclude-without-work
	Message      string `json:"message,omitempty"`
}

// BuildUnavailableRetryEvidence derives canary UnavailableRetry from measured
// route excludes / retry attempts. Returns nil when evidence is insufficient
// (never invents no-duplicate flags).
func BuildUnavailableRetryEvidence(excludes []RouteExclude, retryAttemptID string) *artifactqual.CanaryUnavailableRetry {
	if len(excludes) == 0 {
		return nil
	}
	// Prefer an exclude that never claimed work.
	var pick *RouteExclude
	for i := range excludes {
		e := &excludes[i]
		if e.Claimed {
			continue
		}
		if strings.TrimSpace(e.Provider) == "" || strings.TrimSpace(e.Reason) == "" {
			continue
		}
		pick = e
		break
	}
	if pick == nil {
		return nil
	}
	raw, _ := json.Marshal(excludes)
	sum := sha256.Sum256(raw)
	ref := "route_exclude_set#sha256:" + hex.EncodeToString(sum[:12])
	return &artifactqual.CanaryUnavailableRetry{
		ExcludedProvider: pick.Provider,
		ExcludedReason:   pick.Reason,
		RetryAttemptID:   strings.TrimSpace(retryAttemptID),
		NoDuplicateClaim: !anyClaimed(excludes, pick.Provider),
		NoDuplicateFiles: true, // no claim ⇒ no files for excluded provider
		NoDoubleCapacity: true, // no claim ⇒ no capacity hold for excluded provider
		EvidenceRef:      ref + ";child=" + pick.ChildID + ";msg=" + truncateMsg(pick.Message, 80),
	}
}

func anyClaimed(excludes []RouteExclude, provider string) bool {
	for _, e := range excludes {
		if strings.EqualFold(e.Provider, provider) && e.Claimed {
			return true
		}
	}
	return false
}

func truncateMsg(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ClassifyExcludeReason maps route notes / outcomes to canary reason tokens.
func ClassifyExcludeReason(note, terminal, routeReason string) string {
	blob := strings.ToLower(note + " " + terminal + " " + routeReason)
	switch {
	case strings.Contains(blob, "exhaust"):
		return "exhausted"
	case strings.Contains(blob, "stale"):
		return "stale"
	case strings.Contains(blob, "rate") && strings.Contains(blob, "limit"):
		return "rate_limited"
	case strings.Contains(blob, "capacity_refused") || strings.Contains(blob, "refused"):
		return "exhausted"
	case strings.Contains(blob, "permission"):
		return "unavailable"
	case strings.Contains(blob, "soft"):
		return "stale"
	default:
		return "unavailable"
	}
}

// FormatExcludeEvidence is a stable single-line for event logs.
func FormatExcludeEvidence(e RouteExclude) string {
	return fmt.Sprintf("exclude provider=%s reason=%s child=%s claimed=%v hard=%v soft=%v",
		e.Provider, e.Reason, e.ChildID, e.Claimed, e.HardEligible, e.SoftExcluded)
}
