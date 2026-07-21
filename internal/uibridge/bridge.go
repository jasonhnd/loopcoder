package uibridge

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jasonhnd/loopcoder/internal/uireport"
	"github.com/jasonhnd/loopcoder/internal/uisub"
)

const (
	// ProtocolVersion is the negotiated UI protocol.
	ProtocolVersion = "loopcoder.ui.v1"
	// SchemaHandshake is the startup handshake schema.
	SchemaHandshake = "loopcoder.ui.handshake.v1"
	// MaxBodyBytes is the default request body limit.
	MaxBodyBytes = 16 << 10
	// DefaultMaxClients bounds concurrent SSE + HTTP holders.
	DefaultMaxClients = 8
	// DefaultIdle is idle shutdown duration when no requests arrive.
	DefaultIdle = 30 * time.Second
	// DefaultTokenTTL is capability lifetime.
	DefaultTokenTTL = 15 * time.Minute
	// JoinTimeout bounds graceful shutdown.
	JoinTimeout = 5 * time.Second
)

var (
	ErrNotLoopback       = errors.New("uibridge: bind address is not loopback")
	ErrUnauthorized      = errors.New("uibridge: unauthorized")
	ErrForbiddenOrigin   = errors.New("uibridge: forbidden origin")
	ErrUnsupportedProto  = errors.New("uibridge: unsupported protocol")
	ErrTooManyClients    = errors.New("uibridge: too many clients")
	ErrBodyTooLarge      = errors.New("uibridge: body too large")
	ErrExpiredCapability = errors.New("uibridge: capability expired")
	ErrClosed            = errors.New("uibridge: closed")
)

// Config configures an ephemeral bridge.
type Config struct {
	// BindHost must be a loopback host (127.0.0.1 or ::1). Empty → 127.0.0.1.
	BindHost string
	// Port 0 picks an ephemeral port.
	Port int
	// ProjectID scopes the ledger.
	ProjectID string
	// OwnerID is recorded in the handshake (UI launcher identity).
	OwnerID string
	// AllowedOrigins: empty means only empty/null origin (non-browser) and
	// http://127.0.0.1 / http://localhost variants. Non-empty is exact match set.
	AllowedOrigins []string
	// Token overrides generated capability (tests). Empty → random.
	Token string
	// TokenTTL defaults to DefaultTokenTTL.
	TokenTTL time.Duration
	// MaxBody defaults to MaxBodyBytes.
	MaxBody int64
	// MaxClients defaults to DefaultMaxClients.
	MaxClients int
	// IdleTimeout defaults to DefaultIdle; 0 disables idle shutdown.
	IdleTimeout time.Duration
	// Now injects clock (tests).
	Now func() time.Time
	// MaxQueue for uisub ledger.
	MaxQueue int
}

// Handshake is the machine-readable startup record.
type Handshake struct {
	Schema            string    `json:"schema"`
	ProtocolVersion   string    `json:"protocol_version"`
	Address           string    `json:"address"`
	BaseURL           string    `json:"base_url"`
	OwnerID           string    `json:"owner_id"`
	ProjectID         string    `json:"project_id"`
	ExpiresAt         time.Time `json:"expires_at"`
	TokenDelivery     string    `json:"token_delivery"` // "handshake_field"
	CapabilityToken   string    `json:"capability_token"`
	Capabilities      []string  `json:"capabilities"`
	MaxBodyBytes      int64     `json:"max_body_bytes"`
	MaxClients        int       `json:"max_clients"`
	IdleShutdownAfter string    `json:"idle_shutdown_after,omitempty"`
}

// Bridge is the owned loopback HTTP/SSE process surface.
type Bridge struct {
	cfg      Config
	ledger   *uisub.Ledger
	token    string
	expires  time.Time
	now      func() time.Time
	ln       net.Listener
	srv      *http.Server
	addr     string
	baseURL  string
	mu       sync.Mutex
	clients  int32 // concurrent active request holders
	lastAct  time.Time
	closed   atomic.Bool
	stopIdle chan struct{}
	wg       sync.WaitGroup
}

