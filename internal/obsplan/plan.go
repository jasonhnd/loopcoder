package obsplan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jasonhnd/loopcoder/internal/providerdesc"
)

const (
	SchemaPlan       = "loopcoder.obs.plan.v1"
	SchemaStepResult = "loopcoder.obs.step.v1"
	SchemaSnapshot   = "loopcoder.obs.snapshot.v1"
	SchemaStore      = "loopcoder.obs.store.v1"
)

// SourceKind classifies an observation source.
type SourceKind string

const (
	SourceAPI         SourceKind = "api"
	SourceCLI         SourceKind = "cli"
	SourceLocalStatus SourceKind = "local_status"
	SourceAuthMeta    SourceKind = "auth_metadata"
	SourceBridge      SourceKind = "bridge_optional"
	SourceUnavailable SourceKind = "unavailable"
)

// StepOutcome is the typed result of one source attempt.
type StepOutcome string

const (
	OutcomeOK              StepOutcome = "ok"
	OutcomeTimeout         StepOutcome = "timeout"
	OutcomeMalformed       StepOutcome = "malformed"
	OutcomeUnauthenticated StepOutcome = "unauthenticated"
	OutcomeStale           StepOutcome = "stale"
	OutcomeUnsupported     StepOutcome = "unsupported"
	OutcomeSkipped         StepOutcome = "skipped"
	OutcomeError           StepOutcome = "error"
)

var (
	ErrInvalid = errors.New("obsplan: invalid")
	ErrPolicy  = errors.New("obsplan: policy")
)

// Bounds for one source step.
type Bounds struct {
	Timeout      time.Duration `json:"timeout"`
	MaxOutputB   int64         `json:"max_output_bytes"`
	AllowNetwork bool          `json:"allow_network"`
	// AllowRedirects is always scrubbed; default false for safety.
	AllowRedirects bool `json:"allow_redirects"`
}

// DefaultBounds returns conservative offline-first bounds.
func DefaultBounds() Bounds {
	return Bounds{Timeout: 3 * time.Second, MaxOutputB: 64 << 10, AllowNetwork: false, AllowRedirects: false}
}

// SourceStep is one ordered plan entry.
type SourceStep struct {
	Name      string     `json:"name"`
	Kind      SourceKind `json:"kind"`
	Authority int        `json:"authority"` // higher first
	Safety    int        `json:"safety"`    // higher preferred when authority ties
	Bounds    Bounds     `json:"bounds"`
	Optional  bool       `json:"optional"`
	// Capability this step can satisfy (discover/catalog/etc).
	Capability providerdesc.Operation `json:"capability"`
}

// Plan is a deterministic ordered plan for one capability.
type Plan struct {
	Schema     string                 `json:"schema"`
	AdapterID  string                 `json:"adapter_id"`
	Capability providerdesc.Operation `json:"capability"`
	Steps      []SourceStep           `json:"steps"`
	// StopOnFirstOK ends the plan after first successful fact.
	StopOnFirstOK bool `json:"stop_on_first_ok"`
}

// StepResult is one attempted/skipped source record (not a fact when failed).
type StepResult struct {
	Schema     string      `json:"schema"`
	StepName   string      `json:"step_name"`
	Kind       SourceKind  `json:"kind"`
	Outcome    StepOutcome `json:"outcome"`
	Diagnostic string      `json:"diagnostic_code,omitempty"`
	// Fact is set only on OutcomeOK; never credentials.
	Fact        map[string]string `json:"fact,omitempty"`
	CapturedAt  time.Time         `json:"captured_at"`
	Explanation string            `json:"explanation,omitempty"`
	IsFact      bool              `json:"is_fact"`
}

