package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/config"
)

func TestResolvePlanDefaultGoCommandsAndNativeDefaults(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example.com/test\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}

	plan, err := ResolvePlan(repo, config.Default(), "")
	if err != nil {
		t.Fatalf("ResolvePlan returned error: %v", err)
	}

	if plan.Threshold != "medium" {
		t.Fatalf("Threshold = %q, want medium", plan.Threshold)
	}
	if len(plan.Commands) != 3 {
		t.Fatalf("Commands = %#v, want three Go defaults", plan.Commands)
	}
	want := [][]string{
		{"govulncheck", "-json", "./..."},
		{"staticcheck", "-f", "json", "./..."},
		{"gosec", "-fmt", "json", "-quiet", "./..."},
	}
	for i, argv := range want {
		if !reflect.DeepEqual(plan.Commands[i].Argv, argv) {
			t.Fatalf("command %d argv = %#v, want %#v", i, plan.Commands[i].Argv, argv)
		}
	}
	if !plan.Native.Secrets || !plan.Native.FilePermissions {
		t.Fatalf("Native defaults = %#v, want both enabled", plan.Native)
	}
}

func TestResolvePlanConfiguredCommandsReplaceDefaults(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example.com/test\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	falseValue := false
	cfg := config.Default()
	cfg.Audit.SeverityThreshold = "high"
	cfg.Audit.SAST.Commands = []config.AuditSASTCommand{{
		ID:             "custom",
		Argv:           []string{"custom-sast", "--json"},
		Parser:         "generic-line",
		TimeoutSeconds: 12,
	}}
	cfg.Audit.SAST.Native.Secrets = &falseValue
	cfg.Audit.SAST.Native.FilePermissions = &falseValue
	cfg.Audit.SAST.Native.Include = []string{"src/**"}
	cfg.Audit.SAST.Native.Exclude = []string{"src/generated/**"}

	plan, err := ResolvePlan(repo, cfg, "low")
	if err != nil {
		t.Fatalf("ResolvePlan returned error: %v", err)
	}

	if plan.Threshold != "low" {
		t.Fatalf("Threshold = %q, want override low", plan.Threshold)
	}
	if len(plan.Commands) != 1 || plan.Commands[0].ID != "custom" {
		t.Fatalf("Commands = %#v, want configured command only", plan.Commands)
	}
	if plan.Native.Secrets || plan.Native.FilePermissions {
		t.Fatalf("Native = %#v, want disabled", plan.Native)
	}
	if !reflect.DeepEqual(plan.Native.Include, []string{"src/**"}) || !reflect.DeepEqual(plan.Native.Exclude, []string{"src/generated/**"}) {
		t.Fatalf("Native patterns = %#v", plan.Native)
	}
}

func TestParsersNormalizeToolFindings(t *testing.T) {
	tests := []struct {
		name     string
		parser   string
		tool     string
		payload  string
		wantRule string
		wantFile string
		wantSev  string
	}{
		{
			name:     "staticcheck",
			parser:   "staticcheck-json",
			tool:     "staticcheck",
			payload:  `{"code":"SA1000","severity":"error","location":{"file":"main.go","line":7,"column":3},"message":"invalid regex"}`,
			wantRule: "SA1000",
			wantFile: "main.go",
			wantSev:  "medium",
		},
		{
			name:     "gosec",
			parser:   "gosec-json",
			tool:     "gosec",
			payload:  `{"Issues":[{"severity":"HIGH","rule_id":"G104","details":"unchecked error","file":"main.go","line":"9","column":"2"}]}`,
			wantRule: "G104",
			wantFile: "main.go",
			wantSev:  "high",
		},
		{
			name:     "govulncheck",
			parser:   "govulncheck-json",
			tool:     "govulncheck",
			payload:  `{"finding":{"osv":"GO-2026-0001","trace":[{"position":{"filename":"main.go","line":11,"column":1}}],"message":"reachable vuln"}}`,
			wantRule: "GO-2026-0001",
			wantFile: "main.go",
			wantSev:  "high",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			findings, err := ParseToolOutput(tt.parser, tt.tool, []byte(tt.payload))
			if err != nil {
				t.Fatalf("ParseToolOutput returned error: %v", err)
			}
			if len(findings) != 1 {
				t.Fatalf("findings = %#v, want one", findings)
			}
			got := findings[0]
			if got.Rule != tt.wantRule || got.File != tt.wantFile || got.Severity != tt.wantSev {
				t.Fatalf("finding = %#v, want rule/file/severity %s/%s/%s", got, tt.wantRule, tt.wantFile, tt.wantSev)
			}
			if got.ID == "" || !strings.HasPrefix(got.Fingerprint, "sha256:") {
				t.Fatalf("finding missing stable id/fingerprint: %#v", got)
			}
		})
	}
}