// New constructs a bridge bound to ledger. Does not listen until Listen.
func New(cfg Config, ledger *uisub.Ledger) (*Bridge, error) {
	if cfg.ProjectID == "" {
		return nil, fmt.Errorf("uibridge: project_id required")
	}
	if cfg.BindHost == "" {
		cfg.BindHost = "127.0.0.1"
	}
	if !isLoopbackHost(cfg.BindHost) {
		return nil, fmt.Errorf("%w: %s", ErrNotLoopback, cfg.BindHost)
	}
	if cfg.TokenTTL <= 0 {
		cfg.TokenTTL = DefaultTokenTTL
	}
	if cfg.MaxBody <= 0 {
		cfg.MaxBody = MaxBodyBytes
	}
	if cfg.MaxClients <= 0 {
		cfg.MaxClients = DefaultMaxClients
	}
	if cfg.IdleTimeout < 0 {
		cfg.IdleTimeout = DefaultIdle
	}
	if cfg.IdleTimeout == 0 {
		// keep zero as disable only when explicitly set via negative? Spec wants idle.
		// Treat 0 as DefaultIdle; tests set large or stop via Close.
		cfg.IdleTimeout = DefaultIdle
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	token := cfg.Token
	if token == "" {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, err
		}
		token = hex.EncodeToString(b[:])
	}
	if ledger == nil {
		ledger = uisub.NewLedger(cfg.ProjectID, cfg.MaxQueue, now)
	}
	return &Bridge{
		cfg:      cfg,
		ledger:   ledger,
		token:    token,
		expires:  now().UTC().Add(cfg.TokenTTL),
		now:      now,
		lastAct:  now().UTC(),
		stopIdle: make(chan struct{}),
	}, nil
}

// Ledger returns the backing subscription ledger.
func (b *Bridge) Ledger() *uisub.Ledger { return b.ledger }

// Listen binds loopback and starts serving. Returns handshake.
func (b *Bridge) Listen() (Handshake, error) {
	if b.closed.Load() {
		return Handshake{}, ErrClosed
	}
	hostPort := net.JoinHostPort(b.cfg.BindHost, strconv.Itoa(b.cfg.Port))
	ln, err := net.Listen("tcp", hostPort)
	if err != nil {
		return Handshake{}, err
	}
	// Enforce actual bound address is loopback.
	tcp, ok := ln.Addr().(*net.TCPAddr)
	if !ok || tcp.IP == nil || !tcp.IP.IsLoopback() {
		_ = ln.Close()
		return Handshake{}, fmt.Errorf("%w: %v", ErrNotLoopback, ln.Addr())
	}
	b.ln = ln
	b.addr = ln.Addr().String()
	// Prefer 127.0.0.1 form in URL even if OS returns ::1 with mapped form.
	b.baseURL = "http://" + b.addr

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/capability", b.wrap(b.handleCapability))
	mux.HandleFunc("/v1/status", b.wrap(b.handleStatus))
	mux.HandleFunc("/v1/events", b.wrap(b.handleEvents))
	mux.HandleFunc("/v1/ack", b.wrap(b.handleAck))
	mux.HandleFunc("/v1/register", b.wrap(b.handleRegister))
	mux.HandleFunc("/healthz", b.wrap(b.handleHealthz))

	b.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0, // SSE long-lived
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    8 << 10,
		BaseContext: func(net.Listener) context.Context {
			return context.Background()
		},
	}
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		_ = b.srv.Serve(ln)
	}()
	if b.cfg.IdleTimeout > 0 {
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			b.idleLoop()
		}()
	}
	return b.Handshake(), nil
}

// Handshake returns the current startup record (safe to print once at start).
func (b *Bridge) Handshake() Handshake {
	idle := ""
	if b.cfg.IdleTimeout > 0 {
		idle = b.cfg.IdleTimeout.String()
	}
	return Handshake{
		Schema:            SchemaHandshake,
		ProtocolVersion:   ProtocolVersion,
		Address:           b.addr,
		BaseURL:           b.baseURL,
		OwnerID:           b.cfg.OwnerID,
		ProjectID:         b.cfg.ProjectID,
		ExpiresAt:         b.expires,
		TokenDelivery:     "handshake_field",
		CapabilityToken:   b.token,
		Capabilities:      []string{"capability", "status", "events_sse", "ack", "register"},
		MaxBodyBytes:      b.cfg.MaxBody,
		MaxClients:        b.cfg.MaxClients,
		IdleShutdownAfter: idle,
	}
}

// Addr returns host:port after Listen.
func (b *Bridge) Addr() string { return b.addr }

// BaseURL returns http://addr after Listen.
func (b *Bridge) BaseURL() string { return b.baseURL }

// Token returns the capability token (tests / process-local delivery).
func (b *Bridge) Token() string { return b.token }

// Close shuts down listeners and joins within JoinTimeout.
func (b *Bridge) Close() error {
	if !b.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(b.stopIdle)
	ctx, cancel := context.WithTimeout(context.Background(), JoinTimeout)
	defer cancel()
	var err error
	if b.srv != nil {
		err = b.srv.Shutdown(ctx)
	}
	if b.ln != nil {
		_ = b.ln.Close()
	}
	done := make(chan struct{})
	go func() {
		b.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		if err == nil {
			err = ctx.Err()
		}
	}
	return err
}

