package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/gitutil"
)

func TestBuildPlanUsesGoDefaultsWhenAuditConfigAbsent(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example.com/audit\n"), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}

	plan, err := BuildPlan(repo, config.Audit{}, Options{})
	if err != nil {
		t.Fatalf("BuildPlan returned error: %v", err)
	}

	if plan.Threshold != SeverityMedium {
		t.Fatalf("Threshold = %q, want medium", plan.Threshold)
	}
	if !reflect.DeepEqual(plan.Layers, []string{LayerSAST}) {
		t.Fatalf("Layers = %#v, want sast", plan.Layers)
	}
	got := commandIDs(plan.Commands)
	want := []string{"govulncheck", "staticcheck", "gosec"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("default commands = %#v, want %#v", got, want)
	}
	if !plan.Native.Secrets || !plan.Native.FilePermissions {
		t.Fatalf("native defaults = %#v, want secrets and file_permissions enabled", plan.Native)
	}
}

func TestBuildPlanNativeDefaultExcludesCoverGeneratedAndLoopcoderDirs(t *testing.T) {
	plan, err := BuildPlan(t.TempDir(), config.Audit{}, Options{})
	if err != nil {
		t.Fatalf("BuildPlan returned error: %v", err)
	}

	for _, want := range []string{
		".git/**",
		".loopcoder/**",
		"node_modules/**",
		"vendor/**",
		"dist/**",
		"build/**",
		"coverage/**",
		".next/**",
		"out/**",
	} {
		if !containsString(plan.Native.Exclude, want) {
			t.Fatalf("native default excludes = %#v, missing %q", plan.Native.Exclude, want)
		}
	}
}

func TestThresholdVerdictAndExitCodePrecedence(t *testing.T) {
	lowOnly := NewResult("repo", []string{LayerSAST}, SeverityMedium)
	lowOnly.Findings = []Finding{makeFinding(Finding{
		Layer:    LayerSAST,
		Tool:     "staticcheck",
		Severity: SeverityLow,
		File:     "a.go",
		Rule:     "SA0000",
		Category: "static-analysis",
		Message:  "low finding",
		Evidence: "low finding",
	})}
	lowOnly = Finalize(lowOnly)
	if lowOnly.Verdict != VerdictClean || ExitCode(lowOnly) != 0 {
		t.Fatalf("low-only result verdict/exit = %s/%d, want clean/0", lowOnly.Verdict, ExitCode(lowOnly))
	}

	gating := lowOnly
	gating.Threshold = SeverityLow
	gating = Finalize(gating)
	if gating.Verdict != VerdictFindings || ExitCode(gating) != 1 {
		t.Fatalf("gating result verdict/exit = %s/%d, want findings/1", gating.Verdict, ExitCode(gating))
	}

	needsHuman := gating
	needsHuman.NeedsHuman = []NeedsHuman{{Layer: LayerSAST, Reason: "ambiguous baseline"}}
	needsHuman = Finalize(needsHuman)
	if needsHuman.Verdict != VerdictNeedsHuman || ExitCode(needsHuman) != 2 {
		t.Fatalf("needs-human result verdict/exit = %s/%d, want needs-human/2", needsHuman.Verdict, ExitCode(needsHuman))
	}

	runtimeFailure := needsHuman
	runtimeFailure.RuntimeFailures = []string{"missing tool"}
	runtimeFailure = Finalize(runtimeFailure)
	if ExitCode(runtimeFailure) != 3 {
		t.Fatalf("runtime failure exit = %d, want 3", ExitCode(runtimeFailure))
	}
}

