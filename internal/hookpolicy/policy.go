package hookpolicy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	SchemaPolicy   = "loopcoder.hook.policy.v1"
	SchemaRun      = "loopcoder.hook.run.v1"
	SchemaEvidence = "loopcoder.hook.evidence.v1"
)

// Mode is frozen hook policy for a run.
type Mode string

const (
	ModeRespect        Mode = "respect"
	ModeApprovedBypass Mode = "approved-bypass"
	ModeUnsupported    Mode = "unsupported"
)

var (
	ErrInvalid      = errors.New("hookpolicy: invalid")
	ErrBypassDenied = errors.New("hookpolicy: bypass not authorized")
	ErrTimeout      = errors.New("hookpolicy: timeout")
	ErrMutation     = errors.New("hookpolicy: out-of-scope mutation")
	ErrUnsupported  = errors.New("hookpolicy: hooks unsupported")
)

// Policy is the frozen run policy.
type Policy struct {
	Schema string `json:"schema"`
	Mode   Mode   `json:"mode"`
	// BypassAuthorized is required for approved-bypass; never inferred.
	BypassAuthorized bool   `json:"bypass_authorized"`
	BypassReason     string `json:"bypass_reason,omitempty"`
	// Discovered hook paths (names only, no content).
	DiscoveredHooks []string      `json:"discovered_hooks"`
	SoftDeadline    time.Duration `json:"soft_deadline"`
	HardDeadline    time.Duration `json:"hard_deadline"`
	OwnedScope      []string      `json:"owned_scope"`
	FrozenAt        time.Time     `json:"frozen_at"`
}

// Discovery lists hook names without executing them.
type Discovery struct {
	RepoHooks   []string `json:"repo_hooks"`
	GlobalHooks []string `json:"global_hooks"`
}

// RunResult is bounded hook execution evidence.
type RunResult struct {
	Schema   string        `json:"schema"`
	Hook     string        `json:"hook"`
	Outcome  string        `json:"outcome"` // pass|fail|timeout|skipped_bypass|unsupported
	ExitCode int           `json:"exit_code"`
	Duration time.Duration `json:"duration"`
	// OutputDigest only — never store raw hook output secrets.
	OutputDigest   string `json:"output_digest"`
	Mutation       bool   `json:"out_of_scope_mutation"`
	BlocksDelivery bool   `json:"blocks_delivery"`
	// VisibleBypass is true when approved bypass was used.
	VisibleBypass bool `json:"visible_bypass"`
}

// Freeze builds immutable policy from discovery + owner mode.
func Freeze(mode Mode, bypassAuth bool, bypassReason string, disc Discovery, owned []string, now time.Time) (Policy, error) {
	if mode == "" {
		mode = ModeRespect
	}
	if mode != ModeRespect && mode != ModeApprovedBypass && mode != ModeUnsupported {
		return Policy{}, ErrInvalid
	}
	if mode == ModeApprovedBypass && !bypassAuth {
		return Policy{}, ErrBypassDenied
	}
	if mode == ModeApprovedBypass && strings.TrimSpace(bypassReason) == "" {
		return Policy{}, fmt.Errorf("%w: bypass reason required", ErrBypassDenied)
	}
	// never allow reason that implies recovery auto
	if strings.Contains(strings.ToLower(bypassReason), "auto recovery") {
		return Policy{}, ErrBypassDenied
	}
	hooks := append([]string{}, disc.RepoHooks...)
	hooks = append(hooks, disc.GlobalHooks...)
	sort.Strings(hooks)
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return Policy{
		Schema: SchemaPolicy, Mode: mode, BypassAuthorized: bypassAuth,
		BypassReason: bypassReason, DiscoveredHooks: hooks,
		SoftDeadline: 10 * time.Second, HardDeadline: 30 * time.Second,
		OwnedScope: append([]string(nil), owned...), FrozenAt: now.UTC(),
	}, nil
}

// DiscoverPreflight lists hooks without executing (names only).
func DiscoverPreflight(repoHooks, globalHooks []string) Discovery {
	return Discovery{
		RepoHooks:   normalizeNames(repoHooks),
		GlobalHooks: normalizeNames(globalHooks),
	}
}

// Runner executes hooks with supervision hooks injected by tests.
type Runner struct {
	// Exec runs one hook; returns exit, output, duration, mutation flag.
	Exec func(ctx context.Context, hook string, env []string) (exit int, output []byte, dur time.Duration, mutation bool, err error)
	// ScrubEnv must remove secrets and git redirects.
	ScrubEnv func([]string) []string
}

