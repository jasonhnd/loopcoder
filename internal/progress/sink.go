package progress

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// SinkKind identifies a negotiated foreground progress transport.
type SinkKind string

const (
	SinkKindTerminalHuman SinkKind = "terminal-human"
	SinkKindJSONL         SinkKind = "jsonl-stream"
	SinkKindHostCallback  SinkKind = "host-callback"
	SinkKindOutboxOnly    SinkKind = "outbox-only"
)

// SinkCapabilities describes what a sink can do without invoking a model.
type SinkCapabilities struct {
	HumanReadable             bool `json:"human_readable"`
	JSONL                     bool `json:"jsonl"`
	HostCallback              bool `json:"host_callback"`
	OutboxOnly                bool `json:"outbox_only"`
	SeparateFromMachineStdout bool `json:"separate_from_machine_stdout"`
}

// Sink is a provider-neutral progress delivery target.
type Sink interface {
	Kind() SinkKind
	Capabilities() SinkCapabilities
	// Deliver attempts active delivery. It must not write to a strict JSON
	// command stdout channel; use the negotiated writer instead.
	Deliver(ctx context.Context, receipt ProgressReceipt) error
}

// SinkNegotiation records which transport was selected at invocation start.
type SinkNegotiation struct {
	Selected      SinkKind         `json:"selected"`
	Capabilities  SinkCapabilities `json:"capabilities"`
	NegotiatedAt  string           `json:"negotiated_at"`
	FallbackOrder []SinkKind       `json:"fallback_order,omitempty"`
	Reason        string           `json:"reason,omitempty"`
}

// NegotiateOptions chooses an active sink for a long-running invocation.
type NegotiateOptions struct {
	// PreferJSONL selects framed JSONL events on EventWriter when true.
	PreferJSONL bool
	// HumanWriter is a human-readable stream (typically stderr).
	HumanWriter io.Writer
	// EventWriter is a dedicated event stream distinct from machine JSON stdout.
	EventWriter io.Writer
	// HostDeliver is an optional host callback/hook transport.
	HostDeliver DeliveryFunc
	// ForceOutboxOnly disables active sinks (durable path only).
	ForceOutboxOnly bool
	// Now defaults to time.Now UTC.
	Now func() time.Time
}

// NegotiateSink selects one active foreground sink with explicit capabilities.
// Preference: host callback → JSONL event stream → human terminal → outbox-only.
func NegotiateSink(opts NegotiateOptions) (Sink, SinkNegotiation) {
	now := time.Now().UTC()
	if opts.Now != nil {
		now = opts.Now().UTC()
	}
	order := []SinkKind{SinkKindHostCallback, SinkKindJSONL, SinkKindTerminalHuman, SinkKindOutboxOnly}
	if opts.ForceOutboxOnly {
		return OutboxOnlySink{}, SinkNegotiation{
			Selected:      SinkKindOutboxOnly,
			Capabilities:  SinkCapabilities{OutboxOnly: true, SeparateFromMachineStdout: true},
			NegotiatedAt:  now.Format(time.RFC3339Nano),
			FallbackOrder: order,
			Reason:        "force outbox-only",
		}
	}
	if opts.HostDeliver != nil {
		return HostCallbackSink{DeliverFn: opts.HostDeliver}, SinkNegotiation{
			Selected: SinkKindHostCallback,
			Capabilities: SinkCapabilities{
				HostCallback:              true,
				SeparateFromMachineStdout: true,
			},
			NegotiatedAt:  now.Format(time.RFC3339Nano),
			FallbackOrder: order,
			Reason:        "host callback available",
		}
	}
	if opts.PreferJSONL && opts.EventWriter != nil {
		return JSONLSink{W: opts.EventWriter}, SinkNegotiation{
			Selected: SinkKindJSONL,
			Capabilities: SinkCapabilities{
				JSONL:                     true,
				SeparateFromMachineStdout: true,
			},
			NegotiatedAt:  now.Format(time.RFC3339Nano),
			FallbackOrder: order,
			Reason:        "jsonl event stream preferred",
		}
	}
	if opts.HumanWriter != nil {
		return TerminalHumanSink{W: opts.HumanWriter}, SinkNegotiation{
			Selected: SinkKindTerminalHuman,
			Capabilities: SinkCapabilities{
				HumanReadable:             true,
				SeparateFromMachineStdout: true,
			},
			NegotiatedAt:  now.Format(time.RFC3339Nano),
			FallbackOrder: order,
			Reason:        "human-readable terminal sink",
		}
	}
	return OutboxOnlySink{}, SinkNegotiation{
		Selected:      SinkKindOutboxOnly,
		Capabilities:  SinkCapabilities{OutboxOnly: true, SeparateFromMachineStdout: true},
		NegotiatedAt:  now.Format(time.RFC3339Nano),
		FallbackOrder: order,
		Reason:        "no active sink available; durable outbox only",
	}
}

