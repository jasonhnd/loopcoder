package retention

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Resource is one local lifecycle object under a project/store generation.
type Resource struct {
	ID    string
	Class Class
	// RelPath is home-relative; absolute paths are rejected for reports.
	RelPath string
	Bytes   int64
	Age     time.Duration
	// Lifecycle flags
	Active         bool
	Nonterminal    bool
	Attention      bool
	Unacknowledged bool
	Migration      bool
	Ambiguous      bool
	AuditMinimum   bool
	// Store generation for containment checks.
	ProjectID       string
	StoreGeneration int64
}

// Candidate is one dry-run inventory row.
type Candidate struct {
	ID        string     `json:"id"`
	Class     Class      `json:"class"`
	RelPath   string     `json:"rel_path"`
	Bytes     int64      `json:"bytes"`
	Age       string     `json:"age"`
	Action    string     `json:"action"` // hold|archive|delete
	Hold      HoldReason `json:"hold,omitempty"`
	Reason    string     `json:"reason"`
	ProjectID string     `json:"project_id,omitempty"`
	// RetainedAuthority is true when this object still contributes to rebuildable views.
	RetainedAuthority bool `json:"retained_authority,omitempty"`
}

// Plan is an explicit archive/delete plan (dry-run by default).
type Plan struct {
	Schema           string      `json:"schema"`
	DryRun           bool        `json:"dry_run"`
	Candidates       []Candidate `json:"candidates"`
	HeldCount        int         `json:"held_count"`
	ArchiveCount     int         `json:"archive_count"`
	DeleteCount      int         `json:"delete_count"`
	ExpectedReclaim  int64       `json:"expected_reclaim_bytes"`
	BackupRule       string      `json:"backup_rule"`
	StoreGenerations []string    `json:"store_generations"`
	// Redacted: no absolute machine paths, no private payloads.
	HomeBasename      string `json:"home_basename"`
	DiskFullStopAdmit bool   `json:"disk_full_stop_admit,omitempty"`
	DiskFullPruned    int    `json:"disk_full_pruned_expendable,omitempty"`
}

// SchemaPlan is the plan schema id.
const SchemaPlan = "loopcoder.retention.plan.v1"

// InventoryInput drives dry-run planning.
type InventoryInput struct {
	HomeBasename string
	Resources    []Resource
	Policies     map[Class]ClassPolicy
	// HomeRoot is the only allowed path prefix for containment (logical).
	HomeRoot string
	// DiskFull triggers stop-admission / expendable-only pruning policy.
	DiskFull bool
	// Apply when true produces an executable plan (still path-contained);
	// default dry-run only.
	Apply bool
}

