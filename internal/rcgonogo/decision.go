package rcgonogo

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/installsmoke"
	"github.com/jasonhnd/loopcoder/internal/packdarwin"
	"github.com/jasonhnd/loopcoder/internal/releaseslo"
)

// SchemaRecord is the GO/NO-GO decision record schema.
const SchemaRecord = "loopcoder.rcgonogo.decision.v1"

// Decision is GO or NO_GO.
type Decision string

const (
	DecisionGO   Decision = "GO"
	DecisionNOGO Decision = "NO_GO"
)

// CanaryID is one consumer canary class.
type CanaryID string

const (
	CanaryPublicPrivate   CanaryID = "public_private_repo"
	CanaryMultiProject    CanaryID = "multi_project"
	CanaryExplicitRoute   CanaryID = "explicit_route"
	CanarySmartRoute      CanaryID = "smart_route"
	CanaryWorkflow        CanaryID = "bounded_workflow"
	CanaryHostVisibility  CanaryID = "host_visibility"
	CanaryCrossMacHandoff CanaryID = "cross_mac_terminal_handoff"
	CanaryCancelRecovery  CanaryID = "cancel_recovery"
	CanaryCleanup         CanaryID = "cleanup"
)

// CanaryResult is one consumer canary outcome.
type CanaryResult struct {
	ID       CanaryID `json:"id"`
	Passed   bool     `json:"passed"`
	Evidence string   `json:"evidence,omitempty"`
	Notes    string   `json:"notes,omitempty"`
}

// Defect is an open P0/P1.
type Defect struct {
	ID       string `json:"id"`
	Severity string `json:"severity"` // P0|P1
	Title    string `json:"title"`
}

// ScopeChange is an owner-approved outcome change.
type ScopeChange struct {
	Issue   int    `json:"issue"`
	Owner   string `json:"owner"`
	Summary string `json:"summary"`
}

// Input aggregates all evidence for the final decision.
type Input struct {
	// Candidate identity
	SHA           string
	ArchiveDigest string
	// Remote checks on exact SHA
	IntegrationVerifyOK bool
	IntegrationCanaryOK bool
	// Scorecard from V090-102
	Scorecard releaseslo.Scorecard
	// Install smoke from V090-082
	InstallSmoke installsmoke.Report
	// Packaging draft/publish readiness
	ArtifactLocalDev bool
	// Consumer canaries
	Canaries []CanaryResult
	// Open P0/P1
	OpenDefects []Defect
	// Security/advisory/SBOM
	SecurityOK  bool
	SBOMPresent bool
	// Docs/capability claims honest
	DocsCapabilityOK bool
	// Migration evidence
	MigrationOK bool
	// Known limitations / deferred
	KnownLimitations []string
	DeferredIssues   []int
	// Rollback limitations
	RollbackLimitations []string
	// Operator approval
	Operator         string
	OperatorApproved bool
	// Protected environment approval for publish
	ProtectedEnvApproved bool
	// Scope changes
	ScopeChanges []ScopeChange
	// Accepted outcome count (catalog completeness signal; not sufficient alone)
	AcceptedOutcomes int
	// Minimum accepted outcomes expected for v0.9 catalog (soft signal)
	ExpectedOutcomes int
	Now              time.Time
}

// Record is the durable GO/NO-GO record.
type Record struct {
	Schema              string   `json:"schema"`
	Decision            Decision `json:"decision"`
	SHA                 string   `json:"sha"`
	ArchiveDigest       string   `json:"archive_digest"`
	Platform            string   `json:"platform"`
	ScorecardGO         bool     `json:"scorecard_go"`
	InstallSmokePass    bool     `json:"install_smoke_pass"`
	CanariesPass        int      `json:"canaries_pass"`
	CanariesTotal       int      `json:"canaries_total"`
	OpenP0P1            int      `json:"open_p0_p1"`
	Reasons             []string `json:"reasons"`
	EvidenceLinks       []string `json:"evidence_links"`
	KnownLimitations    []string `json:"known_limitations,omitempty"`
	DeferredIssues      []int    `json:"deferred_issues,omitempty"`
	RollbackLimitations []string `json:"rollback_limitations,omitempty"`
	Operator            string   `json:"operator,omitempty"`
	PublicationSteps    []string `json:"publication_steps,omitempty"`
	// PublishAllowed only when GO and protected env approved.
	PublishAllowed bool      `json:"publish_allowed"`
	At             time.Time `json:"at"`
}

