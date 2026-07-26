package goalrun

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/artifactqual"
	"github.com/jasonhnd/loopcoder/internal/workclaim"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
)

// CanaryEmitOptions configures exact-binary canary_evidence.v1 emission at goal end.
// All fields must come from measured run + archive identity — never hand booleans.
type CanaryEmitOptions struct {
	// OutPath is where to write canary_evidence.v1 JSON.
	OutPath string
	// ArchiveDigest / PreProdSHA / BinaryVersion / BinaryCommit bind the exact artifact.
	ArchiveDigest string
	PreProdSHA    string
	BinaryVersion string
	BinaryCommit  string
	// HomeDir for event log discovery when Workflow.EventLogPath is empty.
	HomeDir string
	// RepoPath is the project repo root for repo-local .loopcoder measurement.
	// When empty at emit time, EmitCanaryFromResult fills from the Execute Request
	// only via the caller wiring opts.RepoPath = req.RepoPath (never invents clean).
	RepoPath string
	// InventoryProvenance must be live_discover for release canary (structural).
	InventoryProvenance InventoryProvenance
	// RequireLiveInventory when true fails closed on non-live inventory provenance.
	RequireLiveInventory bool
}

// EmitCanaryFromResult builds and writes canary_evidence.v1 from a completed goal Result.
// Requires interrupt events for restart section; fails closed otherwise when
// RequireRestart is implied by Resumed||Interrupted in workflow.
//
// Event log loading is one authoritative path: prefer exact Workflow.EventLogPath
// (strict JSONL readback). Only when that path is empty, resolve via HomeDir.
// Unavailable retry proof receives those same read-back events; on readback
// failure for claimed model_unavailable, unavailable_retry stays nil (not_run).
func EmitCanaryFromResult(res Result, opts CanaryEmitOptions) (artifactqual.CanaryEvidence, error) {
	if strings.TrimSpace(opts.OutPath) == "" {
		return artifactqual.CanaryEvidence{}, fmt.Errorf("goalrun: canary out path required")
	}
	// Structural inventory provenance: release canary never from snapshot/injected.
	if opts.RequireLiveInventory && opts.InventoryProvenance != InventoryProvenanceLiveDiscover {
		return artifactqual.CanaryEvidence{}, fmt.Errorf("goalrun: canary emit rejects inventory provenance %q (require live_discover)", opts.InventoryProvenance)
	}

	events, evPath, loadErr := loadWorkflowEvents(res, opts.HomeDir)
	var claims []workclaim.Claim
	if loadErr == nil && evPath != "" {
		claimPath := filepath.Join(filepath.Dir(evPath), "workclaims.json")
		if _, statErr := os.Stat(claimPath); statErr == nil {
			store, claimErr := workclaim.OpenPath(claimPath, time.Now)
			if claimErr != nil {
				return artifactqual.CanaryEvidence{}, fmt.Errorf("goalrun: canary claim evidence: %w", claimErr)
			}
			claims = store.AllClaims()
		} else if !os.IsNotExist(statErr) {
			return artifactqual.CanaryEvidence{}, fmt.Errorf("goalrun: canary claim evidence stat: %w", statErr)
		}
	}
	// Other canary metrics may still emit; claimed model_unavailable proof needs events.
	var unavail *artifactqual.CanaryUnavailableRetry
	if loadErr != nil {
		// If any claimed model_unavailable exclude exists, unavailable_retry MUST
		// stay nil/not_run — never fall back to a different unclaimed exclude.
		if hasClaimedModelUnavailableExclude(res.RouteExcludes) {
			unavail = nil
		} else {
			// Unclaimed-only path does not require EventLog readback.
			unavail = BuildUnavailableRetryEvidence(res.RouteExcludes, firstRetryAttempt(res))
		}
	} else {
		unavail = BuildUnavailableRetryEvidenceWithProof(
			res.RouteExcludes, firstRetryAttempt(res), proofFromResult(res, events),
		)
	}

	children := canaryChildrenFromReports(res)
	obs := canaryProviderObsFromReports(res)

	var prURL, prBranch, prBase, prHead, prRepo, prVer, prVerRef, prVerProv, prVerAtt string
	var prNum int
	var prChecks []string
	var prGreen, prOwned, prHuman bool
	if res.PR != nil {
		prURL = res.PR.URL
		prBranch = res.PR.Branch
		prNum = res.PR.Number
		prBase = res.PR.BaseRef
		prHead = res.PR.HeadOID
		prChecks = res.PR.RequiredChecks
		prGreen = res.PR.RequiredChecksGreen
		prVerProv = res.PR.VerifierProvider
		prVer = firstNonEmpty(res.PR.VerifierProvider, res.PR.IndependentVerifier)
		prVerRef = res.PR.VerifierEvidenceRef
		prVerAtt = res.PR.VerifierAttemptID
		prOwned = res.PR.CreatedByLoopCoder
		prHuman = res.PR.HumanMergeGate
		// Repository inferred from URL when possible (owner/repo).
		prRepo = repoFromPRURL(res.PR.URL)
	}

	in := artifactqual.EmitInput{
		ArchiveDigest: opts.ArchiveDigest, PreProdSHA: opts.PreProdSHA,
		BinaryVersion: opts.BinaryVersion, BinaryCommit: opts.BinaryCommit,
		ProjectID: res.ProjectID, RunID: res.RunID,
		InventoryProvenance:   string(opts.InventoryProvenance),
		InventoryReportDigest: res.InventoryReportDigest,
		Children:              children, ProviderObs: obs, Events: events, Claims: claims,
		ClaudeCatalogReceipts: canaryClaudeCatalogReceipts(res),
		LedgerEntries:         res.CapacityLedgerEntries, EventLogPath: evPath,
		ReuseCount: res.ReuseCount, WorktreePeak: res.WorktreePeak, ProcessPeak: res.ProcessPeak,
		// Occupancy measured from workflowrun.Result at return (never invent zeros).
		WorktreeActive: res.Workflow.WorktreeActive, ProcessActive: res.Workflow.ProcessActive,
		ActiveOccupancyMeasured: true,
		// Interrupted is not evidence of a completed durable resume.
		Resumed:  res.Resumed,
		RepoPath: opts.RepoPath,
		PRURL:    prURL, PRRepository: prRepo, PRBranch: prBranch, PRNumber: prNum,
		PRBaseRef: prBase, PRHeadOID: prHead,
		PRRequiredChecks: prChecks, PRRequiredChecksGreen: prGreen,
		PRIndependentVerifier: prVer, PRVerifierEvidenceRef: prVerRef,
		PRVerifierProvider: prVerProv, PRVerifierAttemptID: prVerAtt,
		PRCreatedByLoopCoder: prOwned, PRAutoMerge: false, PRHumanMergeGate: prHuman || prOwned,
		Unavailable: unavail,
		ProducedAt:  time.Now().UTC(),
	}
	ev, err := artifactqual.EmitCanaryEvidence(in)
	if err != nil {
		return artifactqual.CanaryEvidence{}, err
	}
	if err := artifactqual.WriteCanaryEvidence(opts.OutPath, ev); err != nil {
		return ev, err
	}
	return ev, nil
}

