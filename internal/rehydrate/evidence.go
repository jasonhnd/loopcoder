package rehydrate

import (
	"fmt"
	"strings"
	"time"
)

// DeliveryState is a terminal-or-not remote delivery classification.
type DeliveryState string

const (
	// StateMerged PR merged into base; safe terminal handoff.
	StateMerged DeliveryState = "merged"
	// StateClosed issue/PR closed without merge (terminal).
	StateClosed DeliveryState = "closed"
	// StateDelivered worker output proven complete; delivery-only continuation.
	StateDelivered DeliveryState = "delivered"
	// StateGated delivery waiting on human gate; terminal for automation.
	StateGated DeliveryState = "gated"
	// StateInFlight open PR / running checks / active review — not adoptable.
	StateInFlight DeliveryState = "in_flight"
	// StateAmbiguous conflicting remote signals — requires explicit reconciliation.
	StateAmbiguous DeliveryState = "ambiguous"
)

// Terminal reports whether the state may be rehydrated without live adoption.
func (s DeliveryState) Terminal() bool {
	switch s {
	case StateMerged, StateClosed, StateDelivered, StateGated:
		return true
	default:
		return false
	}
}

// RepoRef is a normalized repository identity.
type RepoRef struct {
	Owner      string `json:"owner"`
	Name       string `json:"name"`
	Visibility string `json:"visibility"` // public|private|internal
}

// Key returns owner/name lowercase for isolation comparisons.
func (r RepoRef) Key() string {
	return strings.ToLower(strings.TrimSpace(r.Owner)) + "/" + strings.ToLower(strings.TrimSpace(r.Name))
}

// IssueRef is normalized issue evidence.
type IssueRef struct {
	Number int    `json:"number"`
	State  string `json:"state"` // open|closed
	Title  string `json:"title"`
}

// PRRef is normalized pull-request evidence.
type PRRef struct {
	Number     int    `json:"number"`
	State      string `json:"state"` // open|closed
	Merged     bool   `json:"merged"`
	MergeSHA   string `json:"merge_sha,omitempty"`
	HeadSHA    string `json:"head_sha,omitempty"`
	BaseBranch string `json:"base_branch,omitempty"`
	HeadBranch string `json:"head_branch,omitempty"`
}

// CommitRef is a remote commit identity.
type CommitRef struct {
	SHA     string `json:"sha"`
	Message string `json:"message,omitempty"`
}

// CheckRef is a CI check identity on a commit/PR.
type CheckRef struct {
	Name       string `json:"name"`
	Status     string `json:"status"`     // queued|in_progress|completed
	Conclusion string `json:"conclusion"` // success|failure|… empty if not completed
}

// ReviewRef is a PR review identity.
type ReviewRef struct {
	ID    string `json:"id"`
	State string `json:"state"` // approved|changes_requested|commented|pending
}

// RemoteEvidence is the complete fixture input from GitHub (never local Mac A state).
type RemoteEvidence struct {
	Schema            string      `json:"schema"`
	Repo              RepoRef     `json:"repo"`
	Issue             IssueRef    `json:"issue"`
	PR                PRRef       `json:"pr"`
	Commits           []CommitRef `json:"commits,omitempty"`
	Checks            []CheckRef  `json:"checks,omitempty"`
	Reviews           []ReviewRef `json:"reviews,omitempty"`
	RouteEvidenceRefs []string    `json:"route_evidence_refs,omitempty"`
	// Classified is optional pre-classified state; empty triggers Classify.
	Classified DeliveryState `json:"classified,omitempty"`
}

// SchemaEvidence is the remote evidence schema id.
const SchemaEvidence = "loopcoder.rehydrate.remote_evidence.v1"

// LocalCheckout is the owner-selected local path on Mac B (required).
type LocalCheckout struct {
	Path    string `json:"path"`
	HeadSHA string `json:"head_sha,omitempty"`
	Branch  string `json:"branch,omitempty"`
}

// Classify derives DeliveryState from PR/issue/check signals.
// Conflicting signals (e.g. merged=true but state=open) → StateAmbiguous.
// Open PR or in-progress checks → StateInFlight.
func Classify(ev RemoteEvidence) DeliveryState {
	if ev.Classified != "" {
		return ev.Classified
	}
	pr := ev.PR
	// Ambiguity: merged flag vs open state, or merge SHA without merged.
	if pr.Merged && strings.EqualFold(pr.State, "open") {
		return StateAmbiguous
	}
	if pr.MergeSHA != "" && !pr.Merged && strings.EqualFold(pr.State, "open") {
		return StateAmbiguous
	}
	if pr.Merged {
		// Running checks after merge are still terminal for handoff purposes.
		return StateMerged
	}
	if strings.EqualFold(pr.State, "closed") || strings.EqualFold(ev.Issue.State, "closed") {
		// Closed without merge: terminal closed.
		if !pr.Merged {
			return StateClosed
		}
	}
	// In-progress checks or open PR → in flight.
	for _, c := range ev.Checks {
		st := strings.ToLower(c.Status)
		if st == "queued" || st == "in_progress" {
			return StateInFlight
		}
	}
	for _, r := range ev.Reviews {
		if strings.EqualFold(r.State, "pending") {
			return StateInFlight
		}
	}
	if strings.EqualFold(pr.State, "open") {
		return StateInFlight
	}
	// No PR / unknown → ambiguous rather than auto-adopt.
	if pr.Number == 0 && !strings.EqualFold(ev.Issue.State, "closed") {
		return StateAmbiguous
	}
	return StateClosed
}

// ValidateEvidence performs structural fail-closed checks on remote evidence.
func ValidateEvidence(ev RemoteEvidence) error {
	if strings.TrimSpace(ev.Repo.Owner) == "" || strings.TrimSpace(ev.Repo.Name) == "" {
		return fmt.Errorf("repo owner/name required")
	}
	vis := strings.ToLower(strings.TrimSpace(ev.Repo.Visibility))
	if vis == "" || vis == "unknown" {
		return fmt.Errorf("repo visibility unknown; fail closed")
	}
	if ev.Issue.Number <= 0 {
		return fmt.Errorf("issue number required")
	}
	return nil
}

// NowFunc is injectable time for events.
type NowFunc func() time.Time
