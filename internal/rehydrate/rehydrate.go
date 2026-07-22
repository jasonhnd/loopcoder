package rehydrate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// SchemaEvent is the local rehydration event schema.
const SchemaEvent = "loopcoder.rehydrate.event.v1"

// SchemaProject is the reconstructed project identity schema.
const SchemaProject = "loopcoder.rehydrate.project.v1"

// SchemaResult is the rehydrate result schema.
const SchemaResult = "loopcoder.rehydrate.result.v1"

// Project is the stable project identity on the local machine (Mac B).
// Never carries Mac A process/lease/attempt ids.
type Project struct {
	Schema     string    `json:"schema"`
	ProjectID  string    `json:"project_id"`
	RepoKey    string    `json:"repo_key"`
	Visibility string    `json:"visibility"`
	LocalPath  string    `json:"local_path"`
	CreatedAt  time.Time `json:"created_at"`
	// MachineID is the Mac B machine id that owns this local projection.
	MachineID string `json:"machine_id"`
}

// Event is appended locally and references remote evidence only.
type Event struct {
	Schema            string        `json:"schema"`
	EventID           string        `json:"event_id"`
	ProjectID         string        `json:"project_id"`
	MachineID         string        `json:"machine_id"`
	At                time.Time     `json:"at"`
	DeliveryState     DeliveryState `json:"delivery_state"`
	IssueNumber       int           `json:"issue_number"`
	PRNumber          int           `json:"pr_number"`
	MergeSHA          string        `json:"merge_sha,omitempty"`
	HeadSHA           string        `json:"head_sha,omitempty"`
	RouteEvidenceRefs []string      `json:"route_evidence_refs,omitempty"`
	CheckNames        []string      `json:"check_names,omitempty"`
	ReviewIDs         []string      `json:"review_ids,omitempty"`
	// ForeignAttemptRejected is set when an in-flight adoption was refused.
	ForeignAttemptRejected bool     `json:"foreign_attempt_rejected,omitempty"`
	Divergences            []string `json:"divergences,omitempty"`
	IdempotentReplay       bool     `json:"idempotent_replay,omitempty"`
}

// Input is one rehydrate request on Mac B.
type Input struct {
	Evidence  RemoteEvidence
	Checkout  LocalCheckout
	MachineID string
	// ExistingProjectID reuses a previously created project on this machine.
	ExistingProjectID string
	// AttemptAdoptForeign is true when a caller tries to adopt another machine's
	// in-flight attempt — always rejected.
	AttemptAdoptForeign bool
	// ForeignAttemptID is the foreign id that must never become local live state.
	ForeignAttemptID string
}

// Result is the pure rehydrate decision + reconstructed status.
type Result struct {
	Schema        string        `json:"schema"`
	Allowed       bool          `json:"allowed"`
	Reasons       []string      `json:"reasons"`
	Project       *Project      `json:"project,omitempty"`
	DeliveryState DeliveryState `json:"delivery_state"`
	Event         *Event        `json:"event,omitempty"`
	Divergences   []string      `json:"divergences,omitempty"`
	// NewLocalExecutionID is a fresh local identity for any future work on Mac B.
	// Never equal to a foreign attempt id.
	NewLocalExecutionID string `json:"new_local_execution_id,omitempty"`
}

// Store is an in-memory Mac B home projection (fixtures only; not SQLite).
type Store struct {
	mu       sync.Mutex
	machine  string
	projects map[string]*Project // project_id
	byRepo   map[string]string   // repo_key → project_id
	events   []Event
	// lastEvidence fingerprint per project for idempotency.
	lastFP map[string]string
	now    NowFunc
	seq    int64
}

// NewStore creates an empty Mac B home store.
func NewStore(machineID string, now NowFunc) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{
		machine:  strings.TrimSpace(machineID),
		projects: map[string]*Project{},
		byRepo:   map[string]string{},
		lastFP:   map[string]string{},
		now:      now,
	}
}