func canaryClaudeCatalogReceipts(res Result) []artifactqual.CanaryClaudeCatalogReceipt {
	out := make([]artifactqual.CanaryClaudeCatalogReceipt, 0, len(res.ClaudeCatalogReceipts))
	for _, receipt := range res.ClaudeCatalogReceipts {
		out = append(out, artifactqual.CanaryClaudeCatalogReceipt{
			InventoryReportDigest: res.InventoryReportDigest,
			Receipt:               receipt,
		})
	}
	return out
}

// repoFromPRURL extracts owner/repo from a GitHub PR URL when possible.
func repoFromPRURL(url string) string {
	url = strings.TrimSpace(url)
	// https://github.com/owner/repo/pull/N
	const marker = "github.com/"
	i := strings.Index(url, marker)
	if i < 0 {
		return ""
	}
	rest := url[i+len(marker):]
	parts := strings.Split(rest, "/")
	if len(parts) < 2 {
		return ""
	}
	return parts[0] + "/" + parts[1]
}

// LoadWorkflowEventsForTest exposes loadWorkflowEvents for package tests.
func LoadWorkflowEventsForTest(res Result, homeDir string) ([]workflowrun.Event, string, error) {
	return loadWorkflowEvents(res, homeDir)
}

// ProofFromResultForTest exposes proofFromResult for package tests.
func ProofFromResultForTest(res Result, events []workflowrun.Event) *UnavailableRetryProof {
	return proofFromResult(res, events)
}

