package artifactqual

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/capacityledger"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
	"github.com/jasonhnd/loopcoder/internal/workclaim"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
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
	// Inventory provenance and digest bind every provider observation to the
	// exact live report used by the product run.
	InventoryProvenance   string `json:"inventory_provenance"`
	InventoryReportDigest string `json:"inventory_report_digest"`
	// Fresh provider quota observations (source-tagged).
	ProviderObservations []CanaryProviderObs `json:"provider_observations"`
	// ClaudeCatalogReceipts bind account-scoped paid model/depth capability
	// probes to the exact inventory digest used by this canary.
	ClaudeCatalogReceipts []CanaryClaudeCatalogReceipt `json:"claude_catalog_receipts,omitempty"`
	// Real child executions (not dry-run plan rows).
	Children []CanaryChild `json:"children"`
	// Unavailable/stale/rate-limit exclude or new attempt without duplicate work.
	UnavailableRetry *CanaryUnavailableRetry `json:"unavailable_retry,omitempty"`
	// Forced interrupt + durable recover + ceilings.
	Restart *CanaryRestart `json:"restart,omitempty"`
	// Real PR human gate (not status=human_gate alone).
	PR *CanaryPR `json:"pr,omitempty"`
	// Raw durable evidence is included so qualification recomputes unavailable
	// retry, forced restart, and capacity accounting without trusting summary
	// booleans or EvidenceRef strings.
	RawEvents             []workflowrun.Event    `json:"raw_events"`
	RawClaims             []workclaim.Claim      `json:"raw_claims"`
	RawLedgerEntries      []capacityledger.Entry `json:"raw_ledger_entries"`
	DurableEvidenceDigest string                 `json:"durable_evidence_digest"`
	// Manifest integrity
	ProducedAt time.Time `json:"produced_at"`
	// Optional content digest of the rest of the body for anti-tamper.
	ContentDigest string `json:"content_digest,omitempty"`
}

type CanaryClaudeCatalogReceipt struct {
	InventoryReportDigest string                                         `json:"inventory_report_digest"`
	Receipt               providerinventory.ClaudeCapabilityProbeReceipt `json:"receipt"`
}

// CanaryProviderObs is one fresh capacity observation from real structured
// before/after evidence — never time.Now / invented "fresh" labels.
type CanaryProviderObs struct {
	Provider   string    `json:"provider"`
	AccountRef string    `json:"account_ref,omitempty"`
	InstallRef string    `json:"install_ref,omitempty"`
	WindowKind string    `json:"window_kind,omitempty"`
	Source     string    `json:"source"`
	Freshness  string    `json:"freshness"`
	Confidence string    `json:"confidence,omitempty"`
	Remaining  *float64  `json:"remaining,omitempty"`
	CapturedAt time.Time `json:"captured_at"`
	// ResetAt is exact window reset identity for finite/fixed windows (UTC).
	ResetAt               *time.Time `json:"reset_at,omitempty"`
	InventoryReportDigest string     `json:"inventory_report_digest"`
}

// CanaryChild is one real provider-executed child.
type CanaryChild struct {
	// ChildID remains for report compatibility; WorkItemID is the structured work kind.
	ChildID    string `json:"child_id"`
	WorkItemID string `json:"work_item_id,omitempty"`
	TaskClass  string `json:"task_class,omitempty"`
	// OutputEvidence is the durable product/output digest from the child outcome.
	OutputEvidence   string   `json:"output_evidence,omitempty"`
	AttemptID        string   `json:"attempt_id"`
	Provider         string   `json:"provider"`
	Model            string   `json:"model"`
	DepthRequired    string   `json:"depth_required"`
	DepthSelected    string   `json:"depth_selected"`
	DepthInvocation  string   `json:"depth_invocation"`
	Permission       string   `json:"permission,omitempty"`
	AccountRef       string   `json:"account_ref,omitempty"`
	InstallRef       string   `json:"install_ref,omitempty"`
	WindowKind       string   `json:"window_kind,omitempty"`
	Terminal         string   `json:"terminal"`
	WorktreePath     string   `json:"worktree_path,omitempty"`
	FilesTouched     []string `json:"files_touched,omitempty"`
	CapacityBefore   *float64 `json:"capacity_before,omitempty"`
	CapacityReserved *float64 `json:"capacity_reserved,omitempty"`
	CapacityActual   *float64 `json:"capacity_actual,omitempty"` // quota-window fraction only
	CapacityAfter    *float64 `json:"capacity_after,omitempty"`  // required when observed
	// ActualSource / ActualConfidence for quota-window Actual (same_window_delta).
	ActualSource     string `json:"actual_source,omitempty"`
	ActualConfidence string `json:"actual_confidence,omitempty"`
	// Structured after evidence (never defaulted from CapacityNote prose).
	AfterSource     string    `json:"after_source,omitempty"`
	AfterFreshness  string    `json:"after_freshness,omitempty"`
	AfterConfidence string    `json:"after_confidence,omitempty"`
	AfterState      string    `json:"after_state,omitempty"` // observed|derived
	AfterObservedAt time.Time `json:"after_observed_at,omitempty"`
	// Structured before evidence at reserve.
	BeforeSource     string    `json:"before_source,omitempty"`
	BeforeFreshness  string    `json:"before_freshness,omitempty"`
	BeforeConfidence string    `json:"before_confidence,omitempty"`
	BeforeCapturedAt time.Time `json:"before_captured_at,omitempty"`
	// ResetAt is exact window reset identity for finite/fixed windows (UTC).
	// Required for capacity_after_runtime when window is not unbounded/non-reset.
	ResetAt *time.Time `json:"reset_at,omitempty"`
	// ActualSources is per-dimension route proof; required for real provider rows.
	// Erasure or forgery of any required dimension must fail qualification.
	ActualSources *CanaryRouteSources `json:"actual_sources,omitempty"`
	// ArgvDigest is redacted exact launched argv fingerprint.
	ArgvDigest           string `json:"argv_digest,omitempty"`
	RealProviderExecuted bool   `json:"real_provider_executed"`
}

// CanaryRouteSources is per-dimension route Actual* proof class.
type CanaryRouteSources struct {
	Model      string `json:"model,omitempty"`
	Effort     string `json:"effort,omitempty"`
	Permission string `json:"permission,omitempty"`
	Account    string `json:"account,omitempty"`
	Install    string `json:"install,omitempty"`
}

// CanaryUnavailableRetry proves exclude/retry without duplicate claim/output.
type CanaryUnavailableRetry struct {
	ExcludedProvider string `json:"excluded_provider"`
	// ExcludedReason must be a real unavailability class only:
	// exhausted|stale|rate_limited|unavailable|soft_excluded|model_unavailable.
	// eligible_not_chosen is multi-provider diversity, not unavailability.
	ExcludedReason   string `json:"excluded_reason"`
	RetryAttemptID   string `json:"retry_attempt_id,omitempty"`
	NoDuplicateClaim bool   `json:"no_duplicate_claim"`
	NoDuplicateFiles bool   `json:"no_duplicate_files"`
	NoDoubleCapacity bool   `json:"no_double_capacity"`
	EvidenceRef      string `json:"evidence_ref,omitempty"`
}

// ProductionSequentialCeiling is the production process/worktree peak limit for
// sequential wave execution (workflowrun runs wave members sequentially).
const ProductionSequentialCeiling = 1

