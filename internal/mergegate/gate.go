package mergegate

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jasonhnd/loopcoder/internal/routepin"
)

const (
	SchemaRequest = "loopcoder.verifier.request.v1"
	SchemaVerdict = "loopcoder.verifier.verdict.v1"
	SchemaHuman   = "loopcoder.merge.human.v1"
)

// VerdictClass normalizes verifier outcomes.
type VerdictClass string

const (
	VerdictPass          VerdictClass = "pass"
	VerdictFail          VerdictClass = "fail"
	VerdictNeedsHuman    VerdictClass = "needs-human"
	VerdictUnavailable   VerdictClass = "unavailable"
	VerdictInvalidOutput VerdictClass = "invalid-output"
	VerdictCancelled     VerdictClass = "cancelled"
	VerdictStale         VerdictClass = "stale"
	VerdictBlocked       VerdictClass = "blocked"
)

// Precondition for launching verifier.
type Precondition struct {
	WorkerCleanupTerminal bool
	PRHeadStable          bool
	PRHeadOID             string
	CIReady               bool // required checks green
	WorkerSlotFree        bool // no concurrent local worker
	VerifierSlotFree      bool
}

// Request is an explicit verifier launch request.
type Request struct {
	Schema     string          `json:"schema"`
	AttemptID  string          `json:"attempt_id"`
	PRNumber   int             `json:"pr_number"`
	PRHeadOID  string          `json:"pr_head_oid"`
	PRBaseOID  string          `json:"pr_base_oid"`
	IssueSnap  string          `json:"issue_snapshot_digest"`
	ChecksSnap string          `json:"checks_snapshot_digest"`
	Route      routepin.Fields `json:"route"`
	// Permission must be read-only.
	Permission  string `json:"permission"`
	RouteDigest string `json:"route_digest"`
}

// Verdict is structured verifier result tied to head.
type Verdict struct {
	Schema         string       `json:"schema"`
	AttemptID      string       `json:"attempt_id"`
	PRNumber       int          `json:"pr_number"`
	PRHeadOID      string       `json:"pr_head_oid"`
	Class          VerdictClass `json:"class"`
	RouteDigest    string       `json:"route_digest"`
	ActualDigest   string       `json:"actual_digest,omitempty"`
	FindingsDigest string       `json:"findings_digest,omitempty"`
	// Mutated is always false for valid read-only runs.
	Mutated   bool      `json:"mutated"`
	CreatedAt time.Time `json:"created_at"`
	Stale     bool      `json:"stale"`
}

// HumanDecision is the merge gate record (no auto-merge).
type HumanDecision struct {
	Schema    string    `json:"schema"`
	PRNumber  int       `json:"pr_number"`
	PRHeadOID string    `json:"pr_head_oid"`
	Decision  string    `json:"decision"` // approve_merge|reject|defer
	Actor     string    `json:"actor"`
	At        time.Time `json:"at"`
	// AutoMerge is always false in v0.9 default path.
	AutoMerge bool `json:"auto_merge"`
}

var (
	ErrNotReady      = errors.New("mergegate: not ready")
	ErrRouteMismatch = errors.New("mergegate: route mismatch")
	ErrConcurrent    = errors.New("mergegate: concurrent worker/verifier")
	ErrReadOnly      = errors.New("mergegate: permission not read-only")
	ErrStale         = errors.New("mergegate: verdict stale")
	ErrInvalid       = errors.New("mergegate: invalid")
)

// Gate owns verifier and human gate state.
type Gate struct {
	mu             sync.Mutex
	verdicts       map[string]*Verdict // attemptID
	human          map[int]*HumanDecision
	workerActive   bool
	verifierActive bool
	now            func() time.Time
}

func NewGate(now func() time.Time) *Gate {
	if now == nil {
		now = time.Now
	}
	return &Gate{verdicts: map[string]*Verdict{}, human: map[int]*HumanDecision{}, now: now}
}

// CanLaunchVerifier enforces preconditions.
func (g *Gate) CanLaunchVerifier(pre Precondition, req Request) error {
	if !pre.WorkerCleanupTerminal {
		return fmt.Errorf("%w: worker not cleanup-terminal", ErrNotReady)
	}
	if !pre.PRHeadStable || pre.PRHeadOID == "" || pre.PRHeadOID != req.PRHeadOID {
		return fmt.Errorf("%w: pr head not stable", ErrNotReady)
	}
	if !pre.CIReady {
		return fmt.Errorf("%w: required checks not ready", ErrNotReady)
	}
	if !pre.WorkerSlotFree || g.workerActive {
		return ErrConcurrent
	}
	if !pre.VerifierSlotFree || g.verifierActive {
		return ErrConcurrent
	}
	if strings.ToLower(req.Permission) != "read-only" && strings.ToLower(req.Permission) != "readonly" {
		return ErrReadOnly
	}
	norm, err := req.Route.Normalize()
	if err != nil {
		return err
	}
	if req.RouteDigest != "" && req.RouteDigest != norm.Digest() {
		return ErrRouteMismatch
	}
	return nil
}

