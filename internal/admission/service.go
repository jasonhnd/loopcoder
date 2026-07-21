package admission

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/authoritystore"
)

// Service evaluates policy and enforces generation-fenced reservations in machine.db.
type Service struct {
	ms     *authoritystore.MachineStore
	budget Budget
	now    func() time.Time
	live   LivenessProbe
}

// Options configures Service.
type Options struct {
	Budget Budget
	Now    func() time.Time
	Live   LivenessProbe
}

// New binds a machine store. Call Ensure once before Claim.
func New(ms *authoritystore.MachineStore, opts Options) (*Service, error) {
	if ms == nil || ms.Foundation() == nil {
		return nil, fmt.Errorf("admission: nil machine store")
	}
	b := opts.Budget
	if b.MaxActiveWorkers == 0 && b.MaxLocalTests == 0 && b.MaxChildProcesses == 0 && b.MaxRSSBytes == 0 && b.MaxCPURate == 0 {
		b = DefaultBudget()
	}
	// Fill zero fields with defaults so partial overrides still make sense.
	def := DefaultBudget()
	if b.MaxActiveWorkers == 0 {
		b.MaxActiveWorkers = def.MaxActiveWorkers
	}
	if b.MaxLocalTests == 0 {
		b.MaxLocalTests = def.MaxLocalTests
	}
	if b.MaxChildProcesses == 0 {
		b.MaxChildProcesses = def.MaxChildProcesses
	}
	if b.MaxRSSBytes == 0 {
		b.MaxRSSBytes = def.MaxRSSBytes
	}
	if b.MaxCPURate == 0 {
		b.MaxCPURate = def.MaxCPURate
	}
	// MaxActiveVerifiers may intentionally be 0.
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	live := opts.Live
	if live == nil {
		live = AlwaysUnknownProbe{}
	}
	return &Service{ms: ms, budget: b, now: now, live: live}, nil
}

// Budget returns the configured machine budget (copy).
func (s *Service) Budget() Budget { return s.budget }

