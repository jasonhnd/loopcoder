package providerdesc

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	ErrDuplicate   = errors.New("providerdesc: duplicate adapter id")
	ErrInvalid     = errors.New("providerdesc: invalid")
	ErrNotFound    = errors.New("providerdesc: not found")
	ErrNotEligible = errors.New("providerdesc: not eligible")
	ErrUnsupported = errors.New("providerdesc: unsupported operation")
	ErrBoundary    = errors.New("providerdesc: adapter boundary violation")
)

// Adapter is the SPI surface. Implementations must not touch route policy,
// project lifecycle, GitHub delivery, or raw credentials.
type Adapter interface {
	Descriptor() Descriptor
	// Observe runs a claimed operation and returns a normalized observation.
	Observe(op Operation, in map[string]string) (Observation, error)
}

// Registered is an eligible registry entry.
type Registered struct {
	Descriptor Descriptor
	Eligible   bool
	// RegisteredAt uses injected clock when provided.
	RegisteredAt time.Time
}

// Registry holds validated descriptors. Failed registration leaves no entry.
type Registry struct {
	mu   sync.Mutex
	byID map[string]Registered
	now  func() time.Time
	// adapters kept only when eligible
	adapters map[string]Adapter
}

// NewRegistry creates an empty registry.
func NewRegistry(now func() time.Time) *Registry {
	if now == nil {
		now = time.Now
	}
	return &Registry{
		byID:     map[string]Registered{},
		adapters: map[string]Adapter{},
		now:      now,
	}
}

// Register validates and accepts an adapter. On any failure, no partial state
// remains for that adapter ID (machine observation left empty).
func (r *Registry) Register(a Adapter) (Registered, error) {
	if a == nil {
		return Registered{}, fmt.Errorf("%w: nil adapter", ErrInvalid)
	}
	d := a.Descriptor()
	d.Schema = SchemaDescriptor
	d.AdapterID = strings.ToLower(strings.TrimSpace(d.AdapterID))
	if err := ValidateDescriptor(d); err != nil {
		return Registered{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	// Capability/result: observe discover if claimed — optional dry check
	if err := assertNoBoundaryKeys(d); err != nil {
		return Registered{}, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[d.AdapterID]; ok {
		return Registered{}, fmt.Errorf("%w: %s", ErrDuplicate, d.AdapterID)
	}
	reg := Registered{Descriptor: d, Eligible: true, RegisteredAt: r.now().UTC()}
	r.byID[d.AdapterID] = reg
	r.adapters[d.AdapterID] = a
	return reg, nil
}

// Get returns a registered eligible descriptor.
func (r *Registry) Get(adapterID string) (Registered, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	reg, ok := r.byID[strings.ToLower(strings.TrimSpace(adapterID))]
	if !ok {
		return Registered{}, ErrNotFound
	}
	if !reg.Eligible {
		return Registered{}, ErrNotEligible
	}
	return reg, nil
}

// List returns sorted adapter IDs.
func (r *Registry) List() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]string, 0, len(r.byID))
	for id := range r.byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Observe dispatches a claimed operation; unsupported fails closed.
func (r *Registry) Observe(adapterID string, op Operation, in map[string]string) (Observation, error) {
	if err := assertInputBoundary(in); err != nil {
		return Observation{}, err
	}
	r.mu.Lock()
	a, ok := r.adapters[strings.ToLower(strings.TrimSpace(adapterID))]
	reg, rok := r.byID[strings.ToLower(strings.TrimSpace(adapterID))]
	r.mu.Unlock()
	if !ok || !rok || !reg.Eligible {
		return Observation{}, ErrNotFound
	}
	if !claims(reg.Descriptor, op) {
		return Observation{
			Schema: SchemaObservation, AdapterID: reg.Descriptor.AdapterID, Operation: op,
			OK: false, Confidence: ConfidenceNone,
			Diagnostic: &Diagnostic{Schema: SchemaDiagnostic, Class: DiagUnsupported, Message: "operation not claimed"},
		}, ErrUnsupported
	}
	obs, err := a.Observe(op, in)
	if err != nil {
		return obs, err
	}
	obs.Schema = SchemaObservation
	obs.AdapterID = reg.Descriptor.AdapterID
	obs.Operation = op
	// Capability/result mismatch: success payload for unclaimed ops already blocked;
	// also reject secret-shaped payload.
	if err := scrubObservation(&obs); err != nil {
		return Observation{}, err
	}
	return obs, nil
}

func claims(d Descriptor, op Operation) bool {
	for _, o := range d.Operations {
		if o == op {
			return true
		}
	}
	return false
}

func assertNoBoundaryKeys(d Descriptor) error {
	// Notes must not mention forbidden domains as control surfaces.
	n := strings.ToLower(d.Notes)
	for _, bad := range []string{"github token", "route policy write", "merge pr"} {
		if strings.Contains(n, bad) {
			return fmt.Errorf("%w: notes mention forbidden surface", ErrBoundary)
		}
	}
	return nil
}

func assertInputBoundary(in map[string]string) error {
	for k, v := range in {
		lk := strings.ToLower(k)
		if strings.Contains(lk, "credential") || strings.Contains(lk, "token") || strings.Contains(lk, "password") {
			return fmt.Errorf("%w: credential key %q", ErrBoundary, k)
		}
		if strings.Contains(lk, "route_decision") || strings.Contains(lk, "project_lifecycle") || strings.Contains(lk, "github_delivery") {
			return fmt.Errorf("%w: forbidden key %q", ErrBoundary, k)
		}
		if looksSecret(v) {
			return fmt.Errorf("%w: secret-shaped value", ErrBoundary)
		}
	}
	return nil
}

func scrubObservation(o *Observation) error {
	if o.Diagnostic != nil {
		o.Diagnostic.Schema = SchemaDiagnostic
		if looksSecret(o.Diagnostic.Message) || looksSecret(o.Diagnostic.Code) {
			return fmt.Errorf("%w: diagnostic secret", ErrBoundary)
		}
	}
	for k, v := range o.Payload {
		if looksSecret(v) || strings.Contains(v, "/Users/") {
			return fmt.Errorf("%w: payload %q", ErrBoundary, k)
		}
	}
	return nil
}
