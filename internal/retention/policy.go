package retention

import "time"

// Class is a retention class for local lifecycle objects.
type Class string

const (
	ClassEvents        Class = "events"
	ClassLogs          Class = "logs"
	ClassOutputExcerpt Class = "output_excerpt"
	ClassUIEvidence    Class = "ui_delivery_evidence"
	ClassTemp          Class = "temp_files"
	ClassStaleWorktree Class = "stale_worktree"
	ClassBackup        Class = "backups"
	// ClassAuditMin is the minimum audit evidence that must be retained.
	ClassAuditMin Class = "audit_minimum"
	// Non-expendable / never-GC classes (policy guards).
	ClassCustomerRepo Class = "customer_repo"
	ClassCredentials  Class = "credentials"
	ClassUnknown      Class = "unknown_file"
)

// HoldReason blocks collection.
type HoldReason string

const (
	HoldActive         HoldReason = "active"
	HoldNonterminal    HoldReason = "nonterminal"
	HoldAttention      HoldReason = "attention"
	HoldUnacknowledged HoldReason = "unacknowledged"
	HoldMigration      HoldReason = "migration"
	HoldAmbiguous      HoldReason = "ambiguous"
	HoldAuditMinimum   HoldReason = "audit_minimum"
	HoldNeverDelete    HoldReason = "never_delete_class"
	HoldPathEscape     HoldReason = "path_not_contained"
	HoldNone           HoldReason = ""
)

// ClassPolicy is default retention for one class.
type ClassPolicy struct {
	Class           Class
	MaxAge          time.Duration
	MaxCount        int
	MaxBytes        int64
	ArchiveEligible bool
	// Expendable allows disk-full pruning only when true.
	Expendable bool
	// NeverDelete hard-blocks GC.
	NeverDelete bool
}

// DefaultPolicies returns conservative defaults (owner may override caps).
func DefaultPolicies() map[Class]ClassPolicy {
	day := 24 * time.Hour
	return map[Class]ClassPolicy{
		ClassEvents:        {Class: ClassEvents, MaxAge: 90 * day, MaxCount: 100_000, MaxBytes: 512 << 20, ArchiveEligible: true, Expendable: false},
		ClassLogs:          {Class: ClassLogs, MaxAge: 30 * day, MaxCount: 10_000, MaxBytes: 256 << 20, ArchiveEligible: true, Expendable: true},
		ClassOutputExcerpt: {Class: ClassOutputExcerpt, MaxAge: 30 * day, MaxCount: 5_000, MaxBytes: 128 << 20, ArchiveEligible: true, Expendable: true},
		ClassUIEvidence:    {Class: ClassUIEvidence, MaxAge: 60 * day, MaxCount: 5_000, MaxBytes: 64 << 20, ArchiveEligible: true, Expendable: true},
		ClassTemp:          {Class: ClassTemp, MaxAge: 2 * day, MaxCount: 1_000, MaxBytes: 64 << 20, ArchiveEligible: false, Expendable: true},
		ClassStaleWorktree: {Class: ClassStaleWorktree, MaxAge: 7 * day, MaxCount: 50, MaxBytes: 2 << 30, ArchiveEligible: false, Expendable: true},
		ClassBackup:        {Class: ClassBackup, MaxAge: 60 * day, MaxCount: 20, MaxBytes: 2 << 30, ArchiveEligible: true, Expendable: false},
		ClassAuditMin:      {Class: ClassAuditMin, MaxAge: 365 * day, MaxCount: 10_000, MaxBytes: 64 << 20, ArchiveEligible: false, Expendable: false, NeverDelete: true},
		ClassCustomerRepo:  {Class: ClassCustomerRepo, NeverDelete: true, Expendable: false},
		ClassCredentials:   {Class: ClassCredentials, NeverDelete: true, Expendable: false},
		ClassUnknown:       {Class: ClassUnknown, NeverDelete: true, Expendable: false},
	}
}

// Overrides are owner-supplied cap adjustments (cannot lift NeverDelete).
type Overrides struct {
	MaxAge   map[Class]time.Duration
	MaxCount map[Class]int
	MaxBytes map[Class]int64
}

// ApplyOverrides returns a copy of policies with owner overrides applied.
func ApplyOverrides(base map[Class]ClassPolicy, o Overrides) map[Class]ClassPolicy {
	out := make(map[Class]ClassPolicy, len(base))
	for k, v := range base {
		if v.NeverDelete {
			out[k] = v
			continue
		}
		if o.MaxAge != nil {
			if d, ok := o.MaxAge[k]; ok {
				v.MaxAge = d
			}
		}
		if o.MaxCount != nil {
			if n, ok := o.MaxCount[k]; ok {
				v.MaxCount = n
			}
		}
		if o.MaxBytes != nil {
			if n, ok := o.MaxBytes[k]; ok {
				v.MaxBytes = n
			}
		}
		out[k] = v
	}
	return out
}
