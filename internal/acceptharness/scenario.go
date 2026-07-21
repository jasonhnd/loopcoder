package acceptharness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/effectivepolicy"
)

// ScenarioOptions configures one acceptance scenario.
type ScenarioOptions struct {
	ID       string
	RepoKind RepoKind
	Failure  FailurePlan
	Clock    Clock
	// WorkDir parent for all temp artifacts.
	WorkDir string
}

// ScenarioResult is the outcome of a golden or fault scenario.
type ScenarioResult struct {
	Manifest     Manifest
	ManifestPath string
	PR           PRFixture
	CommitSHA    string
	Events       []string
	LivePIDs     []int
	PolicyDigest string
}

// RunGoldenScenario creates a docs/go fixture repo, runs a fake provider,
// commits a change, opens a PR with green checks, delivers UI acks, and writes
// an evidence manifest. No network is used.
func RunGoldenScenario(ctx context.Context, opts ScenarioOptions) (ScenarioResult, error) {
	return runScenario(ctx, opts, false)
}

// RunFaultScenario injects a planned failure and optionally resumes.
func RunFaultScenario(ctx context.Context, opts ScenarioOptions) (ScenarioResult, error) {
	if opts.Failure.Point == FailNone {
		opts.Failure.Point = FailPushTimeout
		opts.Failure.Resume = true
	}
	return runScenario(ctx, opts, true)
}

