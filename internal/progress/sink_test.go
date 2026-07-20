package progress

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestNegotiateSinkPreferenceOrder(t *testing.T) {
	var hostCalls int
	sink, neg := NegotiateSink(NegotiateOptions{
		PreferJSONL: true,
		HumanWriter: ioDiscard{},
		EventWriter: ioDiscard{},
		HostDeliver: func(context.Context, ProgressReceipt) error {
			hostCalls++
			return nil
		},
		Now: func() time.Time { return time.Date(2026, 7, 17, 17, 0, 0, 0, time.UTC) },
	})
	if sink.Kind() != SinkKindHostCallback || neg.Selected != SinkKindHostCallback {
		t.Fatalf("selected = %s/%s, want host-callback", sink.Kind(), neg.Selected)
	}
	if !neg.Capabilities.HostCallback || !neg.Capabilities.SeparateFromMachineStdout {
		t.Fatalf("capabilities = %#v", neg.Capabilities)
	}

	sink, neg = NegotiateSink(NegotiateOptions{
		PreferJSONL: true,
		EventWriter: &bytes.Buffer{},
		HumanWriter: &bytes.Buffer{},
	})
	if sink.Kind() != SinkKindJSONL {
		t.Fatalf("kind = %s, want jsonl", sink.Kind())
	}

	sink, neg = NegotiateSink(NegotiateOptions{HumanWriter: &bytes.Buffer{}})
	if sink.Kind() != SinkKindTerminalHuman {
		t.Fatalf("kind = %s, want terminal-human", sink.Kind())
	}

	sink, neg = NegotiateSink(NegotiateOptions{})
	if sink.Kind() != SinkKindOutboxOnly || !neg.Capabilities.OutboxOnly {
		t.Fatalf("kind/caps = %s/%#v", sink.Kind(), neg.Capabilities)
	}
}

func TestTerminalAndJSONLSinksDoNotInvokeModel(t *testing.T) {
	receipt := ProgressReceipt{
		ProgressReceiptID:   "pr-1",
		ProjectID:           "proj",
		DeliveryRunID:       "run-1",
		RunID:               "run-1",
		CorrelationID:       "corr",
		CorrelationSequence: 2,
		Phase:               "provider-exec",
		Blocker:             ActionState{State: "none"},
		NextAction:          ActionState{State: "continue", Summary: "wait for provider"},
		OccurredAt:          "2026-07-17T17:00:00Z",
		SemanticFingerprint: "fp",
	}

	var human bytes.Buffer
	if err := (TerminalHumanSink{W: &human}).Deliver(context.Background(), receipt); err != nil {
		t.Fatalf("human deliver: %v", err)
	}
	if !strings.Contains(human.String(), "phase=provider-exec") || !strings.Contains(human.String(), "blocker=none") {
		t.Fatalf("human output = %q", human.String())
	}

	var events bytes.Buffer
	if err := (JSONLSink{W: &events}).Deliver(context.Background(), receipt); err != nil {
		t.Fatalf("jsonl deliver: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(events.Bytes()), &parsed); err != nil {
		t.Fatalf("jsonl parse: %v\n%s", err, events.String())
	}
	if parsed["schema_version"] != "loopcoder.progress_event.v1" {
		t.Fatalf("event = %#v", parsed)
	}
}

func TestDeliveryFuncFromSink(t *testing.T) {
	var buf bytes.Buffer
	fn := DeliveryFuncFromSink(TerminalHumanSink{W: &buf})
	if fn == nil {
		t.Fatal("expected deliver func")
	}
	if err := fn(context.Background(), ProgressReceipt{
		DeliveryRunID: "run-x",
		Phase:         "wait",
		Blocker:       ActionState{State: "none"},
		NextAction:    ActionState{State: "none"},
	}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if buf.Len() == 0 {
		t.Fatal("expected output")
	}
}

// ioDiscard is a tiny writer that drops bytes.
type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }
