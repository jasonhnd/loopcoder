package multiproject

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	SchemaProject  = "loopcoder.machine.project_reg.v1"
	SchemaBudget   = "loopcoder.machine.global_budget.v1"
	SchemaDecision = "loopcoder.machine.admission.v1"
	SchemaSummary  = "loopcoder.machine.summary.v1"
	SchemaArchive  = "loopcoder.machine.archive.v1"
)

// ProjectRef is a registered local project (never just short name).
type ProjectRef struct {
	Schema    string `json:"schema"`
	ProjectID string `json:"project_id"` // durable unique id
	// ShortName may collide across owners.
	ShortName string `json:"short_name"`
	Owner     string `json:"owner"`
	// LocalPath absolute path; uniqueness enforced.
	LocalPath string `json:"local_path"`
	// StoreKey machine-local store path key.
	StoreKey  string    `json:"store_key"`
	Archived  bool      `json:"archived"`
	CreatedAt time.Time `json:"created_at"`
}

// GlobalBudget is shared across projects.
type GlobalBudget struct {
	MaxWorkers   int
	MaxVerifiers int
	MaxTests     int
	MaxProcesses int
	MaxCPUUnits  int
	MaxRSSMiB    int
	// Per-provider concurrency.
	MaxPerProvider map[string]int
}

// DefaultBudget returns conservative machine budgets.
func DefaultBudget() GlobalBudget {
	return GlobalBudget{
		MaxWorkers: 4, MaxVerifiers: 2, MaxTests: 4, MaxProcesses: 16,
		MaxCPUUnits: 800, MaxRSSMiB: 8192,
		MaxPerProvider: map[string]int{"codex": 2, "claude": 2, "gemini": 2, "grok": 2, "antigravity": 2},
	}
}

// Usage is one project's live usage sample.
type Usage struct {
	Workers, Verifiers, Tests, Processes int
	CPUUnits, RSSMiB                     int
	ByProvider                           map[string]int
}

// AdmissionDecision is explainable admit/wait.
type AdmissionDecision struct {
	Schema    string   `json:"schema"`
	ProjectID string   `json:"project_id"`
	Admitted  bool     `json:"admitted"`
	Wait      bool     `json:"wait"`
	Reasons   []string `json:"reasons"`
	// Attention for operator if needed.
	Attention bool `json:"attention,omitempty"`
}

// MachineSummary is redacted by default.
type MachineSummary struct {
	Schema   string              `json:"schema"`
	Projects []ProjectPublicFact `json:"projects"`
	// Global resource totals only.
	GlobalWorkers   int `json:"global_workers"`
	GlobalProcesses int `json:"global_processes"`
	GlobalRSSMiB    int `json:"global_rss_mib"`
}

// ProjectPublicFact is bounded identity/status only.
type ProjectPublicFact struct {
	ProjectID string `json:"project_id"`
	ShortName string `json:"short_name"`
	Owner     string `json:"owner"`
	// LocalPath redacted by default (basename only).
	PathBasename string `json:"path_basename"`
	Archived     bool   `json:"archived"`
	Workers      int    `json:"workers"`
	// Private fields never present: issue text, full paths, prompts, outputs.
}

// Registry is the machine project + admission ledger.
type Registry struct {
	mu       sync.Mutex
	projects map[string]*ProjectRef // project_id
	usage    map[string]Usage       // project_id
	// reservations project_id → reservation id
	reservations map[string]string
	budget       GlobalBudget
	now          func() time.Time
	seq          int64
}

// NewRegistry creates a multi-project registry.
func NewRegistry(budget GlobalBudget, now func() time.Time) *Registry {
	if now == nil {
		now = time.Now
	}
	if budget.MaxWorkers <= 0 {
		budget = DefaultBudget()
	}
	if budget.MaxPerProvider == nil {
		budget.MaxPerProvider = DefaultBudget().MaxPerProvider
	}
	return &Registry{
		projects: map[string]*ProjectRef{}, usage: map[string]Usage{},
		reservations: map[string]string{}, budget: budget, now: now,
	}
}

