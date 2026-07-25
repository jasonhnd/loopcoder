package artifactqual

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/workflowrun"
)

// EmitInput is the exact-binary input set for canary_evidence.v1.
// Do not construct from hand-written booleans: pass raw events + measured results.
type EmitInput struct {
	ArchiveDigest string
	PreProdSHA    string
	BinaryVersion string
	BinaryCommit  string
	ProjectID     string
	RunID         string
	Children      []CanaryChild
	ProviderObs   []CanaryProviderObs
	Events        []workflowrun.Event
	EventLogPath  string
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
	repoChecked, repoPresent := measureRepoLocalRuntime(in.RepoPath)
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
		Schema:               SchemaCanaryEvidence,
		ArchiveDigest:        in.ArchiveDigest,
		PreProdSHA:           in.PreProdSHA,
		BinaryVersion:        in.BinaryVersion,
		BinaryCommit:         in.BinaryCommit,
		ProjectID:            in.ProjectID,
		RunID:                in.RunID,
		ProviderObservations: in.ProviderObs,
		Children:             in.Children,
		UnavailableRetry:     in.Unavailable,
		Restart:              restart,
		PR:                   pr,
		ProducedAt:           at,
	}
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
		fc = strings.ToLower(strings.TrimSpace(fc))
		ic = strings.ToLower(strings.TrimSpace(ic))
		switch fc {
		case "forced_interrupt", "hard_kill_recovery":
			// ok
		default:
			return false
		}
		switch ic {
		case "service_forced_interrupt", "hard_kill_recovery", "forced_interrupt":
			return true
		default:
			// Accept failure_class alone when interrupt_class empty on legacy.
			return ic == "" && (fc == "forced_interrupt" || fc == "hard_kill_recovery")
		}
	}

	for _, e := range events {
		wi := strings.TrimSpace(e.WorkItemID)
		att := strings.TrimSpace(e.AttemptID)
		pm := payloadMap(e)
		switch e.Kind {
		case "interrupt":
			out.Interrupted = true
			fc := firstNonEmpty(e.FailureClass, pm["failure_class"])
			ic := firstNonEmpty(pm["interrupt_class"], "")
			intID := strings.TrimSpace(pm["interrupt_id"])
			if wi == "" || att == "" || intID == "" || !isForcedClass(fc, ic) {
				continue
			}
			interrupts[attKey{wi, att}] = intPair{wi: wi, att: att, intID: intID, fc: fc, ic: ic}
		case "terminal":
			term := strings.ToLower(strings.TrimSpace(e.Terminal))
			if term == "" {
				term = strings.ToLower(strings.TrimSpace(pm["terminal"]))
			}
			fc := firstNonEmpty(e.FailureClass, pm["failure_class"])
			ic := firstNonEmpty(pm["interrupt_class"], "")
			intID := strings.TrimSpace(pm["interrupt_id"])
			k := attKey{wi, att}
			if term == "cancelled" && wi != "" && att != "" && intID != "" {
				if ip, ok := interrupts[k]; ok && ip.intID == intID && isForcedClass(fc, ic) {
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
					if g <= 0 && e.Generation > 0 {
						g = e.Generation
					}
					launchGens[wi] = append(launchGens[wi], g)
				}
			}
		case "reuse":
			reuseCount++
		}
	}
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
	// Durable resume flag also contributes when events show reuse after abort.
	if resumed && reuseCount > 0 && len(out.AbortedAttempts) > 0 && !out.LaterGenerationResume {
		// Still require a distinct later generation in the ledger when possible.
		// If generation parse fails but reuse exists with aborted, keep false
		// (fail closed) unless a second launch string differs.
		for wi, att := range out.AbortedAttempts {
			for _, e := range events {
				if e.Kind != "launch" || strings.TrimSpace(e.WorkItemID) != wi {
					continue
				}
				if strings.TrimSpace(e.AttemptID) != "" && strings.TrimSpace(e.AttemptID) != att {
					out.LaterGenerationResume = true
					break
				}
			}
		}
	}
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

// measureRepoLocalRuntime lstats <repo>/.loopcoder.
// Returns (checked=false, present=false) on empty path or measurement error (fail closed).
// Returns (true, true) when the path exists; (true, false) when absent (ErrNotExist).
func measureRepoLocalRuntime(repoPath string) (checked bool, present bool) {
	repoPath = strings.TrimSpace(repoPath)
	if repoPath == "" {
		return false, false
	}
	p := filepath.Join(repoPath, ".loopcoder")
	_, err := os.Lstat(p)
	if err == nil {
		return true, true
	}
	if os.IsNotExist(err) {
		return true, false
	}
	// Permission / IO error: cannot claim clean.
	return false, false
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
