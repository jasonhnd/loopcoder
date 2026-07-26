package machinerebuild

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// FSEntry is one child under projects/ as observed by a scanner (pure fixture).
type FSEntry struct {
	// Name is the directory base name under projects/.
	Name string
	// AbsPath full path.
	AbsPath string
	// Mode flags
	IsDir     bool
	IsSymlink bool
	IsFile    bool
	// Owner matches expected home owner when set.
	Owner string
	// Self is parsed project self-id when present; nil if partial.
	Self *ProjectSelfID
}

// ScanInput is the pure rebuild input (no real FS required in unit tests).
type ScanInput struct {
	Home            string
	ExpectedOwner   string
	ProjectsEntries []FSEntry
	Damaged         DamagedStore
	// Live process evidence from OS fixtures.
	Live []ProcessEvidence
	// Prior reservations recovered as uncertain claims (never auto-trusted).
	PriorReservations []Reservation
	// Provider probe results (fresh); stale snapshots are not passed here.
	ProviderProbes []ProviderFact
	Now            time.Time
}

// Result is one rebuild outcome.
type Result struct {
	Allowed  bool
	Reasons  []string
	Store    *MachineStore
	Manifest *Manifest
	// Rejected candidates isolated from authority.
	Rejected []Candidate
	Accepted []Candidate
}

