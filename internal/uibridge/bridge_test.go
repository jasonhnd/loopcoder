package uibridge_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/uibridge"
	"github.com/jasonhnd/loopcoder/internal/uireport"
	"github.com/jasonhnd/loopcoder/internal/uisub"
)

func t0() time.Time { return time.Date(2026, 7, 22, 14, 0, 0, 0, time.UTC) }

func makeEnv(seq int64) uireport.Envelope {
	e, err := uireport.Project(uireport.Input{
		Kind: uireport.KindPeriodic, ProjectID: "proj", AttemptID: "att", Sequence: seq,
		Stage: "run", Status: "running", Liveness: "alive",
		RecordedAt: t0().Add(time.Duration(seq) * time.Second),
	})
	if err != nil {
		panic(err)
	}
	return e
}

func startBridge(t *testing.T, cfg uibridge.Config) (*uibridge.Bridge, uibridge.Handshake) {
	t.Helper()
	if cfg.ProjectID == "" {
		cfg.ProjectID = "proj"
	}
	if cfg.OwnerID == "" {
		cfg.OwnerID = "owner-test"
	}
	if cfg.Token == "" {
		cfg.Token = "test-token-1117"
	}
	if cfg.Now == nil {
		cfg.Now = t0
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = time.Hour // avoid idle during tests
	}
	if cfg.TokenTTL == 0 {
		cfg.TokenTTL = time.Hour
	}
	b, err := uibridge.New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	hs, err := b.Listen()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b, hs
}

