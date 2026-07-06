// Package audit runs loopcoder's built-in repository security audit.
package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/attestation"
)

const (
	SchemaVersion = 1

	LayerSAST = "sast"
	LayerLLM  = "llm"

	VerdictClean      = "clean"
	VerdictFindings   = "findings"
	VerdictNeedsHuman = "needs-human"

	SeverityCritical = "critical"
	SeverityHigh     = "high"
	SeverityMedium   = "medium"
	SeverityLow      = "low"
	SeverityInfo     = "info"

	FindingTierSignature = "signature"
	FindingTierEntropy   = "entropy"

	FindingGateGate    = "gate"
	FindingGateWarning = "warning"
)

type Options struct {
	RepoPath          string
	Layers            []string
	ThresholdOverride string
	BaseBranch        string
	ConfigFromBase    bool
	Provider          string
	Model             string
	Effort            string
	Timeout           time.Duration
	Stderr            io.Writer
}

type Result struct {
	SchemaVersion   int                            `json:"schema_version"`
	Repo            string                         `json:"repo"`
	Layers          []string                       `json:"layers"`
	Threshold       string                         `json:"threshold"`
	Verdict         string                         `json:"verdict"`
	Summary         ResultSummary                  `json:"summary"`
	Findings        []Finding                      `json:"findings"`
	ToolResults     []ToolResult                   `json:"tool_results"`
	NeedsHuman      []NeedsHuman                   `json:"needs_human"`
	BaselineNotices []BaselineNotice               `json:"baseline_notices,omitempty"`
	Attestation     *attestation.AttestationRecord `json:"-"`
	RuntimeFailures []string                       `json:"runtime_failures,omitempty"`
}

type ResultSummary struct {
	GateFindings   int `json:"gate_findings"`
	Warnings       int `json:"warnings"`
	WaivedFindings int `json:"waived_findings"`
	NeedsHuman     int `json:"needs_human"`
}

type Finding struct {
	ID          string `json:"id"`
	Layer       string `json:"layer"`
	Tool        string `json:"tool,omitempty"`
	Severity    string `json:"severity"`
	File        string `json:"file"`
	Line        int    `json:"line,omitempty"`
	Column      int    `json:"column,omitempty"`
	Rule        string `json:"rule"`
	Category    string `json:"category"`
	Message     string `json:"message"`
	Evidence    string `json:"evidence"`
	Tier        string `json:"tier,omitempty"`
	Gate        string `json:"gate,omitempty"`
	Fingerprint string `json:"fingerprint"`
	Waived      bool   `json:"waived"`
	WaiverID    string `json:"waiver_id,omitempty"`
}

type ToolResult struct {
	ID              string   `json:"id"`
	Argv            []string `json:"argv"`
	Parser          string   `json:"parser"`
	DurationMS      int64    `json:"duration_ms"`
	ExitCode        int      `json:"exit_code"`
	OutputTruncated bool     `json:"output_truncated"`
	ParseStatus     string   `json:"parse_status"`
	Error           string   `json:"error,omitempty"`
}

type NeedsHuman struct {
	Layer  string `json:"layer"`
	Reason string `json:"reason"`
}

type BaselineNotice struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Reason string `json:"reason"`
}

type SASTCommand struct {
	ID      string
	Argv    []string
	Parser  string
	Timeout time.Duration
}

type NativeConfig struct {
	Secrets         bool
	FilePermissions bool
	Include         []string
	Exclude         []string
}

type Plan struct {
	Threshold string
	Layers    []string
	Commands  []SASTCommand
	Native    NativeConfig
	Review    ReviewConfig
	Baseline  BaselineConfig
}

type ReviewConfig struct {
	RubricPath string
}

type BaselineConfig struct {
	Path string
}

type CommandInvocation struct {
	ID         string
	Argv       []string
	Parser     string
	WorkingDir string
	Timeout    time.Duration
	OutputCap  int64
}

type CommandRunResult struct {
	ExitCode        int
	Stdout          string
	Stderr          string
	Duration        time.Duration
	TimedOut        bool
	OutputTruncated bool
	Err             error
}

type CommandRunner interface {
	RunCommand(ctx context.Context, invocation CommandInvocation) CommandRunResult
}

func NewResult(repo string, layers []string, threshold string) Result {
	return Result{
		SchemaVersion: SchemaVersion,
		Repo:          repo,
		Layers:        append([]string(nil), layers...),
		Threshold:     threshold,
		Verdict:       VerdictClean,
		Findings:      []Finding{},
		ToolResults:   []ToolResult{},
		NeedsHuman:    []NeedsHuman{},
	}
}

func Finalize(result Result) Result {
	if result.SchemaVersion == 0 {
		result.SchemaVersion = SchemaVersion
	}
	if result.Findings == nil {
		result.Findings = []Finding{}
	}
	if result.ToolResults == nil {
		result.ToolResults = []ToolResult{}
	}
	if result.NeedsHuman == nil {
		result.NeedsHuman = []NeedsHuman{}
	}
	for index := range result.Findings {
		result.Findings[index].Severity = NormalizeSeverity(result.Findings[index].Severity)
		result.Findings[index].Layer = normalizeLayer(result.Findings[index].Layer)
		result.Findings[index].File = normalizeRepoPath(result.Findings[index].File)
		result.Findings[index].Tier = normalizeFindingMetadata(result.Findings[index].Tier)
		result.Findings[index].Gate = normalizeFindingMetadata(result.Findings[index].Gate)
		result.Findings[index].Gate = effectiveFindingGate(result.Findings[index], result.Threshold)
		if strings.TrimSpace(result.Findings[index].Fingerprint) == "" {
			result.Findings[index].Fingerprint = FindingFingerprint(result.Findings[index])
		}
		if strings.TrimSpace(result.Findings[index].ID) == "" {
			result.Findings[index].ID = findingID(result.Findings[index])
		}
	}
	SortFindings(result.Findings)
	result.Summary = summarizeResult(result)
	result.Verdict = VerdictFor(result)
	return result
}

