package machinerebuild

import "time"

// Schema constants for rebuild artifacts.
const (
	SchemaCandidate = "loopcoder.machinerebuild.candidate.v1"
	SchemaManifest  = "loopcoder.machinerebuild.manifest.v1"
	SchemaStore     = "loopcoder.machinerebuild.store.v1"
	SchemaReserv    = "loopcoder.machinerebuild.reservation.v1"
)

// ProjectSelfID is self-identifying metadata embedded in a project store.
// Machine-registration generation is independent of mutable local path.
type ProjectSelfID struct {
	Schema     string `json:"schema"`
	ProjectID  string `json:"project_id"`
	RepoOwner  string `json:"repo_owner"`
	RepoName   string `json:"repo_name"`
	Visibility string `json:"visibility"`
	// RegistrationGen increments when the project re-registers on a machine;
	// independent of path moves.
	RegistrationGen int64  `json:"registration_gen"`
	SchemaVersion   string `json:"schema_version"`
	// LocalPath is advisory only at rebuild time; not authority for identity.
	LocalPath string `json:"local_path,omitempty"`
	Owner     string `json:"owner,omitempty"`
}

// Candidate is one scanned projects/ child before validation.
type Candidate struct {
	Schema   string `json:"schema"`
	Path     string `json:"path"`
	BaseName string `json:"base_name"`
	// Flags set during scan.
	IsSymlink   bool           `json:"is_symlink,omitempty"`
	IsFile      bool           `json:"is_file,omitempty"`
	WrongOwner  bool           `json:"wrong_owner,omitempty"`
	DuplicateID bool           `json:"duplicate_id,omitempty"`
	Partial     bool           `json:"partial,omitempty"`
	Valid       bool           `json:"valid"`
	Diagnostic  string         `json:"diagnostic,omitempty"`
	Self        *ProjectSelfID `json:"self,omitempty"`
}

// ReservationStatus after reconciliation.
type ReservationStatus string

const (
	// ResReleased no live process evidence; capacity free.
	ResReleased ReservationStatus = "released"
	// ResLiveOwned live process evidence matches owner project.
	ResLiveOwned ReservationStatus = "live_owned"
	// ResAttention unknown ownership / ambiguous process; do not double-admit.
	ResAttention ReservationStatus = "attention_required"
)

// Reservation is one capacity hold reconstructed conservatively.
type Reservation struct {
	Schema    string            `json:"schema"`
	ID        string            `json:"id"`
	ProjectID string            `json:"project_id"`
	Kind      string            `json:"kind"` // worker|verifier|test|process
	Status    ReservationStatus `json:"status"`
	LivePIDs  []int             `json:"live_pids,omitempty"`
	Attention bool              `json:"attention,omitempty"`
	Reason    string            `json:"reason,omitempty"`
}

// ProviderFact is a refreshed observation with provenance (never credentials).
type ProviderFact struct {
	Provider   string    `json:"provider"`
	Available  bool      `json:"available"`
	Provenance string    `json:"provenance"` // probe|absent|stale_ignored
	ObservedAt time.Time `json:"observed_at"`
	// No credential fields by design.
}

// MachineStore is the rebuilt authority (in-memory fixture / pure model).
type MachineStore struct {
	Schema       string                   `json:"schema"`
	Projects     map[string]ProjectSelfID `json:"projects"` // project_id
	Aliases      map[string]string        `json:"aliases"`  // short or path basename → project_id
	Reservations []Reservation            `json:"reservations"`
	Providers    []ProviderFact           `json:"providers"`
	BuiltAt      time.Time                `json:"built_at"`
	// Backup of damaged store (path + digest); never overwritten in place.
	DamagedBackupPath   string `json:"damaged_backup_path,omitempty"`
	DamagedBackupDigest string `json:"damaged_backup_digest,omitempty"`
}

// Manifest is the redacted rebuild record (idempotent across unchanged evidence).
type Manifest struct {
	Schema             string    `json:"schema"`
	At                 time.Time `json:"at"`
	Home               string    `json:"home"` // redacted basename only in tests may be full fixture path
	AcceptedProjectIDs []string  `json:"accepted_project_ids"`
	RejectedCount      int       `json:"rejected_count"`
	// Diagnostics are labels only — no private content.
	RejectedDiagnostics []string       `json:"rejected_diagnostics"`
	ReservationSummary  map[string]int `json:"reservation_summary"` // status → count
	ProviderCount       int            `json:"provider_count"`
	DamagedBackupPath   string         `json:"damaged_backup_path,omitempty"`
	DamagedBackupDigest string         `json:"damaged_backup_digest,omitempty"`
	IdempotentReplay    bool           `json:"idempotent_replay,omitempty"`
	EvidenceFingerprint string         `json:"evidence_fingerprint"`
}

// ProcessEvidence is OS/process evidence for one PID (fixture).
type ProcessEvidence struct {
	PID       int
	ProjectID string // empty if unknown ownership
	Kind      string
	Alive     bool
}

// DamagedStore describes a missing/corrupt machine.db to preserve read-only.
type DamagedStore struct {
	// Path of the damaged file (empty if missing).
	Path string
	// Content digest when present (for backup identity).
	Digest string
	// Missing is true when the store was absent.
	Missing bool
	// Corrupt is true when present but unreadable.
	Corrupt bool
}
