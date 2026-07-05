package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jasonhnd/loopcoder/internal/agent"
	"github.com/jasonhnd/loopcoder/internal/attestation"
	"github.com/jasonhnd/loopcoder/internal/config"
	lcdefaults "github.com/jasonhnd/loopcoder/internal/defaults"
	"github.com/jasonhnd/loopcoder/internal/mcp"
)

const (
	auditReviewTool              = "llm-review"
	auditReviewEvidenceMaxBytes  = 2048
	auditReviewMessageMaxBytes   = 1024
	auditReviewFileListMaxBytes  = lcdefaults.ReviewPacketChangedFilesBudgetBytes
	auditReviewContentMaxBytes   = lcdefaults.ReviewPacketDiffBudgetBytes
	auditReviewPerFileMaxBytes   = lcdefaults.RenderedArtifactFileBudgetBytes
	auditReviewTotalPromptBudget = lcdefaults.ReviewPacketTotalPromptBudgetBytes
)

const AuditReviewJSONSchema = `{"type":"object","additionalProperties":false,"required":["findings","evidence"],"properties":{"findings":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["id","layer","severity","file","rule","category","message","evidence"],"properties":{"id":{"type":"string"},"layer":{"type":"string","enum":["llm"]},"tool":{"type":"string"},"severity":{"type":"string","enum":["critical","high","medium","low","info"]},"file":{"type":"string"},"line":{"type":"integer","minimum":1},"column":{"type":"integer","minimum":1},"rule":{"type":"string"},"category":{"type":"string"},"message":{"type":"string"},"evidence":{"type":"string"},"fingerprint":{"type":"string"},"waived":{"type":"boolean"},"waiver_id":{"type":"string"}}}},"evidence":{"type":"string"}}}`

func runLLMLayer(ctx context.Context, repoPath string, cfg config.Config, plan Plan, opts Options, deps Deps, result *Result) {
	provider, model, effort, timeout := reviewProviderOptions(cfg, opts)
	if strings.TrimSpace(provider) == "" {
		addNeedsHuman(result, LayerLLM, "verifier provider is not configured")
		return
	}
	runner, err := deps.AgentLookup(provider)
	if err != nil {
		addNeedsHuman(result, LayerLLM, fmt.Sprintf("resolve verifier provider %q: %v", provider, err))
		return
	}
	if runner == nil {
		addNeedsHuman(result, LayerLLM, fmt.Sprintf("provider %q resolved to nil runner", provider))
		return
	}

	packet, err := buildAuditReviewPacket(ctx, repoPath, plan.Review)
	if err != nil {
		addNeedsHuman(result, LayerLLM, err.Error())
		return
	}
	prompt := buildAuditReviewPrompt(packet)
	mcpServers, err := mcp.ServersForInvocation(cfg.MCP, mcp.RoleVerifier, true)
	if err != nil {
		addNeedsHuman(result, LayerLLM, fmt.Sprintf("select read-only verifier MCP servers: %v", err))
		return
	}
	logPath, err := prepareAuditReviewLogPath(repoPath)
	if err != nil {
		addNeedsHuman(result, LayerLLM, fmt.Sprintf("prepare audit review log path: %v", err))
		return
	}
	stallTimeout := config.DurationSeconds(cfg.Resilience.Verifier.StallTimeoutSeconds, lcdefaults.VerifierStallTimeout)
	agentResult, agentErr := runner.Run(ctx, agent.Invocation{
		WorktreePath: repoPath,
		Prompt:       prompt,
		Model:        model,
		Effort:       effort,
		ReadOnly:     true,
		OutputSchema: AuditReviewJSONSchema,
		LogPath:      logPath,
		Stderr:       opts.Stderr,
		HardCap:      timeout,
		StallTimeout: stallTimeout,
		RunID:        "audit-llm",
		Role:         "verifier",
		MCPServers:   mcpServers,
	})
	if agentResult.Hung {
		record := auditReviewAttestation(provider, agentResult)
		emitAuditReviewHeader(opts.Stderr, record)
		addNeedsHuman(result, LayerLLM, auditReviewHungReason(provider, logPath, timeout, agentResult.HungReason))
		attachAuditReviewAttestation(result, record)
		return
	}
	record := auditReviewAttestation(provider, agentResult)
	emitAuditReviewHeader(opts.Stderr, record)
	if agentErr != nil {
		addNeedsHuman(result, LayerLLM, fmt.Sprintf("%s audit review failed: %v; see %s", provider, agentErr, logPath))
		attachAuditReviewAttestation(result, record)
		return
	}
	if agentResult.ExitCode != 0 {
		addNeedsHuman(result, LayerLLM, fmt.Sprintf("%s audit review exited with code %d; see %s", provider, agentResult.ExitCode, logPath))
		attachAuditReviewAttestation(result, record)
		return
	}

	findings, err := parseAuditReviewFindings(agentResult.Summary)
	if err != nil {
		addNeedsHuman(result, LayerLLM, fmt.Sprintf("structured audit review parse failed: %v", err))
		attachAuditReviewAttestation(result, record)
		return
	}
	result.Findings = append(result.Findings, findings...)
	attachAuditReviewAttestation(result, record)
}

