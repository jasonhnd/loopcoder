package processtree

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/process"
)

// Observer reads process tables. Tests inject fakes; production uses DarwinPS.
type Observer interface {
	// List returns a flat process table snapshot (bounded by caller).
	List() ([]RawProc, error)
}

// RawProc is one OS row before ownership classification.
type RawProc struct {
	PID    int
	PPID   int
	PGID   int
	LStart string
	Comm   string
	State  string // e.g. "Z" zombie, "R", "S"
}

// Tracker holds launch evidence and evaluates liveness.
type Tracker struct {
	Evidence LaunchEvidence
	// MaxNodes caps snapshot size (default DefaultMaxNodes).
	MaxNodes int
	// Observer defaults to DarwinPS on empty.
	Observer Observer
	// Clock for ObservedAt; defaults to time.Now.
	Now func() time.Time
}

// RecordLaunch stores durable root identity. Prefer process.Snapshot fields.
func RecordLaunch(pid int, attemptID string, now time.Time) (LaunchEvidence, error) {
	if pid <= 0 {
		return LaunchEvidence{}, fmt.Errorf("processtree: invalid root pid")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	id, err := process.Snapshot(pid, now)
	if err != nil {
		return LaunchEvidence{}, err
	}
	return LaunchEvidence{
		RootPID:              pid,
		PGID:                 id.PGID,
		ProcessBirthIdentity: id.ProcessBirthIdentity,
		ExecutableIdentity:   redactComm(id.ExecutableIdentity),
		RecordedAt:           now.UTC(),
		AttemptID:            attemptID,
	}, nil
}

// Observe builds a bounded tree snapshot and liveness assessment.
func (t *Tracker) Observe() Assessment {
	now := time.Now().UTC()
	if t.Now != nil {
		now = t.Now().UTC()
	}
	if t.Evidence.RootPID <= 0 {
		return Assessment{
			Liveness: LivenessNotStarted, Confidence: ConfidenceNone,
			Reasons:  []string{"not_started"},
			Snapshot: Snapshot{ObservedAt: now},
		}
	}
	max := t.MaxNodes
	if max <= 0 {
		max = DefaultMaxNodes
	}
	obs := t.Observer
	if obs == nil {
		obs = DarwinPS{}
	}
	raw, err := obs.List()
	snap := Snapshot{Root: t.Evidence, ObservedAt: now}
	if err != nil {
		snap.ObservationError = "list_failed"
		return Assessment{
			Liveness: LivenessUnknown, Confidence: ConfidenceNone,
			AttentionRequired: true,
			Reasons:           []string{"observation_failed"},
			Snapshot:          snap,
		}
	}

	byPID := map[int]RawProc{}
	children := map[int][]int{}
	for _, p := range raw {
		byPID[p.PID] = p
		children[p.PPID] = append(children[p.PPID], p.PID)
	}
	for k := range children {
		sort.Ints(children[k])
	}

	root, rootOK := byPID[t.Evidence.RootPID]
	rootAlive := process.Alive(t.Evidence.RootPID)

	// PID reuse: alive but birth identity differs.
	if rootAlive && rootOK {
		if t.Evidence.ProcessBirthIdentity != "" &&
			strings.TrimSpace(root.LStart) != "" &&
			root.LStart != t.Evidence.ProcessBirthIdentity {
			return Assessment{
				Liveness: LivenessUnknown, Confidence: ConfidenceFull,
				AttentionRequired: true,
				Reasons:           []string{"pid_reuse"},
				Snapshot:          snap,
			}
		}
		// Also verify via process package when available.
		if t.Evidence.ProcessBirthIdentity != "" {
			curBirth := processBirth(t.Evidence.RootPID)
			if curBirth != "" && curBirth != t.Evidence.ProcessBirthIdentity {
				return Assessment{
					Liveness: LivenessUnknown, Confidence: ConfidenceFull,
					AttentionRequired: true,
					Reasons:           []string{"pid_reuse"},
					Snapshot:          snap,
				}
			}
		}
	}

	// Walk owned tree: root + descendants with matching PGID (when known) or
	// reachable by PPID chain from root, capped at max nodes.
	// When the wrapper root has already exited, still seed from orphans that
	// list PPID==root or share the launch PGID.
	ownedSet := map[int]struct{}{}
	var queue []int
	if rootOK || rootAlive {
		queue = append(queue, t.Evidence.RootPID)
	} else {
		// Wrapper gone: adopt live descendants still claiming this root/PGID.
		for _, c := range children[t.Evidence.RootPID] {
			queue = append(queue, c)
		}
		if t.Evidence.PGID > 0 {
			for pid, p := range byPID {
				if p.PGID == t.Evidence.PGID && pid != t.Evidence.RootPID {
					queue = append(queue, pid)
				}
			}
		}
		sort.Ints(queue)
	}
	for len(queue) > 0 && len(ownedSet) < max {
		pid := queue[0]
		queue = queue[1:]
		if _, seen := ownedSet[pid]; seen {
			continue
		}
		// Do not re-add a dead root into ownedSet just for walking.
		if pid == t.Evidence.RootPID && !rootAlive && !rootOK {
			continue
		}
		ownedSet[pid] = struct{}{}
		for _, c := range children[pid] {
			if len(ownedSet)+len(queue) >= max {
				snap.Truncated = true
				break
			}
			// Prefer same PGID when root PGID known; still include direct children.
			if t.Evidence.PGID > 0 {
				if cp, ok := byPID[c]; ok && cp.PGID != t.Evidence.PGID && cp.PGID != 0 {
					// Child outside process group: mark escaped later if was expected.
					continue
				}
			}
			queue = append(queue, c)
		}
	}

	// Detect escaped: processes with PPID in owned set but not in owned (group mismatch).
	var escaped []int
	for pid, p := range byPID {
		if _, ok := ownedSet[p.PPID]; !ok {
			continue
		}
		if _, ok := ownedSet[pid]; ok {
			continue
		}
		escaped = append(escaped, pid)
	}
	sort.Ints(escaped)

	nodes := make([]Node, 0, len(ownedSet)+len(escaped))
	for pid := range ownedSet {
		p, ok := byPID[pid]
		n := Node{PID: pid, Owned: true}
		if ok {
			n.PPID = p.PPID
			n.PGID = p.PGID
			n.ProcessBirthIdentity = p.LStart
			n.Comm = redactComm(p.Comm)
			n.Zombie = isZombie(p.State)
		} else if process.Alive(pid) {
			// Unlisted but alive — partial observation.
			n.Comm = "?"
		}
		nodes = append(nodes, n)
	}
	for _, pid := range escaped {
		p := byPID[pid]
		nodes = append(nodes, Node{
			PID: pid, PPID: p.PPID, PGID: p.PGID,
			ProcessBirthIdentity: p.LStart,
			Comm:                 redactComm(p.Comm),
			Owned:                false, Escaped: true,
			Zombie: isZombie(p.State),
		})
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].PID < nodes[j].PID })
	snap.Nodes = nodes

	liveOwned := 0
	zombieOnly := true
	for _, n := range nodes {
		if !n.Owned {
			continue
		}
		if n.Zombie {
			continue
		}
		if process.Alive(n.PID) || byPIDHas(byPID, n.PID) {
			// If in table and not zombie, count as live for tree purposes.
			if !n.Zombie {
				liveOwned++
				zombieOnly = false
			}
		}
	}
	// Recount live owned carefully.
	liveOwned = 0
	for _, n := range nodes {
		if !n.Owned || n.Zombie {
			continue
		}
		if process.Alive(n.PID) {
			liveOwned++
		} else if _, ok := byPID[n.PID]; ok && !n.Zombie {
			// Present in table without Z — treat as live.
			liveOwned++
		}
	}
	_ = zombieOnly

	reasons := []string{}
	attention := false
	if len(escaped) > 0 {
		attention = true
		reasons = append(reasons, "escaped_descendant")
	}
	if snap.Truncated {
		reasons = append(reasons, "truncated")
		attention = true
	}
	// Unobservable: root not in table, not alive, but we expected something — unknown
	// if we can't prove exit of entire tree.
	rootInTable := rootOK
	rootIsZombie := rootOK && isZombie(root.State)

	var live Liveness
	conf := ConfidenceFull
	terminal := false

	switch {
	case !rootAlive && liveOwned == 0 && len(escaped) == 0:
		// Entire owned tree gone.
		live = LivenessExited
		terminal = true
		reasons = append(reasons, "tree_exited")
	case !rootAlive && liveOwned > 0:
		// Wrapper exit with still-owned descendants — NOT terminal.
		live = LivenessAlive
		reasons = append(reasons, "wrapper_exited_descendants_alive")
	case rootAlive && (rootIsZombie) && liveOwned == 0:
		live = LivenessExited
		terminal = true
		reasons = append(reasons, "root_zombie")
	case rootAlive:
		live = LivenessAlive
		if !rootInTable {
			conf = ConfidencePartial
			reasons = append(reasons, "root_partial")
		}
	case !rootAlive && liveOwned == 0 && len(escaped) > 0:
		live = LivenessUnknown
		attention = true
		conf = ConfidencePartial
		reasons = append(reasons, "escaped_after_root_exit")
	default:
		live = LivenessUnknown
		attention = true
		conf = ConfidencePartial
		reasons = append(reasons, "ambiguous")
	}

	// Permission/unobservable: if root claimed alive by signal but list missing many kids.
	if live == LivenessUnknown {
		attention = true
	}

	return Assessment{
		Liveness:          live,
		Confidence:        conf,
		AttentionRequired: attention,
		Reasons:           reasons,
		Snapshot:          snap,
		Terminal:          terminal,
	}
}

