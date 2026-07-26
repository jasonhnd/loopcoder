package uireport_test

import (
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/uireport"
)

func baseIn(kind uireport.Kind) uireport.Input {
	return uireport.Input{
		Kind:             kind,
		ProjectID:        "proj_1",
		AttemptID:        "att_1",
		Sequence:         1,
		Stage:            "worker_running",
		Status:           "running",
		Elapsed:          5 * time.Minute,
		Liveness:         "alive",
		SemanticProgress: true,
		DeliveryStage:    "checks_pending",
		Evidence:         map[string]string{"output_bytes": "12"},
		Requested:        uireport.Route{Provider: "codex", Model: "o"},
		Actual:           uireport.Route{Provider: "codex", Model: "o3", Effort: "high"},
		Resources:        uireport.ResourceState{State: "ok", Processes: 2},
		Next:             uireport.NextAction{Action: "wait"},
		NextReportAt:     time.Date(2026, 7, 22, 10, 5, 0, 0, time.UTC),
		RecordedAt:       time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC),
	}
}

func TestAllKindsProject(t *testing.T) {
	for _, k := range []uireport.Kind{
		uireport.KindStart, uireport.KindStateChange, uireport.KindPeriodic,
		uireport.KindAttention, uireport.KindBlocker, uireport.KindTerminal,
	} {
		in := baseIn(k)
		if k == uireport.KindBlocker {
			in.Blocker = "ci_red"
		}
		if k == uireport.KindAttention {
			in.Attention = []string{"need_approval"}
		}
		env, err := uireport.Project(in)
		if err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		if env.Schema != uireport.SchemaEnvelope || env.ContentDigest == "" {
			t.Fatalf("%#v", env)
		}
		h := uireport.Human(env)
		if h.ContentDigest != env.ContentDigest {
			t.Fatalf("digest mismatch machine vs human")
		}
		// Distinct fields not collapsed
		if h.Liveness == "" || h.Stage == "" {
			t.Fatal("missing fields")
		}
		if k == uireport.KindBlocker && h.Blocker == "" {
			t.Fatal("blocker hidden")
		}
		text := uireport.PrettyText(h)
		if strings.Contains(text, "sk-") {
			t.Fatal("secret in pretty")
		}
		// Pretty is not authority — digest comes from envelope only.
		_ = text
	}
}

func TestRedactsSecretsAndPaths(t *testing.T) {
	in := baseIn(uireport.KindPeriodic)
	in.Evidence = map[string]string{
		"token": "sk-abc",
		"path":  "/Users/alice/secret",
		"ok":    "commit_abc",
	}
	in.Blocker = "password=nope"
	env, err := uireport.Project(in)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := env.LastConcreteEvidence["token"]; ok {
		t.Fatal("secret field retained")
	}
	if _, ok := env.LastConcreteEvidence["path"]; ok {
		t.Fatal("path retained")
	}
	if _, ok := env.LastConcreteEvidence["ok"]; !ok {
		t.Fatal("safe field dropped")
	}
	if env.Blocker != nil {
		t.Fatal("forbidden blocker retained")
	}
}

func TestDoesNotCollapseWorking(t *testing.T) {
	in := baseIn(uireport.KindPeriodic)
	in.SemanticProgress = false
	in.Liveness = "alive"
	in.Status = "running"
	env, err := uireport.Project(in)
	if err != nil {
		t.Fatal(err)
	}
	h := uireport.Human(env)
	// Must expose liveness separately from progress
	if h.Liveness != "alive" {
		t.Fatal(h.Liveness)
	}
	if env.SemanticProgress {
		t.Fatal("progress should be false")
	}
}

func TestNarrowAndDesktopFieldsPresent(t *testing.T) {
	env, err := uireport.Project(baseIn(uireport.KindPeriodic))
	if err != nil {
		t.Fatal(err)
	}
	h := uireport.Human(env)
	for _, f := range []string{h.Stage, h.Elapsed, h.ActualRoute, h.NextAction} {
		if f == "" {
			t.Fatalf("missing required view field %#v", h)
		}
	}
	if h.ActualRoute == "(none)" {
		t.Fatal("actual model missing")
	}
	if h.NextReportAt.IsZero() {
		t.Fatal("next report deadline missing")
	}
}
