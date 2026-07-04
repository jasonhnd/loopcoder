package examples

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/agent"
	"github.com/jasonhnd/loopcoder/internal/attestation"
	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/loopreview"
	"github.com/jasonhnd/loopcoder/internal/mcp"
	"github.com/jasonhnd/loopcoder/internal/orchestration"
	"github.com/jasonhnd/loopcoder/internal/skills"
	gh "github.com/jasonhnd/loopcoder/internal/vcs/github"
)

func TestDocsDomainExampleWiresValidationSlice(t *testing.T) {
	repo := copyDocsDomainFixture(t)
	cfg, err := config.Load(filepath.Join(repo, ".delivery.yml"))
	if err != nil {
		t.Fatalf("load docs-domain .delivery.yml: %v", err)
	}
	assertDocsDomainConfig(t, cfg)
	assertAbsentDomainDefault(t)
	assertDocsDomainSkills(t, repo, cfg)
	assertDocsDomainMCP(t, cfg)
	assertDocsDomainRiskGate(t, cfg)

	result, fakeAgent, producerInvocation := runDocsDomainLoopreview(t, repo)
	if result.Verdict.Verdict != loopreview.VerdictPass {
		t.Fatalf("loopreview verdict = %q, want pass: %#v", result.Verdict.Verdict, result.Verdict)
	}
	if producerInvocation.Command != cfg.Domain.Evidence.Producer.Command {
		t.Fatalf("producer command = %q, want %q", producerInvocation.Command, cfg.Domain.Evidence.Producer.Command)
	}
	if producerInvocation.WorktreePath == "" {
		t.Fatal("producer worktree path is empty")
	}
	if len(result.Verdict.RenderedArtifacts) != 1 {
		t.Fatalf("rendered artifacts = %#v, want one produced artifact", result.Verdict.RenderedArtifacts)
	}
	artifact := result.Verdict.RenderedArtifacts[0]
	if artifact.Source != "domain.evidence.producer" || artifact.DeclaredOutput != "out/report.txt" ||
		artifact.Path != "out/report.txt" || artifact.Kind != "text" || artifact.Status != "available" ||
		artifact.SHA256 == "" {
		t.Fatalf("rendered artifact = %#v, want available text output from producer allow-list", artifact)
	}
	assertOrder(t, fakeAgent.invocation.Prompt,
		"# Rendered artifacts",
		"# Rubric",
		"# Changed files",
		"# Diff excerpts",
		"# Issue",
		"# Merged design/spec",
	)
	for _, want := range []string{
		"Rubric budget:",
		"## Inline checklist",
		"## governance/qa-checklist.md",
		"Rendered IR report",
		"Disclosure review: required",
	} {
		if !strings.Contains(fakeAgent.invocation.Prompt, want) {
			t.Fatalf("loopreview prompt missing %q:\n%s", want, fakeAgent.invocation.Prompt)
		}
	}

	missingRubricRepo := copyDocsDomainFixture(t)
	rewriteDeliveryConfig(t, missingRubricRepo, "governance/qa-checklist.md", "governance/missing-qa-checklist.md")
	missingResult, _, _ := runDocsDomainLoopreview(t, missingRubricRepo)
	if missingResult.Verdict.Verdict != loopreview.VerdictNeedsHuman || missingResult.ExitCode != 2 {
		t.Fatalf("missing rubric result = %#v, want needs-human exit 2", missingResult)
	}
	if !strings.Contains(missingResult.Verdict.Evidence, "configured rubric evidence unavailable") {
		t.Fatalf("missing rubric evidence = %q, want configured rubric evidence note", missingResult.Verdict.Evidence)
	}
	if len(missingResult.Verdict.Findings) == 0 || missingResult.Verdict.Findings[len(missingResult.Verdict.Findings)-1].File != "governance/missing-qa-checklist.md" {
		t.Fatalf("missing rubric findings = %#v, want missing rubric file finding", missingResult.Verdict.Findings)
	}
}

