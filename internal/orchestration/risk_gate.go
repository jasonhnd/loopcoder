package orchestration

import (
	"bufio"
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	gh "github.com/jasonhnd/loopcoder/internal/vcs/github"
)

const (
	RiskGateStatusClean      = "clean"
	RiskGateStatusNeedsHuman = "needs-human"

	PreProdHealthStatusGreen   = "green"
	PreProdHealthStatusRed     = "red"
	PreProdHealthStatusPending = "pending"
	PreProdHealthStatusUnknown = "unknown"

	RiskRedLineDestructive = "destructive-change"
	RiskRedLineBuild       = "build-not-green"
	RiskRedLineCore        = "loopcoder-core"
	RiskRedLineRaised      = "raised-risk"
)

const (
	massDeletionFileThreshold  = 25
	massDeletionRatioThreshold = 5
)

type RiskGateReader interface {
	PRChecks(ctx context.Context, number int) ([]gh.Check, error)
	PRDiff(ctx context.Context, number int) (string, error)
	PRDiffNameOnly(ctx context.Context, number int) ([]string, error)
}

type PreProdWriter interface {
	MergeToPreProd(ctx context.Context, prNumber int, preProdBranch string) (gh.PreProdMergeResult, error)
	RevertOnPreProd(ctx context.Context, prNumber int, preProdBranch, mergeSHA string) (gh.PreProdRevertResult, error)
}

type PreProdHealthReader interface {
	BranchChecks(ctx context.Context, branch string) (gh.BranchChecksResult, error)
}

type BranchHeadReader interface {
	BranchHeadSHA(ctx context.Context, branch string) (string, error)
}

type RiskGateFunc func(ctx context.Context, opts RiskGateOptions) (RiskGateDecision, error)

type RiskGateOptions struct {
	Reader             RiskGateReader
	PRNumber           int
	RequiredChecks     []string
	AdditionalRedLines []RiskRedLine
}

type RiskGateDecision struct {
	PRNumber       int           `json:"pr_number"`
	Status         string        `json:"status"`
	RequiredChecks []string      `json:"required_checks"`
	ChangedFiles   []string      `json:"changed_files"`
	Checks         []gh.Check    `json:"checks"`
	RedLines       []RiskRedLine `json:"red_lines"`
}

type RiskRedLine struct {
	Category string `json:"category"`
	Detail   string `json:"detail"`
}

func EvaluateRiskGate(ctx context.Context, opts RiskGateOptions) (RiskGateDecision, error) {
	if opts.Reader == nil {
		return RiskGateDecision{}, fmt.Errorf("risk gate reader is required")
	}
	if opts.PRNumber <= 0 {
		return RiskGateDecision{}, fmt.Errorf("pull request number is required")
	}

	requiredChecks := normalizeRequiredChecks(opts.RequiredChecks)
	changedFiles, filesErr := opts.Reader.PRDiffNameOnly(ctx, opts.PRNumber)
	diff, diffErr := opts.Reader.PRDiff(ctx, opts.PRNumber)
	checks, checksErr := opts.Reader.PRChecks(ctx, opts.PRNumber)

	decision := RiskGateDecision{
		PRNumber:       opts.PRNumber,
		Status:         RiskGateStatusClean,
		RequiredChecks: requiredChecks,
		ChangedFiles:   normalizeChangedFiles(changedFiles),
		Checks:         append([]gh.Check(nil), checks...),
		RedLines:       []RiskRedLine{},
	}

	if filesErr != nil {
		decision.RedLines = append(decision.RedLines, RiskRedLine{
			Category: RiskRedLineDestructive,
			Detail:   fmt.Sprintf("changed files unavailable: %v", filesErr),
		})
	}
	if diffErr != nil {
		decision.RedLines = append(decision.RedLines, RiskRedLine{
			Category: RiskRedLineDestructive,
			Detail:   fmt.Sprintf("PR diff unavailable: %v", diffErr),
		})
	} else {
		decision.RedLines = append(decision.RedLines, destructiveRedLines(diff)...)
	}
	decision.RedLines = append(decision.RedLines, buildRedLines(requiredChecks, checks, checksErr)...)
	decision.RedLines = append(decision.RedLines, corePathRedLines(decision.ChangedFiles)...)
	decision.RedLines = append(decision.RedLines, normalizeAdditionalRedLines(opts.AdditionalRedLines)...)

	if len(decision.RedLines) > 0 {
		decision.Status = RiskGateStatusNeedsHuman
	}
	return decision, nil
}