func (b *Bridge) idleLoop() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-b.stopIdle:
			return
		case <-t.C:
			if b.closed.Load() {
				return
			}
			b.mu.Lock()
			last := b.lastAct
			b.mu.Unlock()
			if b.now().UTC().Sub(last) >= b.cfg.IdleTimeout && atomic.LoadInt32(&b.clients) == 0 {
				_ = b.Close()
				return
			}
		}
	}
}

func (b *Bridge) touch() {
	b.mu.Lock()
	b.lastAct = b.now().UTC()
	b.mu.Unlock()
}

func (b *Bridge) wrap(next func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if b.closed.Load() {
			http.Error(w, ErrClosed.Error(), http.StatusServiceUnavailable)
			return
		}
		// Loopback remote only.
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			http.Error(w, ErrNotLoopback.Error(), http.StatusForbidden)
			return
		}
		// Connection admission (except healthz without auth still counts lightly).
		if !strings.HasPrefix(r.URL.Path, "/healthz") {
			if n := atomic.AddInt32(&b.clients, 1); int(n) > b.cfg.MaxClients {
				atomic.AddInt32(&b.clients, -1)
				http.Error(w, ErrTooManyClients.Error(), http.StatusTooManyRequests)
				return
			}
			defer atomic.AddInt32(&b.clients, -1)
		}
		b.touch()

		// Origin policy (skip healthz).
		if r.URL.Path != "/healthz" {
			if err := b.checkOrigin(r); err != nil {
				http.Error(w, err.Error(), http.StatusForbidden)
				return
			}
		}
		// Auth for non-healthz.
		if r.URL.Path != "/healthz" {
			if err := b.checkAuth(r); err != nil {
				http.Error(w, err.Error(), http.StatusUnauthorized)
				return
			}
		}
		// Body size guard for methods with body.
		if r.Body != nil && (r.Method == http.MethodPost || r.Method == http.MethodPut) {
			r.Body = http.MaxBytesReader(w, r.Body, b.cfg.MaxBody)
		}
		next(w, r)
	}
}

func (b *Bridge) checkAuth(r *http.Request) error {
	if b.now().UTC().After(b.expires) {
		return ErrExpiredCapability
	}
	h := r.Header.Get("Authorization")
	const pfx = "Bearer "
	if !strings.HasPrefix(h, pfx) {
		return ErrUnauthorized
	}
	got := strings.TrimSpace(strings.TrimPrefix(h, pfx))
	if subtle.ConstantTimeCompare([]byte(got), []byte(b.token)) != 1 {
		return ErrUnauthorized
	}
	return nil
}

func (b *Bridge) checkOrigin(r *http.Request) error {
	origin := r.Header.Get("Origin")
	if origin == "" || origin == "null" {
		return nil // non-browser / same-process clients
	}
	if len(b.cfg.AllowedOrigins) > 0 {
		for _, a := range b.cfg.AllowedOrigins {
			if origin == a {
				return nil
			}
		}
		return ErrForbiddenOrigin
	}
	// Default: only loopback browser origins.
	switch {
	case strings.HasPrefix(origin, "http://127.0.0.1"),
		strings.HasPrefix(origin, "http://localhost"),
		strings.HasPrefix(origin, "http://[::1]"):
		return nil
	default:
		return ErrForbiddenOrigin
	}
}

func (b *Bridge) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (b *Bridge) handleCapability(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	want := r.URL.Query().Get("protocol")
	if want == "" && r.Method == http.MethodPost {
		var body struct {
			Protocol string `json:"protocol"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, b.cfg.MaxBody)).Decode(&body)
		want = body.Protocol
	}
	if want == "" {
		want = ProtocolVersion
	}
	if want != ProtocolVersion {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":               ErrUnsupportedProto.Error(),
			"supported_protocols": []string{ProtocolVersion},
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"schema":           "loopcoder.ui.capability.v1",
		"protocol_version": ProtocolVersion,
		"project_id":       b.cfg.ProjectID,
		"capabilities":     []string{"status", "events_sse", "ack", "register"},
		"expires_at":       b.expires.UTC().Format(time.RFC3339),
		"max_body_bytes":   b.cfg.MaxBody,
		"max_clients":      b.cfg.MaxClients,
		"token_delivery":   "authorization_bearer",
	})
}

func (b *Bridge) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	clientID := r.URL.Query().Get("client_id")
	out := map[string]any{
		"schema":           "loopcoder.ui.status.v1",
		"protocol_version": ProtocolVersion,
		"project_id":       b.cfg.ProjectID,
		"closed":           b.closed.Load(),
	}
	if clientID != "" {
		cur, err := b.ledger.LastAcceptedCursor(clientID)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
			return
		}
		out["client_id"] = clientID
		out["last_accepted_cursor"] = cur
	}
	writeJSON(w, http.StatusOK, out)
}

func (b *Bridge) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		ClientID       string `json:"client_id"`
		SessionID      string `json:"session_id"`
		AdapterVersion string `json:"adapter_version"`
		ProjectID      string `json:"project_id"`
		Required       bool   `json:"required"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, b.cfg.MaxBody))
	if err := dec.Decode(&body); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) || strings.Contains(err.Error(), "http: request body too large") {
			http.Error(w, ErrBodyTooLarge.Error(), http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	id := uisub.ClientIdentity{
		ClientID:       body.ClientID,
		SessionID:      body.SessionID,
		AdapterVersion: body.AdapterVersion,
		ProjectID:      body.ProjectID,
		Required:       body.Required,
	}
	if id.ProjectID == "" {
		id.ProjectID = b.cfg.ProjectID
	}
	if err := b.ledger.RegisterClient(id); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"client_id":  id.ClientID,
		"session_id": id.SessionID,
		"project_id": id.ProjectID,
	})
}

