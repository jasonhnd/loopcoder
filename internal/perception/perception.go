// Package perception turns observed delivery-loop failures into work items.
package perception

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	gh "github.com/jasonhnd/loopcoder/internal/vcs/github"
)

const (
	ReportVersion      = 1
	baseIssueLabel     = "delivery:unit"
	failureIssueKind   = "Code"
	failureMarkerLabel = "lc:d1"
)

type CIReader interface {
	RepoName(ctx context.Context) (string, error)
	ListOpenPRs(ctx context.Context) ([]gh.PullRequest, error)
	PRChecks(ctx context.Context, number int) ([]gh.Check, error)
}

type Options struct {
	RepoPath string
	CI       CIReader
	Writer   gh.IssueWriter
	Now      time.Time
}

type Report struct {
	Version          int            `json:"version"`
	Repo             string         `json:"repo"`
	RepoPath         string         `json:"repo_path"`
	GeneratedAt      string         `json:"generated_at"`
	Created          []CreatedEntry `json:"created"`
	SkippedHeld      []SkippedEntry `json:"skipped_held"`
	SkippedDuplicate []SkippedEntry `json:"skipped_duplicate"`
	Summary          Summary        `json:"summary"`
}

type Summary struct {
	CreatedCount          int `json:"created_count"`
	SkippedHeldCount      int `json:"skipped_held_count"`
	SkippedDuplicateCount int `json:"skipped_duplicate_count"`
	TotalFailures         int `json:"total_failures"`
}

type Failure struct {
	Signature    string `json:"signature"`
	Source       string `json:"source"`
	PRNumber     int    `json:"pr_number"`
	PRTitle      string `json:"pr_title"`
	PRURL        string `json:"pr_url,omitempty"`
	HeadRefName  string `json:"head_ref_name,omitempty"`
	CheckName    string `json:"check_name"`
	CheckState   string `json:"check_state,omitempty"`
	CheckBucket  string `json:"check_bucket,omitempty"`
	IssueNumbers []int  `json:"issue_numbers,omitempty"`
}

type CreatedEntry struct {
	Issue   int     `json:"issue"`
	Title   string  `json:"title"`
	Failure Failure `json:"failure"`
}

type SkippedEntry struct {
	Reason        string  `json:"reason"`
	TrackingIssue int     `json:"tracking_issue,omitempty"`
	Failure       Failure `json:"failure"`
}

type existingIssue struct {
	Number int
	Held   bool
}

func Run(ctx context.Context, opts Options) (Report, error) {
	if opts.CI == nil {
		return Report{}, errors.New("ci reader is required")
	}
	if opts.Writer == nil {
		return Report{}, errors.New("github issue writer is required")
	}
	if strings.TrimSpace(opts.RepoPath) == "" {
		return Report{}, errors.New("repo path is required")
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now().UTC()
	} else {
		opts.Now = opts.Now.UTC()
	}

	repoName, err := opts.CI.RepoName(ctx)
	if err != nil || strings.TrimSpace(repoName) == "" {
		repoName, err = opts.Writer.RepoName(ctx)
		if err != nil || strings.TrimSpace(repoName) == "" {
			repoName = opts.RepoPath
		}
	}

	issues, err := opts.Writer.ListIssues(ctx, "all")
	if err != nil {
		return Report{}, fmt.Errorf("list GitHub issues: %w", err)
	}
	tracked := trackedFailures(issues)
	heldIssues := heldIssueNumbers(issues)

	failures, heldReasons, err := discoverFailures(ctx, opts.CI, heldIssues)
	if err != nil {
		return Report{}, err
	}

	report := Report{
		Version:     ReportVersion,
		Repo:        strings.TrimSpace(repoName),
		RepoPath:    filepath.ToSlash(opts.RepoPath),
		GeneratedAt: opts.Now.Format(time.RFC3339),
	}

	for _, failure := range failures {
		if reason := heldReasons[failure.Signature]; reason != "" {
			report.SkippedHeld = append(report.SkippedHeld, SkippedEntry{
				Reason:  reason,
				Failure: failure,
			})
			continue
		}
		if existing, ok := tracked[failure.Signature]; ok {
			reason := fmt.Sprintf("already tracked by issue #%d", existing.Number)
			if existing.Held {
				report.SkippedHeld = append(report.SkippedHeld, SkippedEntry{
					Reason:        reason + " with held label",
					TrackingIssue: existing.Number,
					Failure:       failure,
				})
				continue
			}
			report.SkippedDuplicate = append(report.SkippedDuplicate, SkippedEntry{
				Reason:        reason,
				TrackingIssue: existing.Number,
				Failure:       failure,
			})
			continue
		}

		title := issueTitle(failure)
		issue, err := opts.Writer.CreateIssue(ctx, title, issueBody(failure), []string{baseIssueLabel})
		if err != nil {
			return Report{}, fmt.Errorf("create issue for %s: %w", failure.Signature, err)
		}
		report.Created = append(report.Created, CreatedEntry{
			Issue:   issue.Number,
			Title:   firstNonEmpty(issue.Title, title),
			Failure: failure,
		})
	}

	report.Summary = Summary{
		CreatedCount:          len(report.Created),
		SkippedHeldCount:      len(report.SkippedHeld),
		SkippedDuplicateCount: len(report.SkippedDuplicate),
		TotalFailures:         len(failures),
	}
	return normalizeReport(report), nil
}

