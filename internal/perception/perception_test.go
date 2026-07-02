package perception

import (
	"context"
	"strings"
	"testing"
	"time"

	gh "github.com/jasonhnd/loopcoder/internal/vcs/github"
)

func TestDiscoverCreatesIssueForCIFailure(t *testing.T) {
	ci := fakeCIReader{
		prs: []gh.PullRequest{{
			Number:      12,
			Title:       "Fix build",
			URL:         "https://github.com/owner/repo/pull/12",
			HeadRefName: "loop/issue-12",
		}},
		checks: map[int][]gh.Check{
			12: {{Name: "verify", Bucket: "fail", State: "FAILURE"}},
		},
	}
	writer := newFakeIssueWriter()

	report := runDiscoverTest(t, ci, writer)
	if len(report.Created) != 1 || report.Created[0].Issue != 1 {
		t.Fatalf("created = %#v, want one issue #1", report.Created)
	}
	if len(report.SkippedHeld) != 0 || len(report.SkippedDuplicate) != 0 {
		t.Fatalf("unexpected skips: held=%#v duplicate=%#v", report.SkippedHeld, report.SkippedDuplicate)
	}
	created := writer.issues[1]
	if created.Title != "Code: Fix CI failure: verify on PR #12" {
		t.Fatalf("created title = %q", created.Title)
	}
	for _, want := range []string{"<!-- lc:d1=", "Spec: `docs/specs/0161-autonomous-delivery-loop.md`", "- Check: `verify`"} {
		if !strings.Contains(created.Body, want) {
			t.Fatalf("created body missing %q:\n%s", want, created.Body)
		}
	}
	if !writer.issueHasLabel(1, "delivery:unit") {
		t.Fatalf("created issue missing delivery:unit label: %#v", created.Labels)
	}
}

func TestDiscoverSkipsHeldFailure(t *testing.T) {
	ci := fakeCIReader{
		prs: []gh.PullRequest{{
			Number: 15,
			Title:  "Paused PR",
			Labels: []gh.Label{{Name: "held"}},
		}},
		checks: map[int][]gh.Check{
			15: {{Name: "go", Bucket: "fail"}},
		},
	}
	writer := newFakeIssueWriter()

	report := runDiscoverTest(t, ci, writer)
	if len(report.Created) != 0 {
		t.Fatalf("created = %#v, want none for held PR", report.Created)
	}
	if len(report.SkippedHeld) != 1 {
		t.Fatalf("skipped held = %#v, want one", report.SkippedHeld)
	}
	if !strings.Contains(report.SkippedHeld[0].Reason, "held label") {
		t.Fatalf("held reason = %q, want held label", report.SkippedHeld[0].Reason)
	}
	if writer.nextNumber != 1 {
		t.Fatalf("next issue = %d, want no issue created", writer.nextNumber)
	}
}

func TestDiscoverRerunSkipsAlreadyTrackedFailure(t *testing.T) {
	ci := fakeCIReader{
		prs: []gh.PullRequest{{Number: 20, Title: "Broken check"}},
		checks: map[int][]gh.Check{
			20: {{Name: "verify", Bucket: "fail"}},
		},
	}
	writer := newFakeIssueWriter()

	first := runDiscoverTest(t, ci, writer)
	if len(first.Created) != 1 {
		t.Fatalf("first created = %#v, want one issue", first.Created)
	}
	second := runDiscoverTest(t, ci, writer)
	if len(second.Created) != 0 {
		t.Fatalf("second created = %#v, want no duplicate", second.Created)
	}
	if len(second.SkippedDuplicate) != 1 || second.SkippedDuplicate[0].TrackingIssue != 1 {
		t.Fatalf("second duplicates = %#v, want tracking issue #1", second.SkippedDuplicate)
	}
	if writer.nextNumber != 2 {
		t.Fatalf("next issue = %d, want only one issue ever created", writer.nextNumber)
	}
}

func TestDiscoverRefilesClosedTrackedFailure(t *testing.T) {
	ci := fakeCIReader{
		prs: []gh.PullRequest{{Number: 30, Title: "Regression"}},
		checks: map[int][]gh.Check{
			30: {{Name: "verify", Bucket: "fail"}},
		},
	}
	writer := newFakeIssueWriter()
	signature := failureSignature(Failure{
		Source:    "github-pr-check",
		PRNumber:  30,
		CheckName: "verify",
	})
	writer.issues[7] = gh.Issue{
		Number: 7,
		Body:   "<!-- lc:d1=" + signature + " -->",
		State:  "CLOSED",
	}
	writer.nextNumber = 8

	report := runDiscoverTest(t, ci, writer)
	if len(report.Created) != 1 || report.Created[0].Issue != 8 {
		t.Fatalf("created = %#v, want one new issue #8", report.Created)
	}
	if len(report.SkippedDuplicate) != 0 {
		t.Fatalf("skipped duplicate = %#v, want none for closed tracker", report.SkippedDuplicate)
	}
}