// Rebuild scans candidates, isolates anomalies, rebuilds machine authority
// beside the damaged store, and reconciles reservations conservatively.
func Rebuild(in ScanInput) Result {
	res := Result{}
	if strings.TrimSpace(in.Home) == "" {
		res.Reasons = append(res.Reasons, "home path required")
		return res
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	// Preserve damaged DB identity for backup — never overwrite in place.
	backupPath, backupDigest := preserveDamaged(in.Damaged, in.Home)

	accepted, rejected := classifyCandidates(in.ProjectsEntries, in.ExpectedOwner)
	res.Accepted = accepted
	res.Rejected = rejected

	store := &MachineStore{
		Schema:              SchemaStore,
		Projects:            map[string]ProjectSelfID{},
		Aliases:             map[string]string{},
		BuiltAt:             now.UTC(),
		DamagedBackupPath:   backupPath,
		DamagedBackupDigest: backupDigest,
	}

	// Register accepted projects only.
	for _, c := range accepted {
		if c.Self == nil {
			continue
		}
		id := c.Self.ProjectID
		// Path is recorded as advisory only.
		self := *c.Self
		self.LocalPath = c.Path
		store.Projects[id] = self
		// Aliases: short basename and repo key — never collide by short name alone
		// when project ids differ; last write wins only for identical project.
		base := c.BaseName
		if base != "" {
			if prev, ok := store.Aliases[base]; ok && prev != id {
				// Do not overwrite; leave first registration (stable).
			} else {
				store.Aliases[base] = id
			}
		}
		repoKey := strings.ToLower(self.RepoOwner + "/" + self.RepoName)
		if repoKey != "/" && self.RepoOwner != "" {
			store.Aliases[repoKey] = id
		}
	}

	// Provider facts: only probe provenance; ignore any stale snapshot notion.
	for _, p := range in.ProviderProbes {
		if p.Provenance == "" || p.Provenance == "stale_ignored" {
			// Stale serialized snapshots are not reconstructed as current truth.
			continue
		}
		p.ObservedAt = now.UTC()
		store.Providers = append(store.Providers, p)
	}

	// Reservation reconciliation.
	store.Reservations = reconcileReservations(in.PriorReservations, in.Live, store.Projects)

	fp := evidenceFingerprint(accepted, rejected, in.Live, in.Damaged, store.Reservations)
	manifest := buildManifest(in.Home, now, accepted, rejected, store, backupPath, backupDigest, fp, false)

	res.Store = store
	res.Manifest = &manifest
	res.Allowed = true
	res.Reasons = append(res.Reasons, fmt.Sprintf(
		"rebuilt machine authority: accepted=%d rejected=%d reservations=%d",
		len(accepted), len(rejected), len(store.Reservations),
	))
	return res
}

// RebuildIdempotent runs Rebuild and marks the manifest when the evidence
// fingerprint matches a prior rebuild (caller supplies prior fingerprint).
func RebuildIdempotent(in ScanInput, priorFingerprint string) Result {
	r := Rebuild(in)
	if r.Manifest != nil && priorFingerprint != "" && r.Manifest.EvidenceFingerprint == priorFingerprint {
		r.Manifest.IdempotentReplay = true
		r.Reasons = append(r.Reasons, "idempotent rebuild against unchanged evidence")
	}
	return r
}

func classifyCandidates(entries []FSEntry, expectedOwner string) (accepted, rejected []Candidate) {
	seenIDs := map[string]string{} // project_id → first path
	for _, e := range entries {
		c := Candidate{
			Schema:   SchemaCandidate,
			Path:     e.AbsPath,
			BaseName: e.Name,
		}
		switch {
		case e.IsSymlink:
			c.IsSymlink = true
			c.Diagnostic = "symlink_rejected"
			rejected = append(rejected, c)
			continue
		case e.IsFile || !e.IsDir:
			c.IsFile = true
			c.Diagnostic = "not_directory"
			rejected = append(rejected, c)
			continue
		case expectedOwner != "" && e.Owner != "" && e.Owner != expectedOwner:
			c.WrongOwner = true
			c.Diagnostic = "wrong_owner"
			rejected = append(rejected, c)
			continue
		case e.Self == nil || strings.TrimSpace(e.Self.ProjectID) == "":
			c.Partial = true
			c.Diagnostic = "partial_or_missing_self_id"
			rejected = append(rejected, c)
			continue
		case strings.TrimSpace(e.Self.RepoOwner) == "" || strings.TrimSpace(e.Self.RepoName) == "":
			c.Partial = true
			c.Self = e.Self
			c.Diagnostic = "partial_repo_identity"
			rejected = append(rejected, c)
			continue
		}
		id := e.Self.ProjectID
		if prev, ok := seenIDs[id]; ok {
			c.DuplicateID = true
			c.Self = e.Self
			c.Diagnostic = "duplicate_project_id:" + path.Base(prev)
			rejected = append(rejected, c)
			continue
		}
		seenIDs[id] = e.AbsPath
		c.Valid = true
		c.Self = e.Self
		accepted = append(accepted, c)
	}
	return accepted, rejected
}

func reconcileReservations(prior []Reservation, live []ProcessEvidence, projects map[string]ProjectSelfID) []Reservation {
	// Index live by project.
	liveByProject := map[string][]ProcessEvidence{}
	unknownLive := []ProcessEvidence{}
	for _, p := range live {
		if !p.Alive {
			continue
		}
		if p.ProjectID == "" || projects[p.ProjectID].ProjectID == "" {
			unknownLive = append(unknownLive, p)
			continue
		}
		liveByProject[p.ProjectID] = append(liveByProject[p.ProjectID], p)
	}

	var out []Reservation
	seen := map[string]bool{}

	// Prior reservations: match to live or release / attention.
	for _, r := range prior {
		id := r.ID
		if id == "" {
			id = fmt.Sprintf("res-%s-%s", r.ProjectID, r.Kind)
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		nr := Reservation{
			Schema:    SchemaReserv,
			ID:        id,
			ProjectID: r.ProjectID,
			Kind:      r.Kind,
		}
		// Project no longer in authority → attention (do not auto-release capacity
		// that might still be live under unknown ownership).
		if _, ok := projects[r.ProjectID]; !ok {
			nr.Status = ResAttention
			nr.Attention = true
			nr.Reason = "reservation_project_not_in_rebuilt_authority"
			out = append(out, nr)
			continue
		}
		lives := liveByProject[r.ProjectID]
		var pids []int
		for _, lp := range lives {
			if r.Kind == "" || lp.Kind == "" || lp.Kind == r.Kind {
				pids = append(pids, lp.PID)
			}
		}
		if len(pids) > 0 {
			nr.Status = ResLiveOwned
			nr.LivePIDs = pids
			nr.Reason = "live_process_evidence_matches_project"
		} else {
			nr.Status = ResReleased
			nr.Reason = "no_live_process_evidence"
		}
		out = append(out, nr)
	}

	// Unknown live processes → attention, never auto-adopt.
	for i, u := range unknownLive {
		out = append(out, Reservation{
			Schema:    SchemaReserv,
			ID:        fmt.Sprintf("attn-unknown-%d-%d", u.PID, i),
			ProjectID: u.ProjectID,
			Kind:      u.Kind,
			Status:    ResAttention,
			LivePIDs:  []int{u.PID},
			Attention: true,
			Reason:    "unknown_live_process_ownership",
		})
	}

	// Stable order for idempotency.
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func preserveDamaged(d DamagedStore, home string) (backupPath, digest string) {
	if d.Missing || d.Path == "" {
		return "", ""
	}
	// New path beside damaged file — never the same path.
	base := filepath.Base(d.Path)
	if base == "" || base == "." {
		base = "machine.db"
	}
	backupPath = filepath.Join(home, "backups", base+".damaged."+shortDigest(d.Digest))
	digest = d.Digest
	if digest == "" && d.Corrupt {
		digest = "corrupt-unknown"
	}
	return backupPath, digest
}

func buildManifest(home string, now time.Time, accepted, rejected []Candidate, store *MachineStore, backupPath, backupDigest, fp string, idempotent bool) Manifest {
	ids := make([]string, 0, len(accepted))
	for _, c := range accepted {
		if c.Self != nil {
			ids = append(ids, c.Self.ProjectID)
		}
	}
	sort.Strings(ids)
	diags := make([]string, 0, len(rejected))
	for _, c := range rejected {
		diags = append(diags, c.Diagnostic)
	}
	sort.Strings(diags)
	summary := map[string]int{}
	for _, r := range store.Reservations {
		summary[string(r.Status)]++
	}
	return Manifest{
		Schema:              SchemaManifest,
		At:                  now.UTC(),
		Home:                home,
		AcceptedProjectIDs:  ids,
		RejectedCount:       len(rejected),
		RejectedDiagnostics: diags,
		ReservationSummary:  summary,
		ProviderCount:       len(store.Providers),
		DamagedBackupPath:   backupPath,
		DamagedBackupDigest: backupDigest,
		IdempotentReplay:    idempotent,
		EvidenceFingerprint: fp,
	}
}

func evidenceFingerprint(accepted, rejected []Candidate, live []ProcessEvidence, damaged DamagedStore, res []Reservation) string {
	type fp struct {
		Accepted []string
		Rejected []string
		Live     []string
		Damaged  string
		Res      []string
	}
	var a, r, l, rs []string
	for _, c := range accepted {
		if c.Self != nil {
			a = append(a, c.Self.ProjectID)
		}
	}
	for _, c := range rejected {
		a0 := c.Diagnostic
		r = append(r, a0)
	}
	for _, p := range live {
		l = append(l, fmt.Sprintf("%d:%s:%v", p.PID, p.ProjectID, p.Alive))
	}
	for _, x := range res {
		rs = append(rs, x.ID+":"+string(x.Status))
	}
	sort.Strings(a)
	sort.Strings(r)
	sort.Strings(l)
	sort.Strings(rs)
	b, _ := json.Marshal(fp{Accepted: a, Rejected: r, Live: l, Damaged: damaged.Digest + fmt.Sprint(damaged.Missing, damaged.Corrupt), Res: rs})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

func shortDigest(d string) string {
	if d == "" {
		return "none"
	}
	if len(d) > 8 {
		return d[:8]
	}
	return d
}
