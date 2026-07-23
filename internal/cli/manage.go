package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/jasonhnd/loopcoder/internal/process"
	"github.com/jasonhnd/loopcoder/internal/providerauthority"
	"github.com/jasonhnd/loopcoder/internal/state"
	"github.com/jasonhnd/loopcoder/internal/supervisedexec"
)

// rootCmdCtx is cancelled on SIGINT/SIGTERM so long-running commands
// (workflow goal) can write interrupt events + partial checkpoints before exit.
// Hard os.Exit alone left the event ledger without interrupt, which blocks
// exact-binary canary evidence (fail-closed: no hand-written interrupted=true).
var (
	rootCmdCtx    context.Context
	rootCmdCancel context.CancelFunc
)

func init() {
	rootCmdCtx, rootCmdCancel = context.WithCancel(context.Background())
}

// CommandContext returns the process root context cancelled on SIGINT/SIGTERM.
// Commands that must record forced-interrupt evidence should use this instead
// of context.Background().
func CommandContext() context.Context {
	if rootCmdCtx == nil {
		return context.Background()
	}
	return rootCmdCtx
}

// installShutdownOnSignal cancels CommandContext, terminates this loopcoder
// instance's managed child process groups, then exits after a short grace so
// workflow can flush interrupt ledger + partial. Second signal exits immediately.
// It only ever touches processes THIS loopcoder spawned (via the in-process
// kill-group registry) — never a process by bare name (spec 0390, Decision 11).
func installShutdownOnSignal(stderr io.Writer) {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		if rootCmdCancel != nil {
			rootCmdCancel()
		}
		if n := supervisedexec.Shutdown(); n > 0 {
			fmt.Fprintf(stderr, "\n[loopcoder] interrupted; terminated %d managed process group(s)\n", n)
		} else {
			fmt.Fprintf(stderr, "\n[loopcoder] interrupted; cancelling in-flight workflow\n")
		}
		// Grace: allow goalrun/workflowrun to observe cancel, append interrupt
		// events, fsync partial, and return. Second signal or timeout force-exits.
		select {
		case <-ch:
		case <-time.After(8 * time.Second):
		}
		os.Exit(130)
	}()
}

type managedRow struct {
	Run             string `json:"run"`
	Attempt         string `json:"attempt"`
	Issue           int    `json:"issue,omitempty"`
	Provider        string `json:"provider,omitempty"`
	PID             int    `json:"pid"`
	PGID            int    `json:"pgid"`
	Status          string `json:"status"`
	Reason          string `json:"reason,omitempty"`
	Verified        bool   `json:"verified"`
	Owner           string `json:"owner,omitempty"`
	Generation      int64  `json:"generation,omitempty"`
	Started         string `json:"started,omitempty"`
	Heartbeat       string `json:"heartbeat,omitempty"`
	Worktree        string `json:"worktree,omitempty"`
	Log             string `json:"log,omitempty"`
	authority       providerauthority.View
	ownershipActive bool
}

// loadManagedProcesses lists loopcoder-managed provider authority rows for a
// repo by reading durable runtime storage. Attempt sidecars are metadata only;
// they are never used as liveness or kill authority.
func loadManagedProcesses(repoPath string, now func() time.Time) ([]managedRow, error) {
	var rows []managedRow
	ctx := context.Background()
	runtime, err := providerauthority.OpenRuntime(ctx, repoPath, now)
	if err != nil {
		return nil, err
	}
	if runtime.Close != nil {
		defer runtime.Close()
	}
	if !runtime.Registered() {
		return rows, nil
	}
	views, err := runtime.List(ctx, "")
	if err != nil {
		return nil, err
	}
	attempts := loadAttemptMetadata(repoPath)
	at := time.Now
	if now != nil {
		at = now
	}
	for _, view := range views {
		meta := attempts[view.Authority.RunID+"\x00"+view.Authority.AttemptID]
		row := managedRow{
			Run:        view.Authority.RunID,
			Attempt:    view.Authority.AttemptID,
			Issue:      meta.Issue,
			Provider:   meta.Provider,
			PID:        view.Authority.ProviderPID,
			PGID:       view.Authority.ProviderPGID,
			Status:     view.State,
			Reason:     view.Reason,
			Verified:   view.Verified,
			Owner:      view.Authority.OwnerID,
			Generation: view.Authority.ClaimGeneration,
			Started:    firstNonEmptyManage(meta.StartedAt, view.Authority.StartedAt),
			Heartbeat:  view.Authority.HeartbeatAt,
			Worktree:   providerauthority.WorktreeDisplay(view.Authority.WorktreePath),
			Log:        providerauthority.WorktreeDisplay(view.Authority.LogPath),
			authority:  view,
		}
		if view.State == providerauthority.StateActive {
			row.ownershipActive = runtime.ValidateOwnership(ctx, view, at()) == nil
			if !row.ownershipActive {
				row.Status = providerauthority.StateStale
				row.Reason = "ownership fence is stale"
			}
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Started < rows[j].Started })
	return rows, nil
}