func TestDocsDomainAbsentProfileKeepsCodeReviewPacketDefault(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".delivery.yml"), []byte("version: 1\nci:\n  checks: [verify]\n"), 0o644); err != nil {
		t.Fatalf("write delivery config: %v", err)
	}

	_, fakeAgent, _ := runDocsDomainLoopreview(t, repo)
	prompt := fakeAgent.invocation.Prompt
	assertOrder(t, prompt,
		"# Changed files",
		"# Diff excerpts",
		"# Issue",
		"# Merged design/spec",
	)
	for _, notWant := range []string{"# Rubric", "# Rendered artifacts"} {
		if strings.Contains(prompt, notWant) {
			t.Fatalf("absent-domain prompt contained %q:\n%s", notWant, prompt)
		}
	}
}

func assertDocsDomainConfig(t *testing.T, cfg config.Config) {
	t.Helper()
	if cfg.Domain.Name != "docs" {
		t.Fatalf("domain name = %q, want docs", cfg.Domain.Name)
	}
	if !cfg.Verification.SpecRequired || cfg.Verification.MaxFixPasses != 3 {
		t.Fatalf("verification defaults = %#v, want Default()-safe values", cfg.Verification)
	}
	if cfg.Domain.Evidence.Producer.Command != "go run ./tools/docs_domain_tool.go render" {
		t.Fatalf("producer command = %q", cfg.Domain.Evidence.Producer.Command)
	}
	if !reflect.DeepEqual(cfg.Domain.Evidence.Producer.Outputs, []string{"out/report.txt"}) {
		t.Fatalf("producer outputs = %#v, want out/report.txt allow-list", cfg.Domain.Evidence.Producer.Outputs)
	}
	if cfg.Domain.Evidence.Producer.IncludeInLoopreview == nil || !*cfg.Domain.Evidence.Producer.IncludeInLoopreview {
		t.Fatalf("include_in_loopreview = %#v, want true", cfg.Domain.Evidence.Producer.IncludeInLoopreview)
	}
	wantOrder := []string{"rendered_artifact", "rubric", "changed_files", "diff", "issue", "spec"}
	if !reflect.DeepEqual(cfg.Domain.Verification.ReviewPacketOrder, wantOrder) {
		t.Fatalf("review_packet_order = %#v, want %#v", cfg.Domain.Verification.ReviewPacketOrder, wantOrder)
	}
	if cfg.Domain.PartialWork.Mode != "report-only" || cfg.Domain.Liveness.Mode != "worktree-mtime" {
		t.Fatalf("partial/liveness = %#v/%#v, want report-only/worktree-mtime", cfg.Domain.PartialWork, cfg.Domain.Liveness)
	}
}

func assertAbsentDomainDefault(t *testing.T) {
	t.Helper()
	cfg, err := config.Parse([]byte("version: 1\n"))
	if err != nil {
		t.Fatalf("parse absent-domain config: %v", err)
	}
	want := config.Default()
	want.Version = 1
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("absent-domain config = %#v, want Default() with version", cfg)
	}
}

func assertDocsDomainSkills(t *testing.T, repo string, cfg config.Config) {
	t.Helper()
	section, err := skills.BuildPromptSection(skills.PromptSectionOptions{
		RepoPath:            repo,
		Paths:               cfg.Domain.Skills.Paths,
		MachineLibraryPaths: cfg.Domain.Skills.MachineLibrary.Paths,
		Select:              cfg.Domain.Skills.Select,
		BudgetBytes:         cfg.Domain.Skills.PromptBudgetBytes,
	})
	if err != nil {
		t.Fatalf("build skill prompt section: %v", err)
	}
	if len(section) > cfg.Domain.Skills.PromptBudgetBytes {
		t.Fatalf("skill prompt length = %d, want <= budget %d", len(section), cfg.Domain.Skills.PromptBudgetBytes)
	}
	for _, want := range []string{
		"Discovery paths:",
		"- skills: `governance/**/skill.md`",
		"- machine_library: `machine-library/**/*.md`",
		"Selection filter: governance, disclosure.",
		"Path: `governance/review/skill.md`",
		"Summary: Review conventions for corporate IR document packets.",
		"Path: `machine-library/disclosure.md`",
		"Summary: Machine-readable disclosure review metadata.",
	} {
		if !strings.Contains(section, want) {
			t.Fatalf("skill prompt section missing %q:\n%s", want, section)
		}
	}
	if strings.Contains(section, "Drafting Notes") {
		t.Fatalf("skill prompt included unselected drafting skill:\n%s", section)
	}
}