// BeginVerifier marks verifier slot and returns accepted request digest.
func (g *Gate) BeginVerifier(pre Precondition, req Request) (Request, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.canLaunchLocked(pre, req); err != nil {
		return Request{}, err
	}
	norm, _ := req.Route.Normalize()
	req.Route = norm
	req.RouteDigest = norm.Digest()
	req.Schema = SchemaRequest
	req.Permission = "read-only"
	g.verifierActive = true
	return req, nil
}

func (g *Gate) canLaunchLocked(pre Precondition, req Request) error {
	// duplicate of CanLaunchVerifier but assumes lock and uses g.workerActive
	if !pre.WorkerCleanupTerminal {
		return fmt.Errorf("%w: worker not cleanup-terminal", ErrNotReady)
	}
	if !pre.PRHeadStable || pre.PRHeadOID == "" || pre.PRHeadOID != req.PRHeadOID {
		return fmt.Errorf("%w: pr head not stable", ErrNotReady)
	}
	if !pre.CIReady {
		return fmt.Errorf("%w: required checks not ready", ErrNotReady)
	}
	if !pre.WorkerSlotFree || g.workerActive {
		return ErrConcurrent
	}
	if !pre.VerifierSlotFree || g.verifierActive {
		return ErrConcurrent
	}
	if !isReadOnly(req.Permission) {
		return ErrReadOnly
	}
	return nil
}

func isReadOnly(p string) bool {
	p = strings.ToLower(strings.TrimSpace(p))
	return p == "read-only" || p == "readonly" || p == ""
}

// CompleteVerifier records verdict; actual route must match request.
func (g *Gate) CompleteVerifier(req Request, class VerdictClass, actual routepin.Fields, findings string, mutated bool) (Verdict, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.verifierActive {
		return Verdict{}, ErrInvalid
	}
	if mutated {
		g.verifierActive = false
		return Verdict{}, errors.New("mergegate: verifier mutated state")
	}
	act, err := actual.Normalize()
	if err != nil {
		g.verifierActive = false
		return Verdict{}, err
	}
	ad := act.Digest()
	if ad != req.RouteDigest {
		g.verifierActive = false
		v := Verdict{
			Schema: SchemaVerdict, AttemptID: req.AttemptID, PRNumber: req.PRNumber,
			PRHeadOID: req.PRHeadOID, Class: VerdictBlocked, RouteDigest: req.RouteDigest,
			ActualDigest: ad, CreatedAt: g.now().UTC(),
		}
		g.verdicts[req.AttemptID] = &v
		return v, ErrRouteMismatch
	}
	v := Verdict{
		Schema: SchemaVerdict, AttemptID: req.AttemptID, PRNumber: req.PRNumber,
		PRHeadOID: req.PRHeadOID, Class: class, RouteDigest: req.RouteDigest,
		ActualDigest: ad, FindingsDigest: digestStr(findings), Mutated: false,
		CreatedAt: g.now().UTC(),
	}
	g.verdicts[req.AttemptID] = &v
	g.verifierActive = false
	return v, nil
}

// InvalidateOnHeadChange marks verdicts stale when PR head changes.
func (g *Gate) InvalidateOnHeadChange(pr int, newHead string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, v := range g.verdicts {
		if v.PRNumber == pr && v.PRHeadOID != newHead {
			v.Stale = true
			v.Class = VerdictStale
		}
	}
}

// GetVerdict returns verdict; stale if head mismatch.
func (g *Gate) GetVerdict(attemptID, currentHead string) (Verdict, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	v, ok := g.verdicts[attemptID]
	if !ok {
		return Verdict{}, ErrInvalid
	}
	cp := *v
	if currentHead != "" && cp.PRHeadOID != currentHead {
		cp.Stale = true
		cp.Class = VerdictStale
	}
	return cp, nil
}

// RecordHumanDecision records explicit human gate (never auto-merge).
func (g *Gate) RecordHumanDecision(pr int, head, decision, actor string) (HumanDecision, error) {
	decision = strings.ToLower(strings.TrimSpace(decision))
	switch decision {
	case "approve_merge", "reject", "defer":
	default:
		return HumanDecision{}, ErrInvalid
	}
	if actor == "" || head == "" || pr <= 0 {
		return HumanDecision{}, ErrInvalid
	}
	// pass still requires human — we always record AutoMerge=false
	d := HumanDecision{
		Schema: SchemaHuman, PRNumber: pr, PRHeadOID: head,
		Decision: decision, Actor: actor, At: g.now().UTC(), AutoMerge: false,
	}
	g.mu.Lock()
	g.human[pr] = &d
	g.mu.Unlock()
	return d, nil
}

// MayAutoMerge is always false for v0.9 default.
func (g *Gate) MayAutoMerge(pr int) bool {
	return false
}

// SetWorkerActive tracks concurrent worker for admission.
func (g *Gate) SetWorkerActive(v bool) {
	g.mu.Lock()
	g.workerActive = v
	g.mu.Unlock()
}

func digestStr(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])[:16]
}