func TestDiscoverDedupsDuplicateCheckNamesWithinRun(t *testing.T) {
	ci := fakeCIReader{
		prs: []gh.PullRequest{{Number: 31, Title: "Duplicate check names"}},
		checks: map[int][]gh.Check{
			31: {
				{Name: "verify", Bucket: "fail", State: "failure"},
				{Name: "verify", Bucket: "fail", State: "error"},
			},
		},
	}
	writer := newFakeIssueWriter()

	report := runDiscoverTest(t, ci, writer)
	if len(report.Created) != 1 || report.Created[0].Issue != 1 {
		t.Fatalf("created = %#v, want one issue #1", report.Created)
	}
	if len(report.SkippedDuplicate) != 1 || report.SkippedDuplicate[0].TrackingIssue != 1 {
		t.Fatalf("skipped duplicate = %#v, want duplicate tracked by issue #1", report.SkippedDuplicate)
	}
	if writer.nextNumber != 2 {
		t.Fatalf("next issue = %d, want one issue created", writer.nextNumber)
	}
}

func TestCheckFailedExcludesCancelledAndSuperseded(t *testing.T) {
	tests := []struct {
		name  string
		check gh.Check
		want  bool
	}{
		{name: "failure bucket", check: gh.Check{Bucket: "fail"}, want: true},
		{name: "timed out bucket", check: gh.Check{Bucket: "timed_out"}, want: true},
		{name: "timed out state", check: gh.Check{State: "timed-out"}, want: true},
		{name: "action required state", check: gh.Check{State: "action_required"}, want: true},
		{name: "cancel bucket", check: gh.Check{Bucket: "cancel"}, want: false},
		{name: "cancel state", check: gh.Check{Bucket: "fail", State: "cancel"}, want: false},
		{name: "cancelled state", check: gh.Check{Bucket: "fail", State: "cancelled"}, want: false},
		{name: "canceled state", check: gh.Check{Bucket: "fail", State: "canceled"}, want: false},
		{name: "superseded bucket", check: gh.Check{Bucket: "superseded"}, want: false},
		{name: "superseded state", check: gh.Check{Bucket: "fail", State: "superseded"}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := checkFailed(tt.check); got != tt.want {
				t.Fatalf("checkFailed(%#v) = %v, want %v", tt.check, got, tt.want)
			}
		})
	}
}

func TestMarkerFromTextExtractsFailureMarker(t *testing.T) {
	text := "before\n<!--  lc:d1=abc-123._:xyz  -->\nafter"
	if got := markerFromText(text); got != "abc-123._:xyz" {
		t.Fatalf("markerFromText() = %q, want marker value", got)
	}
}

func runDiscoverTest(t *testing.T, ci fakeCIReader, writer *fakeIssueWriter) Report {
	t.Helper()
	report, err := Run(context.Background(), Options{
		RepoPath: "repo",
		CI:       ci,
		Writer:   writer,
		Now:      time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	return report
}

type fakeCIReader struct {
	prs    []gh.PullRequest
	checks map[int][]gh.Check
}

func (f fakeCIReader) RepoName(context.Context) (string, error) {
	return "owner/repo", nil
}

func (f fakeCIReader) ListOpenPRs(context.Context) ([]gh.PullRequest, error) {
	return append([]gh.PullRequest(nil), f.prs...), nil
}

func (f fakeCIReader) PRChecks(_ context.Context, number int) ([]gh.Check, error) {
	return append([]gh.Check(nil), f.checks[number]...), nil
}

type fakeIssueWriter struct {
	issues     map[int]gh.Issue
	nextNumber int
}

func newFakeIssueWriter() *fakeIssueWriter {
	return &fakeIssueWriter{
		issues:     map[int]gh.Issue{},
		nextNumber: 1,
	}
}

func (f *fakeIssueWriter) RepoName(context.Context) (string, error) {
	return "owner/repo", nil
}

func (f *fakeIssueWriter) ListIssues(context.Context, string) ([]gh.Issue, error) {
	out := make([]gh.Issue, 0, len(f.issues))
	for _, issue := range f.issues {
		out = append(out, issue)
	}
	return out, nil
}

func (f *fakeIssueWriter) CreateIssue(_ context.Context, title, body string, labels []string) (gh.Issue, error) {
	number := f.nextNumber
	f.nextNumber++
	issue := gh.Issue{
		Number: number,
		Title:  title,
		Body:   body,
		State:  "OPEN",
		Labels: labelsFromStrings(labels),
	}
	f.issues[number] = issue
	return issue, nil
}

func (f *fakeIssueWriter) UpdateIssue(_ context.Context, number int, title, body string, addLabels, removeLabels []string) (gh.Issue, error) {
	issue := f.issues[number]
	issue.Title = title
	issue.Body = body
	issue.Labels = labelsFromStrings(append(addLabels, removeLabels...))
	f.issues[number] = issue
	return issue, nil
}

func (f *fakeIssueWriter) CloseIssue(_ context.Context, number int) error {
	issue := f.issues[number]
	issue.State = "CLOSED"
	f.issues[number] = issue
	return nil
}

func (f *fakeIssueWriter) issueHasLabel(number int, name string) bool {
	for _, label := range f.issues[number].Labels {
		if label.Name == name {
			return true
		}
	}
	return false
}

func labelsFromStrings(names []string) []gh.Label {
	labels := make([]gh.Label, 0, len(names))
	for _, name := range names {
		labels = append(labels, gh.Label{Name: name})
	}
	return labels
}