func TestNativeScansRedactSecretsAndSensitiveWrites(t *testing.T) {
	repo := t.TempDir()
	source := strings.Join([]string{
		"package main",
		`var token = "ghp_abcdefghijklmnopqrstuvwxyz123456"`,
		`func writePrompt(prompt string) {`,
		`    _ = os.WriteFile("prompt.txt", []byte(prompt), 0o644)`,
		`}`,
	}, "\n")
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte(source), 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}

	findings := ScanNative(repo, NativePlan{
		Secrets:         true,
		FilePermissions: true,
		Include:         []string{"**/*"},
		Exclude:         []string{".git/**"},
	})

	if len(findings) != 2 {
		t.Fatalf("findings = %#v, want secret and sensitive-write", findings)
	}
	byRule := map[string]Finding{}
	for _, finding := range findings {
		byRule[finding.Rule] = finding
		if strings.Contains(finding.Evidence, "abcdefghijklmnopqrstuvwxyz") {
			t.Fatalf("evidence leaked raw secret: %#v", finding)
		}
	}
	if byRule["native:secret"].Severity != "high" {
		t.Fatalf("secret finding = %#v", byRule["native:secret"])
	}
	if byRule["native:sensitive-write"].Severity != "medium" {
		t.Fatalf("sensitive-write finding = %#v", byRule["native:sensitive-write"])
	}
}

