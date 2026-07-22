package autoroute

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/capclass"
	"github.com/jasonhnd/loopcoder/internal/eligibility"
	"github.com/jasonhnd/loopcoder/internal/quotamode"
	"github.com/jasonhnd/loopcoder/internal/quotapolicy"
	"github.com/jasonhnd/loopcoder/internal/routedecision"
)

// Outcome classifies resolve results.
type Outcome string

const (
	OutcomeExplicitPin Outcome = "explicit_pin"
	OutcomeSelected    Outcome = "selected"
	OutcomeNoRoute     Outcome = "no_route"
	OutcomePinFail     Outcome = "pin_fail_closed"
	OutcomeInvalid     Outcome = "invalid"
)

// Input is the run-facing route resolution request.
type Input struct {
	// AutoRoute is true when --auto-route was set or when both provider and model omitted.
	AutoRoute  bool
	Provider   string
	Model      string
	Effort     string
	Permission string
	ProjectID  string
	// DecisionKey should be stable per run attempt for idempotency.
	DecisionKey string
	// TaskClass defaults to ClassTera (ordinary code) when empty.
	// Production run must pass a classified TaskClass (CRO-006); default remains
	// for pin-only paths and transitional callers.
	TaskClass capclass.Class
	Now       time.Time
	// Inventory is required for auto-route. Nil inventory fails closed — production
	// must never silently fall back to a fake/static matrix (V090-CRO-002 / #1335).
	// Tests inject FakeInventory() explicitly when they need selectable candidates.
	Inventory *Inventory
}

// Result is the resolved route or typed failure.
type Result struct {
	Outcome    Outcome
	Provider   string
	Model      string
	Effort     string
	Permission string
	// Decision is non-nil when auto-route path ran Evaluate.
	Decision *routedecision.Decision
	Explain  *routedecision.ExplainResult
	Message  string
	// Digest of decision when selected/no_route for evidence.
	Digest string
}

// Inventory is a frozen eligibility + soft ranking snapshot (no live probes).
type Inventory struct {
	EvidenceDigest string
	Candidates     []eligibility.Candidate
	Soft           []quotapolicy.Candidate
	Machine        eligibility.MachineAdmission
	Mode           quotamode.ModeConfig
}

var (
	ErrInvalid = errors.New("autoroute: invalid")
	ErrNoRoute = errors.New("autoroute: no route")
	ErrPinFail = errors.New("autoroute: pin fail closed")
)

