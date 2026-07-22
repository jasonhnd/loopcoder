package directrun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/directattempt"
	"github.com/jasonhnd/loopcoder/internal/eventstream"
	"github.com/jasonhnd/loopcoder/internal/home"
	"github.com/jasonhnd/loopcoder/internal/preflight"
	"github.com/jasonhnd/loopcoder/internal/providerexec"
	"github.com/jasonhnd/loopcoder/internal/routepin"
	"github.com/jasonhnd/loopcoder/internal/termui"
	"github.com/jasonhnd/loopcoder/internal/uireport"
	"github.com/jasonhnd/loopcoder/internal/uisub"
)

// Request is a validated loopcoder run request (non-dry-run path).
type Request struct {
	Repo       string
	Issue      string
	Provider   string
	Model      string
	Effort     string
	Permission string
	BaseBranch string
	RequiredUI []string
	OptionalUI []string
	Detach     bool
	ProjectID  string
	RunID      string
	BaseSHA    string
	ReportOut  io.Writer
	ReportMode termui.Mode
}

// Result is durable worker cleanup-terminal evidence.
type Result struct {
	RunID           string
	AttemptID       string
	ProjectID       string
	State           directattempt.State
	ProviderLaunchN int
	ExitCode        int
	WorktreePath    string
	RouteDigest     string
	StartEventID    string
	StartDigest     string
	Message         string
	Error           string
}

// Deps injects ports for production and tests.
type Deps struct {
	Now            func() time.Time
	HomeDir        string
	Provider       func(ctx context.Context, req providerexec.Request) (providerexec.Outcome, error)
	Preflight      func(ctx context.Context, in preflight.Input) (preflight.Snapshot, error)
	ModelAvailable func(provider, model string) bool
}

// Service is the production direct-run application service.
type Service struct {
	Deps Deps
}

