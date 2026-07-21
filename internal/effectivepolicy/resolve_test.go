package effectivepolicy_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/effectivepolicy"
)

func TestResolveDeterministicDigestAndProvenance(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	in := effectivepolicy.Inputs{
		Explicit: effectivepolicy.Layer{
			Provider:   "grok",
			Model:      "grok-4.5",
			Effort:     "high",
			Permission: "bounded_write",
			BaseBranch: "pre-prod",
		},
		Defaults: effectivepolicy.CompiledDefaults(),
		Now:      now,
	}
	a, err := effectivepolicy.Resolve(in)
	if err != nil {
		t.Fatalf("Resolve a: %v", err)
	}
	b, err := effectivepolicy.Resolve(in)
	if err != nil {
		t.Fatalf("Resolve b: %v", err)
	}
	if a.Digest == "" || a.Digest != b.Digest {
		t.Fatalf("digests differ: %q vs %q", a.Digest, b.Digest)
	}
	if !strings.HasPrefix(a.Digest, "sha256:") {
		t.Fatalf("digest = %q", a.Digest)
	}
	// FrozenAt is metadata and must not affect digest; change time and compare.
	in2 := in
	in2.Now = now.Add(time.Hour)
	c, err := effectivepolicy.Resolve(in2)
	if err != nil {
		t.Fatalf("Resolve c: %v", err)
	}
	if c.Digest != a.Digest {
		t.Fatalf("digest changed with freeze time: %q vs %q", a.Digest, c.Digest)
	}
	prov, ok := a.Get(effectivepolicy.FieldProvider)
	if !ok || prov.Source != effectivepolicy.SourceExplicitCLI || prov.Value != "grok" {
		t.Fatalf("provider provenance = %#v", prov)
	}
	base, _ := a.Get(effectivepolicy.FieldBaseBranch)
	if base.Source != effectivepolicy.SourceExplicitCLI {
		t.Fatalf("base_branch source = %s, want explicit", base.Source)
	}
	// Defaults fill unset resource limits.
	procs, _ := a.Get(effectivepolicy.FieldMaxChildProcesses)
	if procs.Source != effectivepolicy.SourceDefault || procs.Value != "8" {
		t.Fatalf("max_child_processes = %#v", procs)
	}
}

func TestExplicitPinsBeatEnvProjectAndDefaults(t *testing.T) {
	native := true
	in := effectivepolicy.Inputs{
		Explicit: effectivepolicy.Layer{
			Provider:        "grok",
			Model:           "grok-4.5",
			Effort:          "high",
			Permission:      "bounded_write",
			ReportClient:    "terminal",
			BaseBranch:      "pre-prod",
			NativeSubagents: boolPtr(false),
		},
		RunRequest: effectivepolicy.Layer{
			Provider: "codex",
			Model:    "should-not-win",
		},
		ProjectPolicyYAML: []byte(`
schema_version: 1
provider: claude
model: should-not-win
base_branch: main
`),
		UserLocalYAML: []byte(`
schema_version: 1
provider: gemini
effort: low
`),
		Defaults: effectivepolicy.Layer{
			SchemaVersion: 1,
			Provider:      "default-provider",
			Permission:    "read_only",
			BaseBranch:    "develop",
		},
		Env: map[string]string{
			"LOOPCODER_PROVIDER":   "env-provider",
			"LOOPCODER_MODEL":      "env-model",
			"LOOPCODER_PERMISSION": "orchestrate",
		},
		Now: time.Unix(0, 0).UTC(),
	}
	_ = native
	snap, err := effectivepolicy.Resolve(in)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertField(t, snap, effectivepolicy.FieldProvider, "grok", effectivepolicy.SourceExplicitCLI)
	assertField(t, snap, effectivepolicy.FieldModel, "grok-4.5", effectivepolicy.SourceExplicitCLI)
	assertField(t, snap, effectivepolicy.FieldEffort, "high", effectivepolicy.SourceExplicitCLI)
	assertField(t, snap, effectivepolicy.FieldPermission, "bounded_write", effectivepolicy.SourceExplicitCLI)
	assertField(t, snap, effectivepolicy.FieldBaseBranch, "pre-prod", effectivepolicy.SourceExplicitCLI)
	assertField(t, snap, effectivepolicy.FieldNativeSubagents, "false", effectivepolicy.SourceExplicitCLI)
	if len(snap.Warnings) == 0 {
		t.Fatal("expected warnings that environment overrides were ignored")
	}
	for _, w := range snap.Warnings {
		if strings.Contains(w, "LOOPCODER_PROVIDER") {
			return
		}
	}
	t.Fatalf("warnings missing env ignore: %v", snap.Warnings)
}