func TestRunThresholdAndParseableNonZeroExit(t *testing.T) {
	repo := t.TempDir()
	cfg := auditConfigWithGenericCommand()
	runner := &fakeRunner{results: []CommandRunResult{{
		Stdout:   []byte("medium finding\n"),
		ExitCode: 1,
	}}}

	result, err := Run(context.Background(), Options{RepoPath: repo, Threshold: "high"}, Deps{
		LoadConfig: fakeLoadConfig(cfg),
		Git:        &fakeGit{},
		Runner:     runner,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Verdict != VerdictClean || ExitCode(result) != 0 {
		t.Fatalf("high threshold result = %#v exit=%d, want clean/0", result, ExitCode(result))
	}
	if len(result.ToolResults) != 1 || result.ToolResults[0].ParseStatus != ParseStatusParsed {
		t.Fatalf("tool results = %#v, want parsed non-zero as findings", result.ToolResults)
	}

	result, err = Run(context.Background(), Options{RepoPath: repo, Threshold: "medium"}, Deps{
		LoadConfig: fakeLoadConfig(cfg),
		Git:        &fakeGit{},
		Runner: &fakeRunner{results: []CommandRunResult{{
			Stdout:   []byte("medium finding\n"),
			ExitCode: 1,
		}}},
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Verdict != VerdictFindings || ExitCode(result) != 1 {
		t.Fatalf("medium threshold result = %#v exit=%d, want findings/1", result, ExitCode(result))
	}
}

func TestRunRuntimeFailurePrecedence(t *testing.T) {
	repo := t.TempDir()
	result, err := Run(context.Background(), Options{RepoPath: repo, Layers: []string{"all"}}, Deps{
		LoadConfig: fakeLoadConfig(auditConfigWithGenericCommand()),
		Git:        &fakeGit{},
		Runner: &fakeRunner{results: []CommandRunResult{{
			ExitCode: -1,
			Err:      errors.New("missing tool"),
		}}},
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Verdict != VerdictNeedsHuman || ExitCode(result) != 3 {
		t.Fatalf("result = %#v exit=%d, want needs-human with runtime failure exit 3", result, ExitCode(result))
	}
	if len(result.RuntimeFailures) == 0 {
		t.Fatalf("RuntimeFailures = %#v, want missing tool failure", result.RuntimeFailures)
	}
}

func TestRunDetectsWorktreeMutation(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	result, err := Run(context.Background(), Options{RepoPath: repo}, Deps{
		LoadConfig: fakeLoadConfig(config.Default()),
		Git: &fakeGit{
			statuses: []string{"", "?? generated.txt\n"},
		},
		Runner: &fakeRunner{},
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if ExitCode(result) != 3 {
		t.Fatalf("ExitCode = %d, want runtime failure 3; result=%#v", ExitCode(result), result)
	}
	if len(result.RuntimeFailures) == 0 || !strings.Contains(result.RuntimeFailures[0], "generated.txt") {
		t.Fatalf("RuntimeFailures = %#v, want changed path", result.RuntimeFailures)
	}
}

func TestRenderJSONAndTextAreDeterministic(t *testing.T) {
	result := Result{
		SchemaVersion: 1,
		Repo:          ".",
		Layers:        []string{"sast"},
		Threshold:     "medium",
		Verdict:       VerdictFindings,
		Findings: []Finding{
			NewFinding(LayerSAST, "native", SeverityLow, "b.go", 2, 0, "B", "cat", "low", "low evidence"),
			NewFinding(LayerSAST, "native", SeverityHigh, "a.go", 1, 0, "A", "cat", "high", "high evidence"),
		},
		ToolResults: []ToolResult{},
		NeedsHuman:  []NeedHuman{},
	}
	SortFindings(result.Findings)

	var jsonOut bytes.Buffer
	if err := RenderJSON(&jsonOut, result); err != nil {
		t.Fatalf("RenderJSON returned error: %v", err)
	}
	var decoded Result
	if err := json.Unmarshal(jsonOut.Bytes(), &decoded); err != nil {
		t.Fatalf("json output did not decode: %v\n%s", err, jsonOut.String())
	}
	if decoded.Findings[0].File != "a.go" {
		t.Fatalf("JSON findings order = %#v, want severity/file order", decoded.Findings)
	}

	var textOut bytes.Buffer
	if err := RenderText(&textOut, result); err != nil {
		t.Fatalf("RenderText returned error: %v", err)
	}
	text := textOut.String()
	if !strings.Contains(text, "AUDIT SUMMARY") || strings.Index(text, "a.go") > strings.Index(text, "b.go") {
		t.Fatalf("unexpected text render:\n%s", text)
	}
}

func fakeLoadConfig(cfg config.Config) func(context.Context, string, config.LoadOptions) (config.Config, error) {
	return func(context.Context, string, config.LoadOptions) (config.Config, error) {
		return cfg, nil
	}
}

func auditConfigWithGenericCommand() config.Config {
	falseValue := false
	cfg := config.Default()
	cfg.Audit.SAST.Commands = []config.AuditSASTCommand{{
		ID:     "generic",
		Argv:   []string{"generic"},
		Parser: "generic-line",
	}}
	cfg.Audit.SAST.Native.Secrets = &falseValue
	cfg.Audit.SAST.Native.FilePermissions = &falseValue
	return cfg
}

type fakeRunner struct {
	results []CommandRunResult
	calls   int
}

func (f *fakeRunner) Run(context.Context, CommandInvocation) CommandRunResult {
	if f.calls >= len(f.results) {
		return CommandRunResult{ExitCode: 0}
	}
	result := f.results[f.calls]
	f.calls++
	return result
}

type fakeGit struct {
	statuses []string
	calls    int
	err      error
}

func (f *fakeGit) StatusPorcelain(context.Context, string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	if f.calls >= len(f.statuses) {
		return "", nil
	}
	status := f.statuses[f.calls]
	f.calls++
	return status, nil
}