// Rehydrate reconstructs local status from remote evidence.
// Does not read any foreign machine database. Rejects in-flight adoption.
func (s *Store) Rehydrate(in Input) Result {
	s.mu.Lock()
	defer s.mu.Unlock()

	res := Result{Schema: SchemaResult}
	if strings.TrimSpace(in.MachineID) == "" {
		in.MachineID = s.machine
	}
	if strings.TrimSpace(in.MachineID) == "" {
		res.Reasons = append(res.Reasons, "machine_id required")
		return res
	}
	if err := ValidateEvidence(in.Evidence); err != nil {
		res.Reasons = append(res.Reasons, err.Error())
		return res
	}
	if strings.TrimSpace(in.Checkout.Path) == "" {
		res.Reasons = append(res.Reasons, "explicit local checkout path required")
		return res
	}

	// Never adopt foreign in-flight attempts.
	if in.AttemptAdoptForeign || strings.TrimSpace(in.ForeignAttemptID) != "" {
		res.DeliveryState = StateInFlight
		res.Reasons = append(res.Reasons,
			"reject adoption of in-flight local process/attempt from another machine")
		ev := s.appendEventLocked(in, "", StateInFlight, nil, true, false)
		res.Event = &ev
		return res
	}

	state := Classify(in.Evidence)
	res.DeliveryState = state
	if state == StateAmbiguous {
		res.Reasons = append(res.Reasons,
			"remote state ambiguous; explicit reconciliation required")
		return res
	}
	if state == StateInFlight || !state.Terminal() {
		res.Reasons = append(res.Reasons,
			"in-flight remote state cannot be treated as a live local attempt or auto-relaunched")
		return res
	}

	// Divergence detection (remote vs selected checkout) — report before mutation.
	div := detectDivergences(in.Evidence, in.Checkout)
	res.Divergences = div

	repoKey := in.Evidence.Repo.Key()
	// Same-name isolation: project keyed by owner/name + visibility, not short name.
	proj, err := s.ensureProjectLocked(in, repoKey)
	if err != nil {
		res.Reasons = append(res.Reasons, err.Error())
		return res
	}

	fp := fingerprint(in.Evidence)
	idempotent := false
	if prev, ok := s.lastFP[proj.ProjectID]; ok && prev == fp {
		idempotent = true
	}
	s.lastFP[proj.ProjectID] = fp

	ev := s.appendEventLocked(in, proj.ProjectID, state, div, false, idempotent)
	res.Project = cloneProject(proj)
	res.Event = &ev
	res.Allowed = true
	res.NewLocalExecutionID = newLocalID(in.MachineID, proj.ProjectID, s.seq)
	if idempotent {
		res.Reasons = append(res.Reasons, "idempotent rehydrate; no project mutation")
	} else {
		res.Reasons = append(res.Reasons, "rehydrated from remote evidence; new local execution identity issued")
	}
	if len(div) > 0 {
		res.Reasons = append(res.Reasons, "remote/local differences reported before mutation")
	}
	return res
}

// Events returns a copy of local rehydration events.
func (s *Store) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, len(s.events))
	copy(out, s.events)
	return out
}

// ProjectByRepo returns the project for a repo key if present.
func (s *Store) ProjectByRepo(repoKey string) *Project {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.byRepo[strings.ToLower(repoKey)]
	if !ok {
		return nil
	}
	return cloneProject(s.projects[id])
}

// ProjectCount for tests.
func (s *Store) ProjectCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.projects)
}

func (s *Store) ensureProjectLocked(in Input, repoKey string) (*Project, error) {
	if id := strings.TrimSpace(in.ExistingProjectID); id != "" {
		p, ok := s.projects[id]
		if !ok {
			return nil, fmt.Errorf("existing project_id %s not found on this machine", id)
		}
		if p.RepoKey != repoKey {
			return nil, fmt.Errorf("existing project repo mismatch: have %s want %s", p.RepoKey, repoKey)
		}
		// Path may move; update to explicit checkout.
		p.LocalPath = filepath.Clean(in.Checkout.Path)
		return p, nil
	}
	if id, ok := s.byRepo[repoKey]; ok {
		p := s.projects[id]
		// Visibility isolation: refuse if visibility changed without explicit reuse.
		wantVis := strings.ToLower(strings.TrimSpace(in.Evidence.Repo.Visibility))
		if p.Visibility != wantVis {
			return nil, fmt.Errorf("repo visibility changed (%s → %s); explicit reconciliation required", p.Visibility, wantVis)
		}
		p.LocalPath = filepath.Clean(in.Checkout.Path)
		return p, nil
	}
	s.seq++
	id := fmt.Sprintf("proj-%s-%d", shortHash(repoKey), s.seq)
	if in.ExistingProjectID != "" {
		id = in.ExistingProjectID
	}
	p := &Project{
		Schema:     SchemaProject,
		ProjectID:  id,
		RepoKey:    repoKey,
		Visibility: strings.ToLower(strings.TrimSpace(in.Evidence.Repo.Visibility)),
		LocalPath:  filepath.Clean(in.Checkout.Path),
		CreatedAt:  s.now().UTC(),
		MachineID:  in.MachineID,
	}
	s.projects[id] = p
	s.byRepo[repoKey] = id
	return p, nil
}