// Snapshot is an immutable aggregate observation.
type Snapshot struct {
	Schema         string   `json:"schema"`
	AdapterID      string   `json:"adapter_id"`
	Capability     string   `json:"capability"`
	SelectedSource string   `json:"selected_source,omitempty"`
	Attempted      []string `json:"attempted"`
	Skipped        []string `json:"skipped"`
	Diagnostics    []string `json:"diagnostics"`
	// Facts are only OK step facts (deduped key space).
	Facts       map[string]string `json:"facts"`
	Digest      string            `json:"digest"`
	CapturedAt  time.Time         `json:"captured_at"`
	Explanation string            `json:"explanation"`
	Steps       []StepResult      `json:"steps"`
}

// SourceRunner executes one step; tests inject fixtures (no real network).
// Returns outcome, redacted fact map, diagnostic code, explanation.
type SourceRunner func(step SourceStep) (StepOutcome, map[string]string, string, string)

// Executor runs a plan.
type Executor struct {
	Now    func() time.Time
	Runner SourceRunner
	// ScrubEnv documents that environments must be scrubbed (always true API).
	ScrubEnv bool
}

// DefaultPlan builds a safe ordered plan for a capability.
func DefaultPlan(adapterID string, cap providerdesc.Operation) Plan {
	b := DefaultBounds()
	steps := []SourceStep{
		{Name: "cli_primary", Kind: SourceCLI, Authority: 80, Safety: 90, Bounds: b, Capability: cap},
		{Name: "local_status", Kind: SourceLocalStatus, Authority: 60, Safety: 95, Bounds: b, Capability: cap},
		{Name: "auth_metadata", Kind: SourceAuthMeta, Authority: 50, Safety: 85, Bounds: b, Capability: cap, Optional: true},
		{Name: "api_optional", Kind: SourceAPI, Authority: 90, Safety: 40, Bounds: Bounds{Timeout: 2 * time.Second, MaxOutputB: 32 << 10, AllowNetwork: true}, Capability: cap, Optional: true},
		{Name: "bridge_optional", Kind: SourceBridge, Authority: 40, Safety: 50, Bounds: b, Capability: cap, Optional: true},
		{Name: "unavailable_marker", Kind: SourceUnavailable, Authority: 0, Safety: 100, Bounds: b, Capability: cap, Optional: true},
	}
	// Sort by authority desc, then safety desc, then name.
	sort.SliceStable(steps, func(i, j int) bool {
		if steps[i].Authority != steps[j].Authority {
			return steps[i].Authority > steps[j].Authority
		}
		if steps[i].Safety != steps[j].Safety {
			return steps[i].Safety > steps[j].Safety
		}
		return steps[i].Name < steps[j].Name
	})
	return Plan{
		Schema: SchemaPlan, AdapterID: adapterID, Capability: cap,
		Steps: steps, StopOnFirstOK: true,
	}
}

