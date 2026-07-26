package artifactqual

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/capacityledger"
	"github.com/jasonhnd/loopcoder/internal/workclaim"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
)

// EmitInput is the exact-binary input set for canary_evidence.v1.
// Do not construct from hand-written booleans: pass raw events + measured results.
type EmitInput struct {
	ArchiveDigest         string
	PreProdSHA            string
	BinaryVersion         string
	BinaryCommit          string
	ProjectID             string
	RunID                 string
	InventoryProvenance   string
	InventoryReportDigest string
	Children              []CanaryChild
	ProviderObs           []CanaryProviderObs
	ClaudeCatalogReceipts []CanaryClaudeCatalogReceipt
	Events                []workflowrun.Event
	Claims                []workclaim.Claim
	LedgerEntries         []capacityledger.Entry
	EventLogPath          string
	// Workflow peaks / resume / occupancy (measured from workflowrun.Result).
	ReuseCount              int
	WorktreePeak            int
	ProcessPeak             int
	WorktreeActive          int
	ProcessActive           int
	ActiveOccupancyMeasured bool // true when Active fields come from workflowrun.Result
	Resumed                 bool
	// RepoPath is the project repo root; used only to lstat <repo>/.loopcoder.
	// Empty path or lstat error fails closed (cannot claim NoRepoLocalRuntime).
	RepoPath string
	// PR from goalpr.Result fields (not hand-edited).
	PRURL                 string
	PRRepository          string
	PRBranch              string
	PRNumber              int
	PRBaseRef             string
	PRHeadOID             string
	PRRequiredChecks      []string
	PRRequiredChecksGreen bool
	PRIndependentVerifier string
	PRVerifierEvidenceRef string
	PRVerifierProvider    string
	PRVerifierAttemptID   string
	PRCreatedByLoopCoder  bool
	PRAutoMerge           bool // must be false
	PRHumanMergeGate      bool // must be true when PR present
	// Unavailable measured (optional; omit → not set).
	Unavailable *CanaryUnavailableRetry
	ProducedAt  time.Time
}