func TestParseGovulncheckIgnoresDefinitionsAndDedupesTraceLevels(t *testing.T) {
	definitionOnly := `{"osv":{"id":"GO-2026-0001","summary":"definition only"}}`
	findings, err := parseGovulncheck("govulncheck", definitionOnly)
	if err != nil {
		t.Fatalf("parseGovulncheck definition-only returned error: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("definition-only messages produced %d findings, want 0: %#v", len(findings), findings)
	}

	stream := strings.Join([]string{
		`{"osv":{"id":"GO-2026-0002","summary":"reachable vulnerable symbol"}}`,
		`{"finding":{"osv":"GO-2026-0002","trace":[{"module":"example.com/vuln"}]}}`,
		`{"finding":{"osv":"GO-2026-0002","trace":[{"module":"example.com/vuln"},{"package":"example.com/vuln/pkg"}]}}`,
		`{"finding":{"osv":"GO-2026-0002","fixed_version":"v1.2.3","trace":[{"module":"example.com/vuln"},{"package":"example.com/vuln/pkg"},{"function":"Danger","position":{"filename":"pkg/vuln.go","line":42,"column":7}}]}}`,
	}, "\n")

	findings, err = parseGovulncheck("govulncheck", stream)
	if err != nil {
		t.Fatalf("parseGovulncheck returned error: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("parseGovulncheck returned %d findings, want 1: %#v", len(findings), findings)
	}
	got := findings[0]
	if got.Rule != "GO-2026-0002" || got.File != "pkg/vuln.go" || got.Line != 42 || got.Column != 7 {
		t.Fatalf("govulncheck finding location = rule %q file %q line %d col %d", got.Rule, got.File, got.Line, got.Column)
	}
	if !strings.Contains(got.Message, "reachable vulnerable symbol") || !strings.Contains(got.Evidence, "callable=Danger") {
		t.Fatalf("govulncheck finding not enriched from definition/symbol frame: %#v", got)
	}
}

func TestParserAdaptersNormalizeFindings(t *testing.T) {
	staticOutput := strings.Join([]string{
		`{"code":"SA1000","severity":"error","location":{"file":"a.go","line":10,"column":3},"message":"bad regexp"}`,
		`{"code":"S1000","severity":"warning","location":{"file":"b.go","line":20},"message":"simplify"}`,
	}, "\n")
	staticFindings, err := parseStaticcheck("staticcheck", staticOutput)
	if err != nil {
		t.Fatalf("parseStaticcheck returned error: %v", err)
	}
	if len(staticFindings) != 2 {
		t.Fatalf("static findings len = %d, want 2", len(staticFindings))
	}
	if staticFindings[0].Severity != SeverityMedium || staticFindings[1].Severity != SeverityLow {
		t.Fatalf("staticcheck severity mapping = %s/%s, want medium/low", staticFindings[0].Severity, staticFindings[1].Severity)
	}

	gosecOutput := `{"Issues":[{"severity":"HIGH","rule_id":"G101","details":"Potential hardcoded credentials","file":"secret.go","line":"12","code":"api_key := \"redacted\""}]}`
	gosecFindings, err := parseGosec("gosec", gosecOutput)
	if err != nil {
		t.Fatalf("parseGosec returned error: %v", err)
	}
	if len(gosecFindings) != 1 {
		t.Fatalf("gosec findings len = %d, want 1", len(gosecFindings))
	}
	if gosecFindings[0].Severity != SeverityHigh || gosecFindings[0].Rule != "G101" || gosecFindings[0].Line != 12 {
		t.Fatalf("gosec finding = %#v, want high G101 line 12", gosecFindings[0])
	}
}

func TestNativeScansRedactSecretsAndFlagSensitiveWrites(t *testing.T) {
	repo := t.TempDir()
	rawSecret := "supersecretvalue1234567890"
	if err := os.WriteFile(filepath.Join(repo, "config.txt"), []byte("api_key = \""+rawSecret+"\"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "prompt.txt"), []byte("sensitive prompt\n"), 0o644); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "CHANGELOG.md"), []byte("# Changelog\n"), 0o644); err != nil {
		t.Fatalf("write changelog: %v", err)
	}
	source := `package main

import "os"

func writePrompt(data []byte) error {
	return os.WriteFile("prompt.txt", data, 0o644)
}
`
	if err := os.WriteFile(filepath.Join(repo, "writer.go"), []byte(source), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}

	findings, err := RunNativeScans(repo, NativeConfig{Secrets: true, FilePermissions: true, Include: []string{"**/*"}})
	if err != nil {
		t.Fatalf("RunNativeScans returned error: %v", err)
	}
	if len(findings) < 2 {
		t.Fatalf("native findings len = %d, want at least secret and sensitive write: %#v", len(findings), findings)
	}
	encoded, err := json.Marshal(findings)
	if err != nil {
		t.Fatalf("marshal findings: %v", err)
	}
	if strings.Contains(string(encoded), rawSecret) {
		t.Fatalf("native findings leaked raw secret: %s", string(encoded))
	}
	for _, wantRule := range []string{"native:secret", "native:sensitive-write"} {
		if !hasRule(findings, wantRule) {
			t.Fatalf("native findings missing %s: %#v", wantRule, findings)
		}
	}
	for _, finding := range findings {
		if finding.Rule == "native:file-permission" && finding.File == "CHANGELOG.md" {
			t.Fatalf("CHANGELOG.md was incorrectly treated as sensitive: %#v", finding)
		}
	}
}

func TestNativeScansUseGitTrackedFilesAndDefaultExcludes(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	runAuditTestGit(t, repo, "init", "-b", "main")
	writeNativeAuditFile(t, repo, ".gitignore", "node_modules/\nbuild/\n.loopcoder/\n")
	writeNativeAuditFile(t, repo, "tracked.txt", `api_key = "trackedsecretvalue1234567890"`+"\n")
	writeNativeAuditFile(t, repo, "untracked.txt", `api_key = "untrackedsecretvalue1234567890"`+"\n")
	writeNativeAuditFile(t, repo, "node_modules/pkg/leak.txt", `api_key = "nodemodulessecretvalue1234567890"`+"\n")
	writeNativeAuditFile(t, repo, "build/out/leak.txt", `api_key = "buildsecretvalue1234567890"`+"\n")
	writeNativeAuditFile(t, repo, ".loopcoder/leak.txt", `api_key = "loopcodersecretvalue1234567890"`+"\n")
	runAuditTestGit(t, repo, "add", ".gitignore", "tracked.txt")
	runAuditTestGit(t, repo, "add", "-f", "node_modules/pkg/leak.txt", "build/out/leak.txt", ".loopcoder/leak.txt")

	findings, err := RunNativeScans(repo, NativeConfig{
		Secrets: true,
		Include: []string{"**/*"},
		Exclude: []string{"custom-ignore/**"},
	})
	if err != nil {
		t.Fatalf("RunNativeScans returned error: %v", err)
	}
	if len(findings) != 1 || findings[0].File != "tracked.txt" {
		t.Fatalf("native findings = %#v, want only tracked.txt", findings)
	}
	for _, notWant := range []string{"untracked.txt", "node_modules/pkg/leak.txt", "build/out/leak.txt", ".loopcoder/leak.txt"} {
		if hasFindingFile(findings, notWant) {
			t.Fatalf("native scan included excluded or untracked file %s: %#v", notWant, findings)
		}
	}
}

func TestNativeScansApplyUserFiltersAfterGitSourceSelection(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	runAuditTestGit(t, repo, "init", "-b", "main")
	writeNativeAuditFile(t, repo, "src/keep.txt", `api_key = "keepsecretvalue1234567890"`+"\n")
	writeNativeAuditFile(t, repo, "src/drop.txt", `api_key = "dropsecretvalue1234567890"`+"\n")
	writeNativeAuditFile(t, repo, "src/untracked.txt", `api_key = "untrackedsecretvalue1234567890"`+"\n")
	writeNativeAuditFile(t, repo, "docs/drop.txt", `api_key = "docssecretvalue1234567890"`+"\n")
	runAuditTestGit(t, repo, "add", "src/keep.txt", "src/drop.txt", "docs/drop.txt")

	findings, err := RunNativeScans(repo, NativeConfig{
		Secrets: true,
		Include: []string{
			"src/**",
		},
		Exclude: []string{
			"src/drop.txt",
		},
	})
	if err != nil {
		t.Fatalf("RunNativeScans returned error: %v", err)
	}
	if len(findings) != 1 || findings[0].File != "src/keep.txt" {
		t.Fatalf("native findings = %#v, want only src/keep.txt", findings)
	}
}

func TestNativeScansNonGitFallbackUsesDefaultExcludes(t *testing.T) {
	repo := t.TempDir()
	writeNativeAuditFile(t, repo, "keep.txt", `api_key = "keepsecretvalue1234567890"`+"\n")
	for _, path := range []string{
		".loopcoder/leak.txt",
		"node_modules/pkg/leak.txt",
		"vendor/pkg/leak.txt",
		"dist/leak.txt",
		"build/leak.txt",
		"coverage/leak.txt",
		".next/leak.txt",
		"out/leak.txt",
	} {
		writeNativeAuditFile(t, repo, path, `api_key = "excludedsecretvalue1234567890"`+"\n")
	}

	findings, err := RunNativeScans(repo, NativeConfig{Secrets: true, Include: []string{"**/*"}})
	if err != nil {
		t.Fatalf("RunNativeScans returned error: %v", err)
	}
	if len(findings) != 1 || findings[0].File != "keep.txt" {
		t.Fatalf("native findings = %#v, want only keep.txt", findings)
	}
}

func TestRunReportsNativeGitListFailureAsRuntimeFailure(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".git"), []byte("gitdir: missing\n"), 0o600); err != nil {
		t.Fatalf("write invalid .git file: %v", err)
	}
	writeNativeAuditFile(t, repo, "leak.txt", `api_key = "fallbackmustnotscan1234567890"`+"\n")

	result, err := Run(context.Background(), Options{RepoPath: repo}, Deps{})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if ExitCode(result) != 3 {
		t.Fatalf("ExitCode = %d, want 3; result=%#v", ExitCode(result), result)
	}
	if !containsText(result.RuntimeFailures, "native audit git ls-files") {
		t.Fatalf("runtime failures = %#v, want git ls-files failure", result.RuntimeFailures)
	}
	if len(result.Findings) != 0 {
		t.Fatalf("native scan fell back to filesystem walk and found: %#v", result.Findings)
	}
}

func TestRunTreatsParseableNonZeroToolFindingsAsAuditVerdict(t *testing.T) {
	repo := t.TempDir()
	writeAuditConfig(t, repo, `
audit:
  sast:
    commands:
      - id: staticcheck
        argv: ["staticcheck", "-f", "json", "./..."]
        parser: staticcheck-json
    native:
      secrets: false
      file_permissions: false
`)

	result, err := Run(context.Background(), Options{RepoPath: repo}, Deps{Runner: fakeRunner(func(context.Context, CommandInvocation) CommandRunResult {
		return CommandRunResult{
			ExitCode: 1,
			Stdout:   `{"code":"SA1000","severity":"error","location":{"file":"a.go","line":1},"message":"bad regexp"}`,
			Duration: time.Millisecond,
		}
	})})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Verdict != VerdictFindings || ExitCode(result) != 1 {
		t.Fatalf("result verdict/exit = %s/%d, want findings/1", result.Verdict, ExitCode(result))
	}
	if len(result.RuntimeFailures) != 0 {
		t.Fatalf("runtime failures = %#v, want none", result.RuntimeFailures)
	}
}

func TestRunAppliesBaselineAndReportsStaleWaiversWithoutGate(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "docs", "security"), 0o755); err != nil {
		t.Fatalf("create security docs dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "docs", "security", "audit-baseline.yml"), []byte(`
version: 1
waivers:
  - id: staticcheck-sa1000
    rule: SA1000
    path: a.go
    normalized_evidence: bad regexp
    original_severity: medium
    justification: Existing parser fixture used to exercise baseline suppression.
    date_added: 2026-07-05
    review_by: 2099-01-01
  - id: stale-waiver
    rule: SA9999
    path: stale.go
    normalized_evidence: stale
    original_severity: medium
    justification: Exercises report-only stale waiver behavior.
    date_added: 2026-07-05
    review_by: 2099-01-01
`), 0o600); err != nil {
		t.Fatalf("write baseline: %v", err)
	}
	writeAuditConfig(t, repo, `
audit:
  baseline:
    path: docs/security/audit-baseline.yml
  sast:
    commands:
      - id: staticcheck
        argv: ["staticcheck", "-f", "json", "./..."]
        parser: staticcheck-json
    native:
      secrets: false
      file_permissions: false
`)

	result, err := Run(context.Background(), Options{RepoPath: repo}, Deps{Runner: fakeRunner(func(context.Context, CommandInvocation) CommandRunResult {
		return CommandRunResult{
			ExitCode: 1,
			Stdout:   `{"code":"SA1000","severity":"error","location":{"file":"a.go","line":1},"message":"bad regexp"}`,
			Duration: time.Millisecond,
		}
	})})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Verdict != VerdictClean || ExitCode(result) != 0 {
		t.Fatalf("result verdict/exit = %s/%d, want clean/0; result=%#v", result.Verdict, ExitCode(result), result)
	}
	if len(result.Findings) != 1 || !result.Findings[0].Waived || result.Findings[0].WaiverID != "staticcheck-sa1000" {
		t.Fatalf("finding was not waived by normalized evidence: %#v", result.Findings)
	}
	if len(result.BaselineNotices) != 1 || result.BaselineNotices[0].ID != "stale-waiver" || result.BaselineNotices[0].Status != "stale" {
		t.Fatalf("baseline notices = %#v, want one stale waiver", result.BaselineNotices)
	}
	if len(result.NeedsHuman) != 0 {
		t.Fatalf("stale waiver should not gate as needs-human: %#v", result.NeedsHuman)
	}
	if result.Summary.GateFindings != 0 || result.Summary.WaivedFindings != 1 {
		t.Fatalf("summary = %#v, want clean baseline diff with one waived finding", result.Summary)
	}
}

func TestRunBaselineDiffFailsOnlyNetNewSignatureGateFindings(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "docs", "security"), 0o755); err != nil {
		t.Fatalf("create security docs dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "docs", "security", "audit-baseline.yml"), []byte(`
version: 1
waivers:
  - id: existing-stripe-secret
    rule: native:secret
    path: secrets.txt
    normalized_evidence: Stripe live key redacted
    original_severity: high
    justification: Existing test fixture for baseline-diff gating.
    date_added: 2026-07-05
    review_by: 2099-01-01
`), 0o600); err != nil {
		t.Fatalf("write baseline: %v", err)
	}
	writeAuditConfig(t, repo, `
audit:
  baseline:
    path: docs/security/audit-baseline.yml
  sast:
    native:
      secrets: true
      file_permissions: false
`)
	writeNativeAuditFile(t, repo, "secrets.txt", strings.Join([]string{
		"stripe=" + stripeLiveKeyForTest(),
		"github=" + githubClassicTokenForTest(),
	}, "\n"))

	result, err := Run(context.Background(), Options{RepoPath: repo}, Deps{})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Verdict != VerdictFindings || ExitCode(result) != 1 {
		t.Fatalf("result verdict/exit = %s/%d, want findings/1; result=%#v", result.Verdict, ExitCode(result), result)
	}
	if len(result.Findings) != 2 {
		t.Fatalf("findings len = %d, want waived existing plus net-new signature: %#v", len(result.Findings), result.Findings)
	}
	if !hasWaivedFinding(result.Findings, "existing-stripe-secret") {
		t.Fatalf("baseline did not keep waived finding visible: %#v", result.Findings)
	}
	if result.Summary.GateFindings != 1 || result.Summary.WaivedFindings != 1 || result.Summary.Warnings != 0 {
		t.Fatalf("summary = %#v, want one net-new gate finding and one waived finding", result.Summary)
	}
}

func TestExpiredBaselineWaiverNeedsHumanWithoutStaleDoubleEmit(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "docs", "security"), 0o755); err != nil {
		t.Fatalf("create security docs dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "docs", "security", "audit-baseline.yml"), []byte(`
version: 1
waivers:
  - id: expired-waiver
    rule: SA1000
    path: a.go
    normalized_evidence: bad regexp
    original_severity: medium
    justification: Exercises expired waiver gating.
    date_added: 2026-07-05
    review_by: 2000-01-01
`), 0o600); err != nil {
		t.Fatalf("write baseline: %v", err)
	}
	writeAuditConfig(t, repo, `
audit:
  baseline:
    path: docs/security/audit-baseline.yml
  sast:
    commands:
      - id: staticcheck
        argv: ["staticcheck", "-f", "json", "./..."]
        parser: staticcheck-json
    native:
      secrets: false
      file_permissions: false
`)

	result, err := Run(context.Background(), Options{RepoPath: repo}, Deps{Runner: fakeRunner(func(context.Context, CommandInvocation) CommandRunResult {
		return CommandRunResult{
			ExitCode: 1,
			Stdout:   `{"code":"SA1000","severity":"error","location":{"file":"a.go","line":1},"message":"bad regexp"}`,
			Duration: time.Millisecond,
		}
	})})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Verdict != VerdictNeedsHuman || ExitCode(result) != 2 {
		t.Fatalf("result verdict/exit = %s/%d, want needs-human/2", result.Verdict, ExitCode(result))
	}
	if len(result.Findings) != 1 || result.Findings[0].Waived {
		t.Fatalf("expired waiver should not suppress finding: %#v", result.Findings)
	}
	if len(result.NeedsHuman) != 1 || !strings.Contains(result.NeedsHuman[0].Reason, "expired-waiver") {
		t.Fatalf("needs-human = %#v, want one expired waiver reason", result.NeedsHuman)
	}
	if len(result.BaselineNotices) != 0 {
		t.Fatalf("expired waiver should not also be reported stale: %#v", result.BaselineNotices)
	}
}

func TestRenderClassifiesGateWarningsWaivedAndNeedsHuman(t *testing.T) {
	result := NewResult("repo", []string{LayerSAST}, SeverityLow)
	waived := makeFinding(Finding{
		Layer:    LayerSAST,
		Tool:     "native",
		Severity: SeverityHigh,
		File:     "existing.txt",
		Rule:     "native:secret",
		Category: "secret-disclosure",
		Message:  "Existing secret signature detected.",
		Evidence: "Stripe live key redacted",
		Tier:     FindingTierSignature,
		Gate:     FindingGateGate,
	})
	waived.Waived = true
	waived.WaiverID = "existing-secret"
	result.Findings = []Finding{
		waived,
		makeFinding(Finding{
			Layer:    LayerSAST,
			Tool:     "native",
			Severity: SeverityHigh,
			File:     "new.txt",
			Rule:     "native:secret",
			Category: "secret-disclosure",
			Message:  "New secret signature detected.",
			Evidence: "GitHub classic token redacted",
			Tier:     FindingTierSignature,
			Gate:     FindingGateGate,
		}),
		makeFinding(Finding{
			Layer:    LayerSAST,
			Tool:     "native",
			Severity: SeverityLow,
			File:     "maybe.txt",
			Rule:     "native:secret",
			Category: "secret-disclosure",
			Message:  "Potential high-entropy secret assignment detected.",
			Evidence: "keyword api_key assignment redacted; entropy=4.20",
			Tier:     FindingTierEntropy,
			Gate:     FindingGateWarning,
		}),
	}
	result.NeedsHuman = []NeedsHuman{{Layer: LayerSAST, Reason: "baseline waiver expired"}}
	result = Finalize(result)

	if result.Summary.GateFindings != 1 || result.Summary.Warnings != 1 || result.Summary.WaivedFindings != 1 || result.Summary.NeedsHuman != 1 {
		t.Fatalf("summary = %#v, want gate/warning/waived/needs-human split", result.Summary)
	}

	var text bytes.Buffer
	if err := RenderText(&text, result); err != nil {
		t.Fatalf("RenderText returned error: %v", err)
	}
	for _, want := range []string{
		"gate_findings: 1",
		"warnings: 1",
		"waived_findings: 1",
		"needs_human_count: 1",
		"gate=gate",
		"gate=warning",
		"waived=true waiver_id=existing-secret",
		"needs_human:",
	} {
		if !strings.Contains(text.String(), want) {
			t.Fatalf("text output missing %q:\n%s", want, text.String())
		}
	}

	var jsonOut bytes.Buffer
	if err := RenderJSON(&jsonOut, result); err != nil {
		t.Fatalf("RenderJSON returned error: %v", err)
	}
	var payload struct {
		Summary    ResultSummary `json:"summary"`
		Findings   []Finding     `json:"findings"`
		NeedsHuman []NeedsHuman  `json:"needs_human"`
	}
	if err := json.Unmarshal(jsonOut.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal rendered JSON: %v\n%s", err, jsonOut.String())
	}
	if payload.Summary != result.Summary {
		t.Fatalf("JSON summary = %#v, want %#v", payload.Summary, result.Summary)
	}
	if !hasGateFinding(payload.Findings, FindingGateGate) || !hasGateFinding(payload.Findings, FindingGateWarning) || !hasWaivedFinding(payload.Findings, "existing-secret") {
		t.Fatalf("JSON findings did not preserve gate/warning/waived classifications: %#v", payload.Findings)
	}
	if len(payload.NeedsHuman) != 1 {
		t.Fatalf("JSON needs_human len = %d, want 1", len(payload.NeedsHuman))
	}
}

func TestRunDetectsTrackedWorktreeMutation(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	runAuditTestGit(t, repo, "init", "-b", "main")
	runAuditTestGit(t, repo, "config", "user.email", "loopcoder-test@example.com")
	runAuditTestGit(t, repo, "config", "user.name", "Loopcoder Test")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# test\n"), 0o600); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runAuditTestGit(t, repo, "add", "README.md")
	runAuditTestGit(t, repo, "commit", "-m", "initial")
	writeAuditConfig(t, repo, `
audit:
  sast:
    commands:
      - id: mutate
        argv: ["mutate"]
        parser: staticcheck-json
    native:
      secrets: false
      file_permissions: false
`)

	result, err := Run(context.Background(), Options{RepoPath: repo}, Deps{Runner: fakeRunner(func(_ context.Context, _ CommandInvocation) CommandRunResult {
		if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# changed\n"), 0o600); err != nil {
			t.Fatalf("mutate README: %v", err)
		}
		return CommandRunResult{ExitCode: 0, Duration: time.Millisecond}
	})})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if ExitCode(result) != 3 {
		t.Fatalf("ExitCode = %d, want 3; result=%#v", ExitCode(result), result)
	}
	if !containsText(result.RuntimeFailures, "changed tracked worktree") {
		t.Fatalf("runtime failures missing worktree mutation: %#v", result.RuntimeFailures)
	}
}

func TestRenderingIsDeterministic(t *testing.T) {
	result := NewResult("repo", []string{LayerSAST}, SeverityMedium)
	result.Findings = []Finding{
		makeFinding(Finding{Layer: LayerSAST, Tool: "staticcheck", Severity: SeverityLow, File: "b.go", Line: 2, Rule: "S1000", Category: "static-analysis", Message: "low", Evidence: "low"}),
		makeFinding(Finding{Layer: LayerSAST, Tool: "gosec", Severity: SeverityHigh, File: "a.go", Line: 1, Rule: "G101", Category: "security", Message: "high", Evidence: "high"}),
	}
	result.ToolResults = []ToolResult{{ID: "staticcheck", Argv: []string{"staticcheck"}, Parser: ParserStaticcheckJSON, ParseStatus: "parsed"}}

	var jsonA, jsonB bytes.Buffer
	if err := RenderJSON(&jsonA, result); err != nil {
		t.Fatalf("RenderJSON A: %v", err)
	}
	if err := RenderJSON(&jsonB, result); err != nil {
		t.Fatalf("RenderJSON B: %v", err)
	}
	if jsonA.String() != jsonB.String() {
		t.Fatalf("JSON rendering differed:\nA=%s\nB=%s", jsonA.String(), jsonB.String())
	}
	if strings.Index(jsonA.String(), `"rule": "G101"`) > strings.Index(jsonA.String(), `"rule": "S1000"`) {
		t.Fatalf("JSON findings not sorted by severity:\n%s", jsonA.String())
	}

	var textA, textB bytes.Buffer
	if err := RenderText(&textA, result); err != nil {
		t.Fatalf("RenderText A: %v", err)
	}
	if err := RenderText(&textB, result); err != nil {
		t.Fatalf("RenderText B: %v", err)
	}
	if textA.String() != textB.String() {
		t.Fatalf("text rendering differed:\nA=%s\nB=%s", textA.String(), textB.String())
	}
}

func commandIDs(commands []SASTCommand) []string {
	ids := make([]string, 0, len(commands))
	for _, command := range commands {
		ids = append(ids, command.ID)
	}
	return ids
}

func hasRule(findings []Finding, rule string) bool {
	for _, finding := range findings {
		if finding.Rule == rule {
			return true
		}
	}
	return false
}

func containsText(values []string, want string) bool {
	for _, value := range values {
		if strings.Contains(value, want) {
			return true
		}
	}
	return false
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func hasFindingFile(findings []Finding, file string) bool {
	for _, finding := range findings {
		if finding.File == file {
			return true
		}
	}
	return false
}

func hasWaivedFinding(findings []Finding, waiverID string) bool {
	for _, finding := range findings {
		if finding.Waived && finding.WaiverID == waiverID {
			return true
		}
	}
	return false
}

func hasGateFinding(findings []Finding, gate string) bool {
	for _, finding := range findings {
		if finding.Gate == gate {
			return true
		}
	}
	return false
}

func writeAuditConfig(t *testing.T, repo, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, ".delivery.yml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write .delivery.yml: %v", err)
	}
}

func writeNativeAuditFile(t *testing.T, repo, rel, content string) {
	t.Helper()
	path := filepath.Join(repo, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func runAuditTestGit(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	cmd.Env = gitutil.CleanEnv(os.Environ())
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, string(output))
	}
}

type fakeRunner func(context.Context, CommandInvocation) CommandRunResult

func (f fakeRunner) RunCommand(ctx context.Context, invocation CommandInvocation) CommandRunResult {
	return f(ctx, invocation)
}