// Claim atomically admits a reservation or returns an explainable denial.
// Concurrent claims share one write transaction on machine.db (MaxOpenConns=1).
func (s *Service) Claim(ctx context.Context, req Request) (Decision, error) {
	if err := validateRequest(req); err != nil {
		return Decision{SchemaVersion: SchemaVersion, Reasons: []string{"invalid_request"}}, err
	}
	ttl := req.LeaseTTL
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}
	var out Decision
	err := s.ms.Foundation().WithWriteTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if err := ensureTx(ctx, tx); err != nil {
			return err
		}
		now := s.now().UTC()
		// Expire/attention-refresh before summing usage.
		if err := s.refreshExpiredTx(ctx, tx, now); err != nil {
			return err
		}

		// Idempotent replay.
		if req.IdempotencyKey != "" {
			existing, ok, err := loadByIdempotency(ctx, tx, req.IdempotencyKey)
			if err != nil {
				return err
			}
			if ok {
				if existing.State == StateActive {
					out = decisionFromReservation(existing, true, s.budget, now)
					out.Replay = true
					return nil
				}
				// Released/expired/attention: new claim needs a new key or explicit resolve.
				if existing.State == StateAttentionRequired {
					out = Decision{
						SchemaVersion:     SchemaVersion,
						Admitted:          false,
						ReservationID:     existing.ID,
						Generation:        existing.Generation,
						State:             existing.State,
						Reasons:           []string{"idempotency_attention_required", existing.AttentionReason},
						AttentionRequired: true,
						Requested:         requestView(req),
					}
					return ErrAttentionRequired
				}
			}
		}

		reserved, err := sumActive(ctx, tx, now)
		if err != nil {
			return err
		}
		// Attention rows block capacity of their dimensions until resolved.
		attention, err := sumAttention(ctx, tx)
		if err != nil {
			return err
		}
		// Unknown capacity from attention is reported but not freely reusable.
		unknown := attention

		available := subtractBudget(s.budget, reserved)
		// Capacity held by attention is unavailable.
		available = subtractView(available, attention)

		requested := requestView(req)
		denied := ResourceView{}
		reasons := []string{}

		// Role-specific slots.
		switch req.Role {
		case RoleWorker:
			if reserved.Workers+attention.Workers+1 > s.budget.MaxActiveWorkers {
				denied.Workers = 1
				reasons = append(reasons, "worker_budget_exhausted")
			}
		case RoleVerifier:
			// Default: zero verifier while any worker active.
			if reserved.Workers+attention.Workers > 0 && s.budget.MaxActiveVerifiers == 0 {
				denied.Verifiers = 1
				reasons = append(reasons, "verifier_blocked_while_worker_active")
			} else if reserved.Verifiers+attention.Verifiers+1 > s.budget.MaxActiveVerifiers {
				denied.Verifiers = 1
				reasons = append(reasons, "verifier_budget_exhausted")
			}
		case RoleLocalTest:
			if reserved.LocalTests+attention.LocalTests+1 > s.budget.MaxLocalTests {
				denied.LocalTests = 1
				reasons = append(reasons, "local_test_budget_exhausted")
			}
		}

		needProc := req.Processes
		if needProc <= 0 {
			needProc = 1
		}
		if reserved.Processes+attention.Processes+needProc > s.budget.MaxChildProcesses {
			denied.Processes = needProc
			reasons = append(reasons, "process_budget_exhausted")
		}
		if req.RSSBytes > 0 && reserved.RSSBytes+attention.RSSBytes+req.RSSBytes > s.budget.MaxRSSBytes {
			denied.RSSBytes = req.RSSBytes
			reasons = append(reasons, "rss_budget_exhausted")
		}
		if req.CPURate > 0 && reserved.CPURate+attention.CPURate+req.CPURate > s.budget.MaxCPURate+1e-9 {
			denied.CPURate = req.CPURate
			reasons = append(reasons, "cpu_budget_exhausted")
		}

		out = Decision{
			SchemaVersion: SchemaVersion,
			Requested:     requested,
			Reserved:      reserved,
			Available:     available,
			Denied:        denied,
			Unknown:       unknown,
		}

		if len(reasons) > 0 {
			out.Admitted = false
			out.Reasons = reasons
			return nil // not an error path for policy denial (caller checks Admitted)
		}

		id := reservationID(req, now)
		lease := now.Add(ttl)
		res := Reservation{
			ID:             id,
			Generation:     1,
			ProjectID:      strings.TrimSpace(req.ProjectID),
			JobID:          strings.TrimSpace(req.JobID),
			AttemptID:      strings.TrimSpace(req.AttemptID),
			Role:           req.Role,
			State:          StateActive,
			Processes:      needProc,
			RSSBytes:       req.RSSBytes,
			CPURate:        req.CPURate,
			LeaseUntil:     lease,
			IdempotencyKey: strings.TrimSpace(req.IdempotencyKey),
			CreatedAt:      now,
			UpdatedAt:      now,
		}
		if res.IdempotencyKey == "" {
			res.IdempotencyKey = id
		}
		if err := insertReservation(ctx, tx, res); err != nil {
			return err
		}
		// Re-read sums after insert for decision transparency.
		reserved2, err := sumActive(ctx, tx, now)
		if err != nil {
			return err
		}
		out.Admitted = true
		out.ReservationID = res.ID
		out.Generation = res.Generation
		out.State = StateActive
		out.LeaseUntil = lease
		out.Reserved = reserved2
		out.Available = subtractBudget(s.budget, reserved2)
		out.Reasons = []string{"admitted"}
		return nil
	})
	if err != nil {
		return out, err
	}
	if !out.Admitted && len(out.Reasons) > 0 && out.Reasons[0] != "admitted" {
		// Policy denial is not a transport error; return ErrDenied for callers that want it.
		return out, ErrDenied
	}
	return out, nil
}

// Renew extends the lease if generation matches and state is active.
func (s *Service) Renew(ctx context.Context, reservationID string, generation int64, ttl time.Duration) (Reservation, error) {
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}
	var out Reservation
	err := s.ms.Foundation().WithWriteTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if err := ensureTx(ctx, tx); err != nil {
			return err
		}
		now := s.now().UTC()
		if err := s.refreshExpiredTx(ctx, tx, now); err != nil {
			return err
		}
		res, ok, err := loadByID(ctx, tx, reservationID)
		if err != nil {
			return err
		}
		if !ok {
			return ErrNotFound
		}
		if res.Generation != generation {
			return ErrGenerationMismatch
		}
		if res.State == StateAttentionRequired {
			return ErrAttentionRequired
		}
		if res.State != StateActive {
			return fmt.Errorf("%w: state=%s", ErrGenerationMismatch, res.State)
		}
		res.Generation++
		res.LeaseUntil = now.Add(ttl)
		res.UpdatedAt = now
		if err := updateReservation(ctx, tx, res); err != nil {
			return err
		}
		out = res
		return nil
	})
	return out, err
}

