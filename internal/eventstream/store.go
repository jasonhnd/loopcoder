package eventstream

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jasonhnd/loopcoder/internal/home"
	"github.com/jasonhnd/loopcoder/internal/termui"
	"github.com/jasonhnd/loopcoder/internal/uibridge"
	"github.com/jasonhnd/loopcoder/internal/uireport"
	"github.com/jasonhnd/loopcoder/internal/uisub"
)

const (
	// SchemaKeepalive is a non-semantic follow keepalive (not a report kind).
	SchemaKeepalive = "loopcoder.ui.keepalive.v1"
	// DefaultMaxQueue bounds unread backlog before overflow disconnect.
	DefaultMaxQueue = 256
	// DefaultPoll is the follow poll interval when idle.
	DefaultPoll = 250 * time.Millisecond
	// DefaultKeepalive every N idle polls emit a keepalive (jsonl only).
	DefaultKeepaliveEvery = 4
)

// Store is a project-scoped durable report log + subscription ledger.
type Store struct {
	mu        sync.Mutex
	projectID string
	path      string
	ledger    *uisub.Ledger
	// bySeq index for run filtering
	reports []uireport.Envelope
	now     func() time.Time
}

// Open opens or creates the project UI report store under LOOPCODER_HOME.
func Open(projectID string, now func() time.Time) (*Store, error) {
	if now == nil {
		now = time.Now
	}
	id, err := home.ValidateProjectID(projectID)
	if err != nil {
		return nil, err
	}
	layout, err := home.ResolveV09(home.Deps{})
	if err != nil {
		return nil, err
	}
	if err := layout.EnsureProject(id); err != nil {
		return nil, err
	}
	root, err := layout.ProjectDir(id)
	if err != nil {
		return nil, err
	}
	uiDir := filepath.Join(root, "ui")
	if err := os.MkdirAll(uiDir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(uiDir, "reports.jsonl")
	s := &Store{
		projectID: id,
		path:      path,
		ledger:    uisub.NewLedger(id, DefaultMaxQueue, now),
		now:       now,
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// OpenAt is like Open but roots at an explicit home directory (tests).
func OpenAt(homeDir, projectID string, now func() time.Time) (*Store, error) {
	if now == nil {
		now = time.Now
	}
	id, err := home.ValidateProjectID(projectID)
	if err != nil {
		return nil, err
	}
	layout, err := home.NewV09(homeDir)
	if err != nil {
		return nil, err
	}
	if err := layout.EnsureProject(id); err != nil {
		return nil, err
	}
	root, err := layout.ProjectDir(id)
	if err != nil {
		return nil, err
	}
	uiDir := filepath.Join(root, "ui")
	if err := os.MkdirAll(uiDir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(uiDir, "reports.jsonl")
	s := &Store{
		projectID: id,
		path:      path,
		ledger:    uisub.NewLedger(id, DefaultMaxQueue, now),
		now:       now,
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// ProjectID returns the scoped project.
func (s *Store) ProjectID() string { return s.projectID }

// Ledger exposes the subscription ledger (for bridge / tests).
func (s *Store) Ledger() *uisub.Ledger { return s.ledger }

// Path is the durable JSONL log path.
func (s *Store) Path() string { return s.path }

func (s *Store) load() error {
	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	// 1MB lines max
	buf := make([]byte, 0, 64*1024)
	sc.Buffer(buf, 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var env uireport.Envelope
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			return fmt.Errorf("eventstream: corrupt report log: %w", err)
		}
		if env.ProjectID != s.projectID {
			return fmt.Errorf("eventstream: cross-project row in %s", s.path)
		}
		s.reports = append(s.reports, env)
		if err := s.ledger.Publish(env); err != nil {
			return err
		}
	}
	return sc.Err()
}

// Publish appends a durable ordered report and notifies the ledger.
// Sequence must be strictly increasing for the project (caller-owned).
func (s *Store) Publish(env uireport.Envelope) error {
	if env.ProjectID != s.projectID {
		return uisub.ErrWrongProject
	}
	if env.Schema == "" {
		env.Schema = uireport.SchemaEnvelope
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.reports) > 0 && env.Sequence <= s.reports[len(s.reports)-1].Sequence {
		return fmt.Errorf("eventstream: non-monotonic sequence %d", env.Sequence)
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(raw, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	s.reports = append(s.reports, env)
	return s.ledger.Publish(env)
}

// NextSequence returns last+1 (or 1).
func (s *Store) NextSequence() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.reports) == 0 {
		return 1
	}
	return s.reports[len(s.reports)-1].Sequence + 1
}

// RegisterClient registers a UI client on the ledger.
func (s *Store) RegisterClient(id uisub.ClientIdentity) error {
	if id.ProjectID == "" {
		id.ProjectID = s.projectID
	}
	return s.ledger.RegisterClient(id)
}

// FollowOptions configures a terminal follow session.
type FollowOptions struct {
	ClientID  string
	SessionID string
	After     int64
	RunID     string // optional filter
	Mode      termui.Mode
	Out       io.Writer
	Poll      time.Duration
	Keepalive bool // emit non-semantic keepalives on idle (jsonl)
	Follow    bool
	// AutoRenderedAck after successful full write (default true for terminal).
	AutoRenderedAck bool
}

// FollowResult summarizes a follow/snapshot session.
type FollowResult struct {
	Rendered int
	Cursor   int64
}

// Follow replays reports after After, optionally continues until ctx done.
// Keepalives are provider-free and do not advance semantic cursors.
func (s *Store) Follow(ctx context.Context, opts FollowOptions) (FollowResult, error) {
	if opts.ClientID == "" {
		opts.ClientID = "terminal"
	}
	if opts.SessionID == "" {
		opts.SessionID = "session"
	}
	if opts.Out == nil {
		return FollowResult{}, fmt.Errorf("eventstream: nil output")
	}
	if opts.Poll <= 0 {
		opts.Poll = DefaultPoll
	}
	if opts.Mode == "" {
		opts.Mode = termui.ModeJSONL
	}
	// default auto ack rendered
	autoAck := true
	if !opts.AutoRenderedAck {
		// still true by default; allow tests to set false only via zero-value? use pointer in future
		autoAck = true
	}
	_ = autoAck

	if err := s.RegisterClient(uisub.ClientIdentity{
		ClientID: opts.ClientID, SessionID: opts.SessionID,
		ProjectID: s.projectID, AdapterVersion: "loopcoder-events/1", Required: true,
	}); err != nil {
		return FollowResult{}, err
	}

	cursor := opts.After
	rendered := 0
	idlePolls := 0

	for {
		if err := ctx.Err(); err != nil {
			return FollowResult{Rendered: rendered, Cursor: cursor}, err
		}
		reps, err := s.ledger.Replay(opts.ClientID, cursor)
		if err != nil {
			return FollowResult{Rendered: rendered, Cursor: cursor}, err
		}
		// filter run
		if opts.RunID != "" {
			var filtered []uireport.Envelope
			for _, e := range reps {
				if e.RunID == opts.RunID || e.RunID == "" {
					filtered = append(filtered, e)
				}
			}
			reps = filtered
		}
		if len(reps) == 0 {
			if !opts.Follow {
				return FollowResult{Rendered: rendered, Cursor: cursor}, nil
			}
			idlePolls++
			if opts.Keepalive && opts.Mode == termui.ModeJSONL && idlePolls%DefaultKeepaliveEvery == 0 {
				ka := map[string]any{
					"schema":            SchemaKeepalive,
					"project_id":        s.projectID,
					"cursor":            cursor,
					"at":                s.now().UTC().Format(time.RFC3339Nano),
					"semantic_progress": false,
				}
				b, _ := json.Marshal(ka)
				if _, err := opts.Out.Write(append(b, '\n')); err != nil {
					return FollowResult{Rendered: rendered, Cursor: cursor}, err
				}
			}
			t := time.NewTimer(opts.Poll)
			select {
			case <-ctx.Done():
				t.Stop()
				return FollowResult{Rendered: rendered, Cursor: cursor}, ctx.Err()
			case <-t.C:
			}
			continue
		}
		idlePolls = 0
		for _, env := range reps {
			if err := ctx.Err(); err != nil {
				return FollowResult{Rendered: rendered, Cursor: cursor}, err
			}
			if err := writeEnvelope(opts.Out, opts.Mode, env); err != nil {
				// no rendered ack
				return FollowResult{Rendered: rendered, Cursor: cursor}, err
			}
			if err := s.ledger.Acknowledge(uisub.Ack{
				ClientID: opts.ClientID, SessionID: opts.SessionID,
				EventID: env.EventID, Sequence: env.Sequence,
				Digest: env.ContentDigest, Stage: uisub.StageRendered,
				At: s.now().UTC(),
			}); err != nil {
				return FollowResult{Rendered: rendered, Cursor: cursor}, err
			}
			cursor = env.Sequence
			rendered++
			if env.ReportKind == uireport.KindTerminal && !opts.Follow {
				return FollowResult{Rendered: rendered, Cursor: cursor}, nil
			}
		}
		if !opts.Follow {
			return FollowResult{Rendered: rendered, Cursor: cursor}, nil
		}
	}
}

// Acknowledge records an explicit client ack (bridge/generic clients).
func (s *Store) Acknowledge(a uisub.Ack) error {
	return s.ledger.Acknowledge(a)
}

// StartBridge listens on loopback with the store ledger.
func (s *Store) StartBridge(cfg uibridge.Config) (*uibridge.Bridge, uibridge.Handshake, error) {
	if cfg.ProjectID == "" {
		cfg.ProjectID = s.projectID
	}
	if cfg.ProjectID != s.projectID {
		return nil, uibridge.Handshake{}, fmt.Errorf("eventstream: bridge project mismatch")
	}
	if cfg.Now == nil {
		cfg.Now = s.now
	}
	b, err := uibridge.New(cfg, s.ledger)
	if err != nil {
		return nil, uibridge.Handshake{}, err
	}
	hs, err := b.Listen()
	if err != nil {
		return nil, uibridge.Handshake{}, err
	}
	return b, hs, nil
}

// ListSequences returns published sequences (tests / diagnostics).
func (s *Store) ListSequences() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]int64, len(s.reports))
	for i, r := range s.reports {
		out[i] = r.Sequence
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func writeEnvelope(w io.Writer, mode termui.Mode, env uireport.Envelope) error {
	var payload []byte
	switch mode {
	case termui.ModeHuman:
		line := uireport.PrettyText(uireport.Human(env)) + "\n"
		payload = []byte(line)
	default:
		b, err := json.Marshal(env)
		if err != nil {
			return err
		}
		payload = append(b, '\n')
	}
	n, err := w.Write(payload)
	if err != nil {
		return err
	}
	if n != len(payload) {
		return errors.New("eventstream: short write")
	}
	return nil
}