type auditReviewPacket struct {
	RepoPath       string
	GitMetadata    string
	ThreatModel    string
	RubricPath     string
	RubricContent  string
	FileList       string
	FileContent    string
	Truncated      bool
	TruncatedNotes []string
}

func buildAuditReviewPacket(ctx context.Context, repoPath string, review ReviewConfig) (auditReviewPacket, error) {
	packet := auditReviewPacket{
		RepoPath:    repoPath,
		GitMetadata: boundedText(gitMetadata(ctx, repoPath), 4096),
		ThreatModel: builtInAuditThreatModel(),
	}
	if strings.TrimSpace(review.RubricPath) != "" {
		path, content, truncated, err := readAuditReviewRubric(repoPath, review.RubricPath)
		if err != nil {
			return auditReviewPacket{}, err
		}
		packet.RubricPath = path
		packet.RubricContent = content
		if truncated {
			packet.Truncated = true
			packet.TruncatedNotes = append(packet.TruncatedNotes, fmt.Sprintf("%s truncated to %d bytes", path, lcdefaults.ReviewPacketRubricBudgetBytes))
		}
	}
	files := listAuditReviewFiles(ctx, repoPath)
	packet.FileList = boundedText(strings.Join(files, "\n"), auditReviewFileListMaxBytes)
	if len(packet.FileList) < len(strings.Join(files, "\n")) {
		packet.Truncated = true
		packet.TruncatedNotes = append(packet.TruncatedNotes, "repository file list truncated")
	}
	content, notes := readAuditReviewFileContent(repoPath, files)
	packet.FileContent = content
	if len(notes) > 0 {
		packet.Truncated = true
		packet.TruncatedNotes = append(packet.TruncatedNotes, notes...)
	}
	return packet, nil
}

func buildAuditReviewPrompt(packet auditReviewPacket) string {
	prompt := fmt.Sprintf(`You are loopcoder's Layer 2 LLM security-review lens.

Review adversarially, but do not modify files, commit, push, create comments, write PR or issue bodies, or run shell commands. Treat this as a read-only verifier invocation.

Return only JSON matching this schema:

%s

# Built-in threat model and rubric
%s

# Configured rubric
path: %s
%s

# Bounded review packet
repo: %s

## Git metadata
%s

## Repository file list
%s

## Bounded file excerpts
%s

## Packet notes
%s

# Review instructions
- Produce audit findings in the same schema as deterministic findings, with layer "llm".
- Use severity critical, high, medium, low, or info.
- Use category values that describe the security class, such as supply-chain-integrity, shared-host-disclosure, path-confinement, untrusted-worktree-execution, verifier-boundary, failure-reporting, local-only-attestation, bounded-io, or config-compatibility.
- Return an empty findings array only when the bounded packet and rubric are sufficient and no finding is present.
- Return findings for concrete security risks; do not report speculative concerns without packet evidence.
- If the packet is too truncated to decide safely, return one medium finding with rule "llm:insufficient-evidence" and category "bounded-io".
`, AuditReviewJSONSchema, packet.ThreatModel, firstNonEmpty(packet.RubricPath, "(none)"), firstNonEmpty(packet.RubricContent, "(no configured rubric)"), packet.RepoPath, firstNonEmpty(packet.GitMetadata, "(unavailable)"), firstNonEmpty(packet.FileList, "(no files discovered)"), firstNonEmpty(packet.FileContent, "(no file excerpts selected)"), firstNonEmpty(strings.Join(packet.TruncatedNotes, "\n"), "(none)"))
	if len(prompt) <= auditReviewTotalPromptBudget {
		return prompt
	}
	overflow := len(prompt) - auditReviewTotalPromptBudget
	if overflow >= len(packet.FileContent) {
		packet.FileContent = "(file excerpts omitted because packet exceeded total prompt budget)"
	} else {
		packet.FileContent = boundedText(packet.FileContent, len(packet.FileContent)-overflow)
	}
	packet.Truncated = true
	packet.TruncatedNotes = append(packet.TruncatedNotes, "total audit review prompt truncated")
	return fmt.Sprintf(`You are loopcoder's Layer 2 LLM security-review lens.

Review adversarially, read-only, and return only JSON matching this schema:

%s

# Built-in threat model and rubric
%s

# Configured rubric
path: %s
%s

# Bounded review packet
repo: %s

## Git metadata
%s

## Repository file list
%s

## Bounded file excerpts
%s

## Packet notes
%s
`, AuditReviewJSONSchema, packet.ThreatModel, firstNonEmpty(packet.RubricPath, "(none)"), firstNonEmpty(packet.RubricContent, "(no configured rubric)"), packet.RepoPath, firstNonEmpty(packet.GitMetadata, "(unavailable)"), firstNonEmpty(packet.FileList, "(no files discovered)"), firstNonEmpty(packet.FileContent, "(no file excerpts selected)"), firstNonEmpty(strings.Join(packet.TruncatedNotes, "\n"), "(none)"))
}