// DefaultScrub removes common secret and git redirect env.
func DefaultScrub(env []string) []string {
	var out []string
	for _, e := range env {
		k, _, _ := strings.Cut(e, "=")
		uk := strings.ToUpper(k)
		if strings.Contains(uk, "TOKEN") || strings.Contains(uk, "SECRET") || strings.Contains(uk, "PASSWORD") {
			continue
		}
		if strings.HasPrefix(uk, "GIT_DIR") || strings.HasPrefix(uk, "GIT_WORK_TREE") || uk == "GIT_INDEX_FILE" {
			continue
		}
		out = append(out, e)
	}
	return out
}

// Reconcile runs applicable hooks under policy; never auto-bypass.
func (r *Runner) Reconcile(ctx context.Context, p Policy, phase string, env []string) ([]RunResult, error) {
	if p.Mode == ModeUnsupported {
		return []RunResult{{
			Schema: SchemaRun, Hook: phase, Outcome: "unsupported", BlocksDelivery: true,
		}}, ErrUnsupported
	}
	if p.Mode == ModeApprovedBypass && p.BypassAuthorized {
		return []RunResult{{
			Schema: SchemaRun, Hook: phase, Outcome: "skipped_bypass",
			VisibleBypass: true, BlocksDelivery: false,
			OutputDigest: digestBytes([]byte(p.BypassReason)),
		}}, nil
	}
	// respect mode
	scrub := r.ScrubEnv
	if scrub == nil {
		scrub = DefaultScrub
	}
	clean := scrub(env)
	var results []RunResult
	for _, hook := range p.DiscoveredHooks {
		if !applies(hook, phase) {
			continue
		}
		if r.Exec == nil {
			return nil, ErrInvalid
		}
		// hard deadline via context
		cctx, cancel := context.WithTimeout(ctx, p.HardDeadline)
		exit, out, dur, mut, err := r.Exec(cctx, hook, clean)
		cancel()
		res := RunResult{
			Schema: SchemaRun, Hook: hook, ExitCode: exit, Duration: dur,
			OutputDigest: digestBytes(out), Mutation: mut,
		}
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrTimeout) {
				res.Outcome = "timeout"
				res.BlocksDelivery = true
				// caller must Git read-back before retry — we only record evidence
				results = append(results, res)
				return results, ErrTimeout
			}
			res.Outcome = "fail"
			res.BlocksDelivery = true
			results = append(results, res)
			return results, err
		}
		if mut {
			res.Outcome = "fail"
			res.BlocksDelivery = true
			results = append(results, res)
			return results, ErrMutation
		}
		if exit != 0 {
			res.Outcome = "fail"
			res.BlocksDelivery = true
			results = append(results, res)
			return results, fmt.Errorf("hook %s exit %d", hook, exit)
		}
		res.Outcome = "pass"
		results = append(results, res)
	}
	if len(results) == 0 {
		results = append(results, RunResult{Schema: SchemaRun, Hook: phase, Outcome: "pass", OutputDigest: digestBytes(nil)})
	}
	return results, nil
}

// EvidenceBundle is report/PR-visible summary without secrets.
type EvidenceBundle struct {
	Schema        string      `json:"schema"`
	PolicyMode    Mode        `json:"policy_mode"`
	VisibleBypass bool        `json:"visible_bypass"`
	BypassReason  string      `json:"bypass_reason,omitempty"`
	Results       []RunResult `json:"results"`
}

// Bundle builds public evidence.
func Bundle(p Policy, results []RunResult) EvidenceBundle {
	vb := false
	for _, r := range results {
		if r.VisibleBypass {
			vb = true
		}
	}
	return EvidenceBundle{
		Schema: SchemaEvidence, PolicyMode: p.Mode, VisibleBypass: vb,
		BypassReason: p.BypassReason, Results: results,
	}
}

// InferBypassFromProse always false — recovery/agent cannot introduce bypass.
func InferBypassFromProse(prose string) bool {
	_ = prose
	return false
}

func applies(hook, phase string) bool {
	hook = strings.ToLower(hook)
	phase = strings.ToLower(phase)
	switch phase {
	case "commit":
		return hook == "pre-commit" || hook == "commit-msg" || hook == "prepare-commit-msg"
	case "push":
		return hook == "pre-push"
	default:
		return hook == phase
	}
}

func normalizeNames(in []string) []string {
	var out []string
	for _, h := range in {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		// basename only
		if i := strings.LastIndex(h, "/"); i >= 0 {
			h = h[i+1:]
		}
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

func digestBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])[:16]
}