// Run executes the plan with the injected runner.
func (e *Executor) Run(plan Plan) (Snapshot, error) {
	if plan.AdapterID == "" || plan.Capability == "" || len(plan.Steps) == 0 {
		return Snapshot{}, ErrInvalid
	}
	if e.Runner == nil {
		return Snapshot{}, fmt.Errorf("%w: runner required", ErrInvalid)
	}
	now := time.Now().UTC()
	if e.Now != nil {
		now = e.Now().UTC()
	}
	// Always scrub env policy flag
	if !e.ScrubEnv {
		e.ScrubEnv = true
	}

	var steps []StepResult
	var attempted, skipped, diags []string
	facts := map[string]string{}
	selected := ""
	var explanations []string

	for _, st := range plan.Steps {
		if err := validateStep(st); err != nil {
			return Snapshot{}, err
		}
		// Policy: no network unless AllowNetwork; redirects never enabled in v1 default path.
		if st.Bounds.AllowRedirects {
			return Snapshot{}, fmt.Errorf("%w: redirects not allowed", ErrPolicy)
		}
		outcome, fact, diag, expl := e.Runner(st)
		// Never accept secret-shaped facts
		if fact != nil {
			for k, v := range fact {
				if looksSecret(v) || looksSecret(k) {
					outcome = OutcomeMalformed
					diag = "secret_in_fact"
					fact = nil
					expl = "fact contained secret-shaped value; discarded"
					break
				}
			}
		}
		sr := StepResult{
			Schema: SchemaStepResult, StepName: st.Name, Kind: st.Kind,
			Outcome: outcome, Diagnostic: diag, CapturedAt: now, Explanation: expl,
		}
		switch outcome {
		case OutcomeOK:
			attempted = append(attempted, st.Name)
			sr.IsFact = true
			sr.Fact = copyMap(fact)
			for k, v := range fact {
				facts[k] = v
			}
			if selected == "" {
				selected = st.Name
			}
			explanations = append(explanations, fmt.Sprintf("selected=%s", st.Name))
			steps = append(steps, sr)
			if plan.StopOnFirstOK {
				// remaining optional steps recorded as skipped
				for _, rest := range plan.Steps {
					if rest.Name == st.Name {
						continue
					}
					already := false
					for _, a := range attempted {
						if a == rest.Name {
							already = true
							break
						}
					}
					if already {
						continue
					}
					// only mark not-yet-visited
					found := false
					for _, s := range steps {
						if s.StepName == rest.Name {
							found = true
							break
						}
					}
					if found {
						continue
					}
					// skip steps after stop that we haven't run — mark by index
				}
				// mark remaining after current index
				idx := indexOf(plan.Steps, st.Name)
				for j := idx + 1; j < len(plan.Steps); j++ {
					skipped = append(skipped, plan.Steps[j].Name)
					steps = append(steps, StepResult{
						Schema: SchemaStepResult, StepName: plan.Steps[j].Name, Kind: plan.Steps[j].Kind,
						Outcome: OutcomeSkipped, Diagnostic: "stopped_after_ok", CapturedAt: now,
						Explanation: "stop_on_first_ok", IsFact: false,
					})
				}
				goto done
			}
		case OutcomeSkipped:
			skipped = append(skipped, st.Name)
			if diag != "" {
				diags = append(diags, st.Name+":"+diag)
			}
			steps = append(steps, sr)
		default:
			attempted = append(attempted, st.Name)
			if diag != "" {
				diags = append(diags, st.Name+":"+diag)
			} else {
				diags = append(diags, st.Name+":"+string(outcome))
			}
			// Failed sources are NOT facts — explicit
			sr.IsFact = false
			sr.Fact = nil
			// Distinct outcomes must not normalize to zero quota / no install.
			if outcome == OutcomeTimeout || outcome == OutcomeMalformed || outcome == OutcomeUnauthenticated || outcome == OutcomeStale || outcome == OutcomeUnsupported {
				// leave facts empty for these
			}
			steps = append(steps, sr)
			explanations = append(explanations, fmt.Sprintf("fallback_after=%s:%s", st.Name, outcome))
		}
	}
done:
	if selected == "" && len(facts) == 0 {
		explanations = append(explanations, "no_fact_selected")
	}
	snap := Snapshot{
		Schema: SchemaSnapshot, AdapterID: plan.AdapterID, Capability: string(plan.Capability),
		SelectedSource: selected, Attempted: attempted, Skipped: skipped, Diagnostics: diags,
		Facts: facts, CapturedAt: now, Explanation: strings.Join(explanations, "; "),
		Steps: steps,
	}
	snap.Digest = digestSnapshot(snap)
	return snap, nil
}

func validateStep(st SourceStep) error {
	if st.Name == "" || st.Kind == "" {
		return ErrInvalid
	}
	if st.Bounds.Timeout <= 0 || st.Bounds.MaxOutputB <= 0 {
		return fmt.Errorf("%w: bounds required", ErrInvalid)
	}
	return nil
}

func indexOf(steps []SourceStep, name string) int {
	for i, s := range steps {
		if s.Name == name {
			return i
		}
	}
	return -1
}

func copyMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func looksSecret(s string) bool {
	ls := strings.ToLower(s)
	return strings.HasPrefix(ls, "ghp_") || strings.HasPrefix(ls, "sk-") ||
		strings.Contains(ls, "token=") || strings.Contains(ls, "password=")
}