func assertDocsDomainMCP(t *testing.T, cfg config.Config) {
	t.Helper()
	servers, err := mcp.ServersForInvocation(cfg.MCP, mcp.RoleVerifier, true)
	if err != nil {
		t.Fatalf("select verifier MCP servers: %v", err)
	}
	if len(servers) != 1 {
		t.Fatalf("verifier MCP servers = %#v, want one read-only governance-index", servers)
	}
	server := servers[0]
	if server.Name != "governance-index" || server.Transport != "stdio" || server.Command != "go" || !server.ReadOnly {
		t.Fatalf("verifier MCP server = %#v, want read-only stdio governance-index", server)
	}
	if !reflect.DeepEqual(server.Args, []string{"run", "./tools/docs_domain_tool.go", "mcp"}) {
		t.Fatalf("MCP args = %#v, want local no-op command", server.Args)
	}
}

func assertDocsDomainRiskGate(t *testing.T, cfg config.Config) {
	t.Helper()
	domainRedLines := orchestration.DomainRedLines(cfg.Domain.RedLines)
	decision, err := orchestration.EvaluateRiskGate(context.Background(), orchestration.RiskGateOptions{
		Reader:             docsDomainRiskReader("disclosure/q4-risk-factors.md", docsModifiedDiff("disclosure/q4-risk-factors.md"), gh.Check{Name: "verify", Bucket: "pass"}),
		PRNumber:           479,
		RequiredChecks:     []string{"verify"},
		AdditionalRedLines: domainRedLines,
	})
	if err != nil {
		t.Fatalf("domain risk gate returned error: %v", err)
	}
	assertRedLine(t, decision, "disclosure-compliance", "disclosure or legal approval")

	builtinCases := []struct {
		name     string
		reader   docsRiskReader
		category string
		detail   string
	}{
		{
			name:     "destructive floor",
			reader:   docsDomainRiskReader("scripts/render.sh", docsAddedLineDiff("scripts/render.sh", "rm -rf /tmp/docs-domain"), gh.Check{Name: "verify", Bucket: "pass"}),
			category: orchestration.RiskRedLineDestructive,
			detail:   "destructive command added",
		},
		{
			name: "build floor",
			reader: docsRiskReader{
				files:  []string{"README.md"},
				diff:   docsModifiedDiff("README.md"),
				checks: []gh.Check{{Name: "verify", Bucket: "fail"}},
			},
			category: orchestration.RiskRedLineBuild,
			detail:   "required checks not green",
		},
		{
			name:     "core floor",
			reader:   docsDomainRiskReader("internal/config/config.go", docsModifiedDiff("internal/config/config.go"), gh.Check{Name: "verify", Bucket: "pass"}),
			category: orchestration.RiskRedLineCore,
			detail:   "human rebuild and tick restart",
		},
	}
	for _, tc := range builtinCases {
		t.Run(tc.name, func(t *testing.T) {
			decision, err := orchestration.EvaluateRiskGate(context.Background(), orchestration.RiskGateOptions{
				Reader:         tc.reader,
				PRNumber:       479,
				RequiredChecks: []string{"verify"},
				AdditionalRedLines: append(domainRedLines, orchestration.RiskRedLine{
					Category:  "domain-safe",
					Detail:    "attempted safe classification",
					PathGlobs: []string{"**"},
				}),
			})
			if err != nil {
				t.Fatalf("risk gate returned error: %v", err)
			}
			assertRedLine(t, decision, tc.category, tc.detail)
		})
	}
}