func (s *Store) appendEventLocked(in Input, projectID string, state DeliveryState, div []string, foreign, idempotent bool) Event {
	s.seq++
	checkNames := make([]string, 0, len(in.Evidence.Checks))
	for _, c := range in.Evidence.Checks {
		checkNames = append(checkNames, c.Name)
	}
	sort.Strings(checkNames)
	reviewIDs := make([]string, 0, len(in.Evidence.Reviews))
	for _, r := range in.Evidence.Reviews {
		reviewIDs = append(reviewIDs, r.ID)
	}
	sort.Strings(reviewIDs)
	refs := append([]string(nil), in.Evidence.RouteEvidenceRefs...)
	ev := Event{
		Schema:                 SchemaEvent,
		EventID:                fmt.Sprintf("reh-%d", s.seq),
		ProjectID:              projectID,
		MachineID:              in.MachineID,
		At:                     s.now().UTC(),
		DeliveryState:          state,
		IssueNumber:            in.Evidence.Issue.Number,
		PRNumber:               in.Evidence.PR.Number,
		MergeSHA:               in.Evidence.PR.MergeSHA,
		HeadSHA:                in.Evidence.PR.HeadSHA,
		RouteEvidenceRefs:      refs,
		CheckNames:             checkNames,
		ReviewIDs:              reviewIDs,
		ForeignAttemptRejected: foreign,
		Divergences:            append([]string(nil), div...),
		IdempotentReplay:       idempotent,
	}
	s.events = append(s.events, ev)
	return ev
}

func detectDivergences(ev RemoteEvidence, co LocalCheckout) []string {
	var d []string
	if co.HeadSHA != "" && ev.PR.HeadSHA != "" && !strings.EqualFold(co.HeadSHA, ev.PR.HeadSHA) {
		if ev.PR.MergeSHA == "" || !strings.EqualFold(co.HeadSHA, ev.PR.MergeSHA) {
			d = append(d, fmt.Sprintf("checkout head_sha %s differs from PR head_sha %s", short(co.HeadSHA), short(ev.PR.HeadSHA)))
		}
	}
	if co.Branch != "" && ev.PR.HeadBranch != "" && co.Branch != ev.PR.HeadBranch {
		// After merge, checkout may be on base branch — not a hard error, report only.
		if ev.PR.BaseBranch == "" || co.Branch != ev.PR.BaseBranch {
			d = append(d, fmt.Sprintf("checkout branch %s differs from PR head_branch %s", co.Branch, ev.PR.HeadBranch))
		}
	}
	return d
}

func fingerprint(ev RemoteEvidence) string {
	// Stable fingerprint of terminal remote identities.
	type fp struct {
		Repo     string
		Issue    int
		PR       int
		Merged   bool
		MergeSHA string
		HeadSHA  string
		State    string
		Refs     []string
	}
	refs := append([]string(nil), ev.RouteEvidenceRefs...)
	sort.Strings(refs)
	b, _ := json.Marshal(fp{
		Repo: ev.Repo.Key(), Issue: ev.Issue.Number, PR: ev.PR.Number,
		Merged: ev.PR.Merged, MergeSHA: ev.PR.MergeSHA, HeadSHA: ev.PR.HeadSHA,
		State: string(Classify(ev)), Refs: refs,
	})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

func newLocalID(machine, project string, seq int64) string {
	return fmt.Sprintf("local-%s-%s-%d", shortHash(machine), shortHash(project), seq)
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:4])
}

func short(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}

func cloneProject(p *Project) *Project {
	if p == nil {
		return nil
	}
	c := *p
	return &c
}