// Evaluate produces the GO/NO-GO record.
func Evaluate(in Input) Record {
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	rec := Record{
		Schema: SchemaRecord, SHA: in.SHA, ArchiveDigest: in.ArchiveDigest,
		Platform: packdarwin.Platform, At: now.UTC(),
		KnownLimitations:    append([]string(nil), in.KnownLimitations...),
		DeferredIssues:      append([]int(nil), in.DeferredIssues...),
		RollbackLimitations: append([]string(nil), in.RollbackLimitations...),
		Operator:            in.Operator,
		PublicationSteps: []string{
			"1. Confirm scorecard GO and install smoke pass on exact digest",
			"2. Confirm dual-green integration on exact pre-prod SHA",
			"3. Confirm no open P0/P1",
			"4. Operator signs GO record",
			"5. Protected environment approval",
			"6. Publish single darwin/arm64 asset + checksums/SBOM",
			"7. Verify public assets; close roadmap tracking",
		},
	}

	var blockers []string

	if in.SHA == "" || in.ArchiveDigest == "" {
		blockers = append(blockers, "sha and archive digest required")
	}
	if !in.IntegrationVerifyOK || !in.IntegrationCanaryOK {
		blockers = append(blockers, "exact-SHA integration-verify+canary not dual green")
	}
	if !in.Scorecard.GO {
		blockers = append(blockers, "release SLO scorecard not GO")
	}
	rec.ScorecardGO = in.Scorecard.GO
	if !in.InstallSmoke.Passed || in.InstallSmoke.RebuiltDuringSmoke {
		blockers = append(blockers, "install smoke failed or rebuilt during smoke")
	}
	if in.InstallSmoke.ArchiveDigest != "" && in.ArchiveDigest != "" &&
		in.InstallSmoke.ArchiveDigest != in.ArchiveDigest {
		blockers = append(blockers, "install smoke digest mismatch vs candidate")
	}
	rec.InstallSmokePass = in.InstallSmoke.Passed && !in.InstallSmoke.RebuiltDuringSmoke
	if in.ArtifactLocalDev {
		blockers = append(blockers, "local_dev artifact cannot publish")
	}
	if !in.SecurityOK || !in.SBOMPresent {
		blockers = append(blockers, "security/SBOM evidence incomplete")
	}
	if !in.DocsCapabilityOK {
		blockers = append(blockers, "docs/capability claims not aligned")
	}
	if !in.MigrationOK {
		blockers = append(blockers, "migration evidence incomplete")
	}

	// Canaries: all required must pass
	required := []CanaryID{
		CanaryPublicPrivate, CanaryMultiProject, CanaryExplicitRoute, CanarySmartRoute,
		CanaryWorkflow, CanaryHostVisibility, CanaryCrossMacHandoff, CanaryCancelRecovery, CanaryCleanup,
	}
	byC := map[CanaryID]CanaryResult{}
	for _, c := range in.Canaries {
		byC[c.ID] = c
	}
	pass, total := 0, len(required)
	for _, id := range required {
		total = len(required)
		c, ok := byC[id]
		if !ok || !c.Passed {
			blockers = append(blockers, fmt.Sprintf("canary %s not pass", id))
			continue
		}
		pass++
		if c.Evidence != "" {
			rec.EvidenceLinks = append(rec.EvidenceLinks, c.Evidence)
		}
	}
	rec.CanariesPass, rec.CanariesTotal = pass, total

	// Open P0/P1
	p0p1 := 0
	for _, d := range in.OpenDefects {
		sev := strings.ToUpper(d.Severity)
		if sev == "P0" || sev == "P1" {
			p0p1++
			blockers = append(blockers, fmt.Sprintf("open %s %s", sev, d.ID))
		}
	}
	rec.OpenP0P1 = p0p1

	// Operator + protected env for publish
	if !in.OperatorApproved || strings.TrimSpace(in.Operator) == "" {
		// Operator approval required for GO publication; still can be NO_GO without it
		// For Decision GO we require operator approval.
		blockers = append(blockers, "operator approval required")
	}

	// Soft outcome count — informational, not sole basis
	if in.ExpectedOutcomes > 0 && in.AcceptedOutcomes < in.ExpectedOutcomes {
		// Do not auto-block solely on count; record limitation if short
		rec.KnownLimitations = append(rec.KnownLimitations,
			fmt.Sprintf("accepted outcomes %d < expected %d (not sole decision basis)", in.AcceptedOutcomes, in.ExpectedOutcomes))
	}

	// Scorecard evidence links
	rec.EvidenceLinks = append(rec.EvidenceLinks, in.Scorecard.EvidenceLinks...)
	rec.EvidenceLinks = unique(rec.EvidenceLinks)
	sort.Strings(rec.EvidenceLinks)

	if len(blockers) == 0 {
		rec.Decision = DecisionGO
		rec.Reasons = []string{"all consumer canaries, scorecard, smoke, dual-green, security/SBOM, docs, migration, no P0/P1, operator approved"}
		rec.PublishAllowed = in.ProtectedEnvApproved
		if !in.ProtectedEnvApproved {
			rec.Reasons = append(rec.Reasons, "GO recorded but publish awaits protected environment approval")
		}
	} else {
		rec.Decision = DecisionNOGO
		rec.Reasons = blockers
		rec.PublishAllowed = false
	}
	return rec
}

// RequiredCanaries returns the fixed consumer canary set.
func RequiredCanaries() []CanaryID {
	return []CanaryID{
		CanaryPublicPrivate, CanaryMultiProject, CanaryExplicitRoute, CanarySmartRoute,
		CanaryWorkflow, CanaryHostVisibility, CanaryCrossMacHandoff, CanaryCancelRecovery, CanaryCleanup,
	}
}

// AllCanariesPass helper for fixtures.
func AllCanariesPass() []CanaryResult {
	var out []CanaryResult
	for _, id := range RequiredCanaries() {
		out = append(out, CanaryResult{ID: id, Passed: true, Evidence: "canary:" + string(id)})
	}
	return out
}

func unique(in []string) []string {
	m := map[string]bool{}
	var out []string
	for _, s := range in {
		if s == "" || m[s] {
			continue
		}
		m[s] = true
		out = append(out, s)
	}
	return out
}
