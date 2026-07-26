package compatshim

import "strings"

// CommandClass is how a legacy or current command is treated.
type CommandClass string

const (
	// ClassRemoved is gone; invoke fails with replacement guidance.
	ClassRemoved CommandClass = "removed"
	// ClassReadOnly is v0.8-compatible read/view only (one release).
	ClassReadOnly CommandClass = "read_only_compat"
	// ClassExporter is the explicit v0.8 exporter path.
	ClassExporter CommandClass = "explicit_exporter"
	// ClassUnsupported is not supported; replacement guidance only.
	ClassUnsupported CommandClass = "unsupported"
	// ClassV09 is a new command routed exclusively to v0.9 stores.
	ClassV09 CommandClass = "v09_only"
)

// CommandSpec describes one command surface.
type CommandSpec struct {
	Name        string
	Class       CommandClass
	Replacement string // guidance when removed/unsupported
	// Mutates is true when the command would write authority.
	Mutates bool
}

// Matrix is the closed support matrix for one release.
func Matrix() map[string]CommandSpec {
	return map[string]CommandSpec{
		// Removed autonomous / old mutation entry points.
		"compile":  {Name: "compile", Class: ClassRemoved, Replacement: "use ordinary development + GitHub PR; no loopcoder compile", Mutates: true},
		"dispatch": {Name: "dispatch", Class: ClassRemoved, Replacement: "use ordinary developer tools; no loopcoder dispatch", Mutates: true},
		"tick":     {Name: "tick", Class: ClassRemoved, Replacement: "removed autonomous tick", Mutates: true},
		"trigger":  {Name: "trigger", Class: ClassRemoved, Replacement: "removed autonomous trigger", Mutates: true},
		"promote":  {Name: "promote", Class: ClassRemoved, Replacement: "removed autonomous promotion", Mutates: true},
		// Read-only compatibility (one release).
		"status":  {Name: "status", Class: ClassReadOnly, Replacement: "", Mutates: false},
		"show":    {Name: "show", Class: ClassReadOnly, Replacement: "", Mutates: false},
		"history": {Name: "history", Class: ClassReadOnly, Replacement: "", Mutates: false},
		// Explicit exporter.
		"export-v08": {Name: "export-v08", Class: ClassExporter, Replacement: "internal/v08export", Mutates: false},
		// Unsupported legacy writers.
		"write-progress": {Name: "write-progress", Class: ClassUnsupported, Replacement: "v0.9 events/store writers only", Mutates: true},
		"write-report":   {Name: "write-report", Class: ClassUnsupported, Replacement: "v0.9 reporter only", Mutates: true},
		// New v0.9-only.
		"doctor":     {Name: "doctor", Class: ClassV09, Mutates: false},
		"import-v09": {Name: "import-v09", Class: ClassV09, Mutates: true},
		"rehydrate":  {Name: "rehydrate", Class: ClassV09, Mutates: true},
	}
}

// Classify returns the spec for a command name (case-insensitive).
func Classify(name string) (CommandSpec, bool) {
	m := Matrix()
	s, ok := m[strings.ToLower(strings.TrimSpace(name))]
	return s, ok
}

// DeprecationSchedule documents when compat classes retire.
type DeprecationSchedule struct {
	// ReadOnlyUntil is the last release major.minor that keeps read-only compat.
	ReadOnlyUntil string
	// ExporterUntil last release for explicit exporter.
	ExporterUntil string
	// RemovedEffective when removed commands stay gone.
	RemovedEffective string
}

// DefaultSchedule is the one-release compatibility window.
func DefaultSchedule() DeprecationSchedule {
	return DeprecationSchedule{
		ReadOnlyUntil:    "0.9.x",
		ExporterUntil:    "0.9.x",
		RemovedEffective: "0.9.0",
	}
}