func normalizeRequiredChecks(checks []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(checks))
	for _, check := range checks {
		name := strings.TrimSpace(check)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func normalizeChangedFiles(files []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(files))
	for _, file := range files {
		normalized := normalizeRepoPath(file)
		if normalized == "" || seen[normalized] {
			continue
		}
		seen[normalized] = true
		out = append(out, normalized)
	}
	sort.Strings(out)
	return out
}

func destructiveRedLines(diff string) []RiskRedLine {
	stats := parseDiffStats(diff)
	var out []RiskRedLine
	if stats.DeletedFiles >= massDeletionFileThreshold ||
		(stats.DeletedFiles >= massDeletionRatioThreshold && stats.DeletedFiles*2 >= maxInt(1, stats.ChangedFiles)) {
		out = append(out, RiskRedLine{
			Category: RiskRedLineDestructive,
			Detail:   fmt.Sprintf("mass deletion: %d deleted files across %d changed files", stats.DeletedFiles, stats.ChangedFiles),
		})
	}
	if len(stats.DangerousCommands) > 0 {
		out = append(out, RiskRedLine{
			Category: RiskRedLineDestructive,
			Detail:   "destructive command added: " + strings.Join(stats.DangerousCommands, ", "),
		})
	}
	return out
}

type diffStats struct {
	ChangedFiles      int
	DeletedFiles      int
	DangerousCommands []string
}

func parseDiffStats(diff string) diffStats {
	var stats diffStats
	deletedInCurrentFile := false
	scanner := bufio.NewScanner(strings.NewReader(diff))
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "diff --git "):
			stats.ChangedFiles++
			deletedInCurrentFile = false
		case strings.HasPrefix(line, "deleted file mode "):
			if !deletedInCurrentFile {
				stats.DeletedFiles++
				deletedInCurrentFile = true
			}
		case strings.HasPrefix(line, "+++ /dev/null"):
			if !deletedInCurrentFile {
				stats.DeletedFiles++
				deletedInCurrentFile = true
			}
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
			if command := dangerousCommand(line[1:]); command != "" {
				stats.DangerousCommands = appendIfMissing(stats.DangerousCommands, command)
			}
		}
	}
	return stats
}

func dangerousCommand(line string) string {
	lower := strings.ToLower(strings.TrimSpace(line))
	dangerous := []string{
		"rm -rf .",
		"rm -rf /",
		"remove-item -recurse",
		"git reset --hard",
		"git push --force",
		"git push -f",
		"git filter-branch",
		"git update-ref -d",
	}
	for _, needle := range dangerous {
		if strings.Contains(lower, needle) {
			return needle
		}
	}
	return ""
}

func buildRedLines(required []string, checks []gh.Check, err error) []RiskRedLine {
	if len(required) == 0 {
		return []RiskRedLine{{
			Category: RiskRedLineBuild,
			Detail:   "no required CI checks configured for unattended pre-prod merge",
		}}
	}
	if err != nil {
		return []RiskRedLine{{
			Category: RiskRedLineBuild,
			Detail:   fmt.Sprintf("PR checks unavailable: %v", err),
		}}
	}

	byName := map[string]gh.Check{}
	for _, check := range checks {
		byName[strings.TrimSpace(check.Name)] = check
	}
	var blocked []string
	for _, requiredCheck := range required {
		check, ok := byName[requiredCheck]
		if !ok {
			blocked = append(blocked, requiredCheck+" missing")
			continue
		}
		if !checkPassed(check) {
			blocked = append(blocked, fmt.Sprintf("%s %s", requiredCheck, checkStatus(check)))
		}
	}
	if len(blocked) == 0 {
		return nil
	}
	return []RiskRedLine{{
		Category: RiskRedLineBuild,
		Detail:   "required checks not green: " + strings.Join(blocked, ", "),
	}}
}

