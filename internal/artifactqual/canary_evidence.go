package artifactqual

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// SchemaCanaryEvidence is the exact-binary real canary evidence package for #1343.
// Dry-run / --capacity-snapshot structural prechecks MUST NOT satisfy this schema.
const SchemaCanaryEvidence = "loopcoder.canary_evidence.v1"

// CanaryEvidence is produced by an exact downloaded binary during a live canary.
// Cross-run reuse is rejected by binding archive_digest + pre_prod_sha + unique
// project/run/attempt identities and a fresh produced_at window.
type CanaryEvidence struct {
	Schema        string `json:"schema"`
	ArchiveDigest string `json:"archive_digest"`
	PreProdSHA    string `json:"pre_prod_sha"`
	// Binary identity that wrote this manifest (must match extracted qualify binary).
	BinaryVersion string `json:"binary_version"`
	BinaryCommit  string `json:"binary_commit"`
	// Durable isolation
	ProjectID string `json:"project_id"`
	RunID     string `json:"run_id"`
	// Fresh provider quota observations (source-tagged).
	ProviderObservations []CanaryProviderObs `json:"provider_observations"`
	// Real child executions (not dry-run plan rows).
	Children []CanaryChild `json:"children"`
	// Unavailable/stale/rate-limit exclude or new attempt without duplicate work.
	UnavailableRetry *CanaryUnavailableRetry `json:"unavailable_retry,omitempty"`
	// Forced interrupt + durable recover + ceilings.
	Restart *CanaryRestart `json:"restart,omitempty"`
	// Real PR human gate (not status=human_gate alone).
	PR *CanaryPR `json:"pr,omitempty"`
	// Manifest integrity
	ProducedAt time.Time `json:"produced_at"`
	// Optional content digest of the rest of the body for anti-tamper.
	ContentDigest string `json:"content_digest,omitempty"`
}

// CanaryProviderObs is one fresh capacity observation.
type CanaryProviderObs struct {
	Provider   string    `json:"provider"`
	AccountRef string    `json:"account_ref,omitempty"`
	Source     string    `json:"source"`
	Freshness  string    `json:"freshness"`
	Confidence string    `json:"confidence,omitempty"`
	Remaining  *float64  `json:"remaining,omitempty"`
	CapturedAt time.Time `json:"captured_at"`
}

// CanaryChild is one real provider-executed child.
type CanaryChild struct {
	ChildID              string   `json:"child_id"`
	AttemptID            string   `json:"attempt_id"`
	Provider             string   `json:"provider"`
	Model                string   `json:"model"`
	DepthRequired        string   `json:"depth_required"`
	DepthSelected        string   `json:"depth_selected"`
	DepthInvocation      string   `json:"depth_invocation"`
	Permission           string   `json:"permission,omitempty"`
	Terminal             string   `json:"terminal"`
	WorktreePath         string   `json:"worktree_path,omitempty"`
	CapacityBefore       *float64 `json:"capacity_before,omitempty"`
	CapacityReserved     *float64 `json:"capacity_reserved,omitempty"`
	CapacityActual       *float64 `json:"capacity_actual,omitempty"` // may be nil/unknown
	CapacityAfter        *float64 `json:"capacity_after,omitempty"`  // required when observed
	ActualSource         string   `json:"actual_source,omitempty"`
	AfterSource          string   `json:"after_source,omitempty"`
	AfterFreshness       string   `json:"after_freshness,omitempty"`
	RealProviderExecuted bool     `json:"real_provider_executed"`
}

// CanaryUnavailableRetry proves exclude/retry without duplicate claim/output.
type CanaryUnavailableRetry struct {
	ExcludedProvider string `json:"excluded_provider"`
	ExcludedReason   string `json:"excluded_reason"` // exhausted|stale|rate_limited|unavailable
	RetryAttemptID   string `json:"retry_attempt_id,omitempty"`
	NoDuplicateClaim bool   `json:"no_duplicate_claim"`
	NoDuplicateFiles bool   `json:"no_duplicate_files"`
	NoDoubleCapacity bool   `json:"no_double_capacity"`
	EvidenceRef      string `json:"evidence_ref,omitempty"`
}

// CanaryRestart proves forced interrupt + recover + ceilings.
type CanaryRestart struct {
	Interrupted        bool   `json:"interrupted"`
	ResumedFromDurable bool   `json:"resumed_from_durable"`
	ExactlyOnce        bool   `json:"exactly_once"`
	ChildCountUseful   int    `json:"child_count_useful"`
	ProcessCeilingOK   bool   `json:"process_ceiling_ok"`
	WorktreeCeilingOK  bool   `json:"worktree_ceiling_ok"`
	NoLeakedProcesses  bool   `json:"no_leaked_processes"`
	NoRepoLocalRuntime bool   `json:"no_repo_local_runtime"`
	EvidenceRef        string `json:"evidence_ref,omitempty"`
}

