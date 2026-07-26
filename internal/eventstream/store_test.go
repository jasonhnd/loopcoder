package eventstream_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/eventstream"
	"github.com/jasonhnd/loopcoder/internal/termui"
	"github.com/jasonhnd/loopcoder/internal/uibridge"
	"github.com/jasonhnd/loopcoder/internal/uireport"
	"github.com/jasonhnd/loopcoder/internal/uisub"
)

func fixed() time.Time {
	return time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
}

func env(project string, kind uireport.Kind, seq int64, run string) uireport.Envelope {
	in := uireport.Input{
		Kind: kind, ProjectID: project, AttemptID: "att-1", Sequence: seq, RunID: run,
		Stage: "worker_running", Status: "running", Liveness: "alive",
		Actual:     uireport.Route{Provider: "codex", Model: "m"},
		Next:       uireport.NextAction{Action: "wait"},
		RecordedAt: fixed().Add(time.Duration(seq) * time.Second),
	}
	if kind == uireport.KindTerminal {
		in.Stage = "done"
		in.Status = "success"
	}
	e, err := uireport.Project(in)
	if err != nil {
		panic(err)
	}
	return e
}

func ownerHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestReconnectAfterSequenceExactlyOnce(t *testing.T) {
	home := ownerHome(t)
	s, err := eventstream.OpenAt(home, "proj-a", fixed)
	if err != nil {
		t.Fatal(err)
	}
	for i, k := range []uireport.Kind{
		uireport.KindStart, uireport.KindPeriodic, uireport.KindTerminal,
	} {
		if err := s.Publish(env("proj-a", k, int64(i+1), "run-1")); err != nil {
			t.Fatal(err)
		}
	}
	var first bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r1, err := s.Follow(ctx, eventstream.FollowOptions{
		ClientID: "c1", SessionID: "s1", After: 0, Mode: termui.ModeJSONL, Out: &first, Follow: false,
	})
	if err != nil || r1.Rendered != 3 {
		t.Fatalf("r1=%#v err=%v out=%s", r1, err, first.String())
	}
	// reconnect after 1 → only 2 and 3
	var second bytes.Buffer
	r2, err := s.Follow(context.Background(), eventstream.FollowOptions{
		ClientID: "c1b", SessionID: "s2", After: 1, Mode: termui.ModeJSONL, Out: &second, Follow: false,
	})
	if err != nil || r2.Rendered != 2 {
		t.Fatalf("r2=%#v err=%v out=%s", r2, err, second.String())
	}
	lines := strings.Split(strings.TrimSpace(second.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines=%d %q", len(lines), second.String())
	}
	var e2, e3 uireport.Envelope
	_ = json.Unmarshal([]byte(lines[0]), &e2)
	_ = json.Unmarshal([]byte(lines[1]), &e3)
	if e2.Sequence != 2 || e3.Sequence != 3 {
		t.Fatalf("seq %d %d", e2.Sequence, e3.Sequence)
	}
	// rendered ack for reconnect client on last
	if _, ok := s.Ledger().AckEvidence("c1b", e3.EventID, uisub.StageRendered); !ok {
		t.Fatal("missing rendered ack")
	}
}

func TestDurableAcrossReopen(t *testing.T) {
	home := ownerHome(t)
	s1, err := eventstream.OpenAt(home, "proj-b", fixed)
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.Publish(env("proj-b", uireport.KindStart, 1, "r")); err != nil {
		t.Fatal(err)
	}
	s2, err := eventstream.OpenAt(home, "proj-b", fixed)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.ListSequences(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("seqs=%v", got)
	}
}

func TestRejectStaleAck(t *testing.T) {
	home := ownerHome(t)
	s, err := eventstream.OpenAt(home, "proj-c", fixed)
	if err != nil {
		t.Fatal(err)
	}
	e := env("proj-c", uireport.KindStart, 1, "r")
	_ = s.Publish(e)
	_ = s.RegisterClient(uisub.ClientIdentity{ClientID: "cli", SessionID: "s", ProjectID: "proj-c"})
	if err := s.Acknowledge(uisub.Ack{
		ClientID: "cli", EventID: e.EventID, Digest: "sha256:deadbeef", Stage: uisub.StageRendered,
	}); err == nil {
		t.Fatal("wrong digest must fail")
	}
}

func TestBridgeSSEAndRenderedAck(t *testing.T) {
	home := ownerHome(t)
	s, err := eventstream.OpenAt(home, "proj-d", fixed)
	if err != nil {
		t.Fatal(err)
	}
	e := env("proj-d", uireport.KindStart, 1, "r")
	_ = s.Publish(e)
	_ = s.RegisterClient(uisub.ClientIdentity{ClientID: "sse", SessionID: "s", ProjectID: "proj-d"})
	b, hs, err := s.StartBridge(uibridge.Config{
		ProjectID: "proj-d", OwnerID: "test", Port: 0, IdleTimeout: time.Minute, Now: fixed,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if hs.CapabilityToken == "" || !strings.Contains(hs.BaseURL, "127.0.0.1") && !strings.Contains(hs.BaseURL, "[::1]") {
		// may be 127.0.0.1:port
		if hs.CapabilityToken == "" {
			t.Fatalf("handshake=%#v", hs)
		}
	}
	req, _ := http.NewRequest(http.MethodGet, hs.BaseURL+"/v1/events?client_id=sse&after=0", nil)
	req.Header.Set("Authorization", "Bearer "+hs.CapabilityToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	// ack rendered
	body := strings.NewReader(`{"client_id":"sse","session_id":"s","event_id":"` + e.EventID + `","sequence":1,"digest":"` + e.ContentDigest + `","stage":"rendered"}`)
	areq, _ := http.NewRequest(http.MethodPost, hs.BaseURL+"/v1/ack", body)
	areq.Header.Set("Authorization", "Bearer "+hs.CapabilityToken)
	areq.Header.Set("Content-Type", "application/json")
	aresp, err := http.DefaultClient.Do(areq)
	if err != nil {
		t.Fatal(err)
	}
	defer aresp.Body.Close()
	if aresp.StatusCode != 200 {
		t.Fatalf("ack status=%d", aresp.StatusCode)
	}
	// stale ack fails
	bad := strings.NewReader(`{"client_id":"sse","session_id":"s","event_id":"` + e.EventID + `","sequence":1,"digest":"sha256:nope","stage":"rendered"}`)
	breq, _ := http.NewRequest(http.MethodPost, hs.BaseURL+"/v1/ack", bad)
	breq.Header.Set("Authorization", "Bearer "+hs.CapabilityToken)
	breq.Header.Set("Content-Type", "application/json")
	bresp, err := http.DefaultClient.Do(breq)
	if err != nil {
		t.Fatal(err)
	}
	defer bresp.Body.Close()
	if bresp.StatusCode == 200 {
		t.Fatal("mismatched digest must not 200")
	}
}

func TestKeepaliveNoSemanticProgress(t *testing.T) {
	home := ownerHome(t)
	s, err := eventstream.OpenAt(home, "proj-e", fixed)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 900*time.Millisecond)
	defer cancel()
	_, _ = s.Follow(ctx, eventstream.FollowOptions{
		ClientID: "k", SessionID: "s", After: 0, Mode: termui.ModeJSONL, Out: &buf,
		Follow: true, Poll: 100 * time.Millisecond, Keepalive: true,
	})
	if !strings.Contains(buf.String(), eventstream.SchemaKeepalive) {
		t.Fatalf("expected keepalive, got %q", buf.String())
	}
	if strings.Contains(buf.String(), `"semantic_progress":true`) {
		t.Fatal("keepalive must not claim semantic progress")
	}
}