// Execute runs through cleanup-terminal or fails typed without false success.
func (s Service) Execute(ctx context.Context, req Request) (Result, error) {
	now := s.Deps.Now
	if now == nil {
		now = time.Now
	}
	if req.ReportMode == "" {
		req.ReportMode = termui.ModeHuman
	}
	if req.ReportOut == nil {
		req.ReportOut = io.Discard
	}
	projectID := strings.TrimSpace(req.ProjectID)
	if projectID == "" {
		projectID = slugProject(req.Repo)
	}
	runID := strings.TrimSpace(req.RunID)
	if runID == "" {
		runID = "run_" + shortHash(fmt.Sprintf("%s|%s|%d", req.Repo, req.Issue, now().UnixNano()))
	}
	attemptID := "att_" + shortHash(runID)
	baseSHA := strings.TrimSpace(req.BaseSHA)
	if baseSHA == "" {
		baseSHA = "fixture-base-sha"
	}
	if len(req.RequiredUI) == 0 {
		return Result{Error: "required UI missing"}, fmt.Errorf("directrun: at least one required UI client")
	}
	requiredClient := req.RequiredUI[0]

	pfIn := preflight.Input{Repo: req.Repo, Provider: req.Provider, Model: req.Model, EnsureLayout: false}
	var snap preflight.Snapshot
	var err error
	if s.Deps.Preflight != nil {
		snap, err = s.Deps.Preflight(ctx, pfIn)
	} else {
		snap, err = preflight.Evaluate(ctx, pfIn, preflight.DefaultDeps())
	}
	if err != nil {
		return Result{RunID: runID, Error: err.Error()}, err
	}
	if !snap.AllowLaunch {
		return Result{RunID: "", Error: "preflight blocked: " + string(snap.Decision)}, fmt.Errorf("directrun: preflight blocked")
	}

	var estore *eventstream.Store
	if s.Deps.HomeDir != "" {
		estore, err = eventstream.OpenAt(s.Deps.HomeDir, projectID, now)
	} else {
		estore, err = eventstream.Open(projectID, now)
	}
	if err != nil {
		return Result{RunID: runID, Error: err.Error()}, err
	}

	wtPath, err := allocateWorktree(s.Deps.HomeDir, projectID, runID)
	if err != nil {
		return Result{RunID: runID, Error: err.Error()}, err
	}

	avail := s.Deps.ModelAvailable
	if avail == nil {
		avail = func(string, string) bool { return true }
	}
	pins := routepin.NewStore(now, avail)
	fields := routepin.Fields{
		Provider: req.Provider, Model: req.Model, Effort: req.Effort, Permission: req.Permission,
	}
	fields, err = fields.Normalize()
	if err != nil {
		return Result{RunID: runID, Error: err.Error()}, err
	}
	pin, err := pins.Persist(projectID, attemptID, fields)
	if err != nil {
		return Result{RunID: runID, Error: err.Error()}, err
	}
	pin, err = pins.Acknowledge(pin.PinID)
	if err != nil {
		return Result{RunID: runID, Error: err.Error()}, err
	}
	routeDigest := pin.Digest

	attempts := directattempt.NewStore(now)
	idem := "idem_" + shortHash(runID+"|"+attemptID+"|"+routeDigest)
	if _, err := attempts.Create(projectID, runID, attemptID, routeDigest, wtPath, baseSHA, idem); err != nil {
		return Result{RunID: runID, Error: err.Error()}, err
	}
	if _, err := attempts.Admit(attemptID); err != nil {
		return Result{RunID: runID, Error: err.Error()}, err
	}

	engine := &directattempt.Engine{
		Attempts: attempts,
		Pins:     pins,
		Ledger:   estore.Ledger(),
		Reserve:  func(string) error { return nil },
		Release:  func(string) error { return nil },
		Provider: s.Deps.Provider,
	}
	if engine.Provider == nil {
		fake := providerexec.NewFake()
		engine.Provider = fake.Execute
	}

	seq := estore.NextSequence()
	startEnv, err := uireport.Project(uireport.Input{
		Kind: uireport.KindStart, ProjectID: projectID, RunID: runID, AttemptID: attemptID, Sequence: seq,
		Stage: "start", Status: "starting", Liveness: "alive", DeliveryStage: "persisted",
		Actual:     uireport.Route{Provider: req.Provider, Model: req.Model, Effort: req.Effort},
		Requested:  uireport.Route{Provider: req.Provider, Model: req.Model, Effort: req.Effort},
		Next:       uireport.NextAction{Action: "await_start_rendered"},
		Evidence:   map[string]string{"issue": req.Issue, "base": req.BaseBranch},
		RecordedAt: now().UTC(),
	})
	if err != nil {
		return Result{RunID: runID, Error: err.Error()}, err
	}
	if err := estore.Publish(startEnv); err != nil {
		return Result{RunID: runID, Error: err.Error()}, err
	}
	_, _ = attempts.MarkStartReport(attemptID, startEnv.EventID, startEnv.ContentDigest)

	if err := estore.RegisterClient(uisub.ClientIdentity{
		ClientID: requiredClient, SessionID: "run-session", ProjectID: projectID,
		AdapterVersion: "loopcoder-run/1", Required: true,
	}); err != nil {
		return Result{RunID: runID, Error: err.Error()}, err
	}
	if err := writeReport(req.ReportOut, req.ReportMode, startEnv); err != nil {
		_, _ = attempts.Fail(attemptID)
		return Result{RunID: runID, AttemptID: attemptID, Error: "start render failed: " + err.Error()}, err
	}
	if err := estore.Acknowledge(uisub.Ack{
		ClientID: requiredClient, SessionID: "run-session",
		EventID: startEnv.EventID, Sequence: startEnv.Sequence,
		Digest: startEnv.ContentDigest, Stage: uisub.StageRendered, At: now().UTC(),
	}); err != nil {
		_, _ = attempts.Fail(attemptID)
		return Result{RunID: runID, AttemptID: attemptID, Error: "start rendered ack failed: " + err.Error()}, err
	}

	launchN := 0
	orig := engine.Provider
	engine.Provider = func(ctx context.Context, r providerexec.Request) (providerexec.Outcome, error) {
		launchN++
		return orig(ctx, r)
	}
	att, err := engine.TryLaunch(ctx, directattempt.LaunchBundle{
		AttemptID: attemptID, Route: fields, RouteDigest: routeDigest,
		WorktreePath: wtPath, BaseSHA: baseSHA, IdempotencyKey: idem,
		StartEventID: startEnv.EventID, StartDigest: startEnv.ContentDigest,
		RequiredClient: requiredClient,
	})
	if err != nil {
		_, _ = attempts.Fail(attemptID)
		return Result{RunID: runID, AttemptID: attemptID, ProviderLaunchN: launchN, Error: err.Error()}, err
	}

	if att.State == directattempt.StateProcessTerminal || att.ProviderExitCode != nil {
		att, err = engine.FinishCleanup(attemptID)
		if err != nil {
			return Result{
				RunID: runID, AttemptID: attemptID, ProjectID: projectID,
				State: att.State, ProviderLaunchN: launchN, WorktreePath: wtPath,
				RouteDigest: routeDigest, StartEventID: startEnv.EventID, StartDigest: startEnv.ContentDigest,
				Error: err.Error(),
			}, err
		}
	}

	exitCode := 0
	if att.ProviderExitCode != nil {
		exitCode = *att.ProviderExitCode
	}

	tseq := estore.NextSequence()
	termEnv, err := uireport.Project(uireport.Input{
		Kind: uireport.KindTerminal, ProjectID: projectID, RunID: runID, AttemptID: attemptID, Sequence: tseq,
		Stage: "cleanup_terminal", Status: "success", Liveness: "dead", DeliveryStage: "persisted",
		Actual: uireport.Route{Provider: req.Provider, Model: req.Model},
		Evidence: map[string]string{
			"exit_code": fmt.Sprintf("%d", exitCode),
			"state":     string(att.State),
		},
		Next:       uireport.NextAction{Action: "inspect_status"},
		RecordedAt: now().UTC(),
	})
	if err == nil {
		_ = estore.Publish(termEnv)
		_ = writeReport(req.ReportOut, req.ReportMode, termEnv)
		_ = estore.Acknowledge(uisub.Ack{
			ClientID: requiredClient, SessionID: "run-session",
			EventID: termEnv.EventID, Digest: termEnv.ContentDigest,
			Stage: uisub.StageRendered, At: now().UTC(),
		})
	}

	msg := "worker cleanup-terminal"
	if att.State != directattempt.StateCleanupTerminal {
		msg = "incomplete terminal state: " + string(att.State)
	}
	return Result{
		RunID: runID, AttemptID: attemptID, ProjectID: projectID,
		State: att.State, ProviderLaunchN: launchN, ExitCode: exitCode,
		WorktreePath: wtPath, RouteDigest: routeDigest,
		StartEventID: startEnv.EventID, StartDigest: startEnv.ContentDigest,
		Message: msg,
	}, nil
}