func checkPassed(check gh.Check) bool {
	bucket := strings.ToLower(strings.TrimSpace(check.Bucket))
	state := strings.ToLower(strings.TrimSpace(check.State))
	// gh exposes both bucket and state, and different check sources can leave
	// either field empty or stale. Accept a passing value from either field for
	// this local green predicate; checks with no passing signal still raise risk.
	return bucket == "pass" || state == "success" || state == "passed"
}

func checkFailed(check gh.Check) bool {
	bucket := strings.ToLower(strings.TrimSpace(check.Bucket))
	state := strings.ToLower(strings.TrimSpace(check.State))
	switch bucket {
	case "fail", "failed", "cancel", "cancelled", "error", "timed_out":
		return true
	}
	switch state {
	case "failure", "failed", "error", "cancelled", "timed_out", "action_required":
		return true
	}
	return false
}

func checkStatus(check gh.Check) string {
	bucket := strings.TrimSpace(check.Bucket)
	if bucket != "" {
		return bucket
	}
	state := strings.TrimSpace(check.State)
	if state != "" {
		return state
	}
	return "unknown"
}

func corePathRedLines(changedFiles []string) []RiskRedLine {
	var matches []string
	for _, file := range changedFiles {
		if isLoopcoderCorePath(file) {
			matches = append(matches, file)
		}
	}
	if len(matches) == 0 {
		return nil
	}
	return []RiskRedLine{{
		Category: RiskRedLineCore,
		Detail:   "loopcoder core paths changed: " + strings.Join(matches, ", ") + "; human rebuild and tick restart required before changes take effect",
	}}
}

func isLoopcoderCorePath(file string) bool {
	file = normalizeRepoPath(file)
	if file == "" {
		return false
	}
	exact := map[string]bool{
		".delivery.yml": true,
		"AGENTS.md":     true,
		"GEMINI.md":     true,
		"SKILL.md":      true,
	}
	if exact[file] {
		return true
	}
	if isLoopcoderCoreOrchestrationPath(file) {
		return true
	}
	prefixes := []string{
		"cmd/loopcoder/",
		"hooks/",
		"internal/agent/",
		"internal/attestation/",
		"internal/compile/",
		"internal/config/",
		"internal/conductorhooks/",
		"internal/guardrails/",
		"internal/loopreview/",
		"internal/vcs/",
		"internal/verify/",
		"internal/worker/",
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(file, prefix) {
			return true
		}
	}
	return false
}

func isLoopcoderCoreOrchestrationPath(file string) bool {
	return path.Dir(file) == "internal/orchestration" && path.Ext(file) == ".go" && path.Base(file) != "doc.go"
}

func normalizeAdditionalRedLines(lines []RiskRedLine) []RiskRedLine {
	out := make([]RiskRedLine, 0, len(lines))
	for _, line := range lines {
		category := strings.TrimSpace(line.Category)
		detail := strings.TrimSpace(line.Detail)
		if category == "" && detail == "" {
			continue
		}
		if category == "" {
			category = RiskRedLineRaised
		}
		if detail == "" {
			detail = "additional risk raised"
		}
		out = append(out, RiskRedLine{Category: category, Detail: detail})
	}
	return out
}

func normalizeRepoPath(file string) string {
	file = strings.TrimSpace(filepathSlash(file))
	file = strings.TrimPrefix(file, "./")
	file = strings.TrimPrefix(file, "/")
	file = path.Clean(file)
	if file == "." {
		return ""
	}
	return file
}

func filepathSlash(file string) string {
	return strings.ReplaceAll(file, "\\", "/")
}

func appendIfMissing(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