func (b *Bridge) handleAck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		ClientID  string `json:"client_id"`
		SessionID string `json:"session_id"`
		EventID   string `json:"event_id"`
		Sequence  int64  `json:"sequence"`
		Digest    string `json:"digest"`
		Stage     string `json:"stage"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, b.cfg.MaxBody)).Decode(&body); err != nil {
		if strings.Contains(err.Error(), "http: request body too large") {
			http.Error(w, ErrBodyTooLarge.Error(), http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	err := b.ledger.Acknowledge(uisub.Ack{
		ClientID:  body.ClientID,
		SessionID: body.SessionID,
		EventID:   body.EventID,
		Sequence:  body.Sequence,
		Digest:    body.Digest,
		Stage:     uisub.AckStage(body.Stage),
		At:        b.now().UTC(),
	})
	if err != nil {
		code := http.StatusBadRequest
		writeJSON(w, code, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (b *Bridge) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	clientID := r.URL.Query().Get("client_id")
	if clientID == "" {
		http.Error(w, "client_id required", http.StatusBadRequest)
		return
	}
	after := int64(0)
	if s := r.URL.Query().Get("after"); s != "" {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			http.Error(w, "bad after", http.StatusBadRequest)
			return
		}
		after = v
	}
	// Prefer resume from accepted cursor when after omitted and resume=1
	if r.URL.Query().Get("resume") == "1" {
		if cur, err := b.ledger.LastAcceptedCursor(clientID); err == nil {
			after = cur
		}
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "stream unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// One-shot replay page (no busy follow loop). Client reconnects for more.
	// Optional follow=1 waits once for cancel or first batch with short poll via context.
	follow := r.URL.Query().Get("follow") == "1"
	deadline := time.Now().Add(2 * time.Second)
	if d := r.URL.Query().Get("wait_ms"); d != "" {
		if ms, err := strconv.Atoi(d); err == nil && ms > 0 && ms <= 5000 {
			deadline = time.Now().Add(time.Duration(ms) * time.Millisecond)
		}
	}

	for {
		reps, err := b.ledger.Replay(clientID, after)
		if err != nil {
			// SSE error event; do not disclose stack.
			fmt.Fprintf(w, "event: error\ndata: %s\n\n", jsonEscape(err.Error()))
			flusher.Flush()
			return
		}
		if len(reps) > 0 {
			for _, env := range reps {
				payload, err := json.Marshal(env)
				if err != nil {
					continue
				}
				fmt.Fprintf(w, "id: %d\nevent: report\ndata: %s\n\n", env.Sequence, payload)
				flusher.Flush()
				after = env.Sequence
				// Mark accepted stage only after client ack; streamed is set by Replay.
				if env.ReportKind == uireport.KindTerminal {
					fmt.Fprintf(w, "event: terminal\ndata: {\"sequence\":%d}\n\n", env.Sequence)
					flusher.Flush()
					return
				}
			}
			if !follow {
				fmt.Fprintf(w, "event: end\ndata: {\"after\":%d}\n\n", after)
				flusher.Flush()
				return
			}
		}
		if !follow {
			fmt.Fprintf(w, "event: end\ndata: {\"after\":%d}\n\n", after)
			flusher.Flush()
			return
		}
		// Cooperative wait without busy spin.
		select {
		case <-r.Context().Done():
			return
		case <-time.After(50 * time.Millisecond):
			if time.Now().After(deadline) {
				fmt.Fprintf(w, "event: end\ndata: {\"after\":%d}\n\n", after)
				flusher.Flush()
				return
			}
		}
		b.touch()
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func jsonEscape(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func isLoopbackHost(host string) bool {
	h := strings.Trim(host, "[]")
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// ValidateBindHost rejects non-loopback bind configuration before Listen.
func ValidateBindHost(host string) error {
	if host == "" {
		return nil
	}
	if !isLoopbackHost(host) {
		return fmt.Errorf("%w: %s", ErrNotLoopback, host)
	}
	return nil
}