// EmitCanaryEvidence derives loopcoder.canary_evidence.v1 from measured inputs.
// Rejects pending-live verifier refs and restart without interrupt events.
// Never invents true restart/ceiling/leak/repo-local flags.
func EmitCanaryEvidence(in EmitInput) (CanaryEvidence, error) {
	if strings.TrimSpace(in.ArchiveDigest) == "" || strings.TrimSpace(in.PreProdSHA) == "" {
		return CanaryEvidence{}, fmt.Errorf("artifactqual: archive digest and pre-prod sha required")
	}
	if strings.TrimSpace(in.ProjectID) == "" || in.ProjectID == "local-project" {
		return CanaryEvidence{}, fmt.Errorf("artifactqual: unique disposable project_id required")
	}
	if strings.TrimSpace(in.RunID) == "" {
		return CanaryEvidence{}, fmt.Errorf("artifactqual: run_id required")
	}
	if in.InventoryProvenance != "live_discover" {
		return CanaryEvidence{}, fmt.Errorf("artifactqual: exact live_discover inventory provenance required")
	}
	if in.InventoryReportDigest == "" || in.InventoryReportDigest != strings.TrimSpace(in.InventoryReportDigest) {
		return CanaryEvidence{}, fmt.Errorf("artifactqual: exact inventory report digest required")
	}
	if strings.Contains(strings.ToLower(in.PRVerifierEvidenceRef), "pending") {
		return CanaryEvidence{}, fmt.Errorf("artifactqual: refuse pending-live verifier evidence")
	}

	at := in.ProducedAt
	if at.IsZero() {
		at = time.Now().UTC()
	}

	// Forced-restart section only when durable interrupt evidence is present.
	// Never invent Restart=true from caller flags alone; when the event log has
	// no interrupt, Restart stays nil (honest not-run) so unavailable_retry /
	// other metrics can still emit from the same production run.
	restartEv := deriveForcedRestartEvidence(in.Events, in.Resumed)
	// Refuse Resumed=true without interrupt evidence (hand-written resume flag).
	if in.Resumed && !restartEv.Interrupted {
		return CanaryEvidence{}, fmt.Errorf("artifactqual: resumed=true without interrupt event in ledger (refusing hand-written resume)")
	}

	useful := 0
	for _, c := range in.Children {
		if c.Terminal == "succeeded" && c.RealProviderExecuted {
			useful++
		}
	}
	evRef := strings.TrimSpace(in.EventLogPath)
	if evRef == "" {
		evRef = "events:inline"
	} else {
		// Supplied EventLogPath must be readable; never degrade to unhashed path on error.
		raw, err := os.ReadFile(in.EventLogPath)
		if err != nil {
			return CanaryEvidence{}, fmt.Errorf("artifactqual: EventLogPath read: %w", err)
		}
		sum := sha256.Sum256(raw)
		evRef = in.EventLogPath + "#sha256:" + hex.EncodeToString(sum[:8])
	}

	processLimit := ProductionSequentialCeiling
	worktreeLimit := ProductionSequentialCeiling
	processCeilingOK := in.ProcessPeak > 0 && in.ProcessPeak <= processLimit
	worktreeCeilingOK := in.WorktreePeak > 0 && in.WorktreePeak <= worktreeLimit

	// Occupancy: only claim clean when measured and both active counters are zero.
	noLeaked := false
	if in.ActiveOccupancyMeasured {
		noLeaked = in.ProcessActive == 0 && in.WorktreeActive == 0
	}

	// Repo-local runtime: lstat <repo>/.loopcoder — never invent clean.
	repoChecked, repoPresent := measureRepoLocalRuntime(in.RepoPath, in.ProjectID, in.RunID)
	noRepoLocal := repoChecked && !repoPresent

	var restart *CanaryRestart
	if restartEv.Interrupted {
		aborted := restartEv.AbortedAttempts
		reuseCount := restartEv.ReuseCount
		dupLaunch := restartEv.DuplicateLaunch
		dupSuccessIntegrate := restartEv.DuplicateSuccessIntegrate
		abortedSucceeded := restartEv.AbortedAttemptSucceeded
		laterGenResume := restartEv.LaterGenerationResume
		// Exactly-once: typed forced-interrupt abort + durable resume + later gen +
		// reuse + no dup launch/success + aborted never succeeded/integrated.
		exactlyOnce := restartEv.Interrupted && in.Resumed && laterGenResume &&
			len(aborted) > 0 && reuseCount > 0 &&
			!dupLaunch && !dupSuccessIntegrate && !abortedSucceeded
		restart = &CanaryRestart{
			Interrupted:               true,
			ResumedFromDurable:        in.Resumed,
			ExactlyOnce:               exactlyOnce,
			ChildCountUseful:          useful,
			ProcessCeilingOK:          processCeilingOK,
			WorktreeCeilingOK:         worktreeCeilingOK,
			NoLeakedProcesses:         noLeaked,
			NoRepoLocalRuntime:        noRepoLocal,
			EvidenceRef:               evRef,
			ProcessPeak:               in.ProcessPeak,
			WorktreePeak:              in.WorktreePeak,
			ProcessActive:             in.ProcessActive,
			WorktreeActive:            in.WorktreeActive,
			ProcessLimit:              processLimit,
			WorktreeLimit:             worktreeLimit,
			ReuseCountMeasured:        reuseCount,
			AbortedAttemptCount:       len(aborted),
			ActiveOccupancyMeasured:   in.ActiveOccupancyMeasured,
			RepoLocalRuntimeChecked:   repoChecked,
			RepoLocalRuntimePresent:   repoPresent,
			DuplicateLaunch:           dupLaunch,
			DuplicateSuccessIntegrate: dupSuccessIntegrate,
			AbortedAttemptSucceeded:   abortedSucceeded,
			LaterGenerationResume:     laterGenResume,
		}
	}

	var pr *CanaryPR
	if strings.TrimSpace(in.PRURL) != "" {
		pr = &CanaryPR{
			URL:                 in.PRURL,
			Repository:          in.PRRepository,
			Branch:              in.PRBranch,
			Number:              in.PRNumber,
			BaseRef:             in.PRBaseRef,
			HeadOID:             in.PRHeadOID,
			RequiredChecks:      in.PRRequiredChecks,
			RequiredChecksGreen: in.PRRequiredChecksGreen,
			IndependentVerifier: in.PRIndependentVerifier,
			VerifierEvidenceRef: in.PRVerifierEvidenceRef,
			VerifierProvider:    firstNonEmpty(in.PRVerifierProvider, in.PRIndependentVerifier),
			VerifierAttemptID:   in.PRVerifierAttemptID,
			CreatedByLoopCoder:  in.PRCreatedByLoopCoder,
			AutoMerge:           in.PRAutoMerge, // caller must leave false
			HumanMergeGate:      in.PRHumanMergeGate || in.PRCreatedByLoopCoder,
		}
		// Fail closed: never emit auto_merge=true.
		if pr.AutoMerge {
			return CanaryEvidence{}, fmt.Errorf("artifactqual: refuse PR with auto_merge=true")
		}
	}

	ev := CanaryEvidence{
		Schema:                SchemaCanaryEvidence,
		ArchiveDigest:         in.ArchiveDigest,
		PreProdSHA:            in.PreProdSHA,
		BinaryVersion:         in.BinaryVersion,
		BinaryCommit:          in.BinaryCommit,
		ProjectID:             in.ProjectID,
		RunID:                 in.RunID,
		InventoryProvenance:   in.InventoryProvenance,
		InventoryReportDigest: in.InventoryReportDigest,
		ProviderObservations:  in.ProviderObs,
		ClaudeCatalogReceipts: append([]CanaryClaudeCatalogReceipt(nil), in.ClaudeCatalogReceipts...),
		Children:              in.Children,
		UnavailableRetry:      in.Unavailable,
		Restart:               restart,
		PR:                    pr,
		RawEvents:             append([]workflowrun.Event(nil), in.Events...),
		RawClaims:             append([]workclaim.Claim(nil), in.Claims...),
		RawLedgerEntries:      append([]capacityledger.Entry(nil), in.LedgerEntries...),
		ProducedAt:            at,
	}
	ev.DurableEvidenceDigest = DigestDurableEvidence(ev)
	ev.ContentDigest = DigestCanaryBody(ev)
	return ev, nil
}