func builtInAuditThreatModel() string {
	return strings.TrimSpace(`Operator-trusted inputs:
- the operator's own .delivery.yml;
- the repository in which the operator intentionally runs loopcoder;
- local tools the operator has installed and configured;
- command argv explicitly authored by the operator.

Untrusted inputs:
- PR and worktree file contents, including build scripts and generated files;
- downloaded release artifacts and checksum/signature files;
- remote MCP servers and data returned by them;
- local users on a shared host who can read permissive temp files, process arguments, logs, or state records.

Language-agnostic rubric categories:
- supply-chain integrity and release-artifact verification;
- shared-host disclosure through files, logs, prompts, argv, or local state;
- path traversal, symlink escape, and path-confinement gaps;
- execution of trusted tools inside untrusted worktrees;
- read-only verifier boundaries, including MCP server classification;
- honest failure reporting and exit-code separation;
- local-only attestation and relay obligations;
- bounded local file reads, output sizes, and scan scope;
- additive config compatibility and absent-config behavior.`)
}

func readAuditReviewRubric(repoPath, rawPath string) (string, string, bool, error) {
	clean, err := safeRepoRelativePath(rawPath)
	if err != nil {
		return "", "", false, fmt.Errorf("audit.review.rubric_path %q is invalid: %w", rawPath, err)
	}
	data, err := os.ReadFile(filepath.Join(repoPath, filepath.FromSlash(clean)))
	if err != nil {
		return clean, "", false, fmt.Errorf("read audit.review.rubric_path %s: %w", clean, err)
	}
	text := string(data)
	truncated := len(text) > lcdefaults.ReviewPacketRubricBudgetBytes
	return clean, boundedText(text, lcdefaults.ReviewPacketRubricBudgetBytes), truncated, nil
}

func safeRepoRelativePath(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("path is required")
	}
	if filepath.IsAbs(raw) {
		return "", errors.New("absolute paths are not allowed")
	}
	clean := filepath.Clean(filepath.FromSlash(raw))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("path must stay inside the repository")
	}
	return normalizeRepoPath(clean), nil
}

func listAuditReviewFiles(ctx context.Context, repoPath string) []string {
	if repoLooksLikeGit(repoPath) {
		if files, err := gitTrackedFiles(ctx, repoPath); err == nil {
			return filterAuditReviewFiles(files)
		}
	}
	files := []string{}
	_ = filepath.WalkDir(repoPath, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := entry.Name()
		if entry.IsDir() {
			switch name {
			case ".git", ".loopcoder", "vendor", "node_modules", "dist":
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(repoPath, path)
		if err != nil {
			return nil
		}
		files = append(files, normalizeRepoPath(rel))
		if len(files) >= lcdefaults.WorktreeLivenessMaxFiles {
			return filepath.SkipAll
		}
		return nil
	})
	sort.Strings(files)
	return filterAuditReviewFiles(files)
}

func repoLooksLikeGit(repoPath string) bool {
	if _, err := os.Stat(filepath.Join(repoPath, ".git")); err == nil {
		return true
	}
	return false
}

func gitTrackedFiles(ctx context.Context, repoPath string) ([]string, error) {
	runCtx, cancel := context.WithTimeout(ctx, lcdefaults.GitCommandHardCap)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "git", "-C", repoPath, "ls-files")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	lines := strings.Split(strings.ReplaceAll(stdout.String(), "\r\n", "\n"), "\n")
	files := make([]string, 0, len(lines))
	for _, line := range lines {
		line = normalizeRepoPath(line)
		if line != "" {
			files = append(files, line)
		}
	}
	sort.Strings(files)
	return files, nil
}

