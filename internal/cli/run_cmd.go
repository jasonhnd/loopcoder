package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"
)

// RunRequest is the normalized loopcoder run command input (V090-025 / #1124).
// Thin shell only: no provider/worktree/GitHub side effects.
type RunRequest struct {
	Repo       string   `json:"repo"`
	Issue      string   `json:"issue"`
	Provider   string   `json:"provider"`
	Model      string   `json:"model"`
	Effort     string   `json:"effort,omitempty"`
	Permission string   `json:"permission,omitempty"`
	BaseBranch string   `json:"base_branch"`
	RequiredUI []string `json:"required_ui,omitempty"`
	OptionalUI []string `json:"optional_ui,omitempty"`
	Detach     bool     `json:"detach"`
	DryRun     bool     `json:"dry_run"`
	Format     string   `json:"format"` // human|json|jsonl
	AutoRoute  bool     `json:"auto_route"`
}

// RunAccepted is printed before long work with a stable run identity.
type RunAccepted struct {
	Schema    string     `json:"schema"`
	RunID     string     `json:"run_id"`
	Request   RunRequest `json:"request"`
	Status    string     `json:"status"` // accepted|dry_run|rejected
	Message   string     `json:"message,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

const schemaRunAccepted = "loopcoder.run.accepted.v1"

// Exit categories for run command.
const (
	exitRunOK           = 0
	exitRunUsage        = 2
	exitRunUnsupported  = 3
	exitRunPrecondition = 4
)

func runRun(args []string, stdout, stderr io.Writer, deps Deps) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		repo       = fs.String("repo", "", "repository path or owner/name")
		issue      = fs.String("issue", "", "issue number or URL")
		provider   = fs.String("provider", "", "explicit provider (required until P4 auto-route)")
		model      = fs.String("model", "", "explicit model")
		effort     = fs.String("effort", "", "explicit effort")
		permission = fs.String("permission", "default", "explicit permission profile")
		base       = fs.String("base", "pre-prod", "base branch")
		requiredUI = fs.String("ui-required", "terminal", "comma-separated required UI clients")
		optionalUI = fs.String("ui-optional", "", "comma-separated optional UI clients")
		detach     = fs.Bool("detach", false, "explicit per-run detach (default foreground)")
		dryRun     = fs.Bool("dry-run", false, "normalize and report without mutation")
		format     = fs.String("format", "human", "human|json|jsonl")
		autoRoute  = fs.Bool("auto-route", false, "request automatic routing (unsupported until P4)")
	)
	if err := fs.Parse(args); err != nil {
		return exitRunUsage
	}
	req := RunRequest{
		Repo: strings.TrimSpace(*repo), Issue: strings.TrimSpace(*issue),
		Provider: strings.TrimSpace(*provider), Model: strings.TrimSpace(*model),
		Effort: strings.TrimSpace(*effort), Permission: strings.TrimSpace(*permission),
		BaseBranch: strings.TrimSpace(*base),
		RequiredUI: splitCSV(*requiredUI), OptionalUI: splitCSV(*optionalUI),
		Detach: *detach, DryRun: *dryRun, Format: strings.ToLower(strings.TrimSpace(*format)),
		AutoRoute: *autoRoute,
	}
	if req.Format == "" {
		req.Format = "human"
	}
	if req.Format != "human" && req.Format != "json" && req.Format != "jsonl" {
		fmt.Fprintf(stderr, "run: invalid --format %q (want human|json|jsonl)\n", req.Format)
		return exitRunUsage
	}
	if req.AutoRoute || (req.Provider == "" && req.Model == "") {
		// Omitted automatic-route inputs are unsupported until P4.
		msg := "automatic routing is unsupported until P4; pass explicit --provider and --model"
		return emitRunRejected(stdout, stderr, req, msg, exitRunUnsupported, deps)
	}
	if req.Provider == "" || req.Model == "" {
		return emitRunRejected(stdout, stderr, req, "missing required --provider and --model", exitRunUsage, deps)
	}
	if req.Repo == "" {
		return emitRunRejected(stdout, stderr, req, "missing required --repo", exitRunUsage, deps)
	}
	if req.Issue == "" {
		return emitRunRejected(stdout, stderr, req, "missing required --issue", exitRunUsage, deps)
	}
	if req.BaseBranch == "" {
		return emitRunRejected(stdout, stderr, req, "missing --base", exitRunUsage, deps)
	}
	if req.Permission == "" {
		return emitRunRejected(stdout, stderr, req, "missing --permission", exitRunUsage, deps)
	}
	if len(req.RequiredUI) == 0 {
		return emitRunRejected(stdout, stderr, req, "at least one --ui-required client is required", exitRunUsage, deps)
	}

	now := deps.Now
	if now == nil {
		now = time.Now
	}
	runID := stableRunID(req, now().UTC())
	accepted := RunAccepted{
		Schema: schemaRunAccepted, RunID: runID, Request: req,
		CreatedAt: now().UTC(),
	}
	if req.DryRun {
		accepted.Status = "dry_run"
		accepted.Message = "normalized inputs; no mutation, no provider, no worktree, no GitHub"
		return emitRunAccepted(stdout, accepted)
	}
	// Contract shell only: record accepted identity; later issues attach execution ports.
	accepted.Status = "accepted"
	accepted.Message = "run recorded; execution ports not yet attached (V090-025 shell)"
	return emitRunAccepted(stdout, accepted)
}

func emitRunRejected(stdout, stderr io.Writer, req RunRequest, msg string, code int, deps Deps) int {
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	acc := RunAccepted{
		Schema: schemaRunAccepted, RunID: "", Request: req,
		Status: "rejected", Message: msg, CreatedAt: now().UTC(),
	}
	_ = emitRunAccepted(stdout, acc)
	fmt.Fprintf(stderr, "run: %s\n", msg)
	return code
}

func emitRunAccepted(stdout io.Writer, acc RunAccepted) int {
	switch acc.Request.Format {
	case "json":
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(acc)
	case "jsonl":
		b, _ := json.Marshal(acc)
		fmt.Fprintln(stdout, string(b))
	default:
		fmt.Fprintf(stdout, "run_id=%s status=%s\n", acc.RunID, acc.Status)
		if acc.Message != "" {
			fmt.Fprintf(stdout, "message=%s\n", acc.Message)
		}
		fmt.Fprintf(stdout, "repo=%s issue=%s provider=%s model=%s base=%s detach=%v dry_run=%v\n",
			acc.Request.Repo, acc.Request.Issue, acc.Request.Provider, acc.Request.Model,
			acc.Request.BaseBranch, acc.Request.Detach, acc.Request.DryRun)
		if len(acc.Request.RequiredUI) > 0 {
			fmt.Fprintf(stdout, "ui_required=%s\n", strings.Join(acc.Request.RequiredUI, ","))
		}
	}
	return exitRunOK
}

func stableRunID(req RunRequest, at time.Time) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s|%s|%s|%s|%d",
		req.Repo, req.Issue, req.Provider, req.Model, req.BaseBranch, req.Permission, at.UnixNano())
	return "run_" + hex.EncodeToString(h.Sum(nil))[:16]
}

func splitCSV(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
