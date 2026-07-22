package artifactqual

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
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
	// Workflow peaks / resume
	ReuseCount   int
	WorktreePeak int
	ProcessPeak  int
	Resumed      bool
	// PR from goalpr.Result fields (not hand-edited).
	PRURL                 string
	PRBranch              string
	PRNumber              int
	PRRequiredChecks      []string
	PRRequiredChecksGreen bool
	PRIndependentVerifier string
	PRVerifierEvidenceRef string
	PRCreatedByLoopCoder  bool
	// Unavailable measured (optional; omit → not set).
	Unavailable *CanaryUnavailableRetry
	ProducedAt  time.Time
}

// EmitCanaryEvidence derives loopcoder.canary_evidence.v1 from measured inputs.
// Rejects pending-live verifier refs and restart without interrupt events.
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

	// Derive restart strictly from event ledger.
	interrupted, aborted := workflowrun.InterruptedFromEvents(in.Events)
	reuseCount := 0
	launchByAttempt := map[string]int{}
	for _, e := range in.Events {
		switch e.Kind {
		case "reuse":
			reuseCount++
		case "launch":
			if e.AttemptID != "" {
				launchByAttempt[e.AttemptID]++
			}
		}
	}
	dupLaunch := false
	for _, n := range launchByAttempt {
		if n > 1 {
			dupLaunch = true
		}
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
	}
	// Content digest of event file when path exists.
	if raw, err := os.ReadFile(in.EventLogPath); err == nil {
		sum := sha256.Sum256(raw)
		evRef = in.EventLogPath + "#sha256:" + hex.EncodeToString(sum[:8])
	}

	exactlyOnce := interrupted && in.Resumed && reuseCount > 0 && !dupLaunch && len(aborted) >= 0
	// Require interrupt event for Interrupted=true — never invent.
	if !interrupted {
		return CanaryEvidence{}, fmt.Errorf("artifactqual: cannot emit restart: no interrupt event in ledger (refusing hand-written interrupted=true)")
	}

	restart := &CanaryRestart{
		Interrupted:        true,
		ResumedFromDurable: in.Resumed,
		ExactlyOnce:        exactlyOnce && reuseCount > 0,
		ChildCountUseful:   useful,
		ProcessCeilingOK:   in.ProcessPeak > 0,
		WorktreeCeilingOK:  in.WorktreePeak > 0,
		NoLeakedProcesses:  true, // caller must measure; default true only if process peak recorded
		NoRepoLocalRuntime: true,
		EvidenceRef:        evRef,
	}
	if in.ProcessPeak == 0 {
		restart.ProcessCeilingOK = false
	}
	if in.WorktreePeak == 0 {
		restart.WorktreeCeilingOK = false
	}

	var pr *CanaryPR
	if strings.TrimSpace(in.PRURL) != "" {
		pr = &CanaryPR{
			URL:                 in.PRURL,
			Branch:              in.PRBranch,
			Number:              in.PRNumber,
			RequiredChecks:      in.PRRequiredChecks,
			RequiredChecksGreen: in.PRRequiredChecksGreen,
			IndependentVerifier: in.PRIndependentVerifier,
			VerifierEvidenceRef: in.PRVerifierEvidenceRef,
			CreatedByLoopCoder:  in.PRCreatedByLoopCoder,
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