func loadAttemptMetadata(repoPath string) map[string]state.Attempt {
	out := map[string]state.Attempt{}
	for _, root := range state.RunsRootsForRead(repoPath) {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() || !state.IsRunID(entry.Name()) {
				continue
			}
			attempts, err := state.LoadAttempts(repoPath, entry.Name())
			if err != nil {
				continue
			}
			for _, attempt := range attempts {
				out[entry.Name()+"\x00"+attempt.JobID] = attempt
			}
		}
	}
	return out
}

func runPs(args []string, stdout, stderr io.Writer, deps Deps) int {
	fs := flag.NewFlagSet("ps", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", ".", "repository path")
	format := fs.String("format", "text", "output format: text or json")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *format != "text" && *format != "json" {
		fmt.Fprintf(stderr, "ps: invalid --format %q; want text or json\n", *format)
		return 2
	}
	rows, err := loadManagedProcesses(*repo, deps.Now)
	if err != nil {
		fmt.Fprintf(stderr, "ps: %v\n", err)
		return 1
	}
	if *format == "json" {
		data, err := json.MarshalIndent(struct {
			SchemaVersion string       `json:"schema_version"`
			Rows          []managedRow `json:"rows"`
		}{
			SchemaVersion: "loopcoder.provider_processes.v1",
			Rows:          rows,
		}, "", "  ")
		if err != nil {
			fmt.Fprintf(stderr, "ps: marshal json: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, string(data))
		return 0
	}
	if len(rows) == 0 {
		fmt.Fprintln(stdout, "no loopcoder-managed provider authorities found")
		return 0
	}
	fmt.Fprintf(stdout, "%-34s %-14s %-6s %-9s %-7s %-7s %-17s %-8s %s\n", "RUN", "ATTEMPT", "ISSUE", "PROVIDER", "PID", "PGID", "STATUS", "VERIFIED", "STARTED")
	for _, r := range rows {
		issue := "-"
		if r.Issue > 0 {
			issue = fmt.Sprintf("#%d", r.Issue)
		}
		fmt.Fprintf(stdout, "%-34s %-14s %-6s %-9s %-7d %-7d %-17s %-8t %s\n", r.Run, displayManage(r.Attempt), issue, displayManage(r.Provider), r.PID, r.PGID, r.Status, r.Verified, r.Started)
	}
	return 0
}

func runKill(args []string, stdout, stderr io.Writer, deps Deps) int {
	fs := flag.NewFlagSet("kill", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", ".", "repository path")
	run := fs.String("run", "", "terminate only this run id")
	all := fs.Bool("all", false, "terminate all loopcoder-managed processes")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *run == "" && !*all {
		fmt.Fprintln(stderr, "kill: specify --run <id> or --all")
		return 2
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	rows, err := loadManagedProcesses(*repo, now)
	if err != nil {
		fmt.Fprintf(stderr, "kill: %v\n", err)
		return 1
	}
	killed := 0
	blocked := 0
	for _, r := range rows {
		if !*all && r.Run != *run {
			continue
		}
		if r.Status != providerauthority.StateActive || !r.Verified || !r.ownershipActive {
			fmt.Fprintf(stderr, "kill: refused %s/%s pid %d: provider authority state=%s reason=%s\n", r.Run, r.Attempt, r.PID, r.Status, firstNonEmptyManage(r.Reason, "not verified"))
			blocked++
			continue
		}
		killGroup := deps.KillProcessGroup
		if killGroup == nil {
			killGroup = process.KillGroup
		}
		if err := killGroup(r.PGID); err != nil {
			fmt.Fprintf(stderr, "kill: pgid %d: %v\n", r.PGID, err)
			continue
		}
		fmt.Fprintf(stdout, "terminated %s (run %s, attempt %s, pid %d, pgid %d)\n", providerLabel(r.Provider), r.Run, r.Attempt, r.PID, r.PGID)
		killed++
	}
	if blocked > 0 && killed == 0 {
		fmt.Fprintf(stdout, "terminated 0 loopcoder-managed provider group(s)\n")
		return 1
	}
	fmt.Fprintf(stdout, "terminated %d loopcoder-managed provider group(s)\n", killed)
	return 0
}

func providerLabel(provider string) string {
	if provider == "" {
		return "process"
	}
	return provider
}

func displayManage(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return strings.TrimSpace(value)
}

func firstNonEmptyManage(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