// forcedRestartEvidence is canary-only derivation of production forced-restart
// semantics. Does not alter workflowrun.InterruptedFromEvents recovery invariants.
type forcedRestartEvidence struct {
	Interrupted               bool
	AbortedAttempts           map[string]string // workItemID → aborted attemptID
	ReuseCount                int
	DuplicateLaunch           bool
	DuplicateSuccessIntegrate bool
	AbortedAttemptSucceeded   bool
	LaterGenerationResume     bool
}

// deriveForcedRestartEvidence finds typed forced-interrupt pairs:
// launch → interrupt(failure_class+interrupt_class+interrupt_id) →
// terminal(cancelled, same interrupt_id) → later generation launch + reuse.
// Cancelled terminals count as real aborted attempts (unlike open-attempt maps
// that drop terminalized attempts).
func deriveForcedRestartEvidence(events []workflowrun.Event, resumed bool) forcedRestartEvidence {
	out := forcedRestartEvidence{AbortedAttempts: map[string]string{}}
	type attKey struct{ wi, att string }
	type intPair struct {
		wi, att, intID, fc, ic string
	}
	interrupts := map[attKey]intPair{} // interrupt by attempt
	cancelledAbort := map[attKey]intPair{}
	succeeded := map[attKey]bool{}
	integrated := map[attKey]bool{}
	successTermByWI := map[string]int{}
	integrateByWI := map[string]int{}
	launchByAttempt := map[string]int{}
	launchGens := map[string][]int{} // wi → generations launched
	reuseCount := 0

	payloadMap := func(e workflowrun.Event) map[string]string {
		m := map[string]string{}
		if len(e.Payload) == 0 {
			return m
		}
		_ = json.Unmarshal(e.Payload, &m)
		return m
	}
	isForcedClass := func(fc, ic string) bool {
		return (fc == "forced_interrupt" && ic == "service_forced_interrupt") ||
			(fc == "hard_kill_recovery" && ic == "hard_kill_recovery")
	}

	for _, e := range events {
		wi := e.WorkItemID
		att := e.AttemptID
		pm := payloadMap(e)
		switch e.Kind {
		case "interrupt":
			fc := e.FailureClass
			if fc == "" {
				fc = pm["failure_class"]
			}
			ic := pm["interrupt_class"]
			intID := pm["interrupt_id"]
			if wi == "" || att == "" || intID == "" || !isForcedClass(fc, ic) {
				continue
			}
			interrupts[attKey{wi, att}] = intPair{wi: wi, att: att, intID: intID, fc: fc, ic: ic}
		case "terminal":
			term := e.Terminal
			if term == "" {
				term = pm["terminal"]
			}
			fc := e.FailureClass
			if fc == "" {
				fc = pm["failure_class"]
			}
			ic := pm["interrupt_class"]
			intID := pm["interrupt_id"]
			k := attKey{wi, att}
			if term == "cancelled" && wi != "" && att != "" && intID != "" {
				if ip, ok := interrupts[k]; ok &&
					ip.intID == intID && ip.fc == fc && ip.ic == ic &&
					isForcedClass(fc, ic) {
					cancelledAbort[k] = ip
					out.AbortedAttempts[wi] = att
				}
			}
			if term == "succeeded" && wi != "" {
				successTermByWI[wi]++
				if att != "" {
					succeeded[k] = true
				}
			}
		case "integrate":
			if wi != "" {
				integrateByWI[wi]++
				if att != "" {
					integrated[attKey{wi, att}] = true
				}
			}
		case "launch":
			if att != "" {
				launchByAttempt[att]++
				if wi != "" {
					g := workflowrun.ParseAttemptGeneration(att)
					launchGens[wi] = append(launchGens[wi], g)
				}
			}
		case "reuse":
			reuseCount++
		}
	}
	out.Interrupted = len(cancelledAbort) > 0
	out.ReuseCount = reuseCount
	for _, n := range launchByAttempt {
		if n > 1 {
			out.DuplicateLaunch = true
			break
		}
	}
	for wi, n := range successTermByWI {
		if n > 1 {
			out.DuplicateSuccessIntegrate = true
			break
		}
		if integrateByWI[wi] > 1 {
			out.DuplicateSuccessIntegrate = true
			break
		}
	}
	// Aborted attempt must not have succeeded or integrated.
	for k := range cancelledAbort {
		if succeeded[k] || integrated[k] {
			out.AbortedAttemptSucceeded = true
			break
		}
	}
	// Later generation/resume: for each aborted attempt, a higher generation launch exists.
	for wi, att := range out.AbortedAttempts {
		ag := workflowrun.ParseAttemptGeneration(att)
		for _, g := range launchGens[wi] {
			if g > ag {
				out.LaterGenerationResume = true
				break
			}
		}
	}
	_ = resumed // caller state never manufactures a higher generation.
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// measureRepoLocalRuntime inspects <repo>/.loopcoder.
// Returns (checked=false, present=false) on empty path or measurement error (fail closed).
// Returns (true, true) when the path exists; (true, false) when absent (ErrNotExist).
// The sole exception is the exact, regular, git-tracked goal-pr receipt owned by
// this run. goalpr deliberately force-adds that immutable human-gate evidence
// under an otherwise ignored namespace; it is product evidence, not live
// runtime state. Any sibling, symlink, untracked receipt, or path mismatch still
// reports runtime present.
func measureRepoLocalRuntime(repoPath, projectID, runID string) (checked bool, present bool) {
	repoPath = strings.TrimSpace(repoPath)
	if repoPath == "" {
		return false, false
	}
	p := filepath.Join(repoPath, ".loopcoder")
	info, err := os.Lstat(p)
	if os.IsNotExist(err) {
		return true, false
	}
	if err != nil {
		// Permission / IO error: cannot claim clean.
		return false, false
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return true, true
	}
	if runID == "" || runID != strings.TrimSpace(runID) ||
		filepath.Base(runID) != runID || strings.ContainsAny(runID, `/\`) {
		return true, true
	}
	wantRel := filepath.ToSlash(filepath.Join(".loopcoder", "goal-pr", runID+"-receipt.json"))
	wantAbs := filepath.Join(repoPath, filepath.FromSlash(wantRel))
	files := 0
	walkErr := filepath.Walk(p, func(path string, entry os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, relErr := filepath.Rel(repoPath, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if entry.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink not allowed: %s", rel)
		}
		if entry.IsDir() {
			if rel != ".loopcoder" && rel != ".loopcoder/goal-pr" {
				return fmt.Errorf("unexpected runtime directory: %s", rel)
			}
			return nil
		}
		if !entry.Mode().IsRegular() || path != wantAbs || rel != wantRel {
			return fmt.Errorf("unexpected runtime file: %s", rel)
		}
		files++
		return nil
	})
	if walkErr != nil || files != 1 {
		return true, true
	}
	if info, statErr := os.Stat(wantAbs); statErr != nil || info.Size() <= 0 || info.Size() > 8<<20 {
		return true, true
	}
	rawReceipt, readErr := os.ReadFile(wantAbs)
	if readErr != nil {
		return true, true
	}
	var receipt struct {
		Schema    string `json:"schema"`
		ProjectID string `json:"project_id"`
		RunID     string `json:"run_id"`
		HumanGate bool   `json:"human_gate"`
		AutoMerge bool   `json:"auto_merge"`
	}
	if json.Unmarshal(rawReceipt, &receipt) != nil ||
		receipt.Schema != "loopcoder.goalpr.receipt.v1" ||
		receipt.ProjectID != projectID ||
		receipt.RunID != runID || !receipt.HumanGate || receipt.AutoMerge {
		return true, true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "ls-files", "--error-unmatch", "--", wantRel)
	raw, err := cmd.Output()
	if err != nil || string(raw) != wantRel+"\n" {
		return true, true
	}
	committed, err := exec.CommandContext(ctx, "git", "-C", repoPath, "show", "HEAD:"+wantRel).Output()
	if err != nil || !bytes.Equal(committed, rawReceipt) {
		return true, true
	}
	return true, false
}

// WriteCanaryEvidence writes the manifest atomically (owner-only mode).
func WriteCanaryEvidence(path string, ev CanaryEvidence) error {
	raw, err := json.MarshalIndent(ev, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