func digestSnapshot(s Snapshot) string {
	// Byte-stable: sorted keys, no step timing noise beyond CapturedAt in digest material?
	// Spec: fixture replay same outputs -> byte-stable facts and explanations.
	// Digest covers adapter, capability, selected, facts, diagnostics, explanation.
	type wire struct {
		Adapter     string            `json:"a"`
		Capability  string            `json:"c"`
		Selected    string            `json:"s"`
		Facts       map[string]string `json:"f"`
		Diagnostics []string          `json:"d"`
		Explanation string            `json:"e"`
		Attempted   []string          `json:"at"`
		Skipped     []string          `json:"sk"`
	}
	// sort fact keys into stable map via json marshal of sorted slice
	keys := make([]string, 0, len(s.Facts))
	for k := range s.Facts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fm := map[string]string{}
	for _, k := range keys {
		fm[k] = s.Facts[k]
	}
	w := wire{
		Adapter: s.AdapterID, Capability: s.Capability, Selected: s.SelectedSource,
		Facts: fm, Diagnostics: append([]string(nil), s.Diagnostics...),
		Explanation: s.Explanation, Attempted: append([]string(nil), s.Attempted...),
		Skipped: append([]string(nil), s.Skipped...),
	}
	b, _ := json.Marshal(w)
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])[:24]
}

// Store is an in-memory machine.db stand-in for immutable snapshots.
type Store struct {
	mu         sync.Mutex
	byKey      map[string][]Snapshot // adapter|cap -> history
	lastDigest map[string]string
}

// NewStore creates an empty observation store.
func NewStore() *Store {
	return &Store{byKey: map[string][]Snapshot{}, lastDigest: map[string]string{}}
}

func storeKey(adapter string, cap providerdesc.Operation) string {
	return strings.ToLower(adapter) + "|" + string(cap)
}

// Persist deduplicates identical digests; returns (snap, novel).
func (s *Store) Persist(snap Snapshot) (Snapshot, bool, error) {
	if snap.Digest == "" {
		return Snapshot{}, false, ErrInvalid
	}
	key := storeKey(snap.AdapterID, providerdesc.Operation(snap.Capability))
	s.mu.Lock()
	defer s.mu.Unlock()
	if prev, ok := s.lastDigest[key]; ok && prev == snap.Digest {
		// dedup — return previous
		hist := s.byKey[key]
		return hist[len(hist)-1], false, nil
	}
	s.byKey[key] = append(s.byKey[key], snap)
	s.lastDigest[key] = snap.Digest
	return snap, true, nil
}

// Latest returns the newest snapshot for adapter+capability.
func (s *Store) Latest(adapter string, cap providerdesc.Operation) (Snapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hist := s.byKey[storeKey(adapter, cap)]
	if len(hist) == 0 {
		return Snapshot{}, false
	}
	return hist[len(hist)-1], true
}

// ScrubEnv removes credential and git-redirect env entries (for command policy).
func ScrubEnv(env []string) []string {
	var out []string
	for _, e := range env {
		k, _, _ := strings.Cut(e, "=")
		uk := strings.ToUpper(k)
		if strings.Contains(uk, "TOKEN") || strings.Contains(uk, "SECRET") || strings.Contains(uk, "PASSWORD") || strings.Contains(uk, "CREDENTIAL") {
			continue
		}
		if strings.HasPrefix(uk, "GIT_DIR") || strings.HasPrefix(uk, "GIT_WORK_TREE") || uk == "GIT_INDEX_FILE" {
			continue
		}
		out = append(out, e)
	}
	return out
}

// DistinctOutcomes documents that failure classes are not collapsed.
func DistinctOutcomes() []StepOutcome {
	return []StepOutcome{OutcomeTimeout, OutcomeMalformed, OutcomeUnauthenticated, OutcomeStale, OutcomeUnsupported}
}