func MarshalReportJSON(report Report) ([]byte, error) {
	report = normalizeReport(report)
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal discover JSON: %w", err)
	}
	return append(data, '\n'), nil
}

func RenderText(report Report) string {
	report = normalizeReport(report)
	var out bytes.Buffer

	fmt.Fprintln(&out, "DISCOVER")
	fmt.Fprintf(&out, "Repo: %s\n", report.Repo)
	fmt.Fprintf(&out, "Created: %d\n", report.Summary.CreatedCount)
	fmt.Fprintf(&out, "Skipped held: %d\n", report.Summary.SkippedHeldCount)
	fmt.Fprintf(&out, "Skipped duplicate: %d\n", report.Summary.SkippedDuplicateCount)
	fmt.Fprintf(&out, "Total failures: %d\n", report.Summary.TotalFailures)

	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Created")
	if len(report.Created) == 0 {
		fmt.Fprintln(&out, "- none")
	} else {
		for _, entry := range report.Created {
			fmt.Fprintf(&out, "- #%d %s\n", entry.Issue, entry.Title)
			renderFailureDetails(&out, entry.Failure)
		}
	}

	renderSkippedSection(&out, "Skipped held", report.SkippedHeld)
	renderSkippedSection(&out, "Skipped duplicate", report.SkippedDuplicate)

	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Next")
	if report.Summary.CreatedCount == 0 {
		fmt.Fprintln(&out, "- No new CI failure issues were filed.")
	} else {
		fmt.Fprintln(&out, "- Continue with ready-set, then dispatch ready issues.")
	}
	return out.String()
}

func renderSkippedSection(out *bytes.Buffer, title string, entries []SkippedEntry) {
	fmt.Fprintln(out)
	fmt.Fprintln(out, title)
	if len(entries) == 0 {
		fmt.Fprintln(out, "- none")
		return
	}
	for _, entry := range entries {
		fmt.Fprintf(out, "- %s\n", entry.Reason)
		if entry.TrackingIssue > 0 {
			fmt.Fprintf(out, "  tracking_issue: #%d\n", entry.TrackingIssue)
		}
		renderFailureDetails(out, entry.Failure)
	}
}

func renderFailureDetails(out *bytes.Buffer, failure Failure) {
	fmt.Fprintf(out, "  signature: %s\n", failure.Signature)
	fmt.Fprintf(out, "  failure: PR #%d %s (%s)\n", failure.PRNumber, failure.CheckName, checkStatus(failure.CheckBucket, failure.CheckState))
	if strings.TrimSpace(failure.PRURL) != "" {
		fmt.Fprintf(out, "  pr: %s\n", failure.PRURL)
	}
	if len(failure.IssueNumbers) > 0 {
		fmt.Fprintf(out, "  linked_issues: %s\n", formatIssueRefs(failure.IssueNumbers))
	}
}

func discoverFailures(ctx context.Context, reader CIReader, heldIssues map[int]string) ([]Failure, map[string]string, error) {
	prs, err := reader.ListOpenPRs(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("list open GitHub PRs: %w", err)
	}
	sort.Slice(prs, func(i, j int) bool {
		return prs[i].Number < prs[j].Number
	})

	var failures []Failure
	heldReasons := map[string]string{}
	for _, pr := range prs {
		checks, err := reader.PRChecks(ctx, pr.Number)
		if err != nil {
			return nil, nil, fmt.Errorf("read checks for PR #%d: %w", pr.Number, err)
		}
		sort.Slice(checks, func(i, j int) bool {
			if checks[i].Name != checks[j].Name {
				return checks[i].Name < checks[j].Name
			}
			if checks[i].Bucket != checks[j].Bucket {
				return checks[i].Bucket < checks[j].Bucket
			}
			return checks[i].State < checks[j].State
		})

		linkedIssues := linkedIssueNumbers(pr)
		heldReason := prHeldReason(pr, linkedIssues, heldIssues)
		for _, check := range checks {
			if !checkFailed(check) {
				continue
			}
			failure := buildFailure(pr, check, linkedIssues)
			failures = append(failures, failure)
			if heldReason != "" {
				heldReasons[failure.Signature] = heldReason
			}
		}
	}
	return failures, heldReasons, nil
}