func VerdictFor(result Result) string {
	if len(result.NeedsHuman) > 0 {
		return VerdictNeedsHuman
	}
	threshold := NormalizeSeverity(result.Threshold)
	if threshold == "" {
		threshold = SeverityMedium
	}
	for _, finding := range result.Findings {
		if finding.Waived {
			continue
		}
		if findingFailsGate(finding, threshold) {
			return VerdictFindings
		}
	}
	return VerdictClean
}

func summarizeResult(result Result) ResultSummary {
	threshold := NormalizeSeverity(result.Threshold)
	if threshold == "" {
		threshold = SeverityMedium
	}
	summary := ResultSummary{NeedsHuman: len(result.NeedsHuman)}
	for _, finding := range result.Findings {
		if finding.Waived {
			summary.WaivedFindings++
			continue
		}
		if findingFailsGate(finding, threshold) {
			summary.GateFindings++
			continue
		}
		summary.Warnings++
	}
	return summary
}

func findingFailsGate(finding Finding, threshold string) bool {
	return effectiveFindingGate(finding, threshold) == FindingGateGate
}

func effectiveFindingGate(finding Finding, threshold string) string {
	switch normalizeFindingMetadata(finding.Tier) {
	case FindingTierEntropy:
		return FindingGateWarning
	case FindingTierSignature:
		return FindingGateGate
	}
	threshold = NormalizeSeverity(threshold)
	if threshold == "" {
		threshold = SeverityMedium
	}
	if SeverityAtOrAbove(finding.Severity, threshold) {
		return FindingGateGate
	}
	return FindingGateWarning
}

func ExitCode(result Result) int {
	if len(result.RuntimeFailures) > 0 {
		return 3
	}
	switch result.Verdict {
	case VerdictClean:
		return 0
	case VerdictFindings:
		return 1
	case VerdictNeedsHuman:
		return 2
	default:
		return 3
	}
}

func SortFindings(findings []Finding) {
	sort.SliceStable(findings, func(i, j int) bool {
		left := findings[i]
		right := findings[j]
		if severityRank(left.Severity) != severityRank(right.Severity) {
			return severityRank(left.Severity) < severityRank(right.Severity)
		}
		if left.File != right.File {
			return left.File < right.File
		}
		if left.Line != right.Line {
			return left.Line < right.Line
		}
		if left.Rule != right.Rule {
			return left.Rule < right.Rule
		}
		return left.Fingerprint < right.Fingerprint
	})
}

func SeverityAtOrAbove(severity, threshold string) bool {
	return severityRank(severity) <= severityRank(threshold)
}

func NormalizeSeverity(severity string) string {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case SeverityCritical:
		return SeverityCritical
	case SeverityHigh, "error":
		return SeverityHigh
	case SeverityMedium:
		return SeverityMedium
	case SeverityLow, "warning":
		return SeverityLow
	case SeverityInfo, "note":
		return SeverityInfo
	default:
		return ""
	}
}

func ValidSeverity(severity string) bool {
	return NormalizeSeverity(severity) != ""
}

func severityRank(severity string) int {
	switch NormalizeSeverity(severity) {
	case SeverityCritical:
		return 0
	case SeverityHigh:
		return 1
	case SeverityMedium:
		return 2
	case SeverityLow:
		return 3
	case SeverityInfo:
		return 4
	default:
		return 5
	}
}

func FindingFingerprint(finding Finding) string {
	material := strings.Join([]string{
		normalizeLayer(finding.Layer),
		strings.TrimSpace(finding.Tool),
		NormalizeSeverity(finding.Severity),
		normalizeRepoPath(finding.File),
		fmt.Sprintf("%d", finding.Line),
		fmt.Sprintf("%d", finding.Column),
		strings.TrimSpace(finding.Rule),
		strings.TrimSpace(finding.Category),
		normalizeEvidence(finding.Evidence),
	}, "\x00")
	sum := sha256.Sum256([]byte(material))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func findingID(finding Finding) string {
	parts := []string{firstNonEmpty(finding.Tool, "audit"), finding.Rule}
	if finding.File != "" {
		parts = append(parts, finding.File)
	}
	if finding.Line > 0 {
		parts = append(parts, fmt.Sprintf("%d", finding.Line))
	}
	fingerprint := strings.TrimPrefix(finding.Fingerprint, "sha256:")
	if len(fingerprint) > 12 {
		fingerprint = fingerprint[:12]
	}
	parts = append(parts, fingerprint)
	return strings.Join(parts, ":")
}

func normalizeLayer(layer string) string {
	switch strings.ToLower(strings.TrimSpace(layer)) {
	case LayerLLM:
		return LayerLLM
	default:
		return LayerSAST
	}
}

func normalizeFindingMetadata(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func normalizeEvidence(evidence string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(evidence)), " ")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