// Release idempotently releases a reservation when generation matches.
// Generation 0 means "any generation" for explicit force-release after attention resolve.
func (s *Service) Release(ctx context.Context, reservationID string, generation int64) (Reservation, error) {
	var out Reservation
	err := s.ms.Foundation().WithWriteTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if err := ensureTx(ctx, tx); err != nil {
			return err
		}
		now := s.now().UTC()
		res, ok, err := loadByID(ctx, tx, reservationID)
		if err != nil {
			return err
		}
		if !ok {
			return ErrNotFound
		}
		if res.State == StateReleased {
			out = res
			return nil // idempotent
		}
		if generation != 0 && res.Generation != generation {
			return ErrGenerationMismatch
		}
		res.State = StateReleased
		res.UpdatedAt = now
		if err := updateReservation(ctx, tx, res); err != nil {
			return err
		}
		out = res
		return nil
	})
	return out, err
}

// ResolveAttention releases or re-activates after human/process authority is known.
// If live processes are confirmed without release intent, state stays attention_required.
func (s *Service) ResolveAttention(ctx context.Context, reservationID string, release bool) (Reservation, error) {
	var out Reservation
	err := s.ms.Foundation().WithWriteTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if err := ensureTx(ctx, tx); err != nil {
			return err
		}
		now := s.now().UTC()
		res, ok, err := loadByID(ctx, tx, reservationID)
		if err != nil {
			return err
		}
		if !ok {
			return ErrNotFound
		}
		if res.State != StateAttentionRequired && res.State != StateExpired {
			out = res
			return nil
		}
		live, unknown, err := s.live.Probe(res)
		if err != nil {
			return err
		}
		if unknown {
			res.State = StateAttentionRequired
			res.AttentionReason = "liveness_unknown"
			res.UpdatedAt = now
			if err := updateReservation(ctx, tx, res); err != nil {
				return err
			}
			out = res
			return ErrAttentionRequired
		}
		if live && !release {
			res.State = StateAttentionRequired
			res.AttentionReason = "process_still_live"
			res.UpdatedAt = now
			if err := updateReservation(ctx, tx, res); err != nil {
				return err
			}
			out = res
			return ErrAttentionRequired
		}
		// Dead or explicit release: free capacity.
		res.State = StateReleased
		res.AttentionReason = ""
		res.UpdatedAt = now
		if err := updateReservation(ctx, tx, res); err != nil {
			return err
		}
		out = res
		return nil
	})
	return out, err
}

// Observe records observed use and emits at most one enforcement request per
// metric transition key. Does not mark attempt failure.
func (s *Service) Observe(ctx context.Context, reservationID string, use ObservedUse) ([]EnforcementRequest, error) {
	var emitted []EnforcementRequest
	err := s.ms.Foundation().WithWriteTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if err := ensureTx(ctx, tx); err != nil {
			return err
		}
		now := s.now().UTC()
		res, ok, err := loadByID(ctx, tx, reservationID)
		if err != nil {
			return err
		}
		if !ok {
			return ErrNotFound
		}
		if res.State != StateActive {
			return nil
		}
		// Thresholds: over reservation units or global budget dimension.
		type cand struct {
			metric    Metric
			observed  float64
			threshold float64
			key       string
		}
		cands := []cand{}
		if use.ProcessCount > res.Processes {
			cands = append(cands, cand{MetricProcesses, float64(use.ProcessCount), float64(res.Processes), "proc_over_reservation"})
		}
		if use.RSSBytes > 0 && res.RSSBytes > 0 && use.RSSBytes > res.RSSBytes {
			cands = append(cands, cand{MetricRSS, float64(use.RSSBytes), float64(res.RSSBytes), "rss_over_reservation"})
		}
		if use.CPURate > 0 && res.CPURate > 0 && use.CPURate > res.CPURate+1e-9 {
			cands = append(cands, cand{MetricCPU, use.CPURate, res.CPURate, "cpu_over_reservation"})
		}
		for _, c := range cands {
			er, created, err := insertEnforcementOnce(ctx, tx, res.ID, c.key, c.metric, c.observed, c.threshold, now)
			if err != nil {
				return err
			}
			if created {
				emitted = append(emitted, er)
			}
		}
		res.UpdatedAt = now
		return updateReservation(ctx, tx, res)
	})
	return emitted, err
}

// Get loads a reservation by id.
func (s *Service) Get(ctx context.Context, reservationID string) (Reservation, bool, error) {
	var out Reservation
	var ok bool
	err := s.ms.Foundation().WithWriteTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if err := ensureTx(ctx, tx); err != nil {
			return err
		}
		var err error
		out, ok, err = loadByID(ctx, tx, reservationID)
		return err
	})
	return out, ok, err
}