func TestPrecedenceWithoutExplicitUsesRunThenProjectThenUserThenDefault(t *testing.T) {
	in := effectivepolicy.Inputs{
		RunRequest: effectivepolicy.Layer{Provider: "codex"},
		ProjectPolicyYAML: []byte(`
schema_version: 1
model: project-model
permission: read_only
`),
		UserLocalYAML: []byte(`
schema_version: 1
effort: medium
report_client: terminal
`),
		Defaults: effectivepolicy.CompiledDefaults(),
		Now:      time.Unix(0, 0).UTC(),
	}
	snap, err := effectivepolicy.Resolve(in)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertField(t, snap, effectivepolicy.FieldProvider, "codex", effectivepolicy.SourceRunRequest)
	assertField(t, snap, effectivepolicy.FieldModel, "project-model", effectivepolicy.SourceProjectPolicy)
	assertField(t, snap, effectivepolicy.FieldEffort, "medium", effectivepolicy.SourceUserLocal)
	assertField(t, snap, effectivepolicy.FieldBaseBranch, "pre-prod", effectivepolicy.SourceDefault)
	assertField(t, snap, effectivepolicy.FieldPermission, "read_only", effectivepolicy.SourceProjectPolicy)
}

func TestUnknownKeysAndBadSchemaFailClosed(t *testing.T) {
	_, err := effectivepolicy.Resolve(effectivepolicy.Inputs{
		ProjectPolicyYAML: []byte("schema_version: 1\nunknown_key: true\n"),
		Defaults:          effectivepolicy.CompiledDefaults(),
	})
	if err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("error = %v, want unknown key", err)
	}

	_, err = effectivepolicy.Resolve(effectivepolicy.Inputs{
		UserLocalYAML: []byte("schema_version: 99\nprovider: x\n"),
		Defaults:      effectivepolicy.CompiledDefaults(),
	})
	if err == nil || !strings.Contains(err.Error(), "incompatible schema_version") {
		t.Fatalf("error = %v, want incompatible schema", err)
	}
}

func TestUnsafePathsAndInvalidLimitsFailClosed(t *testing.T) {
	_, err := effectivepolicy.Resolve(effectivepolicy.Inputs{
		Explicit: effectivepolicy.Layer{ProjectPolicyPath: "/etc/passwd"},
		Defaults: effectivepolicy.CompiledDefaults(),
	})
	if err == nil || !strings.Contains(err.Error(), "repository-relative") {
		t.Fatalf("error = %v, want absolute path rejection", err)
	}

	_, err = effectivepolicy.Resolve(effectivepolicy.Inputs{
		Explicit: effectivepolicy.Layer{ProjectPolicyPath: "../outside.yml"},
		Defaults: effectivepolicy.CompiledDefaults(),
	})
	if err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("error = %v, want path escape rejection", err)
	}

	_, err = effectivepolicy.Resolve(effectivepolicy.Inputs{
		Explicit: effectivepolicy.Layer{MaxChildProcesses: -1},
		Defaults: effectivepolicy.CompiledDefaults(),
	})
	if err == nil || !strings.Contains(err.Error(), "max_child_processes") {
		t.Fatalf("error = %v, want invalid limit", err)
	}

	_, err = effectivepolicy.Resolve(effectivepolicy.Inputs{
		Explicit: effectivepolicy.Layer{Permission: "rooted"},
		Defaults: effectivepolicy.CompiledDefaults(),
	})
	if err == nil || !strings.Contains(err.Error(), "invalid permission") {
		t.Fatalf("error = %v, want invalid permission", err)
	}
}