// DeliveryFuncFromSink adapts a Sink to EmitterOptions.Deliver.
func DeliveryFuncFromSink(sink Sink) DeliveryFunc {
	if sink == nil {
		return nil
	}
	return sink.Deliver
}

// TerminalHumanSink writes a short redacted human line to W (usually stderr).
type TerminalHumanSink struct {
	W  io.Writer
	mu sync.Mutex
}

func (s TerminalHumanSink) Kind() SinkKind { return SinkKindTerminalHuman }

func (s TerminalHumanSink) Capabilities() SinkCapabilities {
	return SinkCapabilities{HumanReadable: true, SeparateFromMachineStdout: true}
}

func (s TerminalHumanSink) Deliver(_ context.Context, receipt ProgressReceipt) error {
	if s.W == nil {
		return fmt.Errorf("terminal human sink has no writer")
	}
	phase := strings.TrimSpace(receipt.Phase)
	if phase == "" {
		phase = "active"
	}
	next := strings.TrimSpace(receipt.NextAction.Summary)
	if next == "" {
		next = receipt.NextAction.State
	}
	if next == "" {
		next = "none"
	}
	blocker := strings.TrimSpace(receipt.Blocker.State)
	if blocker == "" {
		blocker = "none"
	}
	line := fmt.Sprintf("[loopcoder progress] run=%s phase=%s next=%s blocker=%s seq=%d\n",
		sanitizeID(firstNonEmpty(receipt.DeliveryRunID, receipt.RunID)),
		sanitizeID(phase),
		sanitizeSummary(next),
		sanitizeID(blocker),
		receipt.CorrelationSequence,
	)
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := io.WriteString(s.W, line)
	return err
}

// JSONLSink writes one framed progress event per receipt to W.
// W must not be the command's machine JSON stdout for strict JSON commands.
type JSONLSink struct {
	W  io.Writer
	mu sync.Mutex
}

func (s JSONLSink) Kind() SinkKind { return SinkKindJSONL }

func (s JSONLSink) Capabilities() SinkCapabilities {
	return SinkCapabilities{JSONL: true, SeparateFromMachineStdout: true}
}

func (s JSONLSink) Deliver(_ context.Context, receipt ProgressReceipt) error {
	if s.W == nil {
		return fmt.Errorf("jsonl sink has no writer")
	}
	event := map[string]any{
		"schema_version":       "loopcoder.progress_event.v1",
		"kind":                 "progress",
		"progress_receipt_id":  receipt.ProgressReceiptID,
		"project_id":           receipt.ProjectID,
		"delivery_run_id":      receipt.DeliveryRunID,
		"run_id":               receipt.RunID,
		"correlation_id":       receipt.CorrelationID,
		"correlation_sequence": receipt.CorrelationSequence,
		"phase":                receipt.Phase,
		"blocker":              receipt.Blocker,
		"next_action":          receipt.NextAction,
		"occurred_at":          receipt.OccurredAt,
		"semantic_fingerprint": receipt.SemanticFingerprint,
	}
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.W.Write(append(data, '\n'))
	return err
}

// HostCallbackSink delivers via a host-owned callback/hook.
type HostCallbackSink struct {
	DeliverFn DeliveryFunc
}

func (s HostCallbackSink) Kind() SinkKind { return SinkKindHostCallback }

func (s HostCallbackSink) Capabilities() SinkCapabilities {
	return SinkCapabilities{HostCallback: true, SeparateFromMachineStdout: true}
}

func (s HostCallbackSink) Deliver(ctx context.Context, receipt ProgressReceipt) error {
	if s.DeliverFn == nil {
		return fmt.Errorf("host callback sink has no deliver function")
	}
	return s.DeliverFn(ctx, receipt)
}

// OutboxOnlySink performs no active delivery; durable persistence remains the
// sole transport. Deliver is a successful no-op so emitters can still tick.
type OutboxOnlySink struct{}

func (OutboxOnlySink) Kind() SinkKind { return SinkKindOutboxOnly }

func (OutboxOnlySink) Capabilities() SinkCapabilities {
	return SinkCapabilities{OutboxOnly: true, SeparateFromMachineStdout: true}
}

func (OutboxOnlySink) Deliver(context.Context, ProgressReceipt) error { return nil }

func sanitizeSummary(value string) string {
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.TrimSpace(value)
	if len(value) > 120 {
		return value[:120]
	}
	return value
}
