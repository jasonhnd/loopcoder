package artifactqual

import (
	"context"
	"strings"
)

// PRLiveState is authoritative live PR observation for real_pr_human_gate.
type PRLiveState struct {
	Repository          string   `json:"repository"` // owner/repo
	Number              int      `json:"number"`
	URL                 string   `json:"url"`
	BaseRef             string   `json:"base_ref"`
	HeadOID             string   `json:"head_oid"`
	State               string   `json:"state"` // open
	AutoMergeEnabled    bool     `json:"auto_merge_enabled"`
	RequiredChecks      []string `json:"required_checks,omitempty"`
	RequiredChecksGreen bool     `json:"required_checks_green"`
	// ChecksAtHead are conclusions at HeadOID (not stale branch tip).
	ChecksAtHead []PRCheck `json:"checks_at_head,omitempty"`
	// HumanMergeGate is true when merge is not automated.
	HumanMergeGate bool `json:"human_merge_gate"`
}

// PRCheck is one required check at a head OID.
type PRCheck struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// PRLiveVerifier fetches authoritative PR state (production: GitHub API).
type PRLiveVerifier interface {
	FetchPR(ctx context.Context, repository string, number int) (PRLiveState, error)
}

// ValidatePRLive checks canary PR section against live state and expected head.
// Manifest booleans alone cannot green real_pr_human_gate.
// All failure reasons are stable IDs (never untrusted check names or raw values).
func ValidatePRLive(pr CanaryPR, live *PRLiveState, expectHeadOID string) (ok bool, reasons []string) {
	add := func(s string) { reasons = append(reasons, s) }

	// --- Manifest requirements ---
	if strings.TrimSpace(pr.URL) == "" || !strings.Contains(pr.URL, "/pull/") {
		add("pr_url_invalid")
	}
	if pr.Number <= 0 {
		add("pr_number_missing")
	}
	if strings.TrimSpace(pr.Repository) == "" {
		add("pr_repository_missing")
	}
	if strings.TrimSpace(pr.BaseRef) == "" {
		add("pr_base_ref_missing")
	}
	if !isExact40Hex(strings.TrimSpace(pr.HeadOID)) {
		add("pr_head_oid_invalid")
	}
	if pr.AutoMerge {
		add("pr_auto_merge_true")
	}
	if !pr.HumanMergeGate {
		add("pr_human_merge_gate_false")
	}
	if !pr.CreatedByLoopCoder {
		add("pr_not_loopcoder_owned")
	}
	if strings.TrimSpace(pr.VerifierProvider) == "" && strings.TrimSpace(pr.IndependentVerifier) == "" {
		add("pr_verifier_provider_missing")
	}
	if strings.TrimSpace(pr.VerifierAttemptID) == "" {
		add("pr_verifier_attempt_missing")
	}
	if strings.TrimSpace(pr.VerifierEvidenceRef) == "" {
		add("pr_verifier_evidence_missing")
	} else if strings.Contains(strings.ToLower(pr.VerifierEvidenceRef), "pending") {
		add("pr_verifier_pending_live")
	}
	if len(pr.RequiredChecks) == 0 {
		add("pr_required_checks_missing")
	}

	// --- Expected head (mandatory; never empty; never defaulted here) ---
	expect := strings.TrimSpace(expectHeadOID)
	if !isExact40Hex(expect) {
		add("pr_expected_head_oid_invalid")
	} else {
		if isExact40Hex(strings.TrimSpace(pr.HeadOID)) &&
			!strings.EqualFold(strings.TrimSpace(pr.HeadOID), expect) {
			add("pr_manifest_head_not_expected")
		}
	}

	// Live state required — cannot green from manifest booleans alone.
	if live == nil {
		add("pr_live_state_missing")
		return false, reasons
	}

	// --- Live requirements ---
	if !strings.EqualFold(strings.TrimSpace(live.Repository), strings.TrimSpace(pr.Repository)) {
		add("pr_live_repository_mismatch")
	}
	if live.Number != pr.Number {
		add("pr_live_number_mismatch")
	}
	if strings.TrimSpace(live.URL) != strings.TrimSpace(pr.URL) {
		add("pr_live_url_mismatch")
	}
	if strings.TrimSpace(live.BaseRef) != strings.TrimSpace(pr.BaseRef) {
		add("pr_live_base_ref_mismatch")
	}
	if !isExact40Hex(strings.TrimSpace(live.HeadOID)) {
		add("pr_live_head_oid_invalid")
	} else if isExact40Hex(expect) && !strings.EqualFold(strings.TrimSpace(live.HeadOID), expect) {
		add("pr_live_head_not_expected")
	}
	if isExact40Hex(strings.TrimSpace(pr.HeadOID)) && isExact40Hex(strings.TrimSpace(live.HeadOID)) &&
		!strings.EqualFold(strings.TrimSpace(live.HeadOID), strings.TrimSpace(pr.HeadOID)) {
		add("pr_live_head_oid_mismatch_manifest")
	}
	if strings.TrimSpace(live.State) != "open" {
		add("pr_live_not_open")
	}
	if live.AutoMergeEnabled {
		add("pr_live_auto_merge_enabled")
	}
	if !live.HumanMergeGate {
		add("pr_live_not_human_gate")
	}
	if !live.RequiredChecksGreen {
		add("pr_live_required_checks_not_green")
	}
	if len(live.ChecksAtHead) == 0 {
		add("pr_live_checks_at_head_missing")
	}

	// Every manifest required check name must be present+green at head.
	// Reasons are stable IDs only — never append check names.
	byName := map[string]PRCheck{}
	for _, c := range live.ChecksAtHead {
		name := strings.TrimSpace(c.Name)
		if name == "" {
			continue
		}
		byName[strings.ToLower(name)] = c
	}
	for _, wantName := range pr.RequiredChecks {
		key := strings.ToLower(strings.TrimSpace(wantName))
		if key == "" {
			add("pr_required_check_name_empty")
			continue
		}
		c, ok := byName[key]
		if !ok {
			add("pr_live_required_check_missing")
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(c.Status), "completed") ||
			!strings.EqualFold(strings.TrimSpace(c.Conclusion), "success") {
			add("pr_live_required_check_not_green")
		}
	}

	ok = len(reasons) == 0
	return ok, reasons
}
