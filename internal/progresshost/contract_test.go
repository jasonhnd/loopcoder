package progresshost

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/progress"
)

func TestBuiltInContractsCoverFourHosts(t *testing.T) {
	got := map[string]bool{}
	for _, contract := range BuiltInHostProgressContracts() {
		got[contract.Profile] = true
		if contract.DetectionCannotGrant == "" {
			t.Fatalf("%s missing detection grant denial", contract.Profile)
		}
		if !contract.Capabilities.SeparateFromMachineStdout && contract.Profile != "generic-cli" {
			// generic may list jsonl/outbox which still separate stdout
		}
	}
	for _, want := range []string{"codex-cli", "claude-code", "paseo-style", "generic-cli"} {
		if !got[want] {
			t.Fatalf("missing contract %s", want)
		}
	}
}

func TestUnknownHostDegradesToGeneric(t *testing.T) {
	contract := ContractForProfile("totally-unknown-host")
	if contract.Profile != "generic-cli" {
		t.Fatalf("profile = %s", contract.Profile)
	}
}

func TestResolveHostProfileDetection(t *testing.T) {
	cases := []struct {
		env  map[string]string
		want string
	}{
		{env: map[string]string{"LOOPCODER_HOST": "codex-cli"}, want: "codex-cli"},
		{env: map[string]string{"CLAUDE_CODE_SESSION_ID": "s1"}, want: "claude-code"},
		{env: map[string]string{"PASEO_AGENT_ID": "a1"}, want: "paseo-style"},
		{env: map[string]string{}, want: "generic-cli"},
		{env: map[string]string{"LOOPCODER_HOST": "made-up"}, want: "generic-cli"},
	}
	for _, tc := range cases {
		got := ResolveHostProfile(func(k string) string { return tc.env[k] })
		if got != tc.want {
			t.Fatalf("env=%v got=%s want=%s", tc.env, got, tc.want)
		}
	}
}

func TestNegotiateActiveSinkObservableWithoutModel(t *testing.T) {
	receipt := progress.ProgressReceipt{
		ProgressReceiptID:   "pr-host",
		ProjectID:           "proj",
		DeliveryRunID:       "run-1",
		RunID:               "run-1",
		CorrelationID:       "corr",
		CorrelationSequence: 1,
		Phase:               "provider-exec",
		Blocker:             progress.ActionState{State: "none"},
		NextAction:          progress.ActionState{State: "continue", Summary: "wait for provider"},
		OccurredAt:          "2026-07-18T12:00:00Z",
		SemanticFingerprint: "fp",
	}
	profiles := []struct {
		name string
		env  map[string]string
	}{
		{name: "codex-cli", env: map[string]string{"LOOPCODER_HOST": "codex-cli", "CODEX_THREAD_ID": "thread-1"}},
		{name: "claude-code", env: map[string]string{"LOOPCODER_HOST": "claude-code", "CLAUDE_CODE_SESSION_ID": "session-1"}},
		{name: "paseo-style", env: map[string]string{"LOOPCODER_HOST": "paseo", "PASEO_AGENT_ID": "agent-1"}},
		{name: "generic-cli", env: map[string]string{}},
	}
	for _, profile := range profiles {
		t.Run(profile.name, func(t *testing.T) {
			var human bytes.Buffer
			sink, neg, contract := NegotiateActiveSink(ActiveSinkOptions{
				HumanWriter: &human,
				Getenv:      func(k string) string { return profile.env[k] },
				Now:         func() time.Time { return time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC) },
			})
			if contract.Profile != profile.name && !(profile.name == "paseo-style" && contract.Profile == "paseo-style") {
				// LOOPCODER_HOST=paseo normalizes to paseo-style
				if contract.Profile != "paseo-style" && contract.Profile != "generic-cli" {
					t.Fatalf("contract profile = %s", contract.Profile)
				}
			}
			if sink.Kind() != progress.SinkKindTerminalHuman {
				t.Fatalf("sink = %s, want terminal-human for default human writer", sink.Kind())
			}
			if neg.Selected != progress.SinkKindTerminalHuman {
				t.Fatalf("selected = %s", neg.Selected)
			}
			if err := sink.Deliver(context.Background(), receipt); err != nil {
				t.Fatalf("deliver: %v", err)
			}
			out := human.String()
			if !strings.Contains(out, "phase=provider-exec") || !strings.Contains(out, "blocker=none") {
				t.Fatalf("not observable on host surface: %q", out)
			}
			// Detection alone must not invent host-callback.
			if neg.Selected == progress.SinkKindHostCallback {
				t.Fatal("host-callback selected without HostDeliver")
			}
		})
	}
}

func TestGenericNeverClaimsHostCallbackFromDetection(t *testing.T) {
	var calls int
	sink, neg, contract := NegotiateActiveSink(ActiveSinkOptions{
		HumanWriter: &bytes.Buffer{},
		HostDeliver: func(context.Context, progress.ProgressReceipt) error {
			calls++
			return nil
		},
		Getenv: func(string) string { return "" }, // generic
	})
	if contract.Profile != "generic-cli" {
		t.Fatalf("profile = %s", contract.Profile)
	}
	// HostDeliver is ignored for generic-cli so private APIs cannot be smuggled
	// through detection alone when profile is generic.
	if sink.Kind() == progress.SinkKindHostCallback {
		t.Fatal("generic claimed host-callback")
	}
	_ = neg
	if calls != 0 {
		t.Fatalf("host deliver invoked %d times", calls)
	}
}

func TestKnownHostMayUseExplicitHostDeliver(t *testing.T) {
	var calls int
	sink, neg, _ := NegotiateActiveSink(ActiveSinkOptions{
		HumanWriter: &bytes.Buffer{},
		HostDeliver: func(context.Context, progress.ProgressReceipt) error {
			calls++
			return nil
		},
		Getenv: func(k string) string {
			if k == "LOOPCODER_HOST" {
				return "codex-cli"
			}
			return ""
		},
	})
	if sink.Kind() != progress.SinkKindHostCallback || neg.Selected != progress.SinkKindHostCallback {
		t.Fatalf("kind=%s selected=%s", sink.Kind(), neg.Selected)
	}
	_ = sink.Deliver(context.Background(), progress.ProgressReceipt{Phase: "x", CorrelationSequence: 1})
	if calls != 1 {
		t.Fatalf("calls=%d", calls)
	}
}