func filterAuditReviewFiles(files []string) []string {
	out := make([]string, 0, len(files))
	for _, file := range files {
		file = normalizeRepoPath(file)
		if file == "" || matchesAnyRepoGlob(file, defaultNativeExclude) {
			continue
		}
		out = append(out, file)
	}
	return out
}

func readAuditReviewFileContent(repoPath string, files []string) (string, []string) {
	var out strings.Builder
	remaining := auditReviewContentMaxBytes
	var notes []string
	for _, file := range files {
		if remaining <= 0 {
			notes = append(notes, "file excerpts truncated by total content budget")
			break
		}
		if !auditReviewShouldReadFile(file) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(repoPath, filepath.FromSlash(file)))
		if err != nil || len(data) == 0 || !utf8.Valid(data) {
			continue
		}
		text := string(data)
		if len(text) > auditReviewPerFileMaxBytes {
			text = boundedText(text, auditReviewPerFileMaxBytes)
			notes = append(notes, fmt.Sprintf("%s truncated to %d bytes", file, auditReviewPerFileMaxBytes))
		}
		section := fmt.Sprintf("\n### %s\n%s\n", file, text)
		if len(section) > remaining {
			section = boundedText(section, remaining)
			notes = append(notes, "file excerpts truncated by total content budget")
		}
		out.WriteString(section)
		remaining -= len(section)
	}
	return strings.TrimSpace(out.String()), notes
}

func auditReviewShouldReadFile(file string) bool {
	base := strings.ToLower(pathBase(file))
	switch base {
	case ".delivery.yml", "go.mod", "go.sum", "package.json", "pnpm-lock.yaml", "package-lock.json", "bun.lockb", "dockerfile", "makefile", "agendid.md", "agents.md", "skill.md", "gemini.md":
		return true
	}
	if strings.HasPrefix(normalizeRepoPath(file), ".github/workflows/") {
		return true
	}
	switch strings.ToLower(filepath.Ext(file)) {
	case ".go", ".js", ".jsx", ".ts", ".tsx", ".mjs", ".cjs", ".py", ".rb", ".rs", ".java", ".kt", ".cs", ".php", ".sh", ".bash", ".ps1", ".psm1", ".yml", ".yaml", ".json", ".toml", ".md":
		return true
	default:
		return false
	}
}

func gitMetadata(ctx context.Context, repoPath string) string {
	if !repoLooksLikeGit(repoPath) {
		return "git metadata unavailable: no .git directory or file"
	}
	parts := []string{}
	for _, args := range [][]string{
		{"rev-parse", "--show-toplevel"},
		{"rev-parse", "HEAD"},
		{"status", "--short"},
	} {
		out, err := runGitMetadata(ctx, repoPath, args...)
		if err != nil {
			parts = append(parts, fmt.Sprintf("git %s: unavailable: %v", strings.Join(args, " "), err))
			continue
		}
		parts = append(parts, fmt.Sprintf("$ git %s\n%s", strings.Join(args, " "), strings.TrimSpace(out)))
	}
	return strings.Join(parts, "\n\n")
}

func runGitMetadata(ctx context.Context, repoPath string, args ...string) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, lcdefaults.GitCommandHardCap)
	defer cancel()
	fullArgs := append([]string{"-C", repoPath}, args...)
	cmd := exec.CommandContext(runCtx, "git", fullArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func prepareAuditReviewLogPath(repoPath string) (string, error) {
	dir := filepath.Join(repoPath, ".loopcoder", "audit")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, fmt.Sprintf("llm-%d.log", time.Now().UTC().UnixNano())), nil
}