func allocateWorktree(homeDir, projectID, runID string) (string, error) {
	var layout home.V09Layout
	var err error
	if homeDir != "" {
		layout, err = home.NewV09(homeDir)
	} else {
		layout, err = home.ResolveV09(home.Deps{})
	}
	if err != nil {
		return "", err
	}
	if err := layout.EnsureProject(projectID); err != nil {
		return "", err
	}
	pdir, err := layout.ProjectDir(projectID)
	if err != nil {
		return "", err
	}
	root := filepath.Join(pdir, "runs", runID, "worktree")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(root, ".loopcoder-owned-worktree"), []byte(runID+"\n"), 0o600); err != nil {
		return "", err
	}
	return root, nil
}

func writeReport(w io.Writer, mode termui.Mode, env uireport.Envelope) error {
	if w == nil {
		return fmt.Errorf("nil report writer")
	}
	var payload []byte
	if mode == termui.ModeJSONL {
		b, err := json.Marshal(env)
		if err != nil {
			return err
		}
		payload = append(b, '\n')
	} else {
		payload = []byte(uireport.PrettyText(uireport.Human(env)) + "\n")
	}
	n, err := w.Write(payload)
	if err != nil {
		return err
	}
	if n != len(payload) {
		return fmt.Errorf("short write")
	}
	return nil
}

func slugProject(repo string) string {
	repo = strings.TrimSpace(repo)
	repo = strings.ReplaceAll(repo, "/", "-")
	repo = strings.ReplaceAll(repo, " ", "-")
	if repo == "" {
		return "local-project"
	}
	var b strings.Builder
	for _, r := range repo {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	s := b.String()
	if s == "" {
		return "local-project"
	}
	if len(s) > 48 {
		s = s[:48]
	}
	return s
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}
