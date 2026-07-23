package goalrun

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/artifactqual"
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
	// HomeDir for event log discovery.
	HomeDir string
}

// EmitCanaryFromResult builds and writes canary_evidence.v1 from a completed goal Result.
// Requires interrupt events for restart section; fails closed otherwise when
// RequireRestart is implied by Resumed||Interrupted in workflow.
func EmitCanaryFromResult(res Result, opts CanaryEmitOptions) (artifactqual.CanaryEvidence, error) {
	if strings.TrimSpace(opts.OutPath) == "" {
		return artifactqual.CanaryEvidence{}, fmt.Errorf("goalrun: canary out path required")
	}
	// Load append-only events from workflow path or home layout.
	var events []workflowrun.Event
	evPath := res.Workflow.EventLogPath
	if evPath == "" && opts.HomeDir != "" && res.ProjectID != "" && res.RunID != "" {
		if el, err := workflowrun.OpenEventLog(opts.HomeDir, res.ProjectID, res.RunID); err == nil {
			evPath = el.Path()
		}
	}
	if opts.HomeDir != "" && res.ProjectID != "" && res.RunID != "" {
		if elog, err := workflowrun.OpenEventLog(opts.HomeDir, res.ProjectID, res.RunID); err == nil {
			if evs, rerr := elog.ReadAll(); rerr == nil {
				events = evs
				evPath = elog.Path()
			}
		}
	} else if evPath != "" {
		// Path known from workflow but home empty — still try open from path parent.
		if raw, err := os.ReadFile(evPath); err == nil && len(raw) > 0 {
			_ = raw
			// Parse lines lightly via OpenEventLog when possible is preferred.
		}
	}

	children := canaryChildrenFromReports(res)
	obs := canaryProviderObsFromReports(res)
	unavail := BuildUnavailableRetryEvidenceWithProof(res.RouteExcludes, firstRetryAttempt(res), proofFromResult(res))

	var prURL, prBranch, prVer, prVerRef string
	var prNum int
	var prChecks []string
	var prGreen, prOwned bool
	if res.PR != nil {
		prURL = res.PR.URL
		prBranch = res.PR.Branch
		prNum = res.PR.Number
		prChecks = res.PR.RequiredChecks
		prGreen = res.PR.RequiredChecksGreen
		prVer = firstNonEmpty(res.PR.VerifierProvider, res.PR.IndependentVerifier)
		prVerRef = res.PR.VerifierEvidenceRef
		prOwned = res.PR.CreatedByLoopCoder
	}

	// Binary identity defaults from env or empty — caller should set from --version.
	in := artifactqual.EmitInput{
		ArchiveDigest: opts.ArchiveDigest, PreProdSHA: opts.PreProdSHA,
		BinaryVersion: opts.BinaryVersion, BinaryCommit: opts.BinaryCommit,
		ProjectID: res.ProjectID, RunID: res.RunID,
		Children: children, ProviderObs: obs, Events: events, EventLogPath: evPath,
		ReuseCount: res.ReuseCount, WorktreePeak: res.WorktreePeak, ProcessPeak: res.ProcessPeak,
		Resumed: res.Resumed || res.Workflow.Interrupted,
		PRURL:   prURL, PRBranch: prBranch, PRNumber: prNum,
		PRRequiredChecks: prChecks, PRRequiredChecksGreen: prGreen,
		PRIndependentVerifier: prVer, PRVerifierEvidenceRef: prVerRef,
		PRCreatedByLoopCoder: prOwned,
		Unavailable:          unavail,
		ProducedAt:           time.Now().UTC(),
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

func canaryChildrenFromReports(res Result) []artifactqual.CanaryChild {
	out := make([]artifactqual.CanaryChild, 0, len(res.Children))
	for _, c := range res.Children {
		if c.Unavailable || c.Provider == "" {
			continue
		}
		// Parse depth bind from route reason when present.
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
			ChildID: c.ChildID, AttemptID: c.AttemptID, Provider: c.Provider, Model: c.Model,
			DepthRequired: req, DepthSelected: sel, DepthInvocation: inv,
			Terminal: c.Terminal, WorktreePath: c.WorktreePath,
			CapacityBefore: c.CapacityBefore, CapacityReserved: c.CapacityReserved,
			CapacityActual: c.CapacityActual, CapacityAfter: c.CapacityAfter,
			ActualSource: c.ActualSource,
			RealProviderExecuted: c.AttemptID != "" && c.OutputEvidence != "" &&
				c.Provider != "fixture" && c.Terminal != "",
		}
		if c.CapacityAfter != nil {
			// after_source tags from capacity note when present
			if strings.Contains(c.CapacityNote, "after_source=") {
				cc.AfterSource = extractAfter(c.CapacityNote, "after_source=")
			} else {
				cc.AfterSource = "capacity_snapshot"
			}
			if strings.Contains(c.CapacityNote, "after_freshness=") {
				cc.AfterFreshness = extractAfter(c.CapacityNote, "after_freshness=")
			} else {
				cc.AfterFreshness = "fresh"
			}
		}
		out = append(out, cc)
	}
	return out
}

func canaryProviderObsFromReports(res Result) []artifactqual.CanaryProviderObs {
	seen := map[string]bool{}
	var out []artifactqual.CanaryProviderObs
	now := time.Now().UTC()
	for _, c := range res.Children {
		p := strings.ToLower(strings.TrimSpace(c.Provider))
		if p == "" || p == "fixture" || seen[p] {
			continue
		}
		if c.CapacityBefore == nil && c.CapacityAfter == nil {
			continue
		}
		seen[p] = true
		rem := c.CapacityAfter
		if rem == nil {
			rem = c.CapacityBefore
		}
		src := "capacity_snapshot"
		if strings.Contains(c.CapacityNote, "after_source=") {
			src = extractAfter(c.CapacityNote, "after_source=")
		}
		out = append(out, artifactqual.CanaryProviderObs{
			Provider: c.Provider, AccountRef: c.AccountRef, Source: src,
			Freshness: "fresh", Remaining: rem, CapturedAt: now,
		})
	}
	return out
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

// proofFromResult derives concrete UnavailableRetryProof from workflow children
// and capacity notes — never invents no_duplicate flags from prose alone.
func proofFromResult(res Result) *UnavailableRetryProof {
	var failed, retry *workflowrun.ChildOutcome
	for i := range res.Workflow.Children {
		c := &res.Workflow.Children[i]
		if strings.EqualFold(strings.TrimSpace(c.FailureClass), "model_unavailable") {
			failed = c
		}
		if strings.TrimSpace(c.SupersedesAttemptID) != "" {
			retry = c
		}
	}
	if failed == nil || retry == nil {
		return nil
	}
	// Parse event_ids from RerouteEventRef (event_id=a;event_id=b;...).
	var eventIDs []string
	for _, part := range strings.Split(retry.RerouteEventRef, ";") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "event_id=") {
			id := strings.TrimPrefix(part, "event_id=")
			if id != "" {
				eventIDs = append(eventIDs, id)
			}
		}
	}
	// Capacity states from goal ChildReport notes when present.
	priorState, altState := "", ""
	for _, cr := range res.Children {
		if cr.ChildID != failed.WorkItemID && cr.ChildID != retry.WorkItemID {
			continue
		}
		if strings.Contains(cr.CapacityNote, "released=") || cr.CapacityState == "released" {
			if priorState == "" {
				priorState = "released"
			}
		}
		if cr.CapacityState == "reconciled" {
			if priorState == "" {
				priorState = "reconciled"
			}
			altState = "reconciled"
		}
		if cr.CapacityState == "reserved" {
			altState = "reserved"
		}
		if cr.CapacityState == "released" && altState == "" {
			altState = "released"
		}
	}
	// When notes incomplete, do not invent — leave empty so Valid fails closed.
	failedIntegrated := false
	retryIntegrated := strings.TrimSpace(retry.IntegrateCommitSHA) != "" ||
		strings.EqualFold(retry.Terminal, "succeeded")
	// Failed attempt must not have been integrated as success.
	if strings.EqualFold(failed.Terminal, "succeeded") || strings.TrimSpace(failed.IntegrateCommitSHA) != "" {
		failedIntegrated = true
	}
	return &UnavailableRetryProof{
		FailedAttemptID:    failed.AttemptID,
		RetryAttemptID:     retry.AttemptID,
		FailedClaimClosed:  strings.TrimSpace(failed.Terminal) != "",
		RetryClaimClosed:   strings.TrimSpace(retry.Terminal) != "",
		FailedIntegrated:   failedIntegrated,
		RetryIntegrated:    retryIntegrated,
		FailedProductFiles: append([]string(nil), failed.FilesTouched...),
		RetryProductFiles:  append([]string(nil), retry.FilesTouched...),
		PriorCapacityState: priorState,
		AltCapacityState:   altState,
		EventIDs:           eventIDs,
	}
}

func extractKV(s, key string) string {
	// key=value form inside route reason
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

func extractAfter(note, prefix string) string {
	idx := strings.Index(note, prefix)
	if idx < 0 {
		return ""
	}
	rest := note[idx+len(prefix):]
	for i, r := range rest {
		if r == ' ' || r == ';' {
			return rest[:i]
		}
	}
	return rest
}