func buildFailure(pr gh.PullRequest, check gh.Check, issueNumbers []int) Failure {
	name := firstNonEmpty(strings.TrimSpace(check.Name), "unnamed check")
	failure := Failure{
		Source:       "github-pr-check",
		PRNumber:     pr.Number,
		PRTitle:      strings.TrimSpace(pr.Title),
		PRURL:        strings.TrimSpace(pr.URL),
		HeadRefName:  strings.TrimSpace(pr.HeadRefName),
		CheckName:    name,
		CheckState:   strings.TrimSpace(check.State),
		CheckBucket:  strings.TrimSpace(check.Bucket),
		IssueNumbers: append([]int(nil), issueNumbers...),
	}
	failure.Signature = failureSignature(failure)
	return failure
}

func failureSignature(failure Failure) string {
	key := fmt.Sprintf("v1|%s|pr:%d|check:%s",
		strings.ToLower(strings.TrimSpace(failure.Source)),
		failure.PRNumber,
		strings.ToLower(strings.TrimSpace(failure.CheckName)),
	)
	sum := sha1.Sum([]byte(key))
	return hex.EncodeToString(sum[:])[:16]
}

func issueTitle(failure Failure) string {
	return fmt.Sprintf("%s: Fix CI failure: %s on PR #%d", failureIssueKind, failure.CheckName, failure.PRNumber)
}

func issueBody(failure Failure) string {
	var out bytes.Buffer
	fmt.Fprintf(&out, "<!-- %s=%s -->\n", failureMarkerLabel, failure.Signature)
	fmt.Fprintln(&out, "Spec: `docs/specs/0161-autonomous-delivery-loop.md`")
	fmt.Fprintln(&out, "Kind: code")
	fmt.Fprintln(&out, "Source: D1 discover")
	fmt.Fprintf(&out, "Failure signature: `%s`\n", failure.Signature)
	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "## CI Failure")
	fmt.Fprintf(&out, "- PR: #%d", failure.PRNumber)
	if strings.TrimSpace(failure.PRTitle) != "" {
		fmt.Fprintf(&out, " %s", failure.PRTitle)
	}
	fmt.Fprintln(&out)
	if strings.TrimSpace(failure.PRURL) != "" {
		fmt.Fprintf(&out, "- URL: %s\n", failure.PRURL)
	}
	if strings.TrimSpace(failure.HeadRefName) != "" {
		fmt.Fprintf(&out, "- Head branch: `%s`\n", failure.HeadRefName)
	}
	fmt.Fprintf(&out, "- Check: `%s`\n", failure.CheckName)
	fmt.Fprintf(&out, "- Status: `%s`\n", checkStatus(failure.CheckBucket, failure.CheckState))
	if len(failure.IssueNumbers) > 0 {
		fmt.Fprintf(&out, "- Linked issues: %s\n", formatIssueRefs(failure.IssueNumbers))
	}
	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "## Acceptance Criteria")
	fmt.Fprintf(&out, "- Diagnose and fix the failing `%s` CI check for PR #%d.\n", failure.CheckName, failure.PRNumber)
	fmt.Fprintln(&out, "- Re-run the relevant local checks and confirm the hosted CI check is green.")
	return out.String()
}

func trackedFailures(issues []gh.Issue) map[string]existingIssue {
	out := map[string]existingIssue{}
	for _, issue := range issues {
		signature := markerFromText(issue.Body)
		if signature == "" {
			continue
		}
		existing := existingIssue{
			Number: issue.Number,
			Held:   labelHeld(issue.Labels),
		}
		if prior, ok := out[signature]; ok && !strings.EqualFold(issue.State, "OPEN") && prior.Number > 0 {
			continue
		}
		out[signature] = existing
	}
	return out
}

func heldIssueNumbers(issues []gh.Issue) map[int]string {
	out := map[int]string{}
	for _, issue := range issues {
		if issue.Number <= 0 || !labelHeld(issue.Labels) {
			continue
		}
		out[issue.Number] = fmt.Sprintf("linked issue #%d has held label", issue.Number)
	}
	return out
}