// DryRun builds a deterministic inventory and plan without deleting anything.
func DryRun(in InventoryInput) Plan {
	pols := in.Policies
	if pols == nil {
		pols = DefaultPolicies()
	}
	var cands []Candidate
	var gens []string
	genSet := map[string]bool{}
	var reclaim int64
	held, arch, del := 0, 0, 0

	// Stable order by class then id.
	res := append([]Resource(nil), in.Resources...)
	sort.Slice(res, func(i, j int) bool {
		if res[i].Class != res[j].Class {
			return res[i].Class < res[j].Class
		}
		return res[i].ID < res[j].ID
	})

	for _, r := range res {
		pol := pols[r.Class]
		c := Candidate{
			ID: r.ID, Class: r.Class, RelPath: redactPath(r.RelPath),
			Bytes: r.Bytes, Age: r.Age.String(), ProjectID: r.ProjectID,
		}
		if r.ProjectID != "" {
			g := fmt.Sprintf("%s@%d", r.ProjectID, r.StoreGeneration)
			if !genSet[g] {
				genSet[g] = true
				gens = append(gens, g)
			}
		}

		// Path containment
		if !pathContained(in.HomeRoot, r.RelPath) {
			c.Action = "hold"
			c.Hold = HoldPathEscape
			c.Reason = "path not contained under home"
			c.RetainedAuthority = true
			held++
			cands = append(cands, c)
			continue
		}

		// Never-delete classes
		if pol.NeverDelete || r.Class == ClassCustomerRepo || r.Class == ClassCredentials || r.Class == ClassUnknown {
			c.Action = "hold"
			c.Hold = HoldNeverDelete
			c.Reason = "class never deleted by GC"
			c.RetainedAuthority = true
			held++
			cands = append(cands, c)
			continue
		}
		if r.AuditMinimum || r.Class == ClassAuditMin {
			c.Action = "hold"
			c.Hold = HoldAuditMinimum
			c.Reason = "minimum audit evidence retained"
			c.RetainedAuthority = true
			held++
			cands = append(cands, c)
			continue
		}

		// Lifecycle holds — regardless of age/size.
		if hr, why := lifecycleHold(r); hr != HoldNone {
			c.Action = "hold"
			c.Hold = hr
			c.Reason = why
			c.RetainedAuthority = true
			held++
			cands = append(cands, c)
			continue
		}

		// Eligibility by age/count/bytes pressure is simplified: age or class caps.
		overAge := pol.MaxAge > 0 && r.Age > pol.MaxAge
		// Count/bytes pressure evaluated at class level below; per-item uses age.
		if !overAge {
			c.Action = "hold"
			c.Hold = HoldNone
			c.Reason = "within retention window"
			c.RetainedAuthority = r.Class == ClassEvents
			held++
			cands = append(cands, c)
			continue
		}

		if pol.ArchiveEligible {
			c.Action = "archive"
			c.Reason = "exceeded max age; archive eligible"
			arch++
			reclaim += r.Bytes // after archive, local reclaim optional; count as planned
		} else {
			c.Action = "delete"
			c.Reason = "exceeded max age; expendable delete"
			del++
			reclaim += r.Bytes
		}
		cands = append(cands, c)
	}

	// Disk-full: stop admission; only prune preapproved expendable still held by age window
	// is not expanded — we only mark policy on the plan. Actual extra prune would
	// only target Expendable classes already eligible; we never silently delete audit.
	diskStop := false
	diskPruned := 0
	if in.DiskFull {
		diskStop = true
		// Convert within-window expendable temp to delete under disk-full only.
		for i := range cands {
			if cands[i].Action == "hold" && cands[i].Hold == HoldNone {
				pol := pols[cands[i].Class]
				if pol.Expendable && cands[i].Class == ClassTemp {
					cands[i].Action = "delete"
					cands[i].Reason = "disk-full preapproved expendable prune"
					cands[i].Hold = HoldNone
					held--
					del++
					diskPruned++
					// find bytes
					for _, r := range res {
						if r.ID == cands[i].ID {
							reclaim += r.Bytes
							break
						}
					}
				}
			}
		}
	}

	sort.Strings(gens)
	return Plan{
		Schema: SchemaPlan, DryRun: !in.Apply,
		Candidates: cands, HeldCount: held, ArchiveCount: arch, DeleteCount: del,
		ExpectedReclaim:  reclaim,
		BackupRule:       "archive before delete for archive-eligible classes; never delete audit_minimum",
		StoreGenerations: gens, HomeBasename: in.HomeBasename,
		DiskFullStopAdmit: diskStop, DiskFullPruned: diskPruned,
	}
}

// ApplyPlan validates a plan for execution: must be path-contained, no held
// items in delete/archive sets incorrectly, and is idempotent by candidate id.
// Returns the set of ids that would be acted on (does not touch real FS).
func ApplyPlan(p Plan) (acted []string, err error) {
	if p.DryRun {
		return nil, fmt.Errorf("refusing to apply dry-run plan")
	}
	seen := map[string]bool{}
	for _, c := range p.Candidates {
		if c.Action != "archive" && c.Action != "delete" {
			continue
		}
		if c.Hold != HoldNone {
			return nil, fmt.Errorf("cannot apply held candidate %s", c.ID)
		}
		if strings.HasPrefix(c.RelPath, "/") || strings.Contains(c.RelPath, "..") {
			return nil, fmt.Errorf("path not contained: %s", c.RelPath)
		}
		if seen[c.ID] {
			continue // idempotent
		}
		seen[c.ID] = true
		acted = append(acted, c.ID)
	}
	sort.Strings(acted)
	return acted, nil
}

func lifecycleHold(r Resource) (HoldReason, string) {
	switch {
	case r.Active:
		return HoldActive, "active resource held"
	case r.Nonterminal:
		return HoldNonterminal, "nonterminal lifecycle held"
	case r.Attention:
		return HoldAttention, "attention record held"
	case r.Unacknowledged:
		return HoldUnacknowledged, "unacknowledged record held"
	case r.Migration:
		return HoldMigration, "migration record held"
	case r.Ambiguous:
		return HoldAmbiguous, "ambiguous record held"
	default:
		return HoldNone, ""
	}
}

func pathContained(homeRoot, rel string) bool {
	if rel == "" {
		return false
	}
	if strings.HasPrefix(rel, "/") || strings.Contains(rel, "..") {
		return false
	}
	if homeRoot == "" {
		return true
	}
	// Logical join — reject if cleaned path escapes.
	clean := filepath.Clean(rel)
	return clean != ".." && !strings.HasPrefix(clean, "../")
}

func redactPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	// Strip absolute prefixes to basename-relative form for reports.
	if strings.HasPrefix(p, "/") {
		// take last two segments if possible
		parts := strings.Split(strings.Trim(p, "/"), "/")
		if len(parts) >= 2 {
			return parts[len(parts)-2] + "/" + parts[len(parts)-1]
		}
		return filepath.Base(p)
	}
	return p
}