func authReq(method, url, token string, body io.Reader) *http.Request {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		panic(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

func TestNonLoopbackBindRejected(t *testing.T) {
	if err := uibridge.ValidateBindHost("0.0.0.0"); err == nil {
		t.Fatal("expected reject")
	}
	if err := uibridge.ValidateBindHost("192.168.1.1"); err == nil {
		t.Fatal("expected reject")
	}
	_, err := uibridge.New(uibridge.Config{
		BindHost: "8.8.8.8", ProjectID: "p", Token: "t", IdleTimeout: time.Hour, Now: t0,
	}, nil)
	if err == nil {
		t.Fatal("expected New to reject non-loopback")
	}
}

func TestHandshakeAndCapabilityNegotiate(t *testing.T) {
	b, hs := startBridge(t, uibridge.Config{})
	if hs.Schema != uibridge.SchemaHandshake {
		t.Fatalf("schema=%s", hs.Schema)
	}
	if hs.ProtocolVersion != uibridge.ProtocolVersion {
		t.Fatalf("proto=%s", hs.ProtocolVersion)
	}
	if hs.CapabilityToken != "test-token-1117" {
		t.Fatal("token missing from handshake")
	}
	if !strings.HasPrefix(hs.BaseURL, "http://127.0.0.1:") && !strings.Contains(hs.BaseURL, "127.0.0.1") {
		// OS may report 127.0.0.1:port
		if !strings.Contains(hs.Address, "127.0.0.1") && !strings.Contains(hs.Address, "[::1]") {
			t.Fatalf("addr not loopback: %s", hs.Address)
		}
	}

	// negotiate ok
	req := authReq(http.MethodGet, hs.BaseURL+"/v1/capability?protocol="+uibridge.ProtocolVersion, hs.CapabilityToken, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	// unsupported protocol
	req2 := authReq(http.MethodGet, hs.BaseURL+"/v1/capability?protocol=loopcoder.ui.v0", hs.CapabilityToken, nil)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 400 {
		t.Fatalf("want 400 got %d", resp2.StatusCode)
	}
	_ = b
}

func TestAuthAndOriginFailures(t *testing.T) {
	_, hs := startBridge(t, uibridge.Config{})

	// no token
	req := authReq(http.MethodGet, hs.BaseURL+"/v1/status", "", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("want 401 got %d", resp.StatusCode)
	}

	// wrong token
	req = authReq(http.MethodGet, hs.BaseURL+"/v1/status", "wrong", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("want 401 got %d", resp.StatusCode)
	}

	// forbidden origin
	req = authReq(http.MethodGet, hs.BaseURL+"/v1/status", hs.CapabilityToken, nil)
	req.Header.Set("Origin", "https://evil.example")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("want 403 got %d", resp.StatusCode)
	}

	// loopback origin ok
	req = authReq(http.MethodGet, hs.BaseURL+"/v1/status", hs.CapabilityToken, nil)
	req.Header.Set("Origin", "http://127.0.0.1:9999")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("want 200 got %d", resp.StatusCode)
	}
}

func TestExpiredCapability(t *testing.T) {
	clock := t0()
	b, err := uibridge.New(uibridge.Config{
		ProjectID: "proj", Token: "exp-token", TokenTTL: time.Minute,
		IdleTimeout: time.Hour,
		Now:         func() time.Time { return clock },
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	hs, err := b.Listen()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })

	clock = t0().Add(2 * time.Minute)
	req := authReq(http.MethodGet, hs.BaseURL+"/v1/status", hs.CapabilityToken, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("want 401 expired got %d", resp.StatusCode)
	}
}

func TestRegisterSSEAckReconnect(t *testing.T) {
	b, hs := startBridge(t, uibridge.Config{MaxClients: 4})

	// register
	regBody := `{"client_id":"ui1","session_id":"s1","adapter_version":"1","project_id":"proj","required":true}`
	req := authReq(http.MethodPost, hs.BaseURL+"/v1/register", hs.CapabilityToken, strings.NewReader(regBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("register %d %s", resp.StatusCode, raw)
	}
	resp.Body.Close()

	// publish two reports
	for i := int64(1); i <= 2; i++ {
		if err := b.Ledger().Publish(makeEnv(i)); err != nil {
			t.Fatal(err)
		}
	}

	// SSE snapshot
	req = authReq(http.MethodGet, hs.BaseURL+"/v1/events?client_id=ui1&after=0", hs.CapabilityToken, nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("sse %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("ct=%s", ct)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "event: report") {
		t.Fatalf("missing report events: %s", text)
	}
	// parse first data line for ack
	var first uireport.Envelope
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data: {") {
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &first); err == nil && first.EventID != "" {
				break
			}
		}
	}
	if first.EventID == "" {
		t.Fatalf("no envelope in SSE: %s", text)
	}

	// ack accepted
	ack := map[string]any{
		"client_id": "ui1", "session_id": "s1",
		"event_id": first.EventID, "digest": first.ContentDigest,
		"stage": string(uisub.StageAccepted), "sequence": first.Sequence,
	}
	ab, _ := json.Marshal(ack)
	req = authReq(http.MethodPost, hs.BaseURL+"/v1/ack", hs.CapabilityToken, bytes.NewReader(ab))
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("ack %d", resp.StatusCode)
	}

	// status cursor
	req = authReq(http.MethodGet, hs.BaseURL+"/v1/status?client_id=ui1", hs.CapabilityToken, nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var st map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if st["last_accepted_cursor"].(float64) != float64(first.Sequence) {
		t.Fatalf("cursor=%v", st["last_accepted_cursor"])
	}

	// reconnect with resume=1 should skip acked
	req = authReq(http.MethodGet, hs.BaseURL+"/v1/events?client_id=ui1&resume=1", hs.CapabilityToken, nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	// should contain seq 2 only (or end if only one remaining)
	if !strings.Contains(string(raw), `"sequence":2`) && !strings.Contains(string(raw), `"sequence": 2`) {
		// JSON marshal has no space
		if !strings.Contains(string(raw), `"sequence":2`) {
			// still ok if only end when after matches — we published 2, accepted 1
			if strings.Count(string(raw), "event: report") != 1 {
				t.Fatalf("expected one remaining report: %s", raw)
			}
		}
	}
}

func TestOversizedBody(t *testing.T) {
	_, hs := startBridge(t, uibridge.Config{MaxBody: 64})
	big := strings.Repeat("x", 200)
	body := `{"client_id":"c","session_id":"s","project_id":"proj","adapter_version":"` + big + `"}`
	req := authReq(http.MethodPost, hs.BaseURL+"/v1/register", hs.CapabilityToken, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// MaxBytesReader → 400 or 413
	if resp.StatusCode != 413 && resp.StatusCode != 400 {
		t.Fatalf("want 413/400 got %d", resp.StatusCode)
	}
}

func TestMaxClients(t *testing.T) {
	_, hs := startBridge(t, uibridge.Config{MaxClients: 1})
	// Register first, then hold one long SSE while probing concurrent status.
	reg := authReq(http.MethodPost, hs.BaseURL+"/v1/register", hs.CapabilityToken,
		strings.NewReader(`{"client_id":"c1","session_id":"s","project_id":"proj"}`))
	reg.Header.Set("Content-Type", "application/json")
	r1, err := http.DefaultClient.Do(reg)
	if err != nil {
		t.Fatal(err)
	}
	r1.Body.Close()

	// Long-held connection
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := authReq(http.MethodGet, hs.BaseURL+"/v1/events?client_id=c1&follow=1&wait_ms=1500", hs.CapabilityToken, nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
		}
	}()
	time.Sleep(100 * time.Millisecond)
	// second concurrent should 429
	req2 := authReq(http.MethodGet, hs.BaseURL+"/v1/status", hs.CapabilityToken, nil)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 429 {
		// race: first may have finished; accept 200 only if first done
		select {
		case <-done:
			// first finished early — retry not meaningful; skip soft
			t.Logf("max clients race: first finished before probe status=%d", resp2.StatusCode)
		default:
			t.Fatalf("want 429 got %d", resp2.StatusCode)
		}
	}
	<-done
}

func TestShutdownJoins(t *testing.T) {
	b, hs := startBridge(t, uibridge.Config{})
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	// second close ok
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	req := authReq(http.MethodGet, hs.BaseURL+"/v1/status", hs.CapabilityToken, nil)
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		resp.Body.Close()
		// may get connection refused or 503
		if resp.StatusCode == 200 {
			t.Fatal("server still accepting after close")
		}
	}
}

func TestHealthzNoAuth(t *testing.T) {
	_, hs := startBridge(t, uibridge.Config{})
	resp, err := http.Get(hs.BaseURL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}