// CanaryPR is a real GitHub PR human merge gate.
type CanaryPR struct {
	URL                 string   `json:"url"`
	Branch              string   `json:"branch,omitempty"`
	Number              int      `json:"number,omitempty"`
	RequiredChecks      []string `json:"required_checks,omitempty"`
	RequiredChecksGreen bool     `json:"required_checks_green"`
	IndependentVerifier string   `json:"independent_verifier,omitempty"`
	VerifierEvidenceRef string   `json:"verifier_evidence_ref,omitempty"`
	CreatedByLoopCoder  bool     `json:"created_by_loopcoder"`
}

// CanaryValidation is the scored result of loading a canary evidence manifest.
type CanaryValidation struct {
	Present            bool
	Valid              bool
	Reasons            []string
	MultiDepthOK       bool
	MultiProviderOK    bool
	CapacityAfterOK    bool
	UnavailableRetryOK bool
	RestartOK          bool
	RealPROK           bool
	UsefulChildren     int
	Providers          []string
	Depths             []string
	ProjectID          string
	RunID              string
	EvidencePath       string
}

// LoadCanaryEvidence reads and schema-validates a canary evidence JSON file.
func LoadCanaryEvidence(path string) (CanaryEvidence, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return CanaryEvidence{}, err
	}
	var ev CanaryEvidence
	if err := json.Unmarshal(raw, &ev); err != nil {
		return CanaryEvidence{}, fmt.Errorf("canary evidence json: %w", err)
	}
	if strings.TrimSpace(ev.Schema) != SchemaCanaryEvidence {
		return CanaryEvidence{}, fmt.Errorf("canary evidence schema %q want %q", ev.Schema, SchemaCanaryEvidence)
	}
	return ev, nil
}

