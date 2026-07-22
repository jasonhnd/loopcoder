package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/eventstream"
	"github.com/jasonhnd/loopcoder/internal/uireport"
)

// TestEventsIsNotReportAlias is a structural regression for V090-RB01:
// events must not dispatch to runReport.
func TestEventsIsNotReportAlias(t *testing.T) {
	src, err := os.ReadFile("cli.go")
	if err != nil {
		// when tests run with module root cwd
		src, err = os.ReadFile(filepath.Join("internal", "cli", "cli.go"))
	}
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	// The thin-alias block must be gone.
	if strings.Contains(text, "Thin alias: events surface reuses report listing") {
		t.Fatal("events still documents thin alias to report")
	}
	if strings.Contains(text, `command.Name == "events"`) {
		// ensure the branch calls runEvents not runReport
		idx := strings.Index(text, `command.Name == "events"`)
		window := text[idx : idx+200]
		if strings.Contains(window, "runReport") {
			t.Fatalf("events still calls runReport: %q", window)
		}
		if !strings.Contains(window, "runEvents") {
			t.Fatalf("events does not call runEvents: %q", window)
		}
	} else {
		t.Fatal("events command branch missing")
	}
}

func TestEventsFollowAfterSequenceBlackBox(t *testing.T) {
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOPCODER_HOME", home)
	projectID := "proj-events-1"
	store, err := eventstream.Open(projectID, func() time.Time {
		return time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, k := range []uireport.Kind{uireport.KindStart, uireport.KindPeriodic, uireport.KindTerminal} {
		in := uireport.Input{
			Kind: k, ProjectID: projectID, AttemptID: "a1", Sequence: int64(i + 1), RunID: "run-9",
			Stage: "s", Status: "running", Liveness: "alive",
			Actual:     uireport.Route{Provider: "codex", Model: "m"},
			Next:       uireport.NextAction{Action: "wait"},
			RecordedAt: time.Date(2026, 7, 22, 12, 0, i, 0, time.UTC),
		}
		if k == uireport.KindTerminal {
			in.Status = "success"
		}
		env, err := uireport.Project(in)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Publish(env); err != nil {
			t.Fatal(err)
		}
	}

	var stdout, stderr bytes.Buffer
	// reconnect after sequence 1
	code := RunWithDeps([]string{
		"events", "--project-id", projectID, "--after", "1", "--format", "jsonl",
	}, &stdout, &stderr, Deps{})
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines after --after 1, got %d %q", len(lines), stdout.String())
	}
	var e uireport.Envelope
	if err := json.Unmarshal([]byte(lines[0]), &e); err != nil {
		t.Fatal(err)
	}
	if e.Sequence != 2 {
		t.Fatalf("first seq=%d want 2", e.Sequence)
	}
	// Ensure help lists follow flags
	var hOut, hErr bytes.Buffer
	_ = RunWithDeps([]string{"events", "--help"}, &hOut, &hErr, Deps{})
	help := hOut.String() + hErr.String()
	for _, want := range []string{"--after", "--follow", "--format", "--bridge", "--project-id"} {
		if !strings.Contains(help, want) {
			t.Fatalf("help missing %s: %s", want, help)
		}
	}
	_ = context.Background()
}

func TestEventsHelpNotReportHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	_ = RunWithDeps([]string{"events", "--help"}, &stdout, &stderr, Deps{})
	help := stdout.String() + stderr.String()
	if strings.Contains(help, "filter by report work id") {
		t.Fatal("events help still looks like report help")
	}
}
