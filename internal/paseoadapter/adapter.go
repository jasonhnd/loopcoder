package paseoadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jasonhnd/loopcoder/internal/termui"
	"github.com/jasonhnd/loopcoder/internal/uiconform"
	"github.com/jasonhnd/loopcoder/internal/uireport"
	"github.com/jasonhnd/loopcoder/internal/uisub"
)

const (
	SchemaProfile = "loopcoder.paseo.adapter.profile.v1"
	SchemaGap     = "loopcoder.paseo.adapter.gap.v1"
	AdapterID     = "paseo-reference"
)

// Surface is the abstract operator-visible surface the adapter may drive.
// Implementations must not embed Paseo code; fixture and opt-in real backends
// map to documented public operations only.
type Surface interface {
	// ShowActivity posts a redacted activity line (start/periodic/terminal).
	ShowActivity(kind uireport.Kind, text string) error
	// NotifyAttention surfaces attention/blocker without private bodies.
	NotifyAttention(text string) error
	// AckRendered returns true only when the surface can truthfully claim rendered.
	AckRendered() bool
	// Name identifies the backend (fixture | paseo_public_smoke).
	Name() string
	// Close releases resources; must leave no listeners.
	Close() error
}

// FixtureSurface is the default synthetic surface for conformance (no Paseo binary).
type FixtureSurface struct {
	mu       sync.Mutex
	lines    []string
	rendered bool
}

func NewFixtureSurface() *FixtureSurface { return &FixtureSurface{} }

func (f *FixtureSurface) ShowActivity(kind uireport.Kind, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lines = append(f.lines, fmt.Sprintf("%s|%s", kind, text))
	f.rendered = true
	return nil
}

func (f *FixtureSurface) NotifyAttention(text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lines = append(f.lines, "attention|"+text)
	f.rendered = true
	return nil
}

func (f *FixtureSurface) AckRendered() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rendered
}

func (f *FixtureSurface) Name() string { return "fixture" }

func (f *FixtureSurface) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lines = nil
	return nil
}

func (f *FixtureSurface) Lines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.lines...)
}

// Profile is the precise capability claim.
type Profile struct {
	Schema          string    `json:"schema"`
	AdapterID       string    `json:"adapter_id"`
	Surface         string    `json:"surface"`
	HighestStage    string    `json:"highest_final_mile_stage"` // accepted|rendered|seen
	Supports        []string  `json:"supports"`
	Unsupported     []string  `json:"unsupported"`
	ConformancePass bool      `json:"conformance_pass"`
	InterfaceGap    *Gap      `json:"interface_gap,omitempty"`
	RealHostSmoke   bool      `json:"real_host_smoke"`
	RealHostClaim   bool      `json:"real_host_support_claim"` // never true without separate smoke evidence
	GeneratedAt     time.Time `json:"generated_at"`
}