// CanaryRestart proves forced interrupt + recover + ceilings.
// Precomputed *OK flags must match recomputation from measured fields; validators
// never trust flags alone.
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
	// Measured transparency (validator recomputes OK flags from these).
	ProcessPeak               int  `json:"process_peak"`
	WorktreePeak              int  `json:"worktree_peak"`
	ProcessActive             int  `json:"process_active"`
	WorktreeActive            int  `json:"worktree_active"`
	ProcessLimit              int  `json:"process_limit"`
	WorktreeLimit             int  `json:"worktree_limit"`
	ReuseCountMeasured        int  `json:"reuse_count_measured"`
	AbortedAttemptCount       int  `json:"aborted_attempt_count"`
	ActiveOccupancyMeasured   bool `json:"active_occupancy_measured"`
	RepoLocalRuntimeChecked   bool `json:"repo_local_runtime_checked"`
	RepoLocalRuntimePresent   bool `json:"repo_local_runtime_present"`
	DuplicateLaunch           bool `json:"duplicate_launch"`
	DuplicateSuccessIntegrate bool `json:"duplicate_success_integrate"`
	AbortedAttemptSucceeded   bool `json:"aborted_attempt_succeeded"`
	// LaterGenerationResume is true when a higher-generation launch exists after
	// a typed forced-interrupt cancelled abort (production resume sequence).
	LaterGenerationResume bool `json:"later_generation_resume"`
}

// CanaryPR is a real GitHub PR human merge gate.
// Live qualification verifies URL/number/head_oid/checks; manifest booleans alone
// cannot green real_pr_human_gate.
type CanaryPR struct {
	URL                 string   `json:"url"`
	Repository          string   `json:"repository,omitempty"` // owner/repo
	Branch              string   `json:"branch,omitempty"`
	Number              int      `json:"number,omitempty"`
	BaseRef             string   `json:"base_ref,omitempty"`
	HeadOID             string   `json:"head_oid,omitempty"`
	RequiredChecks      []string `json:"required_checks,omitempty"`
	RequiredChecksGreen bool     `json:"required_checks_green"`
	IndependentVerifier string   `json:"independent_verifier,omitempty"`
	VerifierEvidenceRef string   `json:"verifier_evidence_ref,omitempty"`
	VerifierProvider    string   `json:"verifier_provider,omitempty"`
	VerifierAttemptID   string   `json:"verifier_attempt_id,omitempty"`
	CreatedByLoopCoder  bool     `json:"created_by_loopcoder"`
	// AutoMerge must be false (human gate).
	AutoMerge bool `json:"auto_merge"`
	// HumanMergeGate must be true.
	HumanMergeGate bool `json:"human_merge_gate"`
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
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ev); err != nil {
		return CanaryEvidence{}, fmt.Errorf("canary evidence json: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return CanaryEvidence{}, fmt.Errorf("canary evidence json: %w", err)
	}
	if strings.TrimSpace(ev.Schema) != SchemaCanaryEvidence {
		return CanaryEvidence{}, fmt.Errorf("canary evidence schema %q want %q", ev.Schema, SchemaCanaryEvidence)
	}
	return ev, nil
}

// CanaryValidateOpts binds operator-expected one-run identity so a prior
// canary manifest cannot be reused for another project/run.
// LivePR (when set) is required to green real_pr_human_gate — manifest booleans alone cannot.
type CanaryValidateOpts struct {
	ExpectedProjectID string
	ExpectedRunID     string
	// ExpectedPRHeadOID is the delivered canary commit that PR head must equal.
	ExpectedPRHeadOID string
	// LivePR is authoritative PR observation; nil → real_pr cannot GO.
	LivePR *PRLiveState
}

