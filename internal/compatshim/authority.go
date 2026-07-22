package compatshim

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// WriterGeneration tracks which authority last wrote a project.
type WriterGeneration string

const (
	// GenNone no writes yet.
	GenNone WriterGeneration = "none"
	// GenV08 legacy path wrote (pre-migration).
	GenV08 WriterGeneration = "v0.8"
	// GenV09 v0.9 path has accepted writes — locks out old mutation.
	GenV09 WriterGeneration = "v0.9"
)

// ProjectAuthority is per-project writer metadata.
type ProjectAuthority struct {
	ProjectID     string           `json:"project_id"`
	Writer        WriterGeneration `json:"writer_generation"`
	LastWriteAt   time.Time        `json:"last_write_at,omitempty"`
	LastWriterCmd string           `json:"last_writer_cmd,omitempty"`
}

// Registry holds project authority and enforces isolation.
type Registry struct {
	mu   sync.Mutex
	auth map[string]*ProjectAuthority
	now  func() time.Time
}

// NewRegistry creates an empty authority registry.
func NewRegistry(now func() time.Time) *Registry {
	if now == nil {
		now = time.Now
	}
	return &Registry{auth: map[string]*ProjectAuthority{}, now: now}
}

// Decision is the result of attempting a command against a project.
type Decision struct {
	Allowed bool
	Reasons []string
	// CompatPrefix is non-empty when output must be marked compatibility-only.
	CompatPrefix string
	// ExcludeFromV09Gates when true (compat output).
	ExcludeFromV09Gates bool
	Class               CommandClass
}

// CompatOutputPrefix is prepended to all compatibility surface output.
const CompatOutputPrefix = "[loopcoder-compat-v0.8] "

// Evaluate decides whether name may run against projectID.
func (r *Registry) Evaluate(projectID, command string) Decision {
	spec, ok := Classify(command)
	if !ok {
		return Decision{
			Allowed: false,
			Reasons: []string{"unknown command; not in support matrix"},
			Class:   ClassUnsupported,
		}
	}
	d := Decision{Class: spec.Class}

	switch spec.Class {
	case ClassRemoved:
		d.Allowed = false
		d.Reasons = append(d.Reasons, "command removed: "+spec.Replacement)
		return d
	case ClassUnsupported:
		d.Allowed = false
		d.Reasons = append(d.Reasons, "unsupported: "+spec.Replacement)
		return d
	case ClassReadOnly, ClassExporter:
		d.Allowed = true
		d.CompatPrefix = CompatOutputPrefix
		d.ExcludeFromV09Gates = true
		d.Reasons = append(d.Reasons, "compatibility surface; output excluded from v0.9 status/gates")
		if spec.Mutates {
			// Exporter is non-mutating by matrix; defensive.
			d.Allowed = false
			d.Reasons = append(d.Reasons, "compatibility path must not mutate")
		}
		return d
	case ClassV09:
		// exclusive v0.9 route
		d.Allowed = true
		d.Reasons = append(d.Reasons, "routed exclusively to v0.9 stores/events/runtime")
		return d
	default:
		d.Allowed = false
		d.Reasons = append(d.Reasons, "unclassified command")
		return d
	}
}

// BeginWrite records a mutation attempt. Returns error if isolation violated.
// v0.9 write on a project locks out subsequent legacy mutation.
// Legacy mutation is refused once Writer==GenV09.
func (r *Registry) BeginWrite(projectID, command string, generation WriterGeneration) error {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return fmt.Errorf("project_id required")
	}
	spec, ok := Classify(command)
	if !ok {
		return fmt.Errorf("unknown command %s", command)
	}
	if !spec.Mutates && generation != GenV09 {
		// non-mutating should not call BeginWrite; allow no-op deny for safety
		return fmt.Errorf("command %s is non-mutating", command)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.auth[projectID]
	if a == nil {
		a = &ProjectAuthority{ProjectID: projectID, Writer: GenNone}
		r.auth[projectID] = a
	}

	switch generation {
	case GenV08:
		if a.Writer == GenV09 {
			return fmt.Errorf("legacy mutation refused: project %s accepted v0.9 writes", projectID)
		}
		// Legacy write only if not yet on v0.9 — still discouraged for removed cmds.
		if spec.Class == ClassRemoved || spec.Class == ClassUnsupported {
			return fmt.Errorf("legacy mutating command %s not permitted", command)
		}
		a.Writer = GenV08
	case GenV09:
		// v0.9 always may write; upgrades generation.
		a.Writer = GenV09
	default:
		return fmt.Errorf("invalid writer generation %s", generation)
	}
	a.LastWriteAt = r.now().UTC()
	a.LastWriterCmd = command
	return nil
}

// Authority returns a copy of project authority.
func (r *Registry) Authority(projectID string) ProjectAuthority {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.auth[projectID]
	if a == nil {
		return ProjectAuthority{ProjectID: projectID, Writer: GenNone}
	}
	return *a
}

// FormatCompat prefixes lines for compatibility output.
func FormatCompat(body string) string {
	if body == "" {
		return CompatOutputPrefix
	}
	lines := strings.Split(body, "\n")
	for i, ln := range lines {
		lines[i] = CompatOutputPrefix + ln
	}
	return strings.Join(lines, "\n")
}

// IncludeInV09Status reports whether a command's output may feed v0.9 gates.
func IncludeInV09Status(command string) bool {
	spec, ok := Classify(command)
	if !ok {
		return false
	}
	return spec.Class == ClassV09
}