func runDocsDomainLoopreview(t *testing.T, repo string) (loopreview.Result, *docsDomainFakeAgent, loopreview.EvidenceProducerInvocation) {
	t.Helper()
	fakeGit := &docsDomainFakeGit{source: repo}
	fakeGitHub := &docsDomainFakeGitHub{}
	fakeAgent := &docsDomainFakeAgent{
		summary: `{"verdict":"pass","findings":[],"evidence":"docs-domain packet satisfies issue, spec, rubric, and rendered artifact","spec_conformance":"pass"}`,
	}
	var producerInvocation loopreview.EvidenceProducerInvocation
	result, err := loopreview.Run(context.Background(), loopreview.Options{
		RepoPath:   repo,
		PRNumber:   479,
		Provider:   "codex",
		BaseBranch: "main",
	}, loopreview.Deps{
		Git: fakeGit,
		GitHub: func(string) loopreview.GitHubClient {
			return fakeGitHub
		},
		AgentLookup: func(string) (agent.Runner, error) {
			return fakeAgent, nil
		},
		AcquireLock: func(string, time.Duration) (loopreview.Lock, error) {
			return &docsDomainFakeLock{}, nil
		},
		MkdirTemp: os.MkdirTemp,
		RemoveAll: os.RemoveAll,
		RunEvidenceProducer: func(_ context.Context, invocation loopreview.EvidenceProducerInvocation) loopreview.EvidenceProducerResult {
			producerInvocation = invocation
			outPath := filepath.Join(invocation.WorktreePath, "out", "report.txt")
			if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
				return loopreview.EvidenceProducerResult{ExitCode: -1, Err: err}
			}
			data := []byte("Rendered IR report\n\nReporting period: FY2026 Q4\nDisclosure review: required\n")
			if err := os.WriteFile(outPath, data, 0o644); err != nil {
				return loopreview.EvidenceProducerResult{ExitCode: -1, Err: err}
			}
			return loopreview.EvidenceProducerResult{ExitCode: 0, Output: "rendered out/report.txt\n"}
		},
		ReviewPacketLimits: loopreview.ReviewPacketLimits{
			RubricBytes: 512,
		},
	})
	if err != nil {
		t.Fatalf("loopreview Run returned error: %v", err)
	}
	return result, fakeAgent, producerInvocation
}

type docsDomainFakeGit struct {
	source string
}

func (g *docsDomainFakeGit) FetchOriginBase(context.Context, string, string) error {
	return nil
}

func (g *docsDomainFakeGit) FetchPRHead(context.Context, string, int) error {
	return nil
}

func (g *docsDomainFakeGit) WorktreeAddDetachedAt(_ context.Context, _ string, worktreePath, _ string) error {
	return copyDir(g.source, worktreePath)
}

func (g *docsDomainFakeGit) WorktreeRemove(context.Context, string, string) error {
	return nil
}

func (g *docsDomainFakeGit) Show(_ context.Context, _ string, revPath string) (string, error) {
	_, relPath, ok := strings.Cut(revPath, ":")
	if !ok || strings.TrimSpace(relPath) == "" {
		return "", os.ErrNotExist
	}
	if relPath == "docs/specs/0459-domain-profiles.md" {
		data, err := os.ReadFile(filepath.Join("..", "docs", "specs", "0459-domain-profiles.md"))
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
	data, err := os.ReadFile(filepath.Join(g.source, filepath.FromSlash(relPath)))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", os.ErrNotExist
		}
		return "", err
	}
	return string(data), nil
}

type docsDomainFakeGitHub struct{}

func (g *docsDomainFakeGitHub) ViewPR(context.Context, int) (gh.PullRequest, error) {
	return gh.PullRequest{
		Number:      479,
		Title:       "Docs-domain validation",
		Body:        "Closes #479",
		HeadRefName: "loop/issue-479",
		ClosingIssuesReferences: []gh.IssueReference{{
			Number: 479,
		}},
	}, nil
}

func (g *docsDomainFakeGitHub) ViewIssue(context.Context, int) (gh.Issue, error) {
	return gh.Issue{
		Number: 479,
		Title:  "Code: 0.5.0 docs-domain validation slice",
		Body:   "Implement per docs/specs/0459-domain-profiles.md.",
	}, nil
}

func (g *docsDomainFakeGitHub) PRDiff(context.Context, int) (string, error) {
	return docsModifiedDiff("content/q4-letter.md") + docsModifiedDiff("disclosure/q4-risk-factors.md"), nil
}

