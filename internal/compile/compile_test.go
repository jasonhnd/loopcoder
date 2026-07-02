package compile

import (
	"context"
	"encoding/json"
	"io/fs"
	"path/filepath"
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

func TestCompileEpicEmitsSliceDAGArtifact(t *testing.T) {
	roadmap := `# ROADMAP

## [epic] Replace storage engine
Move storage behind a new backend.

- doc: Design storage seam
- code: Add storage adapter
`
	writer := newFakeIssueWriter()
	report, _, files := runCompileTestFiles(t, writer, roadmap)

	if !report.PlanApprovalRequired {
		t.Fatal("PlanApprovalRequired = false, want true for first epic DAG")
	}
	if len(report.Created) != 2 {
		t.Fatalf("created count = %d, want two epic slices: %#v", len(report.Created), report.Created)
	}
	if report.Created[0].Kind != "doc" || report.Created[0].EpicID == "" {
		t.Fatalf("created doc slice = %#v, want epic doc slice", report.Created[0])
	}
	if report.Created[1].Kind != "code" || !reflect.DeepEqual(report.Created[1].BlockedBy, []int{1}) {
		t.Fatalf("created code slice = %#v, want code blocked by doc issue #1", report.Created[1])
	}
	if !writer.issueHasLabel(1, "epic") || !writer.issueHasLabel(2, "epic") {
		t.Fatalf("epic slice issues missing epic label")
	}
	if !writer.issueHasLabel(2, "blocked-by:#1") {
		t.Fatalf("epic code slice missing blocked-by doc label")
	}
	if len(report.EpicDAGs) != 1 || !report.EpicDAGs[0].PlanApprovalRequired {
		t.Fatalf("epic DAG report = %#v, want one approval-gated artifact", report.EpicDAGs)
	}
	artifact := readEpicArtifactTest(t, files, report.EpicDAGs[0])
	if artifact.Version != EpicDAGVersion || artifact.EpicTitle != "Replace storage engine" {
		t.Fatalf("artifact header = %#v", artifact)
	}
	if len(artifact.Nodes) != 2 || len(artifact.Edges) != 1 {
		t.Fatalf("artifact graph nodes=%d edges=%d: %#v", len(artifact.Nodes), len(artifact.Edges), artifact)
	}
	if !artifact.Nodes[0].ImplementableAndTestable || !strings.Contains(artifact.Nodes[0].IsolationNotes, "implementable and testable") {
		t.Fatalf("artifact node missing isolation invariant: %#v", artifact.Nodes[0])
	}
	if artifact.Edges[0].From != artifact.Nodes[0].ID || artifact.Edges[0].To != artifact.Nodes[1].ID {
		t.Fatalf("artifact edge = %#v, want doc node -> code node", artifact.Edges[0])
	}
	if artifact.Ordering == nil || artifact.Ordering.CriticalPathETA != 2 {
		t.Fatalf("artifact ordering = %#v, want critical path ETA 2", artifact.Ordering)
	}
	if got := orderNodeRefs(artifact.Ordering.Ready); !reflect.DeepEqual(got, []string{"replace-storage-engine/doc-1"}) {
		t.Fatalf("artifact ready = %#v, want doc slice first", got)
	}
}

func TestCompileEpicDAGRerunPatchesWithoutApproval(t *testing.T) {
	roadmap := `# ROADMAP

## [epic] Replace storage engine
Move storage behind a new backend.

- doc: Design storage seam
- code: Add storage adapter
`
	writer := newFakeIssueWriter()
	_, _, files := runCompileTestFiles(t, writer, roadmap)
	files[filepath.Join("repo", RoadmapFilename)] = strings.Replace(files[filepath.Join("repo", RoadmapFilename)], "Add storage adapter", "Add durable storage adapter", 1)
	writer.resetCalls()

	report := runCompileTestWithFiles(t, writer, files)
	if report.PlanApprovalRequired {
		t.Fatal("PlanApprovalRequired = true, want false for non-merged epic DAG patch")
	}
	if len(report.Updated) != 1 || report.Updated[0].Issue != 2 {
		t.Fatalf("updated = %#v, want code slice issue #2 patched", report.Updated)
	}
	if len(report.EpicDAGs) != 1 || report.EpicDAGs[0].PlanApprovalRequired {
		t.Fatalf("epic DAG report = %#v, want patched without approval", report.EpicDAGs)
	}
	artifact := readEpicArtifactTest(t, files, report.EpicDAGs[0])
	if artifact.Nodes[1].Title != "Add durable storage adapter" {
		t.Fatalf("patched artifact code title = %q", artifact.Nodes[1].Title)
	}
}

func TestCompileEpicDAGChurnMergedSliceReescalates(t *testing.T) {
	roadmap := `# ROADMAP

## [epic] Replace storage engine
Move storage behind a new backend.

- doc: Design storage seam
- code: Add storage adapter
`
	writer := newFakeIssueWriter()
	_, _, files := runCompileTestFiles(t, writer, roadmap)
	merged := writer.issues[2]
	merged.State = "CLOSED"
	merged.StateReason = "COMPLETED"
	writer.issues[2] = merged
	files[filepath.Join("repo", RoadmapFilename)] = strings.Replace(files[filepath.Join("repo", RoadmapFilename)], "Add storage adapter", "Replace storage adapter", 1)
	writer.resetCalls()

	report := runCompileTestWithFiles(t, writer, files)
	if !report.PlanApprovalRequired {
		t.Fatal("PlanApprovalRequired = false, want true when patch churns merged slice")
	}
	if len(report.EpicDAGs) != 1 || !reflect.DeepEqual(report.EpicDAGs[0].ChurnedMergedSlices, []string{"replace-storage-engine/code-1"}) {
		t.Fatalf("epic DAG report = %#v, want merged code slice churn", report.EpicDAGs)
	}
}

func TestCompileEmptyEpicEmitsFallbackDecompositionSlice(t *testing.T) {
	roadmap := `# ROADMAP

## [epic] Replace storage engine
Move storage behind a new backend.
`
	writer := newFakeIssueWriter()
	report, _, files := runCompileTestFiles(t, writer, roadmap)

	if len(report.Created) != 1 || report.Created[0].Kind != "doc" || report.Created[0].EpicID == "" {
		t.Fatalf("created = %#v, want one epic fallback doc slice", report.Created)
	}
	artifact := readEpicArtifactTest(t, files, report.EpicDAGs[0])
	if len(artifact.Nodes) != 1 || artifact.Nodes[0].Ref != "replace-storage-engine/doc-1" {
		t.Fatalf("artifact = %#v, want one fallback doc node", artifact)
	}
	if !strings.Contains(artifact.Nodes[0].Title, "Decompose Replace storage engine") {
		t.Fatalf("fallback title = %q", artifact.Nodes[0].Title)
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

func TestCompileEpicCondensesCycleIntoAtomicSlice(t *testing.T) {
	roadmap := `# ROADMAP

## [epic] Untangle modules
- code: Move module A (needs: code-2)
- code: Move module B (needs: code-1)
- code: Move module C (needs: code-1)
`
	writer := newFakeIssueWriter()
	report, _, files := runCompileTestFiles(t, writer, roadmap)

	if len(report.Created) != 2 {
		t.Fatalf("created = %#v, want atomic cycle issue plus dependent issue", report.Created)
	}
	if !strings.Contains(report.Created[0].Title, "Atomic slice") {
		t.Fatalf("first issue title = %q, want atomic slice", report.Created[0].Title)
	}
	if !reflect.DeepEqual(report.Created[1].BlockedBy, []int{1}) {
		t.Fatalf("dependent blocked_by = %#v, want atomic issue #1", report.Created[1].BlockedBy)
	}
	artifact := readEpicArtifactTest(t, files, report.EpicDAGs[0])
	if len(artifact.Nodes) != 2 || !artifact.Nodes[0].Atomic {
		t.Fatalf("artifact nodes = %#v, want first node as atomic cycle", artifact.Nodes)
	}
	if got := memberRefs(artifact.Nodes[0].AtomicMembers); !reflect.DeepEqual(got, []string{"untangle-modules/code-1", "untangle-modules/code-2"}) {
		t.Fatalf("atomic members = %#v", got)
	}
	if artifact.Ordering == nil || len(artifact.Ordering.AtomicSlices) != 1 {
		t.Fatalf("ordering atomic slices = %#v, want one atomic slice", artifact.Ordering)
	}
	if got := report.EpicDAGs[0].AtomicSlices; len(got) != 1 || !strings.Contains(got[0], "code-1") || !strings.Contains(got[0], "code-2") {
		t.Fatalf("report atomic slices = %#v, want member refs surfaced", got)
	}
}

func TestComputeEpicOrderingReadySetCriticalPathAndTieBreaks(t *testing.T) {
	ordering, err := computeEpicOrdering([]EpicSliceNode{
		{ID: "z", Ref: "epic/z", Completed: true},
		{ID: "a", Ref: "epic/a"},
		{ID: "b", Ref: "epic/b"},
		{ID: "c", Ref: "epic/c", DependsOn: []string{"z"}},
		{ID: "d", Ref: "epic/d"},
		{ID: "a1", Ref: "epic/a1", DependsOn: []string{"a"}},
		{ID: "b1", Ref: "epic/b1", DependsOn: []string{"b"}},
		{ID: "b2", Ref: "epic/b2", DependsOn: []string{"b1"}},
	})
	if err != nil {
		t.Fatalf("computeEpicOrdering returned error: %v", err)
	}

	if got, want := orderNodeRefs(ordering.Ready), []string{"epic/b", "epic/a", "epic/c", "epic/d"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ready order = %#v, want %#v", got, want)
	}
	if got, want := ordering.CriticalPath, []string{"epic/b", "epic/b1", "epic/b2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("critical path = %#v, want %#v", got, want)
	}
	if ordering.CriticalPathETA != 3 {
		t.Fatalf("critical path ETA = %d, want 3", ordering.CriticalPathETA)
	}
	if len(ordering.Layers) != 3 {
		t.Fatalf("layers = %#v, want three Kahn layers", ordering.Layers)
	}
}

func TestCompileEpicIncludesMockedGoListBackbone(t *testing.T) {
	roadmap := `# ROADMAP

## [epic] Replace storage engine
- code: Add storage adapter
`
	writer := newFakeIssueWriter()
	files := map[string]string{
		filepath.Join("repo", RoadmapFilename): roadmap,
	}
	report := runCompileTestWithDeps(t, writer, files, Deps{
		GoListBackbone: func(context.Context, string) (GoListBackbone, error) {
			return GoListBackbone{
				Tool:      "go list",
				Pattern:   "./...",
				Available: true,
				Packages: []GoListPackage{
					{ImportPath: "example.com/repo/internal/storage", Dir: "internal/storage", Name: "storage"},
					{ImportPath: "example.com/repo/internal/api", Dir: "internal/api", Name: "api"},
				},
				Edges: []GoListEdge{{From: "example.com/repo/internal/storage", To: "example.com/repo/internal/api"}},
			}, nil
		},
	})

	artifact := readEpicArtifactTest(t, files, report.EpicDAGs[0])
	if artifact.GoListBackbone == nil || !artifact.GoListBackbone.Available {
		t.Fatalf("go list backbone = %#v, want available mocked backbone", artifact.GoListBackbone)
	}
	if len(artifact.GoListBackbone.Packages) != 2 || len(artifact.GoListBackbone.Edges) != 1 {
		t.Fatalf("go list backbone = %#v, want two packages and one edge", artifact.GoListBackbone)
	}
}

func TestCompileMigrationEpicAnnotatesDisciplineToggleAndDarkState(t *testing.T) {
	roadmap := `# ROADMAP

## [epic] Rewrite billing engine
Migrate billing behind Branch-by-Abstraction.

- code: Add billing seam
- code: Port invoice read path
- code: Flip and delete old invoice path
- code: Remove migration toggles cleanup
`
	writer := newFakeIssueWriter()
	report, _, files := runCompileTestFiles(t, writer, roadmap)

	if len(report.Created) != 4 {
		t.Fatalf("created = %#v, want four migration slices", report.Created)
	}
	artifact := readEpicArtifactTest(t, files, report.EpicDAGs[0])
	if len(artifact.Nodes) != 4 {
		t.Fatalf("artifact nodes = %#v, want four nodes", artifact.Nodes)
	}
	gotTypes := make([]string, 0, len(artifact.Nodes))
	for _, node := range artifact.Nodes {
		gotTypes = append(gotTypes, node.SliceType)
	}
	wantTypes := []string{EpicSliceTypeSeam, EpicSliceTypeImplementation, EpicSliceTypeFlipDelete, EpicSliceTypeCleanup}
	if !reflect.DeepEqual(gotTypes, wantTypes) {
		t.Fatalf("slice types = %#v, want %#v", gotTypes, wantTypes)
	}

	impl := artifact.Nodes[1]
	if impl.BuildTagToggle == nil {
		t.Fatalf("implementation node missing build-tag toggle: %#v", impl)
	}
	if impl.BuildTagToggle.BuildTag != "lc_rewrite_billing_engine_code_2" || impl.BuildTagToggle.DefaultState != EpicToggleStateOff {
		t.Fatalf("implementation toggle = %#v", impl.BuildTagToggle)
	}
	if !impl.Dark || impl.BuildTagToggle.State != EpicToggleStateOff || !strings.Contains(impl.DarkReason, "not complete") {
		t.Fatalf("implementation dark state = dark %t toggle %#v reason %q", impl.Dark, impl.BuildTagToggle, impl.DarkReason)
	}
	if !strings.Contains(writer.issues[2].Body, "## Build-tag toggle") || !strings.Contains(writer.issues[2].Body, "`lc_rewrite_billing_engine_code_2`") {
		t.Fatalf("implementation issue body missing toggle instructions:\n%s", writer.issues[2].Body)
	}
	if !nodeDependsOn(artifact.Nodes[1], artifact.Nodes[0].ID) {
		t.Fatalf("implementation deps = %#v, want seam dependency", artifact.Nodes[1].DependsOn)
	}
	if !nodeDependsOn(artifact.Nodes[2], artifact.Nodes[1].ID) {
		t.Fatalf("flip+delete deps = %#v, want implementation dependency", artifact.Nodes[2].DependsOn)
	}
	if !nodeDependsOn(artifact.Nodes[3], artifact.Nodes[2].ID) {
		t.Fatalf("cleanup deps = %#v, want flip+delete dependency", artifact.Nodes[3].DependsOn)
	}
}

func TestCompileMigrationEpicAddsMissingCleanupSlice(t *testing.T) {
	roadmap := `# ROADMAP

## [epic] Migrate ledger backend
Migrate the ledger storage path.

- code: Port ledger read path
`
	writer := newFakeIssueWriter()
	report, _, files := runCompileTestFiles(t, writer, roadmap)

	if len(report.Created) != 4 {
		t.Fatalf("created = %#v, want generated seam, impl, flip+delete, cleanup slices", report.Created)
	}
	artifact := readEpicArtifactTest(t, files, report.EpicDAGs[0])
	gotTypes := make([]string, 0, len(artifact.Nodes))
	for _, node := range artifact.Nodes {
		gotTypes = append(gotTypes, node.SliceType)
	}
	wantTypes := []string{EpicSliceTypeSeam, EpicSliceTypeImplementation, EpicSliceTypeFlipDelete, EpicSliceTypeCleanup}
	if !reflect.DeepEqual(gotTypes, wantTypes) {
		t.Fatalf("slice types = %#v, want %#v", gotTypes, wantTypes)
	}
	if !strings.Contains(artifact.Nodes[3].Title, "cleanup") {
		t.Fatalf("cleanup title = %q", artifact.Nodes[3].Title)
	}
}

func runCompileTest(t *testing.T, writer *fakeIssueWriter, roadmap string) (Report, string) {
	t.Helper()
	report, written, _ := runCompileTestFiles(t, writer, roadmap)
	return report, written
}

func runCompileTestFiles(t *testing.T, writer *fakeIssueWriter, roadmap string) (Report, string, map[string]string) {
	t.Helper()
	files := map[string]string{
		filepath.Join("repo", RoadmapFilename): roadmap,
	}
	report := runCompileTestWithFiles(t, writer, files)
	return report, files[filepath.Join("repo", RoadmapFilename)], files
}

func runCompileTestWithFiles(t *testing.T, writer *fakeIssueWriter, files map[string]string) Report {
	t.Helper()
	return runCompileTestWithDeps(t, writer, files, Deps{})
}

func runCompileTestWithDeps(t *testing.T, writer *fakeIssueWriter, files map[string]string, extra Deps) Report {
	t.Helper()
	deps := Deps{
		GoListBackbone: extra.GoListBackbone,
	}
	report, err := Run(context.Background(), Options{
		RepoPath: "repo",
		Writer:   writer,
		Now:      time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC),
	}, mergeTestDeps(deps, Deps{
		ReadFile: func(name string) ([]byte, error) {
			data, ok := files[name]
			if !ok {
				return nil, fs.ErrNotExist
			}
			return []byte(data), nil
		},
		WriteFile: func(name string, data []byte, _ fs.FileMode) error {
			files[name] = string(data)
			return nil
		},
		MkdirAll: func(string, fs.FileMode) error {
			return nil
		},
		Stat: func(name string) (fs.FileInfo, error) {
			if _, ok := files[name]; !ok {
				return nil, fs.ErrNotExist
			}
			return fakeFileInfo{name: filepath.Base(name)}, nil
		},
	}))
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	return report
}

func mergeTestDeps(first, second Deps) Deps {
	if first.ReadFile == nil {
		first.ReadFile = second.ReadFile
	}
	if first.WriteFile == nil {
		first.WriteFile = second.WriteFile
	}
	if first.MkdirAll == nil {
		first.MkdirAll = second.MkdirAll
	}
	if first.Stat == nil {
		first.Stat = second.Stat
	}
	if first.GoListBackbone == nil {
		first.GoListBackbone = second.GoListBackbone
	}
	return first
}

func readEpicArtifactTest(t *testing.T, files map[string]string, entry EpicDAGEntry) EpicSliceDAGArtifact {
	t.Helper()
	data, ok := files[filepath.FromSlash(entry.ArtifactPath)]
	if !ok {
		t.Fatalf("artifact %q was not written; files=%#v", entry.ArtifactPath, files)
	}
	var artifact EpicSliceDAGArtifact
	if err := json.Unmarshal([]byte(data), &artifact); err != nil {
		t.Fatalf("artifact %q is not valid JSON: %v\n%s", entry.ArtifactPath, err, data)
	}
	return artifact
}

type fakeFileInfo struct {
	name string
}

func (f fakeFileInfo) Name() string {
	return f.name
}

func (fakeFileInfo) Size() int64 {
	return 0
}

func (fakeFileInfo) Mode() fs.FileMode {
	return 0o644
}

func (fakeFileInfo) ModTime() time.Time {
	return time.Time{}
}

func (fakeFileInfo) IsDir() bool {
	return false
}

func (fakeFileInfo) Sys() any {
	return nil
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

func memberRefs(members []EpicAtomicMember) []string {
	out := make([]string, 0, len(members))
	for _, member := range members {
		out = append(out, member.Ref)
	}
	return out
}

func nodeDependsOn(node EpicSliceNode, depID string) bool {
	for _, got := range node.DependsOn {
		if got == depID {
			return true
		}
	}
	return false
}
