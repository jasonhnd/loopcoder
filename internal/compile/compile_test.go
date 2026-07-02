package compile

import (
	"context"
	"io/fs"
	"reflect"
	"strings"
	"testing"
	"time"

	gh "github.com/jasonhnd/loopcoder/internal/vcs/github"
)

func TestCompileCreatesIssuesDocFirstAndWritesMarkers(t *testing.T) {
	roadmap := `# ROADMAP

## Auth Flow
Build the login path.

- code: Add login middleware
- doc: Design login session
`
	writer := newFakeIssueWriter()
	report, written := runCompileTest(t, writer, roadmap)

	if !report.PlanApprovalRequired {
		t.Fatal("PlanApprovalRequired = false, want true on first compile")
	}
	if len(report.Created) != 2 {
		t.Fatalf("created count = %d, want 2: %#v", len(report.Created), report.Created)
	}
	if got := createdTitles(writer); !reflect.DeepEqual(got, []string{"Doc: Design login session", "Code: Add login middleware"}) {
		t.Fatalf("created titles = %#v, want doc then code", got)
	}
	if !reflect.DeepEqual(report.Created[1].BlockedBy, []int{1}) {
		t.Fatalf("code blocked_by = %#v, want [1]", report.Created[1].BlockedBy)
	}
	if !writer.issueHasLabel(2, "blocked-by:#1") {
		t.Fatalf("code issue missing blocked-by label: %#v", writer.issues[2].Labels)
	}
	if count := strings.Count(written, "<!-- lc:u="); count != 3 {
		t.Fatalf("marker count = %d, want heading + 2 slices:\n%s", count, written)
	}
}

func TestCompileIdempotentRerunNoOp(t *testing.T) {
	roadmap := `# ROADMAP

## Auth Flow
Build the login path.

- doc: Design login session
- code: Add login middleware
`
	writer := newFakeIssueWriter()
	_, written := runCompileTest(t, writer, roadmap)
	writer.resetCalls()

	report, writtenAgain := runCompileTest(t, writer, written)
	if report.PlanApprovalRequired {
		t.Fatal("PlanApprovalRequired = true, want false after marked no-op compile")
	}
	if len(report.Created) != 0 || len(report.Updated) != 0 || len(report.Closed) != 0 || len(report.Unchanged) != 2 {
		t.Fatalf("rerun report = %#v, want 2 unchanged and no mutations", report)
	}
	if writer.createCount != 0 || writer.updateCount != 0 || writer.closeCount != 0 {
		t.Fatalf("mutation counts = create %d update %d close %d, want all zero", writer.createCount, writer.updateCount, writer.closeCount)
	}
	if writtenAgain != written {
		t.Fatalf("roadmap changed on no-op rerun:\n--- before ---\n%s\n--- after ---\n%s", written, writtenAgain)
	}
}

func TestCompileIncrementalAddEditRemove(t *testing.T) {
	roadmap := `# ROADMAP

## Auth Flow
Build the login path.

- doc: Design login session
- code: Add login middleware
`
	writer := newFakeIssueWriter()
	_, written := runCompileTest(t, writer, roadmap)
	edited := strings.Replace(written, "- doc: Design login session", "- doc: Design stronger login session", 1)
	edited = removeLineContaining(edited, "Add login middleware")
	edited = insertLineAfterContaining(edited, "Design stronger login session", "- code: Add session tests")
	writer.resetCalls()

	report, _ := runCompileTest(t, writer, edited)
	if len(report.Created) != 1 || len(report.Updated) != 1 || len(report.Closed) != 1 {
		t.Fatalf("incremental report = %#v, want created=1 updated=1 closed=1", report)
	}
	if report.Updated[0].Issue != 1 || report.Closed[0].Issue != 2 || report.Created[0].Issue != 3 {
		t.Fatalf("issue numbers = updated %#v closed %#v created %#v", report.Updated, report.Closed, report.Created)
	}
	if !writer.issueHasLabel(3, "blocked-by:#1") {
		t.Fatalf("new code issue missing blocked-by doc label")
	}
	if writer.issues[2].State != "CLOSED" {
		t.Fatalf("removed issue state = %q, want CLOSED", writer.issues[2].State)
	}
}

func TestCompileEpicCreatesSingleEpicIssue(t *testing.T) {
	roadmap := `# ROADMAP

## [epic] Replace storage engine
Move storage behind a new backend.

- doc: This should remain part of epic intent, not a separate slice.
- code: This should not become a code issue.
`
	writer := newFakeIssueWriter()
	report, _ := runCompileTest(t, writer, roadmap)

	if len(report.Created) != 1 {
		t.Fatalf("created count = %d, want single epic issue: %#v", len(report.Created), report.Created)
	}
	if report.Created[0].Kind != "epic" || report.Created[0].Title != "Epic: Replace storage engine" {
		t.Fatalf("created epic entry = %#v", report.Created[0])
	}
	if !writer.issueHasLabel(1, "epic") {
		t.Fatalf("epic issue missing epic label")
	}
	if strings.Contains(writer.issues[1].Title, "This should") {
		t.Fatalf("epic was decomposed unexpectedly: %#v", writer.issues[1])
	}
}