func parseAuditReviewFindings(raw string) ([]Finding, error) {
	decoder := json.NewDecoder(strings.NewReader(strings.TrimSpace(raw)))
	decoder.DisallowUnknownFields()
	var payload struct {
		Findings *[]Finding `json:"findings"`
		Evidence *string    `json:"evidence"`
	}
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("parse review JSON: %w", err)
	}
	var trailing struct{}
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("parse review JSON: trailing data after structured result")
	}
	if payload.Findings == nil {
		return nil, errors.New("missing findings")
	}
	if payload.Evidence == nil || strings.TrimSpace(*payload.Evidence) == "" {
		return nil, errors.New("missing evidence")
	}
	findings := append([]Finding(nil), (*payload.Findings)...)
	for i := range findings {
		if strings.TrimSpace(findings[i].ID) == "" {
			return nil, fmt.Errorf("finding %d missing id", i)
		}
		if normalizeLayer(findings[i].Layer) != LayerLLM {
			return nil, fmt.Errorf("finding %d layer must be llm", i)
		}
		if !ValidSeverity(findings[i].Severity) {
			return nil, fmt.Errorf("finding %d invalid severity %q", i, findings[i].Severity)
		}
		if strings.TrimSpace(findings[i].Rule) == "" {
			return nil, fmt.Errorf("finding %d missing rule", i)
		}
		if strings.TrimSpace(findings[i].Category) == "" {
			return nil, fmt.Errorf("finding %d missing category", i)
		}
		if strings.TrimSpace(findings[i].Message) == "" {
			return nil, fmt.Errorf("finding %d missing message", i)
		}
		if strings.TrimSpace(findings[i].Evidence) == "" {
			return nil, fmt.Errorf("finding %d missing evidence", i)
		}
		findings[i].Layer = LayerLLM
		findings[i].Tool = firstNonEmpty(findings[i].Tool, auditReviewTool)
		findings[i].Severity = NormalizeSeverity(findings[i].Severity)
		findings[i].File = normalizeRepoPath(findings[i].File)
		findings[i].Rule = strings.TrimSpace(findings[i].Rule)
		findings[i].Category = strings.TrimSpace(findings[i].Category)
		findings[i].Message = boundedText(strings.TrimSpace(findings[i].Message), auditReviewMessageMaxBytes)
		findings[i].Evidence = boundedText(strings.TrimSpace(findings[i].Evidence), auditReviewEvidenceMaxBytes)
	}
	return findings, nil
}

func auditReviewAttestation(provider string, result agent.Result) attestation.AttestationRecord {
	return attestation.AttestationRecord{
		Role:        attestation.RoleVerifier,
		Provider:    provider,
		Model:       result.Model,
		ModelSource: attestation.ModelSourceParsed,
		Effort:      result.Effort,
		Permission:  attestation.PermissionReadOnly,
		Action:      "audit LLM security review",
		ExitCode:    result.ExitCode,
		StartedAt:   result.StartedAt,
		EndedAt:     result.EndedAt,
		DurationMS:  result.DurationMS,
		Usage:       result.Usage,
		Verified:    true,
	}
}

func attachAuditReviewAttestation(result *Result, record attestation.AttestationRecord) {
	result.Attestation = &record
	if err := record.Validate(); err != nil {
		addNeedsHuman(result, LayerLLM, "incomplete verifier attestation: "+err.Error())
		return
	}
	if record.Role != attestation.RoleVerifier || record.Permission != attestation.PermissionReadOnly || !record.Verified {
		addNeedsHuman(result, LayerLLM, "invalid verifier attestation semantics")
	}
}

func emitAuditReviewHeader(w io.Writer, record attestation.AttestationRecord) {
	if w != nil {
		fmt.Fprintln(w, record.Header())
	}
}

func auditReviewHungReason(provider, logPath string, timeout time.Duration, reason string) string {
	switch reason {
	case agent.HungReasonDeadline:
		return fmt.Sprintf("%s audit review timed out after %s; see %s", provider, timeout, logPath)
	case agent.HungReasonStall:
		return fmt.Sprintf("%s audit review hung after verifier stall timeout; see %s", provider, logPath)
	default:
		return fmt.Sprintf("%s audit review hung (reason=%s); see %s", provider, firstNonEmpty(reason, "unknown"), logPath)
	}
}

func addNeedsHuman(result *Result, layer, reason string) {
	result.NeedsHuman = append(result.NeedsHuman, NeedsHuman{
		Layer:  layer,
		Reason: strings.TrimSpace(reason),
	})
}

func boundedText(text string, maxBytes int) string {
	text = strings.TrimSpace(text)
	if maxBytes <= 0 || len(text) <= maxBytes {
		return text
	}
	if maxBytes <= len("\n[TRUNCATED]") {
		return text[:maxBytes]
	}
	cut := maxBytes - len("\n[TRUNCATED]")
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return strings.TrimSpace(text[:cut]) + "\n[TRUNCATED]"
}