// ValidateCanaryEvidence binds a live canary manifest to this qualify archive+SHA.
// Dry-run structural prechecks must never call this with synthetic green data.
// Optional opts: ExpectedProjectID/ExpectedRunID for anti-reuse challenge.
func ValidateCanaryEvidence(ev CanaryEvidence, archiveDigest, preProdSHA string, now time.Time, opts ...CanaryValidateOpts) CanaryValidation {
	v := CanaryValidation{Present: true}
	add := func(r string) { v.Reasons = append(v.Reasons, r) }
	var expect CanaryValidateOpts
	if len(opts) > 0 {
		expect = opts[0]
	}

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
	} else if wantSHA != "" && gotSHA != wantSHA {
		// Exact match only — no prefix fuzzy reuse.
		add("pre_prod_sha_mismatch")
	}
	// BinaryCommit must exactly match PreProdSHA (exact binary identity).
	binCommit := strings.TrimSpace(ev.BinaryCommit)
	if binCommit == "" {
		add("binary_commit_missing")
	} else if wantSHA != "" && binCommit != wantSHA {
		add("binary_commit_pre_prod_sha_mismatch")
	} else if gotSHA != "" && binCommit != gotSHA {
		add("binary_commit_pre_prod_sha_mismatch")
	}
	if strings.TrimSpace(ev.ProjectID) == "" || ev.ProjectID == "local-project" {
		add("project_id_not_unique_disposable")
	}
	if strings.TrimSpace(ev.RunID) == "" {
		add("run_id_missing")
	}
	if ev.InventoryProvenance != "live_discover" {
		add("inventory_provenance_not_live_discover")
	}
	if ev.InventoryReportDigest == "" || ev.InventoryReportDigest != strings.TrimSpace(ev.InventoryReportDigest) {
		add("inventory_report_digest_missing_or_noncanonical")
	} else {
		bound := false
		for _, observation := range ev.ProviderObservations {
			if observation.InventoryReportDigest == ev.InventoryReportDigest {
				bound = true
				break
			}
		}
		if !bound {
			add("inventory_report_digest_unbound")
		}
	}
	if exp := strings.TrimSpace(expect.ExpectedProjectID); exp != "" && exp != strings.TrimSpace(ev.ProjectID) {
		add("expected_project_id_mismatch")
	}
	if exp := strings.TrimSpace(expect.ExpectedRunID); exp != "" && exp != strings.TrimSpace(ev.RunID) {
		add("expected_run_id_mismatch")
	}
	if ev.ProducedAt.IsZero() {
		add("produced_at_missing")
	} else if now.Sub(ev.ProducedAt) > 2*time.Hour || ev.ProducedAt.After(now.Add(15*time.Minute)) {
		// Tighten anti-reuse: canary must be from the current qualify window.
		add("produced_at_stale_or_skewed")
	}
	if strings.TrimSpace(ev.BinaryVersion) == "" && strings.TrimSpace(ev.BinaryCommit) == "" {
		add("binary_identity_missing")
	}
	// ContentDigest is required and must match recomputation of the body.
	if strings.TrimSpace(ev.ContentDigest) == "" {
		add("content_digest_missing")
	} else {
		want := DigestCanaryBody(ev)
		if !strings.EqualFold(normHex(ev.ContentDigest), normHex(want)) {
			add("content_digest_mismatch")
		}
	}
	if ev.DurableEvidenceDigest == "" {
		add("durable_evidence_digest_missing")
	} else if normHex(ev.DurableEvidenceDigest) != normHex(DigestDurableEvidence(ev)) {
		add("durable_evidence_digest_mismatch")
	}
	for _, reason := range validateRawDurableEnvelope(ev) {
		add(reason)
	}

	// Build counted real-child identity keys for provider-obs correspondence.
	type childIDKey struct{ p, acc, inst, win string }
	realChildIDs := map[childIDKey]bool{}
	validClaudeReceipts := map[string]bool{}
	for _, wrapped := range ev.ClaudeCatalogReceipts {
		receipt := wrapped.Receipt
		key := strings.Join([]string{receipt.AccountProfileID, receipt.ProviderInstallationID, receipt.ActualModel, receipt.AcceptedEffort}, "\x00")
		if validClaudeReceipts[key] {
			add("claude_catalog_receipt_duplicate")
			continue
		}
		if wrapped.InventoryReportDigest == "" || wrapped.InventoryReportDigest != ev.InventoryReportDigest {
			add("claude_catalog_receipt_inventory_digest_mismatch")
			continue
		}
		if err := providerinventory.ValidateClaudeCapabilityProbeReceipt(receipt, ev.ProducedAt); err != nil {
			add("claude_catalog_receipt_invalid")
			continue
		}
		validClaudeReceipts[key] = true
	}

	// Real children first (so provider obs can require correspondence).
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
			continue
		}
		if c.Terminal != "succeeded" {
			add("real_provider_not_succeeded:" + c.ChildID)
			continue
		}
		// Identity required for production canary children.
		if canonicalProviderCompany(c.Provider) == "" ||
			c.AccountRef == "" || c.AccountRef != strings.TrimSpace(c.AccountRef) ||
			c.InstallRef == "" || c.InstallRef != strings.TrimSpace(c.InstallRef) ||
			c.WindowKind == "" || c.WindowKind != strings.TrimSpace(c.WindowKind) {
			add("child_identity_incomplete:" + c.ChildID)
			continue
		}
		// Structured before evidence required.
		if c.CapacityBefore == nil || c.CapacityReserved == nil {
			add("capacity_before_or_reserved_missing:" + c.ChildID)
		} else {
			if strings.TrimSpace(c.BeforeSource) == "" || isForbiddenCanarySource(c.BeforeSource) {
				add("before_source_invalid:" + c.ChildID)
			}
			if c.BeforeFreshness != "fresh" {
				add("before_freshness_not_fresh:" + c.ChildID)
			}
			if c.BeforeCapturedAt.IsZero() {
				add("before_captured_at_missing:" + c.ChildID)
			} else if !inCanaryRunWindow(c.BeforeCapturedAt, ev.ProducedAt, now) {
				add("before_captured_at_outside_canary_run:" + c.ChildID)
			}
			if strings.TrimSpace(c.BeforeConfidence) == "" {
				add("before_confidence_missing:" + c.ChildID)
			}
		}
		// Observed after with chronological order vs before.
		if c.CapacityAfter == nil {
			add("capacity_after_missing:" + c.ChildID)
		} else {
			state := c.AfterState
			src := c.AfterSource
			fr := c.AfterFreshness
			if state != "observed" {
				add("capacity_after_not_observed:" + c.ChildID + ":" + state)
			} else if src == "" {
				add("after_source_missing:" + c.ChildID)
			} else if isForbiddenCanarySource(src) {
				add("after_forbidden_source:" + c.ChildID + ":" + src)
			} else if fr != "fresh" {
				add("after_freshness_not_fresh:" + c.ChildID + ":" + fr)
			} else if c.AfterObservedAt.IsZero() {
				add("after_observed_at_missing:" + c.ChildID)
			} else if !inCanaryRunWindow(c.AfterObservedAt, ev.ProducedAt, now) {
				add("after_observed_at_outside_canary_run:" + c.ChildID)
			} else if !c.BeforeCapturedAt.IsZero() && c.AfterObservedAt.Before(c.BeforeCapturedAt) {
				add("after_before_before_timestamp:" + c.ChildID)
			} else if strings.TrimSpace(c.AfterConfidence) == "" {
				add("after_confidence_missing:" + c.ChildID)
			} else if !capacityResetOK(c.WindowKind, c.ResetAt, c.BeforeCapturedAt, c.AfterObservedAt, add, "child:"+c.ChildID) {
				// stable reason already added
			} else {
				afterOK++
			}
		}
		// Structured Actual in quota-window fraction unit required for useful kids.
		if c.CapacityActual == nil || strings.TrimSpace(c.ActualSource) == "" {
			add("capacity_actual_missing:" + c.ChildID)
		} else if isTokenProxyActualSourceCanary(c.ActualSource) {
			add("capacity_actual_token_proxy:" + c.ChildID)
		} else if !isGroupDeltaActualSource(c.ActualSource) {
			add("capacity_actual_source_not_group_delta:" + c.ChildID)
		} else if strings.TrimSpace(c.ActualConfidence) == "" {
			add("capacity_actual_confidence_missing:" + c.ChildID)
		} else if c.ActualConfidence != "estimated" {
			// Window aggregate group deltas are always estimated.
			add("capacity_actual_confidence_not_estimated:" + c.ChildID)
		}

		useful++
		company := canonicalProviderCompany(c.Provider)
		if company != "" {
			childProv[company] = true
		}
		realChildIDs[childIDKey{
			p: company, acc: c.AccountRef,
			inst: strings.TrimSpace(c.InstallRef), win: strings.TrimSpace(c.WindowKind),
		}] = true
		req := strings.ToLower(strings.TrimSpace(c.DepthRequired))
		sel := strings.ToLower(strings.TrimSpace(c.DepthSelected))
		inv := strings.ToLower(strings.TrimSpace(c.DepthInvocation))
		effortOK := c.ActualSources != nil && canaryTruthfulActualSource(c.ActualSources.Effort)
		if effortOK && req != "" {
			depths[req] = true
		}
		if effortOK && req != "" && req == sel && req == inv {
			depthBindOK++
		}
		// Reject dry-run markers
		if strings.Contains(strings.ToLower(c.AttemptID), "dry") ||
			strings.Contains(strings.ToLower(c.WorktreePath), "dry-run") {
			add("dry_run_child_not_allowed:" + c.ChildID)
		}
		// Route ActualSources + ArgvDigest required for real provider rows.
		// Erasure or forgery of any required dimension must fail closed.
		if c.ActualSources == nil {
			add("route_actual_sources_missing:" + c.ChildID)
		} else {
			// Provider/model/permission must be truthful accepted proof.
			// auth_binding/install_binding alone never qualify execution.
			truthful := map[string]bool{
				"provider_stream": true, "accepted_invocation": true,
			}
			allowedBinding := map[string]bool{
				"provider_stream": true, "accepted_invocation": true,
				"auth_binding": true, "install_binding": true,
			}
			checkTruthful := func(dim, val string) {
				if strings.TrimSpace(val) == "" || !truthful[val] {
					add("route_source_invalid:" + c.ChildID + ":" + dim + ":" + val)
				}
			}
			checkBinding := func(dim, val string) {
				if strings.TrimSpace(val) == "" || !allowedBinding[val] {
					add("route_source_invalid:" + c.ChildID + ":" + dim + ":" + val)
				}
			}
			checkTruthful("model", c.ActualSources.Model)
			checkTruthful("effort", c.ActualSources.Effort)
			checkTruthful("permission", c.ActualSources.Permission)
			if strings.TrimSpace(c.AccountRef) != "" {
				checkBinding("account", c.ActualSources.Account)
			}
			if strings.TrimSpace(c.InstallRef) != "" {
				checkBinding("install", c.ActualSources.Install)
			}
		}
		if strings.TrimSpace(c.ArgvDigest) == "" {
			add("argv_digest_missing:" + c.ChildID)
		}
		if canonicalProviderCompany(c.Provider) == "anthropic" {
			key := strings.Join([]string{c.AccountRef, c.InstallRef, c.Model, strings.ToLower(strings.TrimSpace(c.DepthInvocation))}, "\x00")
			if !validClaudeReceipts[key] {
				add("claude_catalog_receipt_missing_or_mismatched:" + c.ChildID)
			}
		}
	}

	// Provider observations must correspond to counted real child identities.
	// Unrelated observation rows cannot satisfy multi-provider freshness.
	provSeen := map[string]bool{}
	freshObs := 0
	for _, o := range ev.ProviderObservations {
		p := canonicalProviderCompany(o.Provider)
		if p == "" {
			continue
		}
		src := o.Source
		fr := o.Freshness
		if src == "" {
			add("provider_obs_source_missing:" + p)
			continue
		}
		if isForbiddenCanarySource(src) {
			add("provider_obs_forbidden_source:" + p + ":" + src)
			continue
		}
		if o.AccountRef == "" || o.AccountRef != strings.TrimSpace(o.AccountRef) ||
			o.InstallRef == "" || o.InstallRef != strings.TrimSpace(o.InstallRef) ||
			o.WindowKind == "" || o.WindowKind != strings.TrimSpace(o.WindowKind) {
			add("provider_obs_identity_incomplete:" + p)
			continue
		}
		switch o.Confidence {
		case "exact", "estimated", "unknown":
		default:
			add("provider_obs_confidence_invalid:" + p)
			continue
		}
		if o.CapturedAt.IsZero() {
			add("provider_obs_captured_at_missing:" + p)
			continue
		}
		if !inCanaryRunWindow(o.CapturedAt, ev.ProducedAt, now) {
			add("provider_obs_captured_at_outside_canary_run:" + p)
			continue
		}
		if o.InventoryReportDigest == "" {
			add("provider_obs_inventory_digest_missing:" + p)
			continue
		}
		k := childIDKey{p: p, acc: o.AccountRef, inst: o.InstallRef, win: o.WindowKind}
		if !realChildIDs[k] {
			add("provider_obs_unrelated_to_real_child:" + p)
			continue
		}
		// Finite/fixed windows used for capacity_after_runtime require reset identity.
		if !capacityResetOK(o.WindowKind, o.ResetAt, o.CapturedAt, o.CapturedAt, add, "provider_obs:"+p) {
			continue
		}
		if fr == "fresh" {
			freshObs++
			provSeen[p] = true
		}
	}
	if len(provSeen) < 2 {
		add("provider_companies_lt_2")
	}
	if freshObs < 2 {
		add("fresh_provider_observations_lt_2")
	}
	rawCapacityOK, rawCapacityReasons := validateRawCapacityEvidence(ev)
	for _, reason := range rawCapacityReasons {
		add(reason)
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
		add("executed_provider_companies_lt_2")
	}
	// multi-depth: at least 2 distinct depths with real bind
	if len(depths) < 2 || depthBindOK < 2 {
		add("multi_depth_runtime_unmet")
	} else {
		v.MultiDepthOK = true
	}
	v.MultiProviderOK = len(childProv) >= 2 && useful >= 4
	if rawCapacityOK && afterOK >= 4 && !hasReasonPrefix(v.Reasons, "capacity_after_missing") {
		v.CapacityAfterOK = true
	} else if rawCapacityOK && afterOK >= useful && useful >= 4 {
		v.CapacityAfterOK = true
	}

	// Unavailable retry is recomputed from raw exact event, claim, and ledger
	// evidence. Summary booleans and EvidenceRef never establish truth.
	if ev.UnavailableRetry == nil {
		add("unavailable_retry_missing")
	} else {
		u := ev.UnavailableRetry
		rawOK, rawReasons := validateRawUnavailableRetry(ev, *u)
		for _, reason := range rawReasons {
			add(reason)
		}
		if rawOK {
			v.UnavailableRetryOK = true
		}
	}

	// Restart / ceilings — recompute from measured fields; never trust *OK alone.
	if ev.Restart == nil {
		add("restart_evidence_missing")
	} else {
		r := ev.Restart
		rawRestart := deriveForcedRestartEvidence(ev.RawEvents, r.ResumedFromDurable)
		rawClaimsOK := validateRawRestartClaims(ev, rawRestart)
		// Recompute ceilings/leaks/repo-local from transparent measured fields.
		reProcessOK := r.ProcessPeak > 0 && r.ProcessLimit > 0 && r.ProcessPeak <= r.ProcessLimit
		reWorktreeOK := r.WorktreePeak > 0 && r.WorktreeLimit > 0 && r.WorktreePeak <= r.WorktreeLimit
		reNoLeaked := r.ActiveOccupancyMeasured && r.ProcessActive == 0 && r.WorktreeActive == 0
		reNoRepoLocal := r.RepoLocalRuntimeChecked && !r.RepoLocalRuntimePresent
		reExactlyOnce := rawRestart.Interrupted && r.ResumedFromDurable &&
			rawRestart.LaterGenerationResume && len(rawRestart.AbortedAttempts) == 1 &&
			rawRestart.ReuseCount > 0 && rawClaimsOK &&
			!rawRestart.DuplicateLaunch && !rawRestart.DuplicateSuccessIntegrate &&
			!rawRestart.AbortedAttemptSucceeded

		if r.Interrupted != rawRestart.Interrupted ||
			r.LaterGenerationResume != rawRestart.LaterGenerationResume ||
			r.AbortedAttemptCount != len(rawRestart.AbortedAttempts) ||
			r.ReuseCountMeasured != rawRestart.ReuseCount ||
			r.DuplicateLaunch != rawRestart.DuplicateLaunch ||
			r.DuplicateSuccessIntegrate != rawRestart.DuplicateSuccessIntegrate ||
			r.AbortedAttemptSucceeded != rawRestart.AbortedAttemptSucceeded {
			add("restart_summary_not_bound_to_raw_events")
		}
		if !rawClaimsOK {
			add("restart_raw_claim_evidence_invalid")
		}

		if r.ProcessCeilingOK != reProcessOK {
			add("restart_process_ceiling_flag_mismatch")
		}
		if r.WorktreeCeilingOK != reWorktreeOK {
			add("restart_worktree_ceiling_flag_mismatch")
		}
		if r.NoLeakedProcesses != reNoLeaked {
			add("restart_no_leaked_flag_mismatch")
		}
		if r.NoRepoLocalRuntime != reNoRepoLocal {
			add("restart_no_repo_local_flag_mismatch")
		}
		if r.ExactlyOnce != reExactlyOnce {
			add("restart_exactly_once_flag_mismatch")
		}

		if !rawRestart.Interrupted || !r.ResumedFromDurable || !reExactlyOnce {
			add("restart_flags_incomplete")
		} else if r.ChildCountUseful < 4 {
			add("restart_useful_children_lt_4")
		} else if !reProcessOK || !reWorktreeOK || !reNoLeaked || !reNoRepoLocal {
			add("restart_ceilings_or_leaks_unmet")
		} else if r.ProcessLimit != ProductionSequentialCeiling || r.WorktreeLimit != ProductionSequentialCeiling {
			add("restart_limit_not_production_sequential_ceiling")
		} else {
			v.RestartOK = true
		}
	}

	// Real PR — require live verification + wi_verify-derived independent verifier.
	// Manifest RequiredChecksGreen alone cannot green real_pr_human_gate.
	if ev.PR == nil {
		add("real_pr_missing")
	} else {
		p := *ev.PR
		liveOK, liveReasons := ValidatePRLive(p, expect.LivePR, expect.ExpectedPRHeadOID)
		for _, r := range liveReasons {
			add(r)
		}
		verOK, verReasons := ValidateIndependentVerifierFromChildren(p, ev.Children)
		for _, r := range verReasons {
			add(r)
		}
		if liveOK && verOK {
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

func canonicalProviderCompany(provider string) string {
	if provider == "" || provider != strings.TrimSpace(provider) ||
		provider != strings.ToLower(provider) {
		return ""
	}
	p := provider
	switch p {
	case "codex", "openai", "chatgpt":
		return "openai"
	case "claude", "anthropic":
		return "anthropic"
	case "grok", "xai", "x.ai":
		return "xai"
	case "antigravity", "gemini", "google":
		return "google"
	case "kimi", "moonshot":
		return "moonshot"
	case "copilot", "github-copilot":
		return "github"
	default:
		return p
	}
}

func validateRawCapacityEvidence(ev CanaryEvidence) (bool, []string) {
	var reasons []string
	add := func(reason string) { reasons = append(reasons, reason) }
	byAttempt := map[string]capacityledger.Entry{}
	for _, entry := range ev.RawLedgerEntries {
		if entry.Schema != capacityledger.SchemaEntry || entry.ProjectID != ev.ProjectID ||
			entry.RunID != ev.RunID || entry.AttemptID == "" ||
			entry.AttemptID != strings.TrimSpace(entry.AttemptID) {
			add("raw_ledger_identity_invalid")
			continue
		}
		if _, exists := byAttempt[entry.AttemptID]; exists {
			add("raw_ledger_duplicate_attempt:" + entry.AttemptID)
			continue
		}
		byAttempt[entry.AttemptID] = entry
	}
	type groupState struct {
		before, after, reserved, actual float64
		hasAfter, hasActual             bool
	}
	groups := map[string]*groupState{}
	useful := 0
	for _, child := range ev.Children {
		if !child.RealProviderExecuted || child.Terminal != "succeeded" {
			continue
		}
		useful++
		entry, ok := byAttempt[child.AttemptID]
		if !ok {
			add("raw_ledger_attempt_missing:" + child.AttemptID)
			continue
		}
		if entry.Provider != child.Provider || entry.Model != child.Model ||
			entry.Depth != child.DepthInvocation || entry.AccountRef != child.AccountRef ||
			entry.InstallRef != child.InstallRef || entry.WindowKind != child.WindowKind {
			add("raw_ledger_route_identity_mismatch:" + child.AttemptID)
		}
		if entry.State != "reconciled" ||
			entry.Freshness != child.BeforeFreshness ||
			string(entry.Confidence) != child.BeforeConfidence ||
			!child.BeforeCapturedAt.Equal(timeValue(entry.BeforeCapturedAt)) ||
			!timesEqual(child.ResetAt, entry.ResetAt) {
			add("raw_ledger_before_evidence_mismatch:" + child.AttemptID)
		}
		if entry.BeforeInventoryDigest == "" ||
			!beforeLedgerObservationBound(ev.ProviderObservations, entry) {
			add("raw_ledger_before_inventory_digest_mismatch:" + child.AttemptID)
		}
		if child.CapacityBefore == nil || !floatEqual(*child.CapacityBefore, entry.Before) ||
			child.CapacityReserved == nil || !floatEqual(*child.CapacityReserved, entry.Reserved) {
			add("raw_ledger_before_reserved_mismatch:" + child.AttemptID)
		}
		if child.CapacityActual == nil || entry.Actual == nil ||
			!floatEqual(*child.CapacityActual, *entry.Actual) ||
			child.ActualSource != entry.ActualSource ||
			child.ActualConfidence != string(entry.ActualConfidence) {
			add("raw_ledger_actual_mismatch:" + child.AttemptID)
		}
		if child.CapacityAfter == nil || entry.After == nil ||
			!floatEqual(*child.CapacityAfter, *entry.After) ||
			child.AfterState != entry.AfterState ||
			child.AfterSource != entry.AfterSource ||
			child.AfterFreshness != entry.AfterFreshness ||
			child.AfterConfidence != string(entry.AfterConfidence) ||
			!child.AfterObservedAt.Equal(timeValue(entry.AfterObservedAt)) ||
			entry.AfterInventoryDigest == "" ||
			!afterLedgerObservationBound(ev.ProviderObservations, entry) {
			add("raw_ledger_after_mismatch:" + child.AttemptID)
		}
		if entry.Before < 0 || entry.Before > 1 || entry.Reserved <= 0 ||
			entry.Reserved > entry.Before || entry.Actual == nil ||
			*entry.Actual < 0 || *entry.Actual > 1 || entry.After == nil ||
			*entry.After < 0 || *entry.After > 1 {
			add("raw_ledger_capacity_bounds_invalid:" + child.AttemptID)
			continue
		}
		groupKey := entry.Provider + "\x00" + entry.AccountRef + "\x00" +
			entry.InstallRef + "\x00" + entry.WindowKind + "\x00" +
			entry.BeforeInventoryDigest + "\x00" + entry.BeforeSource + "\x00" +
			timeValue(entry.BeforeCapturedAt).UTC().Format(time.RFC3339Nano)
		group := groups[groupKey]
		if group == nil {
			group = &groupState{before: entry.Before, after: *entry.After, hasAfter: true}
			groups[groupKey] = group
		} else if !floatEqual(group.before, entry.Before) ||
			!floatEqual(group.after, *entry.After) {
			add("raw_ledger_group_before_after_conflict:" + child.AttemptID)
		}
		group.reserved += entry.Reserved
		group.actual += *entry.Actual
		group.hasActual = true
	}
	if useful == 0 {
		add("raw_ledger_useful_children_missing")
	}
	for _, group := range groups {
		if !group.hasAfter || !group.hasActual ||
			!floatEqual(group.before-group.actual, group.after) {
			add("raw_ledger_capacity_arithmetic_mismatch")
		}
		if group.reserved <= 0 || group.reserved > group.before+1e-9 {
			add("raw_ledger_reserved_arithmetic_invalid")
		}
	}
	for _, obs := range ev.ProviderObservations {
		matched := false
		for _, entry := range byAttempt {
			if entry.Provider != obs.Provider || entry.AccountRef != obs.AccountRef ||
				entry.InstallRef != obs.InstallRef || entry.WindowKind != obs.WindowKind {
				continue
			}
			beforeMatch := obs.InventoryReportDigest == entry.BeforeInventoryDigest &&
				obs.Source == entry.BeforeSource &&
				obs.Freshness == entry.Freshness &&
				obs.Confidence == string(entry.Confidence) &&
				obs.CapturedAt.Equal(timeValue(entry.BeforeCapturedAt)) &&
				timesEqual(obs.ResetAt, entry.ResetAt) &&
				obs.Remaining != nil && floatEqual(*obs.Remaining, entry.Before)
			afterMatch := entry.After != nil &&
				obs.InventoryReportDigest == entry.AfterInventoryDigest &&
				obs.Source == entry.AfterSource &&
				obs.Freshness == entry.AfterFreshness &&
				obs.Confidence == string(entry.AfterConfidence) &&
				obs.CapturedAt.Equal(timeValue(entry.AfterObservedAt)) &&
				timesEqual(obs.ResetAt, entry.ResetAt) &&
				obs.Remaining != nil && floatEqual(*obs.Remaining, *entry.After)
			if beforeMatch || afterMatch {
				matched = true
				break
			}
		}
		if !matched {
			add("provider_obs_not_bound_to_raw_ledger:" + obs.Provider)
		}
	}
	return len(reasons) == 0, reasons
}

func validateRawUnavailableRetry(ev CanaryEvidence, summary CanaryUnavailableRetry) (bool, []string) {
	var reasons []string
	add := func(reason string) { reasons = append(reasons, reason) }
	if summary.ExcludedProvider == "" || summary.ExcludedReason != "model_unavailable" ||
		summary.RetryAttemptID == "" ||
		summary.RetryAttemptID != strings.TrimSpace(summary.RetryAttemptID) {
		add("unavailable_retry_exact_identity_incomplete")
		return false, reasons
	}
	payload := func(event workflowrun.Event) map[string]string {
		var fields map[string]string
		if len(event.Payload) == 0 || json.Unmarshal(event.Payload, &fields) != nil {
			return nil
		}
		return fields
	}
	var unavailable []workflowrun.Event
	for _, event := range ev.RawEvents {
		fields := payload(event)
		if event.Kind == "model_unavailable" && event.FailureClass == "model_unavailable" &&
			event.AttemptID != "" && event.WorkItemID != "" &&
			fields != nil && fields["provider"] == summary.ExcludedProvider {
			unavailable = append(unavailable, event)
		}
	}
	if len(unavailable) != 1 {
		add("unavailable_retry_raw_model_unavailable_count")
		return false, reasons
	}
	failed := unavailable[0]
	failedAttempt, retryAttempt, workItem := failed.AttemptID, summary.RetryAttemptID, failed.WorkItemID
	failedFields := payload(failed)
	if failedFields["attempt_id"] != failedAttempt ||
		failedFields["work_item_id"] != workItem ||
		failedFields["failure_class"] != "model_unavailable" ||
		failedFields["provider"] != summary.ExcludedProvider ||
		failedFields["model"] == "" {
		add("unavailable_retry_model_unavailable_payload_mismatch")
	}
	if failedAttempt == retryAttempt {
		add("unavailable_retry_attempts_not_distinct")
	}
	countEvent := func(kind, attempt string) int {
		count := 0
		for _, event := range ev.RawEvents {
			if event.Kind == kind && event.AttemptID == attempt && event.WorkItemID == workItem {
				count++
			}
		}
		return count
	}
	findEvent := func(kind, attempt string) (workflowrun.Event, bool) {
		for _, event := range ev.RawEvents {
			if event.Kind == kind && event.AttemptID == attempt && event.WorkItemID == workItem {
				return event, true
			}
		}
		return workflowrun.Event{}, false
	}
	for _, required := range []struct {
		kind, attempt string
	}{
		{"claim", failedAttempt}, {"launch", failedAttempt},
		{"model_unavailable", failedAttempt}, {"terminal", failedAttempt},
		{"claim", retryAttempt}, {"reroute", retryAttempt},
		{"launch", retryAttempt}, {"terminal", retryAttempt},
	} {
		if countEvent(required.kind, required.attempt) != 1 {
			add("unavailable_retry_raw_event_count:" + required.kind + ":" + required.attempt)
		}
	}
	failedTerminal, failedTermOK := findEvent("terminal", failedAttempt)
	retryClaim, retryClaimOK := findEvent("claim", retryAttempt)
	retryTerminal, retryTermOK := findEvent("terminal", retryAttempt)
	if !failedTermOK || failedTerminal.Terminal != "failed" ||
		failedTerminal.FailureClass != "model_unavailable" ||
		failedTerminal.Evidence == "" {
		add("unavailable_retry_failed_terminal_invalid")
	}
	if !retryClaimOK || retryClaim.Generation <= failed.Generation {
		add("unavailable_retry_generation_not_higher")
	}
	if !retryTermOK || retryTerminal.Terminal != "succeeded" || retryTerminal.Evidence == "" {
		add("unavailable_retry_retry_terminal_invalid")
	}
	if retryClaimOK {
		fields := payload(retryClaim)
		if fields == nil || fields["supersedes_attempt_id"] != failedAttempt ||
			fields["retry_attempt_id"] != retryAttempt {
			add("unavailable_retry_claim_supersedes_mismatch")
		}
	}
	claimsByAttempt := map[string][]workclaim.Claim{}
	for _, claim := range ev.RawClaims {
		if claim.ProjectID == ev.ProjectID && claim.WorkItemID == workItem {
			claimsByAttempt[claim.AttemptID] = append(claimsByAttempt[claim.AttemptID], claim)
		}
	}
	failedClaims, retryClaims := claimsByAttempt[failedAttempt], claimsByAttempt[retryAttempt]
	noDuplicateClaim := len(failedClaims) == 1 && len(retryClaims) == 1
	if noDuplicateClaim {
		fc, rc := failedClaims[0], retryClaims[0]
		noDuplicateClaim = fc.State == workclaim.StateClosed &&
			string(fc.Terminal) == "failed" && fc.OutputEvidence == failedTerminal.Evidence &&
			rc.State == workclaim.StateClosed && string(rc.Terminal) == "succeeded" &&
			rc.OutputEvidence == retryTerminal.Evidence &&
			rc.Generation > fc.Generation
	}
	if !noDuplicateClaim {
		add("unavailable_retry_raw_claim_invalid_or_duplicate")
	}
	ledgerByAttempt := map[string][]capacityledger.Entry{}
	for _, entry := range ev.RawLedgerEntries {
		if entry.ProjectID == ev.ProjectID && entry.RunID == ev.RunID {
			ledgerByAttempt[entry.AttemptID] = append(ledgerByAttempt[entry.AttemptID], entry)
		}
	}
	failedLedger, retryLedger := ledgerByAttempt[failedAttempt], ledgerByAttempt[retryAttempt]
	noDoubleCapacity := len(failedLedger) == 1 && len(retryLedger) == 1
	if noDoubleCapacity {
		fentry, rentry := failedLedger[0], retryLedger[0]
		noDoubleCapacity = fentry.State == "released" && fentry.Actual == nil &&
			fentry.ReservationID != "" && rentry.State == "reconciled" &&
			rentry.Actual != nil && rentry.After != nil && rentry.ReservationID != "" &&
			fentry.ReservationID != rentry.ReservationID &&
			fentry.Provider == summary.ExcludedProvider &&
			fentry.BeforeInventoryDigest != "" &&
			fentry.BeforeInventoryDigest == rentry.BeforeInventoryDigest &&
			beforeLedgerObservationBound(ev.ProviderObservations, fentry) &&
			beforeLedgerObservationBound(ev.ProviderObservations, rentry)
	}
	if !noDoubleCapacity {
		add("unavailable_retry_raw_ledger_invalid_or_duplicate")
	}
	childrenByAttempt := map[string][]CanaryChild{}
	for _, child := range ev.Children {
		if child.WorkItemID == workItem {
			childrenByAttempt[child.AttemptID] = append(childrenByAttempt[child.AttemptID], child)
		}
	}
	noDuplicateFiles := len(childrenByAttempt[failedAttempt]) == 1 &&
		len(childrenByAttempt[retryAttempt]) == 1
	if noDuplicateFiles {
		seen := map[string]bool{}
		for _, path := range childrenByAttempt[failedAttempt][0].FilesTouched {
			if path == "" || path != strings.TrimSpace(path) {
				noDuplicateFiles = false
			}
			seen[path] = true
		}
		for _, path := range childrenByAttempt[retryAttempt][0].FilesTouched {
			if path == "" || path != strings.TrimSpace(path) || seen[path] {
				noDuplicateFiles = false
			}
		}
	}
	if !noDuplicateFiles {
		add("unavailable_retry_raw_files_duplicate_or_missing")
	}
	if countEvent("integrate", failedAttempt) != 0 || countEvent("integrate", retryAttempt) != 1 {
		add("unavailable_retry_integrate_count_invalid")
	}
	if summary.NoDuplicateClaim != noDuplicateClaim ||
		summary.NoDuplicateFiles != noDuplicateFiles ||
		summary.NoDoubleCapacity != noDoubleCapacity {
		add("unavailable_retry_summary_flag_mismatch")
	}
	return len(reasons) == 0, reasons
}

func validateRawRestartClaims(ev CanaryEvidence, raw forcedRestartEvidence) bool {
	if !raw.Interrupted || len(raw.AbortedAttempts) != 1 {
		return false
	}
	for workItem, abortedAttempt := range raw.AbortedAttempts {
		var aborted []workclaim.Claim
		seenAttempts := map[string]bool{}
		later := 0
		abortedGeneration := workflowrun.ParseAttemptGeneration(abortedAttempt)
		for _, claim := range ev.RawClaims {
			if claim.ProjectID != ev.ProjectID || claim.WorkItemID != workItem {
				continue
			}
			if claim.AttemptID == "" || claim.AttemptID != strings.TrimSpace(claim.AttemptID) ||
				seenAttempts[claim.AttemptID] {
				return false
			}
			seenAttempts[claim.AttemptID] = true
			if claim.AttemptID == abortedAttempt {
				aborted = append(aborted, claim)
				continue
			}
			generation := workflowrun.ParseAttemptGeneration(claim.AttemptID)
			if generation > abortedGeneration && claim.Generation == int64(generation+1) {
				later++
			}
		}
		if len(aborted) != 1 || aborted[0].State != workclaim.StateClosed ||
			string(aborted[0].Terminal) != "cancelled" ||
			aborted[0].Generation != int64(abortedGeneration+1) || later < 1 {
			return false
		}
	}
	// Every raw reuse line must bind to one exact closed succeeded claim. This
	// proves durable sibling reuse instead of trusting a run-level counter.
	reusedAttempts := map[string]bool{}
	reuseCount := 0
	for _, event := range ev.RawEvents {
		if event.Kind != "reuse" {
			continue
		}
		reuseCount++
		if reusedAttempts[event.AttemptID] {
			return false
		}
		reusedAttempts[event.AttemptID] = true
		matches := 0
		for _, claim := range ev.RawClaims {
			if claim.ProjectID == ev.ProjectID &&
				claim.WorkItemID == event.WorkItemID &&
				claim.AttemptID == event.AttemptID &&
				claim.State == workclaim.StateClosed &&
				string(claim.Terminal) == "succeeded" &&
				claim.OutputEvidence != "" &&
				claim.OutputEvidence == event.Evidence &&
				claim.Generation == int64(event.Generation) {
				matches++
			}
		}
		if matches != 1 {
			return false
		}
	}
	if reuseCount != raw.ReuseCount || reuseCount == 0 {
		return false
	}
	return true
}

func validateRawDurableEnvelope(ev CanaryEvidence) []string {
	var reasons []string
	if len(ev.RawEvents) == 0 {
		reasons = append(reasons, "raw_events_missing")
	} else {
		seenEvent := map[string]bool{}
		for _, event := range ev.RawEvents {
			if event.Schema != workflowrun.EventSchema ||
				event.EventID == "" || event.EventID != strings.TrimSpace(event.EventID) ||
				seenEvent[event.EventID] ||
				event.ProjectID != ev.ProjectID || event.RunID != ev.RunID ||
				event.At.IsZero() {
				reasons = append(reasons, "raw_event_envelope_invalid")
				break
			}
			seenEvent[event.EventID] = true
			if err := workflowrun.ValidateChildEventIdentity(event); err != nil {
				reasons = append(reasons, "raw_event_child_identity_invalid")
				break
			}
		}
		if err := workflowrun.ValidateEventStreamInvariants(ev.RawEvents); err != nil {
			reasons = append(reasons, "raw_event_stream_invariants_invalid")
		}
	}
	if len(ev.RawClaims) == 0 {
		reasons = append(reasons, "raw_claims_missing")
	} else {
		seenClaimID := map[string]bool{}
		seenAttempt := map[string]bool{}
		generations := map[string]map[int64]bool{}
		claimEnvelopeInvalid := false
		for _, claim := range ev.RawClaims {
			if claim.Schema != workclaim.SchemaClaim ||
				claim.ClaimID == "" || claim.ClaimID != strings.TrimSpace(claim.ClaimID) ||
				seenClaimID[claim.ClaimID] ||
				claim.ProjectID != ev.ProjectID ||
				claim.GraphID == "" || claim.GraphID != strings.TrimSpace(claim.GraphID) ||
				claim.GraphVersion <= 0 ||
				claim.WorkItemID == "" || claim.WorkItemID != strings.TrimSpace(claim.WorkItemID) ||
				claim.AttemptID == "" || claim.AttemptID != strings.TrimSpace(claim.AttemptID) ||
				seenAttempt[claim.AttemptID] ||
				claim.ExecutorID != workflowrun.WorkflowrunExecutorID ||
				workflowrun.ParseAttemptGeneration(claim.AttemptID) < 0 ||
				claim.Generation <= 0 {
				claimEnvelopeInvalid = true
				break
			}
			logical := strings.Join([]string{
				claim.ProjectID, claim.GraphID, fmt.Sprintf("%d", claim.GraphVersion), claim.WorkItemID,
			}, "\x00")
			if generations[logical] == nil {
				generations[logical] = map[int64]bool{}
			}
			if generations[logical][claim.Generation] {
				claimEnvelopeInvalid = true
				break
			}
			generations[logical][claim.Generation] = true
			seenClaimID[claim.ClaimID] = true
			seenAttempt[claim.AttemptID] = true
		}
		if !claimEnvelopeInvalid {
			for _, byGeneration := range generations {
				for generation := int64(1); generation <= int64(len(byGeneration)); generation++ {
					if !byGeneration[generation] {
						claimEnvelopeInvalid = true
						break
					}
				}
				if claimEnvelopeInvalid {
					break
				}
			}
		}
		if claimEnvelopeInvalid {
			reasons = append(reasons, "raw_claim_envelope_invalid")
		}
	}
	return reasons
}

func beforeLedgerObservationBound(observations []CanaryProviderObs, entry capacityledger.Entry) bool {
	for _, obs := range observations {
		if obs.Provider == entry.Provider &&
			obs.AccountRef == entry.AccountRef &&
			obs.InstallRef == entry.InstallRef &&
			obs.WindowKind == entry.WindowKind &&
			obs.InventoryReportDigest == entry.BeforeInventoryDigest &&
			obs.Source == entry.BeforeSource &&
			obs.Freshness == entry.Freshness &&
			obs.Confidence == string(entry.Confidence) &&
			obs.CapturedAt.Equal(timeValue(entry.BeforeCapturedAt)) &&
			timesEqual(obs.ResetAt, entry.ResetAt) &&
			obs.Remaining != nil && floatEqual(*obs.Remaining, entry.Before) {
			return true
		}
	}
	return false
}

func afterLedgerObservationBound(observations []CanaryProviderObs, entry capacityledger.Entry) bool {
	if entry.After == nil {
		return false
	}
	for _, obs := range observations {
		if obs.Provider == entry.Provider &&
			obs.AccountRef == entry.AccountRef &&
			obs.InstallRef == entry.InstallRef &&
			obs.WindowKind == entry.WindowKind &&
			obs.InventoryReportDigest == entry.AfterInventoryDigest &&
			obs.Source == entry.AfterSource &&
			obs.Freshness == entry.AfterFreshness &&
			obs.Confidence == string(entry.AfterConfidence) &&
			obs.CapturedAt.Equal(timeValue(entry.AfterObservedAt)) &&
			timesEqual(obs.ResetAt, entry.ResetAt) &&
			obs.Remaining != nil && floatEqual(*obs.Remaining, *entry.After) {
			return true
		}
	}
	return false
}

func timeValue(value *time.Time) time.Time {
	if value == nil {
		return time.Time{}
	}
	return value.UTC()
}

func timesEqual(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

func floatEqual(a, b float64) bool {
	return math.Abs(a-b) <= 1e-9
}

// windowRequiresCapacityReset is true for finite/fixed quota windows.
// Unbounded / non-reset capacity does not require ResetAt.
func windowRequiresCapacityReset(windowKind string) bool {
	k := strings.ToLower(strings.TrimSpace(windowKind))
	if k == "" {
		return false // incomplete identity already fails separately
	}
	if strings.Contains(k, "unbounded") || strings.Contains(k, "non-reset") ||
		strings.Contains(k, "non_reset") || k == "nonreset" {
		return false
	}
	return true
}

// capacityResetOK validates structured ResetAt for capacity_after_runtime evidence.
// Reasons are stable IDs only (no credentials). Returns false when fail-closed.
func capacityResetOK(windowKind string, reset *time.Time, obsA, obsB time.Time, add func(string), id string) bool {
	if !windowRequiresCapacityReset(windowKind) {
		return true
	}
	if reset == nil || reset.IsZero() {
		add("capacity_reset_at_missing:" + id)
		return false
	}
	r := reset.UTC()
	// Reset must be strictly after every observation timestamp (not stale/expired).
	for _, o := range []time.Time{obsA, obsB} {
		if o.IsZero() {
			continue
		}
		if !r.After(o.UTC()) {
			add("capacity_reset_at_stale_vs_observation:" + id)
			return false
		}
	}
	return true
}

// canaryTruthfulActualSource matches goalrun collectUsage: only provider_stream
// or accepted_invocation prove model/depth/permission actual diversity.
// auth_binding / install_binding alone never count.
func canaryTruthfulActualSource(s string) bool {
	switch strings.TrimSpace(s) {
	case "provider_stream", "accepted_invocation":
		return true
	default:
		return false
	}
}

// isForbiddenCanarySource rejects fixture/test/fake/defaulted sources in release evidence.
// Exact tokens and known prefixes only — never substring "test" matching "attest".
func isForbiddenCanarySource(src string) bool {
	s := strings.ToLower(strings.TrimSpace(src))
	if s == "" {
		return true
	}
	switch s {
	case "fixture", "test", "fake", "capacity_snapshot", "before_minus_actual",
		"unknown", "n/a", "na", "estimated":
		return true
	}
	for _, p := range []string{"fixture:", "fixture_", "test:", "test_", "fake:", "fake_",
		"before_minus_actual", "capacity_snapshot", "token"} {
		if strings.HasPrefix(s, p) || s == p {
			return true
		}
	}
	return false
}

// inCanaryRunWindow requires t within the current canary ProducedAt window
// (not a generic 24h/7d reuse window).
func inCanaryRunWindow(t, producedAt, now time.Time) bool {
	if t.IsZero() || producedAt.IsZero() {
		return false
	}
	// Observation must not be after ProducedAt + skew or before ProducedAt - 2h.
	if t.After(producedAt.Add(15 * time.Minute)) {
		return false
	}
	if t.Before(producedAt.Add(-2 * time.Hour)) {
		return false
	}
	// Qualify time must still be near the canary production.
	if now.Sub(producedAt) > 2*time.Hour || producedAt.After(now.Add(15*time.Minute)) {
		return false
	}
	return true
}

// isTokenProxyActualSourceCanary rejects soft-window token estimates as Actual.
func isTokenProxyActualSourceCanary(src string) bool {
	s := strings.ToLower(strings.TrimSpace(src))
	if s == "" || s == "unknown" {
		return true
	}
	// Bare "estimated" without group_delta prefix is a soft-window proxy.
	if s == "estimated" {
		return true
	}
	if strings.Contains(s, "soft_window") || strings.Contains(s, "softwindow") {
		return true
	}
	// token_count/soft proxy — but allow estimated_group_delta_token_weighted
	if strings.HasPrefix(s, "estimated_group_delta_") {
		return false
	}
	return strings.Contains(s, "token")
}

// isGroupDeltaActualSource accepts only auditable group allocation sources.
func isGroupDeltaActualSource(src string) bool {
	s := strings.ToLower(strings.TrimSpace(src))
	return strings.HasPrefix(s, "estimated_group_delta_token_weighted:") ||
		strings.HasPrefix(s, "estimated_group_delta_reservation_weighted:") ||
		strings.HasPrefix(s, "estimated_group_delta_zero:") ||
		strings.HasPrefix(s, "estimated_group_delta_empty:")
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

// DigestDurableEvidence binds the raw event, claim, capacity-ledger, and live
// inventory evidence carried by the manifest.
func DigestDurableEvidence(ev CanaryEvidence) string {
	body := struct {
		InventoryProvenance   string
		InventoryReportDigest string
		Events                []workflowrun.Event
		Claims                []workclaim.Claim
		Ledger                []capacityledger.Entry
	}{
		InventoryProvenance:   ev.InventoryProvenance,
		InventoryReportDigest: ev.InventoryReportDigest,
		Events:                ev.RawEvents,
		Claims:                ev.RawClaims,
		Ledger:                ev.RawLedgerEntries,
	}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