// Resolve applies P4 routing policy for loopcoder run.
//
// - Both provider and model set → OutcomeExplicitPin (never overridden).
// - AutoRoute or both empty → Evaluate inventory; select or fail closed.
// - Only one of provider/model set without AutoRoute → invalid (partial pin).
func Resolve(in Input) (Result, error) {
	provider := strings.TrimSpace(in.Provider)
	model := strings.TrimSpace(in.Model)
	effort := strings.TrimSpace(in.Effort)
	perm := strings.TrimSpace(in.Permission)
	if perm == "" {
		perm = "default"
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	projectID := strings.TrimSpace(in.ProjectID)
	if projectID == "" {
		projectID = "local-project"
	}
	key := strings.TrimSpace(in.DecisionKey)
	if key == "" {
		key = "route_" + shortHash(fmt.Sprintf("%s|%s|%v|%d", provider, model, in.AutoRoute, now.UnixNano()))
	}
	taskClass := in.TaskClass
	if !taskClass.Valid() {
		taskClass = capclass.ClassTera
	}

	// Partial explicit pin without auto-route is usage error.
	if !in.AutoRoute && ((provider == "") != (model == "")) {
		return Result{
			Outcome: OutcomeInvalid,
			Message: "partial route pin: pass both --provider and --model, or use --auto-route / omit both",
		}, fmt.Errorf("%w: partial pin", ErrInvalid)
	}

	// Full explicit pin: never override.
	if provider != "" && model != "" && !in.AutoRoute {
		return Result{
			Outcome: OutcomeExplicitPin, Provider: provider, Model: model,
			Effort: effort, Permission: perm,
			Message: "explicit owner pin retained",
		}, nil
	}

	// Auto-route with both empty, or --auto-route (with or without pin — pin becomes hard eligibility pin).
	inv := in.Inventory
	if inv == nil {
		return Result{
			Outcome: OutcomeNoRoute,
			Message: "no real inventory snapshot: auto-route requires provider/account/model/quota evidence (refusing DefaultInventory/fake matrix)",
		}, fmt.Errorf("%w: missing real inventory", ErrNoRoute)
	}
	if strings.TrimSpace(inv.EvidenceDigest) == "" {
		inv.EvidenceDigest = "inventory-" + shortHash(fmt.Sprintf("%d", len(inv.Candidates)))
	}
	// Refuse the historical fake evidence digest even if someone re-supplies it.
	if inv.EvidenceDigest == fakeInventoryEvidenceDigest {
		return Result{
			Outcome: OutcomeNoRoute,
			Message: "refusing fake inventory evidence digest for production auto-route",
		}, fmt.Errorf("%w: fake inventory digest", ErrNoRoute)
	}

	elig := eligibility.Snapshot{
		TaskRequiredClass: taskClass,
		Candidates:        inv.Candidates,
		Machine:           inv.Machine,
		CapturedAt:        now,
	}
	// Explicit pin under --auto-route is still a hard pin in eligibility.
	if provider != "" && model != "" {
		elig.ExplicitPin = &eligibility.PinFields{Provider: provider, Model: model, Effort: effort, Permission: perm}
	}

	mode := inv.Mode
	if mode.Mode == "" {
		// Default soft policy: use-before-reset (burn_before_reset) after floors.
		mode = quotamode.DefaultModeConfig(quotamode.ModeBurnBeforeReset)
	}

	req := routedecision.Request{
		DecisionKey: key, ProjectID: projectID, EvidenceDigest: inv.EvidenceDigest,
		TaskClass: taskClass, Eligibility: elig, SoftCandidates: inv.Soft,
		Mode: mode, Now: now,
	}
	d, err := routedecision.Evaluate(req)
	explain := routedecision.Explain(d)
	res := Result{Decision: &d, Explain: &explain, Digest: d.Digest}

	switch d.Outcome {
	case routedecision.OutcomeSelected:
		if d.Winner == nil {
			return Result{Outcome: OutcomeNoRoute, Message: "selected without winner", Decision: &d}, ErrNoRoute
		}
		res.Outcome = OutcomeSelected
		res.Provider = d.Winner.Provider
		res.Model = d.Winner.Model
		// Default remaining empty effort to medium — never force high (CRO-005).
		res.Effort = firstNonEmpty(d.Winner.Effort, effort, "medium")
		res.Permission = firstNonEmpty(d.Winner.Permission, perm)
		res.Message = fmt.Sprintf("auto-route selected %s/%s digest=%s", res.Provider, res.Model, shortHash(d.Digest))
		return res, nil
	case routedecision.OutcomePinFail:
		res.Outcome = OutcomePinFail
		res.Message = "explicit pin fail-closed: " + strings.Join(d.Reasons, "; ")
		if res.Message == "explicit pin fail-closed: " {
			res.Message = "explicit pin fail-closed: pin ineligible"
		}
		return res, ErrPinFail
	default:
		res.Outcome = OutcomeNoRoute
		res.Message = "no eligible route: " + strings.Join(d.Reasons, "; ")
		if strings.TrimSuffix(res.Message, ": ") == "no eligible route" {
			res.Message = "no eligible route from inventory evidence"
		}
		if err != nil && !errors.Is(err, routedecision.ErrNoRoute) && !errors.Is(err, routedecision.ErrPinFailed) {
			res.Message = err.Error()
			return res, err
		}
		return res, ErrNoRoute
	}
}

// fakeInventoryEvidenceDigest is the historical digest of the official fake
// matrix. Resolve refuses this digest so production cannot smuggle fakes.
const fakeInventoryEvidenceDigest = "default-official-fake-v1"

// FakeInventory builds an explicit test-only candidate matrix.
//
// It must never be called from production run/resolve paths. Callers that need
// selectable inventory in unit tests pass the result as Input.Inventory.
// Production auto-route must supply a real capacity/inventory snapshot (CRO-003+).
//
// Deprecated name DefaultInventory remains as a thin alias for one release of
// test migration; new tests must call FakeInventory.
func FakeInventory(now time.Time) Inventory {
	_ = now
	type pm struct {
		p, m string
		cl   capclass.Class
		rem  float64
		ttr  time.Duration
	}
	rows := []pm{
		{"codex", "gpt-5.3-codex", capclass.ClassSoul, 0.7, 30 * time.Minute},
		{"claude", "claude-sonnet-4-5", capclass.ClassTera, 0.6, 2 * time.Hour},
		{"gemini", "gemini-2.5-flash", capclass.ClassTera, 0.5, 4 * time.Hour},
		{"antigravity", "gemini-2.5-pro", capclass.ClassSoul, 0.55, 90 * time.Minute},
		{"grok", "grok-4.5", capclass.ClassSoul, 0.65, 45 * time.Minute},
		// fixture adapter used by directrun local tests
		{"fixture", "fixture-model", capclass.ClassTera, 0.9, 10 * time.Minute},
	}
	var cands []eligibility.Candidate
	var softs []quotapolicy.Candidate
	for _, r := range rows {
		cands = append(cands, healthy(r.p, r.m, r.cl))
		softs = append(softs, soft(r.p, r.m, r.rem, r.ttr))
	}
	// Use a distinct test digest so Resolve does not refuse it when tests inject
	// FakeInventory explicitly. The historical default-official-fake-v1 digest
	// remains banned in Resolve.
	return Inventory{
		EvidenceDigest: "test-fake-inventory-v1",
		Candidates:     cands,
		Soft:           softs,
		Machine: eligibility.MachineAdmission{
			CapacityOK: okFact("mach"), ConcurrentSlots: 4,
		},
		Mode: quotamode.DefaultModeConfig(quotamode.ModeBalanced),
	}
}

// DefaultInventory is a deprecated alias of FakeInventory for test migration.
// Production Resolve never calls this function.
func DefaultInventory(now time.Time) Inventory {
	return FakeInventory(now)
}

func healthy(p, m string, cl capclass.Class) eligibility.Candidate {
	return eligibility.Candidate{
		Provider: p, Model: m, Effort: "medium", Permission: "bounded_write", ModelClass: cl,
		Installed: okFact(p + "-i"), Authenticated: okFact(p + "-a"), ModelPresent: okFact(p + "-m"),
		PermissionOK: okFact(p + "-p"), EffortOK: okFact(p + "-e"), Healthy: okFact(p + "-h"),
		CooldownActive: falseFact(p + "-cd"), ResourceFit: okFact(p + "-r"), QuotaRemaining: 9999,
	}
}

func soft(p, m string, rem float64, ttr time.Duration) quotapolicy.Candidate {
	rf, d := rem, ttr
	rel := 0.9
	return quotapolicy.Candidate{
		Provider: p, Model: m,
		Windows: []quotapolicy.Window{{
			Kind: quotapolicy.WindowFiveHour, RemainingFraction: &rf,
			Evidence: quotapolicy.EvidenceExact, TimeToReset: &d,
		}},
		Reliability: &rel, ReliabilityEvidence: quotapolicy.EvidenceExact,
	}
}

func okFact(id string) eligibility.Fact {
	return eligibility.Fact{State: eligibility.FactTrue, EvidenceID: id, Freshness: eligibility.FreshFresh}
}

func falseFact(id string) eligibility.Fact {
	return eligibility.Fact{State: eligibility.FactFalse, EvidenceID: id, Freshness: eligibility.FreshFresh}
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