func TestCompileMarkerSurvivesRetitleWithoutDuplicate(t *testing.T) {
	roadmap := `# ROADMAP

## Auth Flow
Build the login path.

- doc: Design login session
`
	writer := newFakeIssueWriter()
	_, written := runCompileTest(t, writer, roadmap)
	retitled := strings.Replace(written, "Auth Flow", "Authentication Flow", 1)
	retitled = strings.Replace(retitled, "Design login session", "Design authentication session", 1)
	writer.resetCalls()

	report, _ := runCompileTest(t, writer, retitled)
	if len(report.Created) != 0 || len(report.Updated) != 1 || report.Updated[0].Issue != 1 {
		t.Fatalf("retitle report = %#v, want one update on issue #1 and no duplicate", report)
	}
	if writer.nextNumber != 2 {
		t.Fatalf("next issue number = %d, want 2 (no duplicate create)", writer.nextNumber)
	}
}

func TestCompileExplicitNeedsReferenceScheme(t *testing.T) {
	roadmap := `# ROADMAP

## Foundation
- code: Add foundation

## Feature
- code: Build feature (needs: foundation/code-1)
`
	writer := newFakeIssueWriter()
	report, _ := runCompileTest(t, writer, roadmap)

	if len(report.Created) != 2 {
		t.Fatalf("created count = %d, want 2", len(report.Created))
	}
	if !reflect.DeepEqual(report.Created[1].BlockedBy, []int{1}) {
		t.Fatalf("feature blocked_by = %#v, want [1]", report.Created[1].BlockedBy)
	}
	if !writer.issueHasLabel(2, "blocked-by:#1") {
		t.Fatalf("feature issue missing explicit blocked-by label")
	}
}

func runCompileTest(t *testing.T, writer *fakeIssueWriter, roadmap string) (Report, string) {
	t.Helper()
	written := roadmap
	report, err := Run(context.Background(), Options{
		RepoPath: "repo",
		Writer:   writer,
		Now:      time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC),
	}, Deps{
		ReadFile: func(string) ([]byte, error) {
			return []byte(written), nil
		},
		WriteFile: func(_ string, data []byte, _ fs.FileMode) error {
			written = string(data)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	return report, written
}

type fakeIssueWriter struct {
	issues      map[int]gh.Issue
	nextNumber  int
	createCount int
	updateCount int
	closeCount  int
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
	f.createCount++
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
	issue.Labels = applyLabelChanges(issue.Labels, addLabels, removeLabels)
	f.issues[number] = issue
	f.updateCount++
	return issue, nil
}

func (f *fakeIssueWriter) CloseIssue(_ context.Context, number int) error {
	issue := f.issues[number]
	issue.State = "CLOSED"
	issue.StateReason = "NOT_PLANNED"
	f.issues[number] = issue
	f.closeCount++
	return nil
}

func (f *fakeIssueWriter) resetCalls() {
	f.createCount = 0
	f.updateCount = 0
	f.closeCount = 0
}

func (f *fakeIssueWriter) issueHasLabel(number int, label string) bool {
	for _, got := range f.issues[number].Labels {
		if got.Name == label {
			return true
		}
	}
	return false
}

func createdTitles(writer *fakeIssueWriter) []string {
	out := make([]string, 0, len(writer.issues))
	for i := 1; i < writer.nextNumber; i++ {
		out = append(out, writer.issues[i].Title)
	}
	return out
}

func labelsFromStrings(labels []string) []gh.Label {
	out := make([]gh.Label, 0, len(labels))
	for _, label := range labels {
		out = append(out, gh.Label{Name: label})
	}
	return out
}

func applyLabelChanges(labels []gh.Label, addLabels, removeLabels []string) []gh.Label {
	remove := map[string]bool{}
	for _, label := range removeLabels {
		remove[label] = true
	}
	seen := map[string]bool{}
	out := make([]gh.Label, 0, len(labels)+len(addLabels))
	for _, label := range labels {
		if remove[label.Name] {
			continue
		}
		seen[label.Name] = true
		out = append(out, label)
	}
	for _, label := range addLabels {
		if !seen[label] {
			out = append(out, gh.Label{Name: label})
		}
	}
	return out
}

func removeLineContaining(text, needle string) string {
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, needle) {
			continue
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func insertLineAfterContaining(text, needle, newLine string) string {
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		lines = append(lines, line)
		if strings.Contains(line, needle) {
			lines = append(lines, newLine)
		}
	}
	return strings.Join(lines, "\n")
}