func (g *docsDomainFakeGitHub) PRDiffNameOnly(context.Context, int) ([]string, error) {
	return []string{"content/q4-letter.md", "disclosure/q4-risk-factors.md"}, nil
}

type docsDomainFakeAgent struct {
	invocation agent.Invocation
	summary    string
}

func (a *docsDomainFakeAgent) Run(_ context.Context, invocation agent.Invocation) (agent.Result, error) {
	a.invocation = invocation
	if err := os.WriteFile(invocation.LogPath, []byte("verifier log\n"), 0o644); err != nil {
		return agent.Result{ExitCode: -1}, err
	}
	return agent.Result{
		ExitCode:   0,
		Summary:    a.summary,
		Model:      "gpt-5",
		Effort:     "high",
		StartedAt:  "2026-07-04T00:00:00Z",
		EndedAt:    "2026-07-04T00:00:02Z",
		DurationMS: 2000,
		Usage: attestation.Usage{
			InputTokens:  int64Ptr(100),
			OutputTokens: int64Ptr(20),
			TotalTokens:  int64Ptr(120),
		},
	}, nil
}

type docsDomainFakeLock struct{}

func (l *docsDomainFakeLock) Release() error {
	return nil
}

type docsRiskReader struct {
	files  []string
	diff   string
	checks []gh.Check
}

func docsDomainRiskReader(file, diff string, checks ...gh.Check) docsRiskReader {
	return docsRiskReader{
		files:  []string{file},
		diff:   diff,
		checks: checks,
	}
}

func (r docsRiskReader) PRChecks(context.Context, int) ([]gh.Check, error) {
	return append([]gh.Check(nil), r.checks...), nil
}

func (r docsRiskReader) PRDiff(context.Context, int) (string, error) {
	return r.diff, nil
}

func (r docsRiskReader) PRDiffNameOnly(context.Context, int) ([]string, error) {
	return append([]string(nil), r.files...), nil
}

func copyDocsDomainFixture(t *testing.T) string {
	t.Helper()
	dst := t.TempDir()
	if err := copyDir("docs-domain", dst); err != nil {
		t.Fatalf("copy docs-domain fixture: %v", err)
	}
	return dst
}

func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dst, 0o755)
		}
		target := filepath.Join(dst, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}

func rewriteDeliveryConfig(t *testing.T, repo, old, new string) {
	t.Helper()
	path := filepath.Join(repo, ".delivery.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read delivery config: %v", err)
	}
	text := strings.Replace(string(data), old, new, 1)
	if text == string(data) {
		t.Fatalf("delivery config did not contain %q", old)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatalf("write delivery config: %v", err)
	}
}

func assertOrder(t *testing.T, text string, labels ...string) {
	t.Helper()
	previous := -1
	for _, label := range labels {
		index := strings.Index(text, label)
		if index < 0 {
			t.Fatalf("text missing %q:\n%s", label, text)
		}
		if index <= previous {
			t.Fatalf("label %q appeared out of order:\n%s", label, text)
		}
		previous = index
	}
}

func assertRedLine(t *testing.T, decision orchestration.RiskGateDecision, category, detail string) {
	t.Helper()
	if decision.Status != orchestration.RiskGateStatusNeedsHuman {
		t.Fatalf("risk gate status = %q, want needs-human: %#v", decision.Status, decision)
	}
	for _, line := range decision.RedLines {
		if line.Category == category && strings.Contains(line.Detail, detail) {
			return
		}
	}
	t.Fatalf("red lines = %#v, want category %q detail containing %q", decision.RedLines, category, detail)
}

func docsModifiedDiff(file string) string {
	return "diff --git a/" + file + " b/" + file + "\n" +
		"--- a/" + file + "\n" +
		"+++ b/" + file + "\n" +
		"@@ -1 +1 @@\n" +
		"+updated\n"
}

func docsAddedLineDiff(file, line string) string {
	return "diff --git a/" + file + " b/" + file + "\n" +
		"--- a/" + file + "\n" +
		"+++ b/" + file + "\n" +
		"@@ -0,0 +1 @@\n" +
		"+" + line + "\n"
}

func int64Ptr(value int64) *int64 {
	return &value
}