var (
	ErrInvalid  = errors.New("multiproject: invalid")
	ErrConflict = errors.New("multiproject: conflict")
	ErrDenied   = errors.New("multiproject: admission denied")
)

// Register adds a project; short name may collide, path/id may not.
func (r *Registry) Register(shortName, owner, localPath string) (ProjectRef, error) {
	if strings.TrimSpace(shortName) == "" || strings.TrimSpace(owner) == "" || strings.TrimSpace(localPath) == "" {
		return ProjectRef{}, fmt.Errorf("%w: short_name/owner/path required", ErrInvalid)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	path := strings.TrimSpace(localPath)
	for _, p := range r.projects {
		if !p.Archived && p.LocalPath == path {
			return ProjectRef{}, fmt.Errorf("%w: path collision %s", ErrConflict, path)
		}
	}
	r.seq++
	// Durable id: owner + short + seq (not short alone)
	id := fmt.Sprintf("proj_%s_%s_%d", sanitize(owner), sanitize(shortName), r.seq)
	store := fmt.Sprintf("store/%s/%s", sanitize(owner), id)
	p := &ProjectRef{
		Schema: SchemaProject, ProjectID: id, ShortName: shortName, Owner: owner,
		LocalPath: path, StoreKey: store, CreatedAt: r.now().UTC(),
	}
	r.projects[id] = p
	r.usage[id] = Usage{ByProvider: map[string]int{}}
	return *p, nil
}

// Admit tries global admission for incremental usage on a project.
func (r *Registry) Admit(projectID string, delta Usage) (AdmissionDecision, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.projects[projectID]
	if !ok || p.Archived {
		return AdmissionDecision{}, fmt.Errorf("%w: project", ErrInvalid)
	}
	d := AdmissionDecision{Schema: SchemaDecision, ProjectID: projectID}
	// compute totals with delta
	totals := r.totalsLocked()
	u := r.usage[projectID]
	newW := totals.Workers - u.Workers + u.Workers + delta.Workers
	// simpler: sum all then add delta for this project
	cur := r.totalsLocked()
	// remove current project usage then add updated
	cur.Workers -= u.Workers
	cur.Verifiers -= u.Verifiers
	cur.Tests -= u.Tests
	cur.Processes -= u.Processes
	cur.CPUUnits -= u.CPUUnits
	cur.RSSMiB -= u.RSSMiB
	nu := Usage{
		Workers: u.Workers + delta.Workers, Verifiers: u.Verifiers + delta.Verifiers,
		Tests: u.Tests + delta.Tests, Processes: u.Processes + delta.Processes,
		CPUUnits: u.CPUUnits + delta.CPUUnits, RSSMiB: u.RSSMiB + delta.RSSMiB,
		ByProvider: map[string]int{},
	}
	for k, v := range u.ByProvider {
		nu.ByProvider[k] = v
	}
	if delta.ByProvider != nil {
		for k, v := range delta.ByProvider {
			nu.ByProvider[k] += v
		}
	}
	cur.Workers += nu.Workers
	cur.Verifiers += nu.Verifiers
	cur.Tests += nu.Tests
	cur.Processes += nu.Processes
	cur.CPUUnits += nu.CPUUnits
	cur.RSSMiB += nu.RSSMiB

	// check limits
	if cur.Workers > r.budget.MaxWorkers {
		d.Reasons = append(d.Reasons, "global_workers")
	}
	if cur.Verifiers > r.budget.MaxVerifiers {
		d.Reasons = append(d.Reasons, "global_verifiers")
	}
	if cur.Tests > r.budget.MaxTests {
		d.Reasons = append(d.Reasons, "global_tests")
	}
	if cur.Processes > r.budget.MaxProcesses {
		d.Reasons = append(d.Reasons, "global_processes")
	}
	if cur.CPUUnits > r.budget.MaxCPUUnits {
		d.Reasons = append(d.Reasons, "global_cpu")
	}
	if cur.RSSMiB > r.budget.MaxRSSMiB {
		d.Reasons = append(d.Reasons, "global_rss")
	}
	// provider totals
	prov := map[string]int{}
	for pid, us := range r.usage {
		if pid == projectID {
			continue
		}
		for k, v := range us.ByProvider {
			prov[k] += v
		}
	}
	for k, v := range nu.ByProvider {
		prov[k] += v
		if max, ok := r.budget.MaxPerProvider[k]; ok && prov[k] > max {
			d.Reasons = append(d.Reasons, "provider:"+k)
		}
	}
	if len(d.Reasons) > 0 {
		d.Wait = true
		d.Attention = true
		d.Admitted = false
		return d, nil
	}
	r.usage[projectID] = nu
	r.seq++
	r.reservations[projectID] = fmt.Sprintf("res_%d", r.seq)
	d.Admitted = true
	d.Reasons = append(d.Reasons, "admitted")
	_ = newW
	return d, nil
}

// Summary redacts private content by default.
func (r *Registry) Summary() MachineSummary {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := MachineSummary{Schema: SchemaSummary}
	ids := make([]string, 0, len(r.projects))
	for id := range r.projects {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	tot := r.totalsLocked()
	s.GlobalWorkers = tot.Workers
	s.GlobalProcesses = tot.Processes
	s.GlobalRSSMiB = tot.RSSMiB
	for _, id := range ids {
		p := r.projects[id]
		u := r.usage[id]
		base := p.LocalPath
		if i := strings.LastIndex(base, "/"); i >= 0 {
			base = base[i+1:]
		}
		s.Projects = append(s.Projects, ProjectPublicFact{
			ProjectID: p.ProjectID, ShortName: p.ShortName, Owner: p.Owner,
			PathBasename: base, Archived: p.Archived, Workers: u.Workers,
		})
	}
	return s
}

// RestartReconcile drops reservations without matching process evidence.
func (r *Registry) RestartReconcile(liveProcesses map[string]int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for pid, u := range r.usage {
		live := liveProcesses[pid]
		if live < u.Processes {
			// shrink to live evidence; no cross-project adoption
			u.Processes = live
			if u.Workers > live {
				u.Workers = live
			}
			r.usage[pid] = u
		}
		if live == 0 {
			delete(r.reservations, pid)
		}
	}
}

// Archive marks a project archived; optional delete of its global payload only.
func (r *Registry) Archive(projectID string, deletePayload bool) (ProjectRef, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.projects[projectID]
	if !ok {
		return ProjectRef{}, fmt.Errorf("%w: not found", ErrInvalid)
	}
	p.Archived = true
	delete(r.reservations, projectID)
	if deletePayload {
		// only this project's usage/reservation — never others
		delete(r.usage, projectID)
	}
	return *p, nil
}

// Get returns project by id.
func (r *Registry) Get(projectID string) (ProjectRef, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.projects[projectID]
	if !ok {
		return ProjectRef{}, fmt.Errorf("%w: not found", ErrInvalid)
	}
	return *p, nil
}

// SameNameProjects returns all non-archived with short name.
func (r *Registry) SameNameProjects(short string) []ProjectRef {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []ProjectRef
	for _, p := range r.projects {
		if !p.Archived && p.ShortName == short {
			out = append(out, *p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProjectID < out[j].ProjectID })
	return out
}

func (r *Registry) totalsLocked() Usage {
	t := Usage{ByProvider: map[string]int{}}
	for _, u := range r.usage {
		t.Workers += u.Workers
		t.Verifiers += u.Verifiers
		t.Tests += u.Tests
		t.Processes += u.Processes
		t.CPUUnits += u.CPUUnits
		t.RSSMiB += u.RSSMiB
		for k, v := range u.ByProvider {
			t.ByProvider[k] += v
		}
	}
	return t
}

func sanitize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, s)
	return s
}