// ValidateCanaryEvidence binds a live canary manifest to this qualify archive+SHA.
// Dry-run structural prechecks must never call this with synthetic green data.
func ValidateCanaryEvidence(ev CanaryEvidence, archiveDigest, preProdSHA string, now time.Time) CanaryValidation {
	v := CanaryValidation{Present: true}
	add := func(r string) { v.Reasons = append(v.Reasons, r) }

	if strings.TrimSpace(ev.Schema) != SchemaCanaryEvidence {
		add("schema_mismatch")
		return v
	}
	if normHex(ev.ArchiveDigest) == "" || normHex(ev.ArchiveDigest) != normHex(archiveDigest) {
		add("archive_digest_mismatch")
	}
	wantSHA := strings.TrimSpace(preProdSHA)
	gotSHA := strings.TrimSpace(ev.PreProdSHA)
	if gotSHA == "" {
		add("pre_prod_sha_missing")
	} else if wantSHA != "" && gotSHA != wantSHA &&
		!strings.HasPrefix(wantSHA, gotSHA) && !strings.HasPrefix(gotSHA, wantSHA) {
		add("pre_prod_sha_mismatch")
	}
	if strings.TrimSpace(ev.ProjectID) == "" || ev.ProjectID == "local-project" {
		add("project_id_not_unique_disposable")
	}
	if strings.TrimSpace(ev.RunID) == "" {
		add("run_id_missing")
	}
	if ev.ProducedAt.IsZero() {
		add("produced_at_missing")
	} else if now.Sub(ev.ProducedAt) > 7*24*time.Hour || ev.ProducedAt.After(now.Add(time.Hour)) {
		// Reject ancient/stale or future-skewed manifests (cross-run reuse guard).
		add("produced_at_stale_or_skewed")
	}
	if strings.TrimSpace(ev.BinaryVersion) == "" && strings.TrimSpace(ev.BinaryCommit) == "" {
		add("binary_identity_missing")
	}

	// Fresh provider observations (≥2 companies).
	provSeen := map[string]bool{}
	freshObs := 0
	for _, o := range ev.ProviderObservations {
		p := strings.ToLower(strings.TrimSpace(o.Provider))
		if p == "" {
			continue
		}
		provSeen[p] = true
		fr := strings.ToLower(strings.TrimSpace(o.Freshness))
		if fr == "fresh" && strings.TrimSpace(o.Source) != "" {
			freshObs++
		}
	}
	if len(provSeen) < 2 {
		add("providers_lt_2")
	}
	if freshObs < 2 {
		add("fresh_provider_observations_lt_2")
	}

	// Real children
	depths := map[string]bool{}
	childProv := map[string]bool{}
	useful := 0
	afterOK := 0
	depthBindOK := 0
	attemptIDs := map[string]bool{}
	for _, c := range ev.Children {
		if strings.TrimSpace(c.AttemptID) == "" {
			add("child_attempt_id_missing:" + c.ChildID)
			continue
		}
		if attemptIDs[c.AttemptID] {
			add("duplicate_attempt_id:" + c.AttemptID)
		}
		attemptIDs[c.AttemptID] = true
		if !c.RealProviderExecuted {
			add("child_not_real_provider:" + c.ChildID)
			continue
		}
		if c.Terminal != "succeeded" {
			// still count for diversity if executed
		} else {
			useful++
		}
		if p := strings.ToLower(strings.TrimSpace(c.Provider)); p != "" {
			childProv[p] = true
		}
		req := strings.ToLower(strings.TrimSpace(c.DepthRequired))
		sel := strings.ToLower(strings.TrimSpace(c.DepthSelected))
		inv := strings.ToLower(strings.TrimSpace(c.DepthInvocation))
		if req != "" {
			depths[req] = true
		}
		if req != "" && req == sel && req == inv {
			depthBindOK++
		}
		if c.CapacityBefore == nil || c.CapacityReserved == nil {
			add("capacity_before_or_reserved_missing:" + c.ChildID)
		}
		// actual may be unknown/nil; after must be present from fresh observation
		if c.CapacityAfter == nil {
			add("capacity_after_missing:" + c.ChildID)
		} else {
			afterOK++
			if strings.TrimSpace(c.AfterSource) == "" {
				add("after_source_missing:" + c.ChildID)
			}
			if strings.TrimSpace(c.AfterFreshness) == "" {
				add("after_freshness_missing:" + c.ChildID)
			}
		}
		// Reject dry-run markers
		if strings.Contains(strings.ToLower(c.AttemptID), "dry") ||
			strings.Contains(strings.ToLower(c.WorktreePath), "dry-run") {
			add("dry_run_child_not_allowed:" + c.ChildID)
		}
	}
	v.UsefulChildren = useful
	for p := range childProv {
		v.Providers = append(v.Providers, p)
	}
	for d := range depths {
		v.Depths = append(v.Depths, d)
	}
	if useful < 4 {
		add("useful_children_lt_4")
	}
	if len(childProv) < 2 {
		add("executed_providers_lt_2")
	}
	// multi-depth: at least 2 distinct depths with real bind
	if len(depths) < 2 || depthBindOK < 2 {
		add("multi_depth_runtime_unmet")
	} else {
		v.MultiDepthOK = true
	}
	v.MultiProviderOK = len(childProv) >= 2 && useful >= 4
	if afterOK >= 4 && !hasReasonPrefix(v.Reasons, "capacity_after_missing") {
		v.CapacityAfterOK = true
	} else if afterOK >= useful && useful >= 4 {
		v.CapacityAfterOK = true
	}

	// Unavailable retry
	if ev.UnavailableRetry == nil {
		add("unavailable_retry_missing")
	} else {
		u := ev.UnavailableRetry
		if strings.TrimSpace(u.ExcludedProvider) == "" || strings.TrimSpace(u.ExcludedReason) == "" {
			add("unavailable_retry_incomplete")
		} else if !u.NoDuplicateClaim || !u.NoDuplicateFiles || !u.NoDoubleCapacity {
			add("unavailable_retry_dup_flags_false")
		} else {
			v.UnavailableRetryOK = true
		}
	}

	// Restart / ceilings
	if ev.Restart == nil {
		add("restart_evidence_missing")
	} else {
		r := ev.Restart
		if !r.Interrupted || !r.ResumedFromDurable || !r.ExactlyOnce {
			add("restart_flags_incomplete")
		} else if r.ChildCountUseful < 4 {
			add("restart_useful_children_lt_4")
		} else if !r.ProcessCeilingOK || !r.WorktreeCeilingOK || !r.NoLeakedProcesses || !r.NoRepoLocalRuntime {
			add("restart_ceilings_or_leaks_unmet")
		} else {
			v.RestartOK = true
		}
	}

	// Real PR
	if ev.PR == nil {
		add("real_pr_missing")
	} else {
		p := ev.PR
		if strings.TrimSpace(p.URL) == "" || !strings.Contains(p.URL, "/pull/") {
			add("real_pr_url_invalid")
		} else if !p.CreatedByLoopCoder {
			add("real_pr_not_loopcoder_owned")
		} else if !p.RequiredChecksGreen || len(p.RequiredChecks) == 0 {
			add("real_pr_checks_unmet")
		} else if strings.TrimSpace(p.IndependentVerifier) == "" && strings.TrimSpace(p.VerifierEvidenceRef) == "" {
			add("real_pr_verifier_missing")
		} else {
			v.RealPROK = true
		}
	}

	v.ProjectID = ev.ProjectID
	v.RunID = ev.RunID
	v.Valid = len(v.Reasons) == 0
	if !v.Valid && len(v.Reasons) > 0 {
		// still allow partial metric scoring from flags above
	}
	return v
}

func normHex(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	s = strings.TrimPrefix(s, "sha256:")
	return s
}

func hasReasonPrefix(reasons []string, prefix string) bool {
	for _, r := range reasons {
		if strings.HasPrefix(r, prefix) {
			return true
		}
	}
	return false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// DigestCanaryBody hashes a stable subset for optional anti-tamper.
func DigestCanaryBody(ev CanaryEvidence) string {
	// Clear content digest field before hashing if present.
	cp := ev
	cp.ContentDigest = ""
	b, _ := json.Marshal(cp)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