// loadWorkflowEvents is the single authoritative EventLog load for canary emission.
// Prefer exact res.Workflow.EventLogPath (do not reopen a different home-derived path
// when that file exists). Strict JSONL: every non-empty line must parse as Event;
// when project/run are known, every event must match or load fails.
func loadWorkflowEvents(res Result, homeDir string) (events []workflowrun.Event, path string, err error) {
	path = strings.TrimSpace(res.Workflow.EventLogPath)
	if path == "" {
		// Resolve only when exact path is absent.
		homeDir = strings.TrimSpace(homeDir)
		if homeDir != "" && res.ProjectID != "" && res.RunID != "" {
			if el, oerr := workflowrun.OpenEventLog(homeDir, res.ProjectID, res.RunID); oerr == nil {
				path = el.Path()
			}
		}
	}
	if path == "" {
		return nil, "", fmt.Errorf("goalrun: event log path unavailable")
	}
	// When the exact path exists, read it directly — never substitute a different OpenEventLog path.
	if _, serr := os.Stat(path); serr != nil {
		return nil, path, fmt.Errorf("goalrun: event log path: %w", serr)
	}
	events, err = readEventLogJSONLStrict(path, res.ProjectID, res.RunID)
	if err != nil {
		return nil, path, err
	}
	return events, path, nil
}

// readEventLogJSONLStrict uses the single authoritative workflowrun parser.
func readEventLogJSONLStrict(path, expectProject, expectRun string) ([]workflowrun.Event, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return workflowrun.ParseEventJSONLStrict(string(raw), expectProject, expectRun)
}

func canaryChildrenFromReports(res Result) []artifactqual.CanaryChild {
	out := make([]artifactqual.CanaryChild, 0, len(res.Children))
	for _, c := range res.Children {
		if c.Unavailable || c.Provider == "" {
			continue
		}
		depth := c.Depth
		req, sel, inv := depth, depth, depth
		if strings.Contains(c.RouteReason, "requirement=") {
			req = extractKV(c.RouteReason, "requirement")
			sel = extractKV(c.RouteReason, "selection")
			inv = extractKV(c.RouteReason, "invocation")
			if req == "" {
				req = depth
			}
			if sel == "" {
				sel = depth
			}
			if inv == "" {
				inv = depth
			}
		}
		cc := artifactqual.CanaryChild{
			ChildID: c.ChildID, WorkItemID: c.ChildID, TaskClass: c.TaskClass,
			OutputEvidence: c.OutputEvidence,
			AttemptID:      c.AttemptID, Provider: c.Provider, Model: c.Model,
			DepthRequired: req, DepthSelected: sel, DepthInvocation: inv,
			Permission: c.Permission, AccountRef: c.AccountRef, InstallRef: c.InstallRef,
			WindowKind: c.WindowKind,
			Terminal:   c.Terminal, WorktreePath: c.WorktreePath,
			FilesTouched:   append([]string(nil), c.FilesTouched...),
			CapacityBefore: c.CapacityBefore, CapacityReserved: c.CapacityReserved,
			CapacityActual: c.CapacityActual, CapacityAfter: c.CapacityAfter,
			ActualSource: c.ActualSource, ActualConfidence: c.CapacityActualConfidence,
			// Group evidence propagated via CapacityNote / structured ActualSource.
			ArgvDigest: c.ArgvDigest,
			// Structured capacity evidence only — never CapacityNote prose defaults.
			AfterSource: c.CapacityAfterSource, AfterFreshness: c.CapacityAfterFreshness,
			AfterConfidence: c.CapacityAfterConfidence, AfterState: c.CapacityAfterState,
			AfterObservedAt: c.CapacityAfterObservedAt,
			BeforeSource:    c.CapacityBeforeSource, BeforeFreshness: c.CapacityBeforeFreshness,
			BeforeConfidence: c.CapacityBeforeConfidence, BeforeCapturedAt: c.CapacityBeforeCapturedAt,
			// Exact window reset identity (UTC) — never prose.
			ResetAt: copyTimeUTC(c.CapacityResetAt),
			// Same fail-closed gate as collectUsage: terminal succeeded + ArgvDigest
			// + truthful accepted-invocation/provider_stream sources. Failed/fake/
			// planned/auth_binding-only never set RealProviderExecuted.
			RealProviderExecuted: childActuallyExecutedProvider(c),
		}
		// Propagate per-dimension route sources (never collapse into ActualSource).
		if c.ActualSources.Model != "" || c.ActualSources.Effort != "" ||
			c.ActualSources.Permission != "" || c.ActualSources.Account != "" ||
			c.ActualSources.Install != "" {
			cc.ActualSources = &artifactqual.CanaryRouteSources{
				Model: c.ActualSources.Model, Effort: c.ActualSources.Effort,
				Permission: c.ActualSources.Permission, Account: c.ActualSources.Account,
				Install: c.ActualSources.Install,
			}
		}
		out = append(out, cc)
	}
	return out
}