// AssessPIDReuse checks a single candidate PID against launch evidence.
func AssessPIDReuse(ev LaunchEvidence, currentBirth string, alive bool) error {
	if !alive {
		return nil
	}
	if ev.ProcessBirthIdentity == "" || currentBirth == "" {
		return nil
	}
	if currentBirth != ev.ProcessBirthIdentity {
		return ErrPIDReuse
	}
	return nil
}

func byPIDHas(m map[int]RawProc, pid int) bool {
	_, ok := m[pid]
	return ok
}

func isZombie(state string) bool {
	state = strings.TrimSpace(strings.ToUpper(state))
	return strings.HasPrefix(state, "Z")
}

func redactComm(comm string) string {
	comm = strings.TrimSpace(comm)
	// Drop path components and cap length; never keep long argv-like strings.
	if i := strings.LastIndex(comm, "/"); i >= 0 && i+1 < len(comm) {
		comm = comm[i+1:]
	}
	if len(comm) > 64 {
		comm = comm[:64]
	}
	// Strip obvious secret-looking tokens.
	if strings.Contains(strings.ToLower(comm), "token") ||
		strings.Contains(strings.ToLower(comm), "secret") ||
		strings.HasPrefix(comm, "sk-") {
		return "[redacted-comm]"
	}
	return comm
}

func processBirth(pid int) string {
	id, err := process.Snapshot(pid, time.Now())
	if err != nil {
		return ""
	}
	return id.ProcessBirthIdentity
}

// FormatSnapshot renders a secret-free ordered view for diagnostics.
func FormatSnapshot(s Snapshot) string {
	var b strings.Builder
	b.WriteString("root_pid=")
	b.WriteString(strconv.Itoa(s.Root.RootPID))
	b.WriteString(" nodes=")
	b.WriteString(strconv.Itoa(len(s.Nodes)))
	if s.Truncated {
		b.WriteString(" truncated=1")
	}
	for _, n := range s.Nodes {
		b.WriteByte('\n')
		b.WriteString(strconv.Itoa(n.PID))
		b.WriteByte(' ')
		if n.Owned {
			b.WriteString("owned")
		} else if n.Escaped {
			b.WriteString("escaped")
		} else {
			b.WriteString("other")
		}
		if n.Zombie {
			b.WriteString(" zombie")
		}
		b.WriteByte(' ')
		b.WriteString(n.Comm)
	}
	return b.String()
}
