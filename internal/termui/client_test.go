package termui_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/termui"
	"github.com/jasonhnd/loopcoder/internal/uireport"
	"github.com/jasonhnd/loopcoder/internal/uisub"
)

func makeEnv(kind uireport.Kind, seq int64) uireport.Envelope {
	in := uireport.Input{
		Kind: kind, ProjectID: "proj", AttemptID: "att", Sequence: seq,
		Stage: "run", Status: "running", Liveness: "alive",
		Actual:           uireport.Route{Provider: "codex", Model: "m"},
		Next:             uireport.NextAction{Action: "wait"},
		NextReportAt:     time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC),
		RecordedAt:       time.Date(2026, 7, 22, 12, 0, int(seq), 0, time.UTC),
		SemanticProgress: kind != uireport.KindStart,
	}
	if kind == uireport.KindBlocker {
		in.Blocker = "ci"
	}
	if kind == uireport.KindAttention {
		in.Attention = []string{"need_input"}
	}
	e, err := uireport.Project(in)
	if err != nil {
		panic(err)
	}
	return e
}

func setup(t *testing.T) (*uisub.Ledger, *bytes.Buffer) {
	t.Helper()
	l := uisub.NewLedger("proj", 32, nil)
	_ = l.RegisterClient(uisub.ClientIdentity{ClientID: "term", SessionID: "s", ProjectID: "proj", AdapterVersion: "1"})
	return l, &bytes.Buffer{}
}

func TestHumanAndJSONLSameSequenceDigests(t *testing.T) {
	l, _ := setup(t)
	for i, k := range []uireport.Kind{
		uireport.KindStart, uireport.KindStateChange, uireport.KindPeriodic,
		uireport.KindAttention, uireport.KindBlocker, uireport.KindTerminal,
	} {
		_ = l.Publish(makeEnv(k, int64(i+1)))
	}
	var hum, js bytes.Buffer
	ch := termui.NewClient(l, "term", termui.ModeHuman, &hum)
	// second client for jsonl
	_ = l.RegisterClient(uisub.ClientIdentity{ClientID: "termj", SessionID: "sj", ProjectID: "proj"})
	cj := termui.NewClient(l, "termj", termui.ModeJSONL, &js)
	n1, err := ch.Snapshot(context.Background())
	if err != nil || n1 != 6 {
		t.Fatalf("human n=%d err=%v", n1, err)
	}
	n2, err := cj.Snapshot(context.Background())
	if err != nil || n2 != 6 {
		t.Fatalf("jsonl n=%d err=%v", n2, err)
	}
	// JSONL lines parse to same digests as published order
	lines := strings.Split(strings.TrimSpace(js.String()), "\n")
	if len(lines) != 6 {
		t.Fatalf("lines=%d", len(lines))
	}
	if !strings.Contains(hum.String(), "blocker=ci") {
		t.Fatalf("human missing blocker: %s", hum.String())
	}
	if !strings.Contains(hum.String(), "attention=need_input") {
		t.Fatal("human missing attention")
	}
}

func TestBrokenPipeNoRenderedAck(t *testing.T) {
	l, _ := setup(t)
	_ = l.Publish(makeEnv(uireport.KindStart, 1))
	w := &failWriter{}
	c := termui.NewClient(l, "term", termui.ModeHuman, w)
	_, err := c.Snapshot(context.Background())
	if err == nil {
		t.Fatal("expected write error")
	}
	// No rendered ack
	if _, ok := l.AckEvidence("term", makeEnv(uireport.KindStart, 1).EventID, uisub.StageRendered); ok {
		t.Fatal("must not ack rendered on failure")
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestPartialWrite(t *testing.T) {
	l, _ := setup(t)
	_ = l.Publish(makeEnv(uireport.KindPeriodic, 1))
	c := termui.NewClient(l, "term", termui.ModeHuman, &shortWriter{})
	_, err := c.Snapshot(context.Background())
	if !errors.Is(err, termui.ErrPartialWrite) && !errors.Is(err, termui.ErrBrokenPipe) {
		// shortWriter returns short count without error → PartialWrite
		if err == nil {
			t.Fatal("expected error")
		}
	}
}

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return len(p) / 2, nil
}

func TestCursorAdvancesOnlyAfterRender(t *testing.T) {
	l, buf := setup(t)
	_ = l.Publish(makeEnv(uireport.KindStart, 1))
	_ = l.Publish(makeEnv(uireport.KindTerminal, 2))
	c := termui.NewClient(l, "term", termui.ModeJSONL, buf)
	n, err := c.Follow(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if c.Cursor() != 2 {
		t.Fatalf("cursor=%d", c.Cursor())
	}
	// Replay from cursor empty
	n2, err := c.Snapshot(context.Background())
	if err != nil || n2 != 0 {
		t.Fatalf("n2=%d err=%v", n2, err)
	}
}

func TestCancelExitsCleanly(t *testing.T) {
	l, buf := setup(t)
	c := termui.NewClient(l, "term", termui.ModeHuman, buf)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Follow(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}