// refreshExpiredTx moves overdue active rows to expired or attention_required.
func (s *Service) refreshExpiredTx(ctx context.Context, tx *sql.Tx, now time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT reservation_id, generation, project_id, job_id, attempt_id, role, state,
		processes, rss_bytes, cpu_rate, lease_until, idempotency_key, created_at, updated_at, attention_reason
		FROM admission_reservations WHERE state='active'`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var expired []Reservation
	for rows.Next() {
		res, err := scanRes(rows)
		if err != nil {
			return err
		}
		if !res.LeaseUntil.IsZero() && !res.LeaseUntil.After(now) {
			expired = append(expired, res)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, res := range expired {
		live, unknown, err := s.live.Probe(res)
		if err != nil {
			return err
		}
		if unknown || live {
			res.State = StateAttentionRequired
			if unknown {
				res.AttentionReason = "lease_expired_liveness_unknown"
			} else {
				res.AttentionReason = "lease_expired_process_possibly_live"
			}
		} else {
			res.State = StateExpired
			res.AttentionReason = ""
		}
		res.UpdatedAt = now
		if err := updateReservation(ctx, tx, res); err != nil {
			return err
		}
	}
	return nil
}

func validateRequest(req Request) error {
	if strings.TrimSpace(req.ProjectID) == "" {
		return fmt.Errorf("%w: project_id required", ErrInvalidRequest)
	}
	switch req.Role {
	case RoleWorker, RoleVerifier, RoleLocalTest:
	default:
		return fmt.Errorf("%w: role", ErrInvalidRequest)
	}
	return nil
}

func requestView(req Request) ResourceView {
	v := ResourceView{Processes: req.Processes, RSSBytes: req.RSSBytes, CPURate: req.CPURate}
	if v.Processes <= 0 {
		v.Processes = 1
	}
	switch req.Role {
	case RoleWorker:
		v.Workers = 1
	case RoleVerifier:
		v.Verifiers = 1
	case RoleLocalTest:
		v.LocalTests = 1
	}
	return v
}

func reservationID(req Request, now time.Time) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s|%s|%s|%d",
		req.ProjectID, req.JobID, req.AttemptID, req.Role, req.IdempotencyKey, now.UnixNano())
	return "adm_" + hex.EncodeToString(h.Sum(nil))[:24]
}

func decisionFromReservation(res Reservation, admitted bool, budget Budget, now time.Time) Decision {
	d := Decision{
		SchemaVersion: SchemaVersion,
		Admitted:      admitted,
		ReservationID: res.ID,
		Generation:    res.Generation,
		State:         res.State,
		LeaseUntil:    res.LeaseUntil,
		Requested: ResourceView{
			Processes: res.Processes,
			RSSBytes:  res.RSSBytes,
			CPURate:   res.CPURate,
		},
		Reasons: []string{"idempotent_replay"},
	}
	switch res.Role {
	case RoleWorker:
		d.Requested.Workers = 1
	case RoleVerifier:
		d.Requested.Verifiers = 1
	case RoleLocalTest:
		d.Requested.LocalTests = 1
	}
	_ = budget
	_ = now
	return d
}

func subtractBudget(b Budget, used ResourceView) ResourceView {
	return ResourceView{
		Workers:    max0(b.MaxActiveWorkers - used.Workers),
		Verifiers:  max0(b.MaxActiveVerifiers - used.Verifiers),
		LocalTests: max0(b.MaxLocalTests - used.LocalTests),
		Processes:  max0(b.MaxChildProcesses - used.Processes),
		RSSBytes:   max0i64(b.MaxRSSBytes - used.RSSBytes),
		CPURate:    max0f(b.MaxCPURate - used.CPURate),
	}
}

func subtractView(a, b ResourceView) ResourceView {
	return ResourceView{
		Workers:    max0(a.Workers - b.Workers),
		Verifiers:  max0(a.Verifiers - b.Verifiers),
		LocalTests: max0(a.LocalTests - b.LocalTests),
		Processes:  max0(a.Processes - b.Processes),
		RSSBytes:   max0i64(a.RSSBytes - b.RSSBytes),
		CPURate:    max0f(a.CPURate - b.CPURate),
	}
}

func max0(v int) int {
	if v < 0 {
		return 0
	}
	return v
}
func max0i64(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}
func max0f(v float64) float64 {
	if v < 0 {
		return 0
	}
	return v
}