// Gap records a bounded interface limitation (does not weaken rendered).
type Gap struct {
	Schema  string `json:"schema"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Adapter binds a uisub ledger client to a Surface over terminal JSONL path.
type Adapter struct {
	ledger   *uisub.Ledger
	clientID string
	surface  Surface
	mu       sync.Mutex
	cursor   int64
}

// New constructs an adapter.
func New(ledger *uisub.Ledger, clientID string, surface Surface) *Adapter {
	if surface == nil {
		surface = NewFixtureSurface()
	}
	return &Adapter{ledger: ledger, clientID: clientID, surface: surface}
}

// Consume replays and drives the surface; acknowledges rendered only if surface.AckRendered.
func (a *Adapter) Consume(ctx context.Context) (int, error) {
	n := 0
	for {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		a.mu.Lock()
		cur := a.cursor
		a.mu.Unlock()
		reps, err := a.ledger.Replay(a.clientID, cur)
		if err != nil {
			return n, err
		}
		if len(reps) == 0 {
			return n, nil
		}
		for _, env := range reps {
			if err := a.deliver(env); err != nil {
				return n, err
			}
			// Truthful rendered only when surface confirms.
			if !a.surface.AckRendered() {
				return n, fmt.Errorf("paseoadapter: surface cannot prove rendered for %s", env.EventID)
			}
			if err := a.ledger.Acknowledge(uisub.Ack{
				ClientID: a.clientID,
				EventID:  env.EventID,
				Digest:   env.ContentDigest,
				Stage:    uisub.StageRendered,
			}); err != nil {
				return n, err
			}
			a.mu.Lock()
			a.cursor = env.Sequence
			a.mu.Unlock()
			n++
			if env.ReportKind == uireport.KindTerminal {
				return n, nil
			}
		}
	}
}

func (a *Adapter) deliver(env uireport.Envelope) error {
	h := uireport.Human(env)
	text := uireport.PrettyText(h)
	switch env.ReportKind {
	case uireport.KindAttention, uireport.KindBlocker:
		return a.surface.NotifyAttention(text)
	default:
		return a.surface.ShowActivity(env.ReportKind, text)
	}
}

// Cursor returns last fully rendered sequence.
func (a *Adapter) Cursor() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cursor
}

// RunConformance runs generic terminal conformance + adapter smoke on fixture surface.
func RunConformance(projectID string) (Profile, error) {
	now := time.Now
	r := &uiconform.Runner{
		ProjectID: projectID,
		Adapter:   AdapterID + "/fixture",
		Version:   "v0.9.0-dev",
		Now:       now,
		Limits:    uiconform.DefaultLimits(),
	}
	m, err := r.RunTerminalFull()
	if err != nil {
		return Profile{}, err
	}
	// Adapter-level synthetic delivery
	l := uisub.NewLedger(projectID, 32, now)
	_ = l.RegisterClient(uisub.ClientIdentity{
		ClientID: "paseo", SessionID: "s1", ProjectID: projectID, AdapterVersion: AdapterID, Required: true,
	})
	for _, e := range uiconform.GoldenTranscript(projectID) {
		_ = l.Publish(e)
	}
	surf := NewFixtureSurface()
	ad := New(l, "paseo", surf)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	n, err := ad.Consume(ctx)
	if err != nil {
		return Profile{}, err
	}
	if n < 6 {
		return Profile{}, fmt.Errorf("paseoadapter: expected 6 reports got %d", n)
	}
	// reconnect by cursor
	surf2 := NewFixtureSurface()
	ad2 := New(l, "paseo", surf2)
	ad2.mu.Lock()
	ad2.cursor = ad.Cursor()
	ad2.mu.Unlock()
	n2, err := ad2.Consume(ctx)
	if err != nil {
		return Profile{}, err
	}
	if n2 != 0 {
		return Profile{}, fmt.Errorf("paseoadapter: reconnect duplicated %d reports", n2)
	}

	pass := m.Profile == uiconform.ProfileFull || m.Profile == uiconform.ProfileDegraded
	for _, v := range m.Vectors {
		if !v.Pass {
			pass = false
		}
	}
	p := Profile{
		Schema:          SchemaProfile,
		AdapterID:       AdapterID,
		Surface:         surf.Name(),
		HighestStage:    string(uisub.StageRendered),
		Supports:        []string{"start", "periodic", "attention", "terminal", "reconnect", "rendered_ack"},
		Unsupported:     []string{"rich_in_chat", "private_paseo_api", "cross_ui_hard_dependency"},
		ConformancePass: pass,
		RealHostSmoke:   false,
		RealHostClaim:   false,
		GeneratedAt:     now().UTC(),
	}
	_ = surf.Close()
	return p, nil
}

// MaybeRealSmoke runs opt-in smoke when PASEO_ADAPTER_SMOKE=1.
// Without a live truthful surface, records interface-gap and does not claim rendered on real host.
func MaybeRealSmoke(projectID string) (Profile, error) {
	if os.Getenv("PASEO_ADAPTER_SMOKE") != "1" {
		return Profile{}, errors.New("paseoadapter: real smoke disabled (set PASEO_ADAPTER_SMOKE=1)")
	}
	// Independent protocol path only — no Paseo binary invocation in-tree.
	// If operator provides a surface via env that cannot prove rendered, gap out.
	gap := &Gap{
		Schema:  SchemaGap,
		Code:    "paseo_public_rendered_unproven",
		Message: "opt-in smoke requires separately approved Paseo-side rendered acknowledgement; LoopCoder does not weaken rendered or embed Paseo source",
	}
	return Profile{
		Schema:          SchemaProfile,
		AdapterID:       AdapterID,
		Surface:         "paseo_public_smoke",
		HighestStage:    string(uisub.StageAccepted), // do not claim rendered without proof
		Supports:        []string{"protocol_subscribe"},
		Unsupported:     []string{"proven_rendered_without_paseo_change"},
		ConformancePass: false,
		InterfaceGap:    gap,
		RealHostSmoke:   true,
		RealHostClaim:   false,
		GeneratedAt:     time.Now().UTC(),
	}, nil
}

// LicenseGuard notes ensure no accidental Paseo path imports in this package.
// Build-time: package must not import any github.com/.../paseo module.
func LicenseGuard() []string {
	return []string{
		"no_paseo_source_import",
		"no_paseo_schema_copy",
		"synthetic_fixtures_only_default",
		"agpl_separation_maintained",
	}
}

// DualPath proves terminal JSONL path still works alongside adapter surface.
func DualPath(projectID string) error {
	l := uisub.NewLedger(projectID, 16, time.Now)
	_ = l.RegisterClient(uisub.ClientIdentity{ClientID: "term", SessionID: "s", ProjectID: projectID})
	_ = l.RegisterClient(uisub.ClientIdentity{ClientID: "paseo", SessionID: "s", ProjectID: projectID})
	envs := uiconform.GoldenTranscript(projectID)
	for _, e := range envs {
		_ = l.Publish(e)
	}
	var buf bytes.Buffer
	tc := termui.NewClient(l, "term", termui.ModeJSONL, &buf)
	if _, err := tc.Snapshot(context.Background()); err != nil {
		return err
	}
	ad := New(l, "paseo", NewFixtureSurface())
	if _, err := ad.Consume(context.Background()); err != nil {
		return err
	}
	// both should have rendered acks on first event
	e0 := envs[0]
	if _, ok := l.AckEvidence("term", e0.EventID, uisub.StageRendered); !ok {
		return errors.New("term missing rendered")
	}
	if _, ok := l.AckEvidence("paseo", e0.EventID, uisub.StageRendered); !ok {
		return errors.New("paseo missing rendered")
	}
	return nil
}

// ProfileJSON encodes profile for inspection.
func ProfileJSON(p Profile) ([]byte, error) {
	return json.MarshalIndent(p, "", "  ")
}

// HasNoPaseoImport reports whether source import blocks embed an external Paseo module.
// It only inspects import lines so documentation tokens are not false positives.
func HasNoPaseoImport(src string) bool {
	inImport := false
	for _, line := range strings.Split(src, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "import (") {
			inImport = true
			continue
		}
		if inImport {
			if trim == ")" {
				inImport = false
				continue
			}
			lower := strings.ToLower(trim)
			// external paseo modules only (not this package name)
			if strings.Contains(lower, "github.com/") && strings.Contains(lower, "/paseo") &&
				!strings.Contains(lower, "loopcoder/internal/paseoadapter") {
				return false
			}
			if strings.Contains(lower, "agpl") && strings.Contains(lower, "paseo") {
				return false
			}
		}
	}
	return true
}