func canaryProviderObsFromReports(res Result) []artifactqual.CanaryProviderObs {
	seen := map[string]bool{}
	var out []artifactqual.CanaryProviderObs
	for _, c := range res.Children {
		p := strings.ToLower(strings.TrimSpace(c.Provider))
		if p == "" || p == "fixture" {
			continue
		}
		// A resumed run legitimately spans multiple immutable inventory
		// snapshots. Emit every distinct structured before/after observation so
		// each raw ledger row can bind to its exact digest instead of being
		// rewritten to the final run snapshot.
		appendObs := func(rem *float64, src, fresh, conf, invDigest string, capAt time.Time) {
			if rem == nil || src == "" || fresh == "" || invDigest == "" || capAt.IsZero() {
				return
			}
			key := strings.Join([]string{
				p, c.AccountRef, c.InstallRef, c.WindowKind, src, fresh, conf,
				invDigest, capAt.UTC().Format(time.RFC3339Nano),
				fmt.Sprintf("%.17g", *rem), formatTimePtr(c.CapacityResetAt),
			}, "\x00")
			if seen[key] {
				return
			}
			seen[key] = true
			out = append(out, artifactqual.CanaryProviderObs{
				Provider: c.Provider, AccountRef: c.AccountRef,
				InstallRef: c.InstallRef, WindowKind: c.WindowKind,
				Source: src, Freshness: fresh, Confidence: conf,
				Remaining: rem, CapturedAt: capAt.UTC(),
				ResetAt:               copyTimeUTC(c.CapacityResetAt),
				InventoryReportDigest: invDigest,
			})
		}
		appendObs(
			c.CapacityBefore, c.CapacityBeforeSource, c.CapacityBeforeFreshness,
			c.CapacityBeforeConfidence, c.CapacityBeforeInventoryDigest,
			c.CapacityBeforeCapturedAt,
		)
		if c.CapacityAfterState == "observed" {
			appendObs(
				c.CapacityAfter, c.CapacityAfterSource, c.CapacityAfterFreshness,
				c.CapacityAfterConfidence, c.CapacityAfterInventoryDigest,
				c.CapacityAfterObservedAt,
			)
		}
	}
	return out
}