func TestSnapshotImmutableViaSuccessor(t *testing.T) {
	in := effectivepolicy.Inputs{
		Explicit: effectivepolicy.Layer{Provider: "grok", Permission: "read_only"},
		Defaults: effectivepolicy.CompiledDefaults(),
		Now:      time.Unix(1, 0).UTC(),
	}
	first, err := effectivepolicy.Resolve(in)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	firstDigest := first.Digest
	// Caller "changes config" by resolving successor inputs, not mutating first.
	nextIn := in
	nextIn.Explicit.Model = "grok-4.5"
	second, err := effectivepolicy.Successor(first, nextIn)
	if err != nil {
		t.Fatalf("successor: %v", err)
	}
	if first.Digest != firstDigest {
		t.Fatal("previous snapshot digest mutated")
	}
	if second.Digest == first.Digest {
		t.Fatal("successor digest should change when model pin changes")
	}
	// Original values unchanged.
	assertField(t, first, effectivepolicy.FieldModel, "", effectivepolicy.SourceAbsent)
	assertField(t, second, effectivepolicy.FieldModel, "grok-4.5", effectivepolicy.SourceExplicitCLI)
}

func TestExplainRedactsSecretsAndOmitsAbsolutePaths(t *testing.T) {
	snap, err := effectivepolicy.Resolve(effectivepolicy.Inputs{
		Explicit: effectivepolicy.Layer{
			Provider:   "grok",
			Permission: "read_only",
			// Model intentionally looks secret-shaped to prove explain redaction.
			Model: "sk-ant-syntheticfixture001",
		},
		Defaults: effectivepolicy.CompiledDefaults(),
		Env: map[string]string{
			"LOOPCODER_PROVIDER": "other",
		},
		Now: time.Unix(0, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	text := snap.Explain()
	if strings.Contains(text, "sk-ant-syntheticfixture001") {
		t.Fatalf("explain leaked model secret-shaped value: %s", text)
	}
	if strings.Contains(text, "/Users/") || strings.Contains(text, "HOME=") {
		t.Fatalf("explain leaked host path: %s", text)
	}
	raw, err := snap.ExplainJSON()
	if err != nil {
		t.Fatalf("ExplainJSON: %v", err)
	}
	if strings.Contains(string(raw), "sk-ant-syntheticfixture001") {
		t.Fatalf("json explain leaked secret: %s", raw)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("json: %v", err)
	}
	if doc["digest"] != snap.Digest {
		t.Fatalf("json digest mismatch")
	}
	if snap.RequiresCapability() != "cap.config_freeze" {
		t.Fatalf("capability = %s", snap.RequiresCapability())
	}
}

func TestWritePermissionRequiresProvider(t *testing.T) {
	_, err := effectivepolicy.Resolve(effectivepolicy.Inputs{
		Explicit: effectivepolicy.Layer{Permission: "bounded_write"},
		Defaults: effectivepolicy.CompiledDefaults(),
	})
	if err == nil || !strings.Contains(err.Error(), "provider is required") {
		t.Fatalf("error = %v, want provider required", err)
	}
}

func assertField(t *testing.T, snap effectivepolicy.Snapshot, field, wantValue string, wantSource effectivepolicy.Source) {
	t.Helper()
	pv, ok := snap.Get(field)
	if !ok {
		t.Fatalf("missing field %s", field)
	}
	if pv.Value != wantValue || pv.Source != wantSource {
		t.Fatalf("%s = {value:%q source:%s}, want {value:%q source:%s}", field, pv.Value, pv.Source, wantValue, wantSource)
	}
}

func boolPtr(v bool) *bool { return &v }
