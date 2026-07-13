package progress

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/jasonhnd/loopcoder/internal/storage"
)

// Recorder is the production seam for LoopCoder-owned progress observations.
// Implementations must persist from supervisor state only; emitting a receipt
// must not renew claims, leases, budgets, quota windows, or watchdog deadlines.
type Recorder interface {
	Emit(context.Context, Observation) (EmitResult, error)
	Terminal(context.Context, Observation) (EmitResult, error)
}

type SupervisorOptions struct {
	Store              storage.Store
	ProjectID          string
	DeliveryRunID      string
	RunID              string
	MaxSilenceInterval time.Duration
	Clock              Clock
	Deliver            DeliveryFunc
	Diagnostic         DiagnosticFunc
}

type DiagnosticFunc func(context.Context, Diagnostic)

type Diagnostic struct {
	Code          string `json:"code"`
	ProjectID     string `json:"project_id"`
	DeliveryRunID string `json:"delivery_run_id"`
	CorrelationID string `json:"correlation_id"`
	Phase         string `json:"phase"`
	Status        string `json:"status"`
	Error         string `json:"error"`
}

// Supervisor owns one independent Emitter per progress correlation. Terminal
// fencing and max-silence ticks are therefore scoped to the run/task/attempt
// lifecycle that produced the observation instead of to the whole delivery run.
type Supervisor struct {
	opts SupervisorOptions

	mu       sync.Mutex
	emitters map[string]*Emitter
	closed   bool
}

func NewSupervisor(opts SupervisorOptions) (*Supervisor, error) {
	if opts.Store == nil {
		return nil, typed(ErrInvalidRecordCode, "store is required")
	}
	if strings.TrimSpace(opts.ProjectID) == "" || strings.TrimSpace(opts.DeliveryRunID) == "" {
		return nil, typed(ErrInvalidRecordCode, "project_id and delivery_run_id are required")
	}
	if _, err := boundMaxSilence(opts.MaxSilenceInterval); err != nil {
		return nil, err
	}
	return &Supervisor{opts: opts, emitters: map[string]*Emitter{}}, nil
}

func (s *Supervisor) Emit(ctx context.Context, observation Observation) (EmitResult, error) {
	emitter, err := s.emitterFor(ctx, observation)
	if err != nil {
		return EmitResult{}, err
	}
	return emitter.Emit(ctx, observation)
}

func (s *Supervisor) Terminal(ctx context.Context, observation Observation) (EmitResult, error) {
	emitter, err := s.emitterFor(ctx, observation)
	if err != nil {
		return EmitResult{}, err
	}
	return emitter.Terminal(ctx, observation)
}

func (s *Supervisor) Stop(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.closed {
		emitters := append([]*Emitter(nil), values(s.emitters)...)
		s.mu.Unlock()
		return stopEmitters(ctx, emitters)
	}
	s.closed = true
	emitters := append([]*Emitter(nil), values(s.emitters)...)
	s.mu.Unlock()
	return stopEmitters(ctx, emitters)
}

func (s *Supervisor) RecordProgressDiagnostic(ctx context.Context, diagnostic Diagnostic) {
	if s == nil || s.opts.Diagnostic == nil {
		return
	}
	s.opts.Diagnostic(ctx, diagnostic)
}

func (s *Supervisor) emitterFor(ctx context.Context, observation Observation) (*Emitter, error) {
	if s == nil {
		return nil, ErrEmitterClosed
	}
	correlationID := observationCorrelationID(observation, s.opts.DeliveryRunID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrEmitterClosed
	}
	if emitter := s.emitters[correlationID]; emitter != nil {
		return emitter, nil
	}
	emitter, err := NewEmitter(EmitterOptions{
		Store:              s.opts.Store,
		ProjectID:          s.opts.ProjectID,
		DeliveryRunID:      s.opts.DeliveryRunID,
		RunID:              firstNonEmpty(s.opts.RunID, s.opts.DeliveryRunID),
		CorrelationID:      correlationID,
		MaxSilenceInterval: s.opts.MaxSilenceInterval,
		Clock:              s.opts.Clock,
		Deliver:            s.opts.Deliver,
	})
	if err != nil {
		return nil, err
	}
	if _, err := emitter.Start(context.Background()); err != nil {
		return nil, err
	}
	s.emitters[correlationID] = emitter
	return emitter, nil
}

func observationCorrelationID(observation Observation, fallback string) string {
	return firstNonEmpty(observation.CorrelationID, observation.AttemptID, observation.TaskID, observation.DeliveryRunID, fallback)
}

func stopEmitters(ctx context.Context, emitters []*Emitter) error {
	var firstErr error
	for _, emitter := range emitters {
		if err := emitter.Stop(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func values(m map[string]*Emitter) []*Emitter {
	out := make([]*Emitter, 0, len(m))
	for _, value := range m {
		out = append(out, value)
	}
	return out
}

func ReportDiagnostic(ctx context.Context, recorder Recorder, observation Observation, err error) {
	if err == nil {
		return
	}
	sink, ok := recorder.(interface {
		RecordProgressDiagnostic(context.Context, Diagnostic)
	})
	if !ok {
		return
	}
	sink.RecordProgressDiagnostic(ctx, Diagnostic{
		Code:          "progress-receipt-generation-failed",
		ProjectID:     observation.ProjectID,
		DeliveryRunID: observation.DeliveryRunID,
		CorrelationID: observationCorrelationID(observation, observation.DeliveryRunID),
		Phase:         observation.Phase,
		Status:        observation.Status,
		Error:         boundedDiagnostic(err.Error()),
	})
}

func boundedDiagnostic(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 240 {
		return value[:240]
	}
	return value
}
