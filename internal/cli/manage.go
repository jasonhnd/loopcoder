package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"syscall"

	"github.com/jasonhnd/loopcoder/internal/process"
	"github.com/jasonhnd/loopcoder/internal/state"
	"github.com/jasonhnd/loopcoder/internal/supervisedexec"
)

// installShutdownOnSignal terminates this loopcoder instance's managed child
// process groups on SIGINT/SIGTERM, so a Ctrl-C'd loopcoder never leaks a
// running provider CLI. It only ever touches processes THIS loopcoder spawned
// (via the in-process kill-group registry) — never a process by bare name
// (spec 0390, Decision 11).
func installShutdownOnSignal(stderr io.Writer) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		if n := supervisedexec.Shutdown(); n > 0 {
			fmt.Fprintf(stderr, "\n[loopcoder] interrupted; terminated %d managed process group(s)\n", n)
		}
		os.Exit(130)
	}()
}

type managedRow struct {
	Run      string
	Issue    int
	Provider string
	PID      int
	Status   string
	Started  string
}

// loadManagedProcesses lists the live loopcoder-managed worker processes for a
// repo by reading persisted attempt sidecars — never by scanning the machine or
// matching a bare process name. Identification is by tracked attempt PID
// (status=="running" and still alive); it does not cross-check the live
// process's LOOPCODER_MANAGED env, so after a crashed loopcoder OS PID reuse
// could in theory misattribute a PID. This is the spec-sanctioned mechanism
// (env cross-check is platform-uneven and out of scope) and is mitigated by
// status tracking.
func loadManagedProcesses(repoPath string) []managedRow {
	var rows []managedRow
	entries, err := os.ReadDir(state.RunsRoot(repoPath))
	if err != nil {
		return rows
	}
	for _, entry := range entries {
		if !entry.IsDir() || !state.IsRunID(entry.Name()) {
			continue
		}
		attempts, err := state.LoadAttempts(repoPath, entry.Name())
		if err != nil {
			continue
		}
		for _, a := range attempts {
			if a.PID == nil || *a.PID <= 0 || a.Status != "running" {
				continue
			}
			if !process.Alive(*a.PID) {
				continue
			}
			rows = append(rows, managedRow{
				Run:      entry.Name(),
				Issue:    a.Issue,
				Provider: a.Provider,
				PID:      *a.PID,
				Status:   a.Status,
				Started:  a.StartedAt,
			})
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Started < rows[j].Started })
	return rows
}

func runPs(args []string, stdout, stderr io.Writer, _ Deps) int {
	fs := flag.NewFlagSet("ps", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", ".", "repository path")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rows := loadManagedProcesses(*repo)
	if len(rows) == 0 {
		fmt.Fprintln(stdout, "no loopcoder-managed processes running")
		return 0
	}
	fmt.Fprintf(stdout, "%-34s %-6s %-9s %-7s %-8s %s\n", "RUN", "ISSUE", "PROVIDER", "PID", "STATUS", "STARTED")
	for _, r := range rows {
		issue := "-"
		if r.Issue > 0 {
			issue = fmt.Sprintf("#%d", r.Issue)
		}
		fmt.Fprintf(stdout, "%-34s %-6s %-9s %-7d %-8s %s\n", r.Run, issue, r.Provider, r.PID, r.Status, r.Started)
	}
	return 0
}

func runKill(args []string, stdout, stderr io.Writer, _ Deps) int {
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
	rows := loadManagedProcesses(*repo)
	killed := 0
	for _, r := range rows {
		if !*all && r.Run != *run {
			continue
		}
		if err := process.KillTree(r.PID); err != nil {
			fmt.Fprintf(stderr, "kill: pid %d: %v\n", r.PID, err)
			continue
		}
		fmt.Fprintf(stdout, "terminated %s (run %s, pid %d)\n", providerLabel(r.Provider), r.Run, r.PID)
		killed++
	}
	fmt.Fprintf(stdout, "terminated %d loopcoder-managed process tree(s)\n", killed)
	return 0
}

func providerLabel(provider string) string {
	if provider == "" {
		return "process"
	}
	return provider
}