func runScenario(ctx context.Context, opts ScenarioOptions, fault bool) (ScenarioResult, error) {
	if opts.WorkDir == "" {
		return ScenarioResult{}, fmt.Errorf("acceptharness: WorkDir required")
	}
	if opts.ID == "" {
		if fault {
			opts.ID = "fault-resume-v1"
		} else {
			opts.ID = "golden-direct-path-v1"
		}
	}
	if opts.RepoKind == "" {
		opts.RepoKind = RepoDocsOnly
	}
	if opts.Clock == nil {
		opts.Clock = NewManualClock(time.Date(2026, 7, 21, 8, 0, 0, 0, time.UTC))
	}

	var events []string
	emit := func(e string) { events = append(events, e) }

	// Freeze synthetic effective policy (public API).
	snap, err := effectivepolicy.Resolve(effectivepolicy.Inputs{
		Explicit: effectivepolicy.Layer{
			Provider:   "test-subprocess",
			Model:      "synthetic-model",
			Effort:     "low",
			Permission: "bounded_write",
			BaseBranch: "main",
		},
		Defaults: effectivepolicy.CompiledDefaults(),
		Now:      opts.Clock.Now(),
	})
	if err != nil {
		return ScenarioResult{}, err
	}
	emit("policy.frozen:" + snap.Digest)
	opts.Clock.Advance(time.Second)

	repo, err := CreateRepo(opts.WorkDir, opts.RepoKind)
	if err != nil {
		return ScenarioResult{}, err
	}
	emit("repo.created:" + string(repo.Kind))

	observer := NewProcessObserver()
	providerDir := filepath.Join(opts.WorkDir, "provider")
	if err := os.MkdirAll(providerDir, 0o700); err != nil {
		return ScenarioResult{}, err
	}

	mode := ProviderCompleteRecord
	switch opts.Failure.Point {
	case FailProviderExit:
		mode = ProviderNonZero
	case FailProviderHang:
		mode = ProviderHang
	}
	provider := &FakeProvider{WorkDir: providerDir, Mode: mode, Observer: observer}

	// First provider attempt.
	pctx := ctx
	var cancel context.CancelFunc
	if mode == ProviderHang {
		pctx, cancel = context.WithCancel(ctx)
	}
	pres, perr := provider.Run(pctx)
	if cancel != nil {
		cancel()
	}
	if mode == ProviderHang {
		emit("provider.hang_cancelled")
		// Ensure cleanup
		if len(observer.LivePIDs()) > 0 {
			for _, pid := range observer.LivePIDs() {
				_ = killProcessGroup(pid)
				observer.untrack(pid)
			}
		}
	} else if perr != nil {
		return ScenarioResult{}, perr
	} else if mode == ProviderNonZero {
		emit(fmt.Sprintf("provider.exit:%d", pres.ExitCode))
		if !opts.Failure.Resume {
			return ScenarioResult{}, fmt.Errorf("provider failed with exit %d", pres.ExitCode)
		}
		// resume with complete mode
		provider.Mode = ProviderCompleteRecord
		pres, perr = provider.Run(ctx)
		if perr != nil {
			return ScenarioResult{}, perr
		}
		emit("provider.resumed_complete")
	} else {
		emit("provider.completed:" + string(pres.Mode))
	}
	if pres.Completion != "" {
		emit("provider.completion_record")
	}

	branch := "fixture/issue-1"
	rel := "docs/CHANGE.md"
	content := "synthetic change\n"
	if opts.RepoKind == RepoSmallGo {
		rel = "main.go"
		content = "package main\n\nfunc main() { println(\"synthetic\") }\n"
	}
	sha, err := repo.CommitFile(branch, rel, content, "synthetic worker commit")
	if err != nil {
		return ScenarioResult{}, err
	}
	emit("git.commit:" + shortSHA(sha))
	opts.Clock.Advance(time.Second)

	gh := NewFakeGitHub()
	gh.SeedIssue(IssueFixture{Number: 1, Title: "synthetic issue", Body: "untrusted synthetic body"})
	ui := NewFakeUI("synthetic-terminal")

	if fault {
		switch opts.Failure.Point {
		case FailPushTimeout:
			gh.PushTimeout = true
		case FailDuplicatePR:
			gh.DuplicateCreate = true
		case FailUIDisconnect:
			ui.Disconnect()
		case FailDuplicateAck:
			ui.DuplicateAck = true
		}
	}

	pr, err := gh.CreatePR(ctx, "synthetic pr", branch, "main", sha)
	if err != nil {
		emit("github.push_timeout")
		if !(fault && opts.Failure.Resume && opts.Failure.Point == FailPushTimeout) {
			return ScenarioResult{}, err
		}
		// resume: retry create
		pr, err = gh.CreatePR(ctx, "synthetic pr", branch, "main", sha)
		if err != nil {
			return ScenarioResult{}, err
		}
		emit("github.pr_resumed")
	} else {
		emit(fmt.Sprintf("github.pr_opened:%d", pr.Number))
	}

	// Second create for duplicate mode.
	if opts.Failure.Point == FailDuplicatePR {
		pr2, err := gh.CreatePR(ctx, "synthetic pr", branch, "main", sha)
		if err != nil {
			return ScenarioResult{}, err
		}
		if pr2.Number != pr.Number {
			return ScenarioResult{}, fmt.Errorf("duplicate create allocated new PR %d", pr2.Number)
		}
		emit("github.pr_duplicate_idempotent")
	}

	gh.SetChecks(pr.Number, []CheckFixture{
		{Name: "verify", Status: CheckSuccess},
		{Name: "test", Status: CheckSuccess},
		{Name: "race", Status: CheckSuccess},
		{Name: "security", Status: CheckSuccess},
	})
	emit("github.checks_green")

	// UI path
	if opts.Failure.Point == FailUIDisconnect && opts.Failure.Resume {
		if err := ui.Deliver(ctx, UIMessage{Sequence: 1, Summary: "running"}); err == nil {
			return ScenarioResult{}, fmt.Errorf("expected ui disconnect on deliver")
		}
		emit("ui.disconnect")
		ui.Reconnect()
		emit("ui.reconnect")
	}
	if err := ui.Deliver(ctx, UIMessage{Sequence: 1, Summary: "worker completed"}); err != nil {
		return ScenarioResult{}, err
	}
	emit("ui.delivered:1")
	if err := ui.Acknowledge(1, AckRendered); err != nil {
		return ScenarioResult{}, err
	}
	emit("ui.acked:rendered")

	// Optional modes coverage for controls (short subprocesses).
	if !fault {
		for _, m := range []ProviderMode{ProviderSilent, ProviderSpawnChild, ProviderFlood, ProviderEmitOutput} {
			fp := &FakeProvider{WorkDir: providerDir, Mode: m, Observer: observer}
			if _, err := fp.Run(ctx); err != nil {
				return ScenarioResult{}, fmt.Errorf("provider mode %s: %w", m, err)
			}
			emit("provider.mode_ok:" + string(m))
		}
	}

	live := observer.LivePIDs()
	cleanup := []string{}
	if len(live) == 0 {
		cleanup = append(cleanup, "zero_surviving_children")
	} else {
		for _, pid := range live {
			_ = killProcessGroup(pid)
			observer.untrack(pid)
			cleanup = append(cleanup, fmt.Sprintf("killed_pid:%d", pid))
		}
		live = observer.LivePIDs()
	}
	emit("process.cleanup_complete")

	sideEffects := []string{
		"git_commit:" + shortSHA(sha),
		fmt.Sprintf("pr:%d", pr.Number),
		"checks:verify,test,race,security",
	}

	manifest := Manifest{
		SchemaVersion:  ManifestSchema,
		ScenarioID:     opts.ID,
		TestedSHA:      sha,
		RepoKind:       repo.Kind,
		Events:         events,
		SideEffects:    sideEffects,
		ProcessCleanup: cleanup,
		Inputs: map[string]string{
			"provider":      "test-subprocess",
			"repo_kind":     string(repo.Kind),
			"issue":         "1",
			"branch":        branch,
			"policy_digest": snap.Digest,
		},
		Expected: map[string]string{
			"checks":             "all_green",
			"ui_ack":             string(AckRendered),
			"surviving_children": "0",
		},
		PolicyDigest: snap.Digest,
		GeneratedAt:  opts.Clock.Now(),
	}
	// Ensure no absolute path leakage in inputs — store synthetic names only.
	manifest.Inputs["repo"] = repo.Owner + "/" + repo.Name

	path, err := WriteManifest(filepath.Join(opts.WorkDir, "evidence"), manifest)
	if err != nil {
		return ScenarioResult{}, err
	}
	emit("manifest.written")

	if len(live) != 0 {
		return ScenarioResult{}, fmt.Errorf("surviving children: %v", live)
	}

	return ScenarioResult{
		Manifest:     manifest,
		ManifestPath: path,
		PR:           pr,
		CommitSHA:    sha,
		Events:       events,
		LivePIDs:     live,
		PolicyDigest: snap.Digest,
	}, nil
}

func shortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