func formatTimePtr(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

// copyTimeUTC returns a UTC copy of t, or nil when t is nil.
func copyTimeUTC(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func firstRetryAttempt(res Result) string {
	for _, c := range res.Workflow.Children {
		if strings.TrimSpace(c.SupersedesAttemptID) != "" && strings.TrimSpace(c.AttemptID) != "" {
			return c.AttemptID
		}
	}
	for _, c := range res.Workflow.Children {
		if strings.Contains(c.AttemptID, "-g1") || strings.Contains(c.AttemptID, "-g2") {
			return c.AttemptID
		}
	}
	return ""
}

// hasClaimedModelUnavailableExclude reports any Claimed model_unavailable exclude.
func hasClaimedModelUnavailableExclude(excludes []RouteExclude) bool {
	for _, e := range excludes {
		if e.Claimed && e.Reason == "model_unavailable" {
			return true
		}
	}
	return false
}

// idExact is defined in durable_identity.go as pure byte-exact equality
// (no TrimSpace normalize of durable identity).

// proofFromResult derives concrete UnavailableRetryProof from pre-loaded EventLog
// events, IntegrateCommits, and CapacityTransitions — never CapacityNote prose
// or Terminal==succeeded alone. events must be the authoritative readback; empty
// or incomplete events yield nil (unavailable_retry not_run).
func proofFromResult(res Result, events []workflowrun.Event) *UnavailableRetryProof {
	// Exactly one failed/retry pair: same WorkItemID and SupersedesAttemptID match
	// with byte-exact durable attempt IDs.
	type pair struct{ failed, retry *workflowrun.ChildOutcome }
	var pairs []pair
	for i := range res.Workflow.Children {
		f := &res.Workflow.Children[i]
		if f.FailureClass != "model_unavailable" {
			continue
		}
		for j := range res.Workflow.Children {
			r := &res.Workflow.Children[j]
			if i == j {
				continue
			}
			if !idExact(f.WorkItemID, r.WorkItemID) {
				continue
			}
			if r.SupersedesAttemptID == "" {
				continue
			}
			if !idExact(r.SupersedesAttemptID, f.AttemptID) {
				continue
			}
			pairs = append(pairs, pair{failed: f, retry: r})
		}
	}
	if len(pairs) != 1 {
		return nil
	}
	failed, retry := pairs[0].failed, pairs[0].retry
	if len(events) == 0 {
		return nil
	}

	failedAtt := failed.AttemptID
	retryAtt := retry.AttemptID
	wi := failed.WorkItemID
	if failedAtt == "" || retryAtt == "" || wi == "" || failedAtt == retryAtt {
		return nil
	}

	countKind := func(kind, attempt string) int {
		n := 0
		for _, e := range events {
			if e.Kind == kind && idExact(e.AttemptID, attempt) {
				n++
			}
		}
		return n
	}
	findKind := func(kind, attempt string) (EventSnapshot, bool) {
		for _, e := range events {
			if e.Kind != kind || !idExact(e.AttemptID, attempt) {
				continue
			}
			if e.EventID == "" {
				continue
			}
			// All structured events must carry nonempty WorkItemID equal to wi.
			if e.WorkItemID == "" || !idExact(e.WorkItemID, wi) {
				continue
			}
			return EventSnapshot{
				EventID: e.EventID, Kind: e.Kind, AttemptID: e.AttemptID,
				Generation: e.Generation, WorkItemID: e.WorkItemID,
			}, true
		}
		return EventSnapshot{}, false
	}
	payloadOf := func(eventID string) (map[string]string, bool) {
		for _, e := range events {
			if e.EventID != eventID {
				continue
			}
			if len(e.Payload) == 0 {
				return nil, false
			}
			var m map[string]string
			if json.Unmarshal(e.Payload, &m) != nil {
				return nil, false
			}
			return m, true
		}
		return nil, false
	}

	// Exactly one model_unavailable + one reroute (and claim/launch/terminal) each.
	if countKind("model_unavailable", failedAtt) != 1 {
		return nil
	}
	if countKind("reroute", retryAtt) != 1 {
		return nil
	}
	muEv, okMU := findKind("model_unavailable", failedAtt)
	failedTmEv, okFT := findKind("terminal", failedAtt)
	clEv, okCL := findKind("claim", retryAtt)
	rrEv, okRR := findKind("reroute", retryAtt)
	lnEv, okLN := findKind("launch", retryAtt)
	tmEv, okTM := findKind("terminal", retryAtt)
	if !okMU || !okFT || !okCL || !okRR || !okLN || !okTM {
		return nil
	}

	// rawEvent looks up full event by EventID for Terminal/Evidence/CommitSHA.
	rawEvent := func(eventID string) (workflowrun.Event, bool) {
		for _, e := range events {
			if e.EventID == eventID {
				return e, true
			}
		}
		return workflowrun.Event{}, false
	}

	// WorkItemID nonempty + exact on every required event; one consistent nonzero gen on retry.
	retryGen := 0
	for _, snap := range []EventSnapshot{muEv, failedTmEv, clEv, rrEv, lnEv, tmEv} {
		if snap.WorkItemID == "" || !idExact(snap.WorkItemID, wi) {
			return nil
		}
	}
	for _, snap := range []EventSnapshot{clEv, rrEv, lnEv, tmEv} {
		if snap.Generation <= 0 {
			return nil
		}
		if retryGen == 0 {
			retryGen = snap.Generation
		} else if snap.Generation != retryGen {
			return nil
		}
	}

	// MU payload + Event.Terminal/Evidence must be nonempty exact matches to failed outcome.
	if m, ok := payloadOf(muEv.EventID); !ok ||
		m["work_item_id"] != wi || m["attempt_id"] != failedAtt ||
		m["provider"] != failed.Provider ||
		m["model"] != failed.Model ||
		m["failure_class"] != "model_unavailable" {
		return nil
	}
	muRaw, ok := rawEvent(muEv.EventID)
	if !ok || muRaw.Terminal == "" || muRaw.Evidence == "" {
		return nil
	}
	if muRaw.Terminal != failed.Terminal || muRaw.Evidence != failed.OutputEvidence {
		return nil
	}
	// Failed terminal event + payload are mandatory and exact (not optional/nonempty-only).
	ftRaw, ok := rawEvent(failedTmEv.EventID)
	if !ok || ftRaw.Terminal == "" || ftRaw.Evidence == "" {
		return nil
	}
	if ftRaw.Terminal != failed.Terminal || ftRaw.Evidence != failed.OutputEvidence {
		return nil
	}
	if m, ok := payloadOf(failedTmEv.EventID); !ok ||
		m["terminal"] != failed.Terminal ||
		m["output_evidence"] != failed.OutputEvidence ||
		m["work_item_id"] != wi || m["attempt_id"] != failedAtt {
		return nil
	}

	// Capacity first: len==2 only; bind prior/alternate before reroute exact checks.
	if len(res.Workflow.CapacityTransitions) != 2 {
		return nil
	}
	var priorT, altT workflowrun.CapacityTransition
	priorN, altN := 0, 0
	for _, tr := range res.Workflow.CapacityTransitions {
		switch tr.Role {
		case "prior":
			priorN++
			priorT = tr
		case "alternate":
			altN++
			altT = tr
		default:
			return nil // unknown role
		}
	}
	if priorN != 1 || altN != 1 {
		return nil
	}
	if !idExact(priorT.AttemptID, failedAtt) || !idExact(altT.AttemptID, retryAtt) {
		return nil
	}
	// reconciled => Actual nonnil+Source nonempty; released => Actual nil+Source empty.
	// Requires Model+Depth (and provider/account/window/reservation).
	if !validCapacityTransition(priorT) || !validCapacityTransition(altT) {
		return nil
	}

	// Claim payload attempt_id must equal retry.
	if m, ok := payloadOf(clEv.EventID); !ok ||
		m["work_item_id"] != wi || m["attempt_id"] != retryAtt ||
		m["supersedes_attempt_id"] != failedAtt || m["retry_attempt_id"] != retryAtt {
		return nil
	}
	// Reroute fields must be nonempty exact matches to retry route/outcome/transition.
	// Permission must match retry when outcome carries it (not arbitrary nonempty).
	if m, ok := payloadOf(rrEv.EventID); !ok ||
		m["work_item_id"] != wi ||
		m["supersedes_attempt_id"] != failedAtt || m["retry_attempt_id"] != retryAtt ||
		m["model_unavailable_event_id"] != muEv.EventID || m["claim_event_id"] != clEv.EventID ||
		m["alt_provider"] == "" || m["alt_provider"] != retry.Provider ||
		m["alt_model"] == "" || m["alt_model"] != retry.Model ||
		m["depth"] == "" || m["depth"] != retry.Depth ||
		m["permission"] == "" ||
		(retry.Permission != "" && m["permission"] != retry.Permission) ||
		m["account_ref"] == "" || m["account_ref"] != retry.AccountRef ||
		m["account_ref"] != altT.AccountRef ||
		m["window_kind"] == "" || m["window_kind"] != altT.WindowKind ||
		(retry.WindowKind != "" && m["window_kind"] != retry.WindowKind) ||
		m["reservation_id"] == "" || m["reservation_id"] != altT.ReservationID ||
		(retry.ReservationID != "" && m["reservation_id"] != retry.ReservationID) {
		return nil
	}
	// Launch must carry full route identity exact to retry/alternate.
	if m, ok := payloadOf(lnEv.EventID); !ok ||
		m["work_item_id"] != wi || m["retry_attempt_id"] != retryAtt ||
		m["reroute_event_id"] != rrEv.EventID || m["supersedes_attempt_id"] != failedAtt ||
		m["provider"] == "" || m["provider"] != retry.Provider ||
		m["model"] == "" || m["model"] != retry.Model ||
		m["depth"] == "" || m["depth"] != retry.Depth ||
		m["permission"] == "" ||
		(retry.Permission != "" && m["permission"] != retry.Permission) ||
		m["account_ref"] == "" || m["account_ref"] != retry.AccountRef ||
		m["window_kind"] == "" || m["window_kind"] != altT.WindowKind ||
		m["reservation_id"] == "" || m["reservation_id"] != altT.ReservationID {
		return nil
	}
	// Terminal payload output_evidence required exact; Event.Terminal/Evidence match too.
	if m, ok := payloadOf(tmEv.EventID); !ok ||
		m["work_item_id"] != wi || m["retry_attempt_id"] != retryAtt ||
		m["supersedes_attempt_id"] != failedAtt ||
		m["terminal"] != retry.Terminal ||
		m["output_evidence"] == "" || m["output_evidence"] != retry.OutputEvidence {
		return nil
	}
	if tmRaw, ok := rawEvent(tmEv.EventID); !ok ||
		tmRaw.Terminal != retry.Terminal ||
		tmRaw.Evidence != retry.OutputEvidence {
		return nil
	}

	// RerouteEventRef complete parse: expected event IDs exactly once,
	// supersedes_attempt_id exact failed, retry_attempt_id exact retry, no unknown/dup keys.
	refIDs := map[string]int{}
	refKeys := map[string]int{}
	var refSupersedes, refRetry string
	for _, part := range strings.Split(retry.RerouteEventRef, ";") {
		if part == "" {
			continue
		}
		key, val, ok := strings.Cut(part, "=")
		if !ok || key == "" || val == "" {
			return nil
		}
		refKeys[key]++
		switch key {
		case "event_id":
			refIDs[val]++
		case "supersedes_attempt_id":
			refSupersedes = val
		case "retry_attempt_id":
			refRetry = val
		default:
			return nil // unknown key
		}
	}
	if refKeys["supersedes_attempt_id"] != 1 || refKeys["retry_attempt_id"] != 1 {
		return nil
	}
	if refSupersedes != failedAtt || refRetry != retryAtt {
		return nil
	}
	required := []string{muEv.EventID, clEv.EventID, rrEv.EventID, lnEv.EventID}
	if len(refIDs) != len(required) || refKeys["event_id"] != len(required) {
		return nil
	}
	for _, id := range required {
		if refIDs[id] != 1 {
			return nil
		}
	}

	// Exact integrate counts: commits + event log + ChildOutcome SHA alignment.
	retryIntegrateCommits := 0
	failedIntegrateCommits := 0
	for _, ic := range res.Workflow.IntegrateCommits {
		if idExact(ic.AttemptID, retryAtt) && ic.CommitSHA != "" {
			retryIntegrateCommits++
		}
		if idExact(ic.AttemptID, failedAtt) && ic.CommitSHA != "" {
			failedIntegrateCommits++
		}
	}
	retryIntegrateEvents := countKind("integrate", retryAtt)
	failedIntegrateEvents := countKind("integrate", failedAtt)
	if failedIntegrateCommits != 0 || failedIntegrateEvents != 0 {
		return nil
	}
	retryIntegrated := false
	var integrateEv EventSnapshot
	if sha := retry.IntegrateCommitSHA; sha != "" {
		matched := 0
		for _, ic := range res.Workflow.IntegrateCommits {
			if idExact(ic.AttemptID, retryAtt) && ic.CommitSHA == sha {
				matched++
			}
		}
		if matched != 1 || retryIntegrateCommits != 1 || retryIntegrateEvents != 1 {
			return nil
		}
		intSnap, ok := findKind("integrate", retryAtt)
		if !ok {
			return nil
		}
		if m, ok := payloadOf(intSnap.EventID); !ok ||
			m["work_item_id"] != wi ||
			m["retry_attempt_id"] != retryAtt || m["supersedes_attempt_id"] != failedAtt ||
			m["commit_sha"] != sha {
			return nil
		}
		if intSnap.Generation != retryGen {
			return nil
		}
		// Validate integrate Event.CommitSHA plus payload.
		if intRaw, ok := rawEvent(intSnap.EventID); !ok ||
			intRaw.CommitSHA != sha {
			return nil
		}
		integrateEv = intSnap
		retryIntegrated = true
	} else {
		if retryIntegrateCommits != 0 || retryIntegrateEvents != 0 {
			return nil
		}
	}
	failedIntegrated := failed.IntegrateCommitSHA != ""

	// Exact claim/launch/terminal counts == 1 per generation.
	if countKind("terminal", failedAtt) != 1 || countKind("terminal", retryAtt) != 1 {
		return nil
	}
	if countKind("claim", failedAtt) != 1 || countKind("claim", retryAtt) != 1 {
		return nil
	}
	if countKind("launch", failedAtt) != 1 || countKind("launch", retryAtt) != 1 {
		return nil
	}

	// Exact full identity on capacity vs outcomes (prior + alternate).
	if priorT.Provider != failed.Provider ||
		priorT.Model != failed.Model ||
		(failed.Depth != "" && priorT.Depth != failed.Depth) ||
		(failed.AccountRef != "" && priorT.AccountRef != failed.AccountRef) ||
		(failed.WindowKind != "" && priorT.WindowKind != failed.WindowKind) ||
		(failed.ReservationID != "" && priorT.ReservationID != failed.ReservationID) {
		return nil
	}
	if altT.Provider != retry.Provider ||
		altT.Model != retry.Model ||
		altT.Depth != retry.Depth ||
		altT.AccountRef != retry.AccountRef ||
		(retry.WindowKind != "" && altT.WindowKind != retry.WindowKind) ||
		(retry.ReservationID != "" && altT.ReservationID != retry.ReservationID) ||
		(retry.Permission != "" && altT.Permission != "" && altT.Permission != retry.Permission) {
		return nil
	}

	proof := &UnavailableRetryProof{
		FailedAttemptID:       failedAtt,
		RetryAttemptID:        retryAtt,
		WorkItemID:            wi,
		FailedProvider:        failed.Provider,
		FailedClaimCount:      countKind("claim", failedAtt),
		RetryClaimCount:       countKind("claim", retryAtt),
		FailedLaunchCount:     countKind("launch", failedAtt),
		RetryLaunchCount:      countKind("launch", retryAtt),
		FailedIntegrateCount:  failedIntegrateEvents,
		RetryIntegrateCount:   retryIntegrateEvents,
		FailedTerminalCount:   countKind("terminal", failedAtt),
		RetryTerminalCount:    countKind("terminal", retryAtt),
		FailedClaimClosed:     failed.Terminal != "",
		RetryClaimClosed:      retry.Terminal != "",
		FailedIntegrated:      failedIntegrated,
		RetryIntegrated:       retryIntegrated,
		FailedProductFiles:    append([]string(nil), failed.FilesTouched...),
		RetryProductFiles:     append([]string(nil), retry.FilesTouched...),
		PriorTransition:       priorT,
		AlternateTransition:   altT,
		ModelUnavailableEvent: muEv,
		FailedTerminalEvent:   failedTmEv,
		ClaimEvent:            clEv,
		RerouteEvent:          rrEv,
		LaunchEvent:           lnEv,
		RetryTerminalEvent:    tmEv,
		IntegrateEvent:        integrateEv,
	}
	return proof
}

func extractKV(s, key string) string {
	idx := strings.Index(s, key+"=")
	if idx < 0 {
		return ""
	}
	rest := s[idx+len(key)+1:]
	for i, r := range rest {
		if r == ' ' || r == ';' || r == ',' {
			return rest[:i]
		}
	}
	return rest
}