func markerFromText(text string) string {
	re := regexp.MustCompile(`<!--\s*lc:d1=([A-Za-z0-9._:-]+)\s*-->`)
	matches := re.FindStringSubmatch(text)
	if len(matches) != 2 {
		return ""
	}
	return strings.TrimSpace(matches[1])
}

func prHeldReason(pr gh.PullRequest, issueNumbers []int, heldIssues map[int]string) string {
	if labelHeld(pr.Labels) {
		return fmt.Sprintf("PR #%d has held label", pr.Number)
	}
	if pr.IsDraft {
		return fmt.Sprintf("PR #%d is draft/paused", pr.Number)
	}
	for _, number := range issueNumbers {
		if reason := heldIssues[number]; reason != "" {
			return reason
		}
	}
	return ""
}

func labelHeld(labels []gh.Label) bool {
	for _, label := range labels {
		name := strings.ToLower(strings.TrimSpace(label.Name))
		switch name {
		case "held", "hold", "on-hold", "paused", "parked", "status:held", "status:paused", "status:on-hold":
			return true
		}
	}
	return false
}

func checkFailed(check gh.Check) bool {
	bucket := strings.ToLower(strings.TrimSpace(check.Bucket))
	state := strings.ToLower(strings.TrimSpace(check.State))
	switch bucket {
	case "fail", "cancel", "cancelled", "timed_out", "timed-out":
		return true
	}
	switch state {
	case "failure", "failed", "error", "cancelled", "canceled", "timed_out", "timed-out", "action_required":
		return true
	default:
		return false
	}
}

func checkStatus(bucket, state string) string {
	bucket = strings.TrimSpace(bucket)
	state = strings.TrimSpace(state)
	if bucket != "" && state != "" {
		return bucket + "/" + state
	}
	if bucket != "" {
		return bucket
	}
	if state != "" {
		return state
	}
	return "unknown"
}

func linkedIssueNumbers(pr gh.PullRequest) []int {
	seen := map[int]bool{}
	for _, issue := range pr.ClosingIssuesReferences {
		if issue.Number > 0 {
			seen[issue.Number] = true
		}
	}
	if number := issueNumberFromText(pr.HeadRefName); number > 0 {
		seen[number] = true
	}
	if number := issueNumberFromText(pr.Title); number > 0 {
		seen[number] = true
	}
	out := make([]int, 0, len(seen))
	for number := range seen {
		out = append(out, number)
	}
	sort.Ints(out)
	return out
}

var (
	branchIssuePattern = regexp.MustCompile(`(?i)(?:^|[/_-])issue[/_-]?(\d+)(?:\D|$)`)
	hashIssuePattern   = regexp.MustCompile(`#(\d+)`)
)

func issueNumberFromText(text string) int {
	if strings.TrimSpace(text) == "" {
		return 0
	}
	if matches := branchIssuePattern.FindStringSubmatch(text); len(matches) == 2 {
		number, _ := strconv.Atoi(matches[1])
		return number
	}
	if matches := hashIssuePattern.FindStringSubmatch(text); len(matches) == 2 {
		number, _ := strconv.Atoi(matches[1])
		return number
	}
	return 0
}

func normalizeReport(report Report) Report {
	if report.Version == 0 {
		report.Version = ReportVersion
	}
	sort.Slice(report.Created, func(i, j int) bool {
		return report.Created[i].Failure.Signature < report.Created[j].Failure.Signature
	})
	sort.Slice(report.SkippedHeld, func(i, j int) bool {
		return report.SkippedHeld[i].Failure.Signature < report.SkippedHeld[j].Failure.Signature
	})
	sort.Slice(report.SkippedDuplicate, func(i, j int) bool {
		return report.SkippedDuplicate[i].Failure.Signature < report.SkippedDuplicate[j].Failure.Signature
	})
	report.Summary = Summary{
		CreatedCount:          len(report.Created),
		SkippedHeldCount:      len(report.SkippedHeld),
		SkippedDuplicateCount: len(report.SkippedDuplicate),
		TotalFailures:         len(report.Created) + len(report.SkippedHeld) + len(report.SkippedDuplicate),
	}
	return report
}

func formatIssueRefs(numbers []int) string {
	parts := make([]string, 0, len(numbers))
	for _, number := range numbers {
		parts = append(parts, fmt.Sprintf("#%d", number))
	}
	return strings.Join(parts, ", ")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
