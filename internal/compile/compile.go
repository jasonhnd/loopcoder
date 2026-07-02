// Package compile turns ROADMAP.md into GitHub issues deterministically.
package compile

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	gh "github.com/jasonhnd/loopcoder/internal/vcs/github"
)

const (
	RoadmapFilename       = "ROADMAP.md"
	ReportVersion         = 1
	EpicDAGVersion        = 1
	largeChangeThreshold  = 5
	referenceScheme       = "slice refs are <unit-slug>/<kind>-<n>; within the same unit, <kind>-<n> is accepted"
	baseDeliveryUnitLabel = "delivery:unit"
	epicLabel             = "epic"
	epicDAGRelDir         = ".loopcoder/epics"
	isolationInvariant    = "Every slice must be implementable and testable in isolation; slice along ownership/module boundaries."

	EpicSliceTypeSeam           = "seam"
	EpicSliceTypeImplementation = "implementation"
	EpicSliceTypeFlipDelete     = "flip+delete"
	EpicSliceTypeCleanup        = "cleanup"

	EpicToggleStateOff = "off"
	EpicToggleStateOn  = "on"
)

type IssueWriter interface {
	RepoName(ctx context.Context) (string, error)
	ListIssues(ctx context.Context, state string) ([]gh.Issue, error)
	CreateIssue(ctx context.Context, title, body string, labels []string) (gh.Issue, error)
	UpdateIssue(ctx context.Context, number int, title, body string, addLabels, removeLabels []string) (gh.Issue, error)
	CloseIssue(ctx context.Context, number int) error
}

type Options struct {
	RepoPath string
	Writer   IssueWriter
	Now      time.Time
}

type Deps struct {
	ReadFile       func(name string) ([]byte, error)
	WriteFile      func(name string, data []byte, perm fs.FileMode) error
	MkdirAll       func(path string, perm fs.FileMode) error
	Stat           func(name string) (fs.FileInfo, error)
	GoListBackbone func(ctx context.Context, repoPath string) (GoListBackbone, error)
}

type Report struct {
	Version              int            `json:"version"`
	Repo                 string         `json:"repo"`
	RepoPath             string         `json:"repo_path"`
	RoadmapPath          string         `json:"roadmap_path"`
	GeneratedAt          string         `json:"generated_at"`
	PlanApprovalRequired bool           `json:"plan_approval_required"`
	ReferenceScheme      string         `json:"reference_scheme"`
	Created              []IssueEntry   `json:"created"`
	Updated              []IssueEntry   `json:"updated"`
	Unchanged            []IssueEntry   `json:"unchanged"`
	Closed               []IssueEntry   `json:"closed"`
	EpicDAGs             []EpicDAGEntry `json:"epic_dags"`
	Summary              Summary        `json:"summary"`
}

type IssueEntry struct {
	Issue     int    `json:"issue"`
	ID        string `json:"id"`
	Ref       string `json:"ref"`
	Kind      string `json:"kind"`
	EpicID    string `json:"epic_id,omitempty"`
	Title     string `json:"title"`
	BlockedBy []int  `json:"blocked_by"`
}

type EpicDAGEntry struct {
	EpicID               string   `json:"epic_id"`
	EpicRef              string   `json:"epic_ref"`
	EpicTitle            string   `json:"epic_title"`
	ArtifactPath         string   `json:"artifact_path"`
	PlanApprovalRequired bool     `json:"plan_approval_required"`
	ChurnedMergedSlices  []string `json:"churned_merged_slices"`
	Ready                []string `json:"ready"`
	CriticalPath         []string `json:"critical_path"`
	CriticalPathETA      int      `json:"critical_path_eta"`
	AtomicSlices         []string `json:"atomic_slices"`
}

type EpicSliceDAGArtifact struct {
	Version             int              `json:"version"`
	EpicID              string           `json:"epic_id"`
	EpicRef             string           `json:"epic_ref"`
	EpicTitle           string           `json:"epic_title"`
	RoadmapPath         string           `json:"roadmap_path"`
	GeneratedAt         string           `json:"generated_at"`
	ReferenceScheme     string           `json:"reference_scheme"`
	AcceptanceInvariant string           `json:"acceptance_invariant"`
	Nodes               []EpicSliceNode  `json:"nodes"`
	Edges               []EpicSliceEdge  `json:"edges"`
	GoListBackbone      *GoListBackbone  `json:"go_list_backbone,omitempty"`
	Ordering            *EpicDAGOrdering `json:"ordering,omitempty"`
}

type EpicSliceNode struct {
	ID                       string              `json:"id"`
	Ref                      string              `json:"ref"`
	Kind                     string              `json:"kind"`
	SliceType                string              `json:"slice_type,omitempty"`
	Title                    string              `json:"title"`
	Issue                    int                 `json:"issue,omitempty"`
	State                    string              `json:"state,omitempty"`
	StateReason              string              `json:"state_reason,omitempty"`
	Completed                bool                `json:"completed"`
	ImplementableAndTestable bool                `json:"implementable_and_testable"`
	IsolationNotes           string              `json:"isolation_notes"`
	BuildTagToggle           *EpicBuildTagToggle `json:"build_tag_toggle,omitempty"`
	Dark                     bool                `json:"dark,omitempty"`
	DarkReason               string              `json:"dark_reason,omitempty"`
	DependsOn                []string            `json:"depends_on"`
	Atomic                   bool                `json:"atomic,omitempty"`
	AtomicMembers            []EpicAtomicMember  `json:"atomic_members,omitempty"`
}

type EpicBuildTagToggle struct {
	Name         string `json:"name"`
	BuildTag     string `json:"build_tag"`
	DefaultState string `json:"default_state"`
	State        string `json:"state"`
}

type EpicSliceEdge struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
}

type EpicAtomicMember struct {
	ID    string `json:"id"`
	Ref   string `json:"ref"`
	Kind  string `json:"kind"`
	Title string `json:"title"`
}

type GoListBackbone struct {
	Tool      string          `json:"tool"`
	Pattern   string          `json:"pattern"`
	Available bool            `json:"available"`
	Packages  []GoListPackage `json:"packages"`
	Edges     []GoListEdge    `json:"edges"`
}

type GoListPackage struct {
	ImportPath string `json:"import_path"`
	Dir        string `json:"dir"`
	Name       string `json:"name"`
}

type GoListEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type EpicDAGOrdering struct {
	Ready           []EpicDAGOrderNode `json:"ready"`
	Layers          []EpicDAGLayer     `json:"layers"`
	CriticalPath    []string           `json:"critical_path"`
	CriticalPathETA int                `json:"critical_path_eta"`
	AtomicSlices    []EpicAtomicSlice  `json:"atomic_slices"`
}

type EpicDAGLayer struct {
	Index int                `json:"index"`
	Nodes []EpicDAGOrderNode `json:"nodes"`
}

type EpicDAGOrderNode struct {
	ID             string   `json:"id"`
	Ref            string   `json:"ref"`
	Issue          int      `json:"issue,omitempty"`
	UnblockCount   int      `json:"unblock_count"`
	OnCriticalPath bool     `json:"on_critical_path"`
	DependsOn      []string `json:"depends_on"`
}

type EpicAtomicSlice struct {
	ID      string             `json:"id"`
	Ref     string             `json:"ref"`
	Issue   int                `json:"issue,omitempty"`
	Members []EpicAtomicMember `json:"members"`
}

type Summary struct {
	CreatedCount   int `json:"created_count"`
	UpdatedCount   int `json:"updated_count"`
	UnchangedCount int `json:"unchanged_count"`
	ClosedCount    int `json:"closed_count"`
	TotalCount     int `json:"total_count"`
}

type roadmapDoc struct {
	lines                 []string
	units                 []*roadmapUnit
	existingIssueMarkers  int
	existingInlineMarkers int
	changed               bool
}

type roadmapUnit struct {
	title      string
	marker     string
	line       int
	order      int
	epic       bool
	intent     string
	slug       string
	slices     []*roadmapSlice
	startLine  int
	finishLine int
}

type roadmapSlice struct {
	unit           *roadmapUnit
	kind           string
	sliceType      string
	text           string
	marker         string
	line           int
	order          int
	ordinal        int
	ref            string
	needs          []string
	needsSpecified bool
}

type planItem struct {
	id             string
	ref            string
	kind           string
	title          string
	unitTitle      string
	unitIntent     string
	sliceText      string
	order          int
	depIDs         []string
	issueNumber    int
	blockedBy      []int
	desiredTitle   string
	desiredBody    string
	desiredLabels  []string
	existing       *gh.Issue
	createdThisRun bool
	epicID         string
	epicRef        string
	epicTitle      string
	atomic         bool
	atomicMembers  []EpicAtomicMember
	sliceType      string
	buildTagToggle *EpicBuildTagToggle
}

func DefaultDeps() Deps {
	return Deps{
		ReadFile:       os.ReadFile,
		WriteFile:      os.WriteFile,
		MkdirAll:       os.MkdirAll,
		Stat:           os.Stat,
		GoListBackbone: ExtractGoListBackbone,
	}
}

func Run(ctx context.Context, opts Options, deps Deps) (Report, error) {
	deps = withDefaults(deps)
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

	roadmapPath := filepath.Join(opts.RepoPath, RoadmapFilename)
	data, err := deps.ReadFile(roadmapPath)
	if err != nil {
		return Report{}, fmt.Errorf("read %s: %w", RoadmapFilename, err)
	}
	doc, err := parseRoadmap(string(data))
	if err != nil {
		return Report{}, err
	}
	if doc.changed {
		if err := deps.WriteFile(roadmapPath, []byte(doc.render()), 0o644); err != nil {
			return Report{}, fmt.Errorf("write %s markers: %w", RoadmapFilename, err)
		}
	}

	items, err := buildPlanItems(doc)
	if err != nil {
		return Report{}, err
	}
	if err := resolveDependencies(items); err != nil {
		return Report{}, err
	}
	items = condensePlanItemCycles(items)
	ordered, err := topologicalItems(items)
	if err != nil {
		return Report{}, err
	}

	repoName, err := opts.Writer.RepoName(ctx)
	if err != nil || strings.TrimSpace(repoName) == "" {
		repoName = opts.RepoPath
	}

	existingIssues, err := opts.Writer.ListIssues(ctx, "all")
	if err != nil {
		return Report{}, fmt.Errorf("list GitHub issues: %w", err)
	}
	existingByMarker := issuesByMarker(existingIssues)
	itemByID := map[string]*planItem{}
	for _, item := range items {
		itemByID[item.id] = item
		if issue, ok := existingByMarker[item.id]; ok {
			copyIssue := issue
			item.existing = &copyIssue
			item.issueNumber = issue.Number
		}
	}

	for _, item := range ordered {
		if item.issueNumber > 0 {
			continue
		}
		item.blockedBy = dependencyNumbers(item, itemByID)
		item.desiredTitle = issueTitle(item)
		item.desiredBody = issueBody(item)
		item.desiredLabels = issueLabels(item)
		issue, err := opts.Writer.CreateIssue(ctx, item.desiredTitle, item.desiredBody, item.desiredLabels)
		if err != nil {
			return Report{}, fmt.Errorf("create issue for %s: %w", item.ref, err)
		}
		item.issueNumber = issue.Number
		item.createdThisRun = true
		copyIssue := issue
		item.existing = &copyIssue
	}

	var created, updated, unchanged []IssueEntry
	for _, item := range items {
		item.blockedBy = dependencyNumbers(item, itemByID)
		item.desiredTitle = issueTitle(item)
		item.desiredBody = issueBody(item)
		item.desiredLabels = issueLabels(item)
		if item.createdThisRun {
			created = append(created, item.entry())
			continue
		}
		if item.existing == nil {
			return Report{}, fmt.Errorf("internal error: issue missing for %s", item.ref)
		}
		addLabels, removeLabels := labelDiff(labelNames(item.existing.Labels), item.desiredLabels)
		if issueNeedsUpdate(*item.existing, item.desiredTitle, item.desiredBody, addLabels, removeLabels) {
			issue, err := opts.Writer.UpdateIssue(ctx, item.issueNumber, item.desiredTitle, item.desiredBody, addLabels, removeLabels)
			if err != nil {
				return Report{}, fmt.Errorf("update issue #%d for %s: %w", item.issueNumber, item.ref, err)
			}
			item.issueNumber = issue.Number
			copyIssue := issue
			item.existing = &copyIssue
			updated = append(updated, item.entry())
			continue
		}
		unchanged = append(unchanged, item.entry())
	}

	desiredIDs := map[string]bool{}
	for _, item := range items {
		desiredIDs[item.id] = true
	}
	closed := closeRemovedIssues(ctx, opts.Writer, existingIssues, desiredIDs)
	if closed.err != nil {
		return Report{}, closed.err
	}

	report := Report{
		Version:         ReportVersion,
		Repo:            repoName,
		RepoPath:        filepath.ToSlash(opts.RepoPath),
		RoadmapPath:     filepath.ToSlash(roadmapPath),
		GeneratedAt:     opts.Now.Format(time.RFC3339),
		ReferenceScheme: referenceScheme,
		Created:         normalizeEntries(created),
		Updated:         normalizeEntries(updated),
		Unchanged:       normalizeEntries(unchanged),
		Closed:          normalizeEntries(closed.entries),
	}
	report.Summary = Summary{
		CreatedCount:   len(report.Created),
		UpdatedCount:   len(report.Updated),
		UnchangedCount: len(report.Unchanged),
		ClosedCount:    len(report.Closed),
		TotalCount:     len(items),
	}
	epicDAGs, epicPlanApproval, err := patchEpicSliceDAGs(ctx, opts, deps, doc, items, existingIssues, roadmapPath)
	if err != nil {
		return Report{}, err
	}
	report.EpicDAGs = normalizeEpicDAGEntries(epicDAGs)
	report.PlanApprovalRequired = planApprovalRequired(doc, report) || epicPlanApproval
	return report, nil
}

func MarshalReportJSON(report Report) ([]byte, error) {
	report = normalizeReport(report)
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal compile JSON: %w", err)
	}
	return append(data, '\n'), nil
}

func RenderText(report Report) string {
	report = normalizeReport(report)
	var out bytes.Buffer
	fmt.Fprintln(&out, "COMPILE")
	fmt.Fprintf(&out, "Repo: %s\n", report.Repo)
	fmt.Fprintf(&out, "Roadmap: %s\n", report.RoadmapPath)
	if report.PlanApprovalRequired {
		fmt.Fprintln(&out, "Plan approval required: yes")
	} else {
		fmt.Fprintln(&out, "Plan approval required: no")
	}
	fmt.Fprintf(&out, "Created: %d\n", report.Summary.CreatedCount)
	fmt.Fprintf(&out, "Updated: %d\n", report.Summary.UpdatedCount)
	fmt.Fprintf(&out, "Unchanged: %d\n", report.Summary.UnchangedCount)
	fmt.Fprintf(&out, "Closed: %d\n", report.Summary.ClosedCount)

	renderEntrySection(&out, "Created", report.Created)
	renderEntrySection(&out, "Updated", report.Updated)
	renderEntrySection(&out, "Unchanged", report.Unchanged)
	renderEntrySection(&out, "Closed", report.Closed)
	renderEpicDAGSection(&out, report.EpicDAGs)

	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Next")
	if report.PlanApprovalRequired {
		fmt.Fprintln(&out, "- Stop before dispatch and ask the human to approve the compiled plan.")
	} else {
		fmt.Fprintln(&out, "- Continue with ready-set, then dispatch ready issues.")
	}
	return out.String()
}

func renderEntrySection(out *bytes.Buffer, title string, entries []IssueEntry) {
	fmt.Fprintln(out)
	fmt.Fprintln(out, title)
	if len(entries) == 0 {
		fmt.Fprintln(out, "- none")
		return
	}
	for _, entry := range entries {
		fmt.Fprintf(out, "- #%d %s %s\n", entry.Issue, entry.Kind, entry.Title)
		if entry.Ref != "" {
			fmt.Fprintf(out, "  ref: %s\n", entry.Ref)
		}
		if len(entry.BlockedBy) == 0 {
			fmt.Fprintln(out, "  blocked_by: none")
		} else {
			fmt.Fprintf(out, "  blocked_by: %s\n", formatIssueRefs(entry.BlockedBy))
		}
	}
}

func renderEpicDAGSection(out *bytes.Buffer, entries []EpicDAGEntry) {
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Epic DAGs")
	if len(entries) == 0 {
		fmt.Fprintln(out, "- none")
		return
	}
	for _, entry := range entries {
		fmt.Fprintf(out, "- %s\n", entry.EpicTitle)
		fmt.Fprintf(out, "  artifact: %s\n", entry.ArtifactPath)
		if entry.PlanApprovalRequired {
			fmt.Fprintln(out, "  plan_approval_required: yes")
		} else {
			fmt.Fprintln(out, "  plan_approval_required: no")
		}
		fmt.Fprintf(out, "  ready: %s\n", formatStringList(entry.Ready))
		fmt.Fprintf(out, "  critical_path_eta: %d\n", entry.CriticalPathETA)
		fmt.Fprintf(out, "  critical_path: %s\n", formatStringList(entry.CriticalPath))
		fmt.Fprintf(out, "  atomic_slices: %s\n", formatStringList(entry.AtomicSlices))
	}
}

func withDefaults(deps Deps) Deps {
	defaults := DefaultDeps()
	if deps.ReadFile == nil {
		deps.ReadFile = defaults.ReadFile
	}
	if deps.WriteFile == nil {
		deps.WriteFile = defaults.WriteFile
	}
	if deps.MkdirAll == nil {
		deps.MkdirAll = defaults.MkdirAll
	}
	if deps.Stat == nil {
		deps.Stat = defaults.Stat
	}
	if deps.GoListBackbone == nil {
		deps.GoListBackbone = defaults.GoListBackbone
	}
	return deps
}

var (
	headingPattern = regexp.MustCompile(`^\s*##\s+(.+?)\s*$`)
	slicePattern   = regexp.MustCompile(`^\s*-\s*(doc|code):\s*(.+?)\s*$`)
	markerPattern  = regexp.MustCompile(`<!--\s*lc:u=([A-Za-z0-9._:-]+)\s*-->`)
	needsPattern   = regexp.MustCompile(`(?i)\s*\(needs:\s*([^)]+)\)\s*$`)
)

func parseRoadmap(text string) (*roadmapDoc, error) {
	normalized := strings.ReplaceAll(text, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")
	lines := strings.Split(normalized, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	doc := &roadmapDoc{lines: lines}

	var current *roadmapUnit
	inFence := false
	order := 0
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}

		if markerPattern.MatchString(line) {
			doc.existingInlineMarkers++
		}
		if matches := headingPattern.FindStringSubmatch(line); len(matches) == 2 {
			title, marker := parseMarkedText(matches[1])
			epic := false
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(title)), "[epic]") {
				epic = true
				title = strings.TrimSpace(title[len("[epic]"):])
			}
			unit := &roadmapUnit{
				title:     firstNonEmpty(title, "Untitled unit"),
				marker:    marker,
				line:      i,
				startLine: i,
				order:     order,
				epic:      epic,
			}
			order++
			if current != nil {
				current.finishLine = i
			}
			current = unit
			doc.units = append(doc.units, unit)
			if epic && marker != "" {
				doc.existingIssueMarkers++
			}
			continue
		}
		if current == nil {
			continue
		}
		if matches := slicePattern.FindStringSubmatch(line); len(matches) == 3 {
			text, marker := parseMarkedText(matches[2])
			text, needs, needsSpecified := parseNeeds(text)
			sliceType, text := parseSliceTypePrefix(text)
			slice := &roadmapSlice{
				unit:           current,
				kind:           strings.ToLower(matches[1]),
				sliceType:      sliceType,
				text:           firstNonEmpty(text, "Untitled slice"),
				marker:         marker,
				line:           i,
				order:          order,
				needs:          needs,
				needsSpecified: needsSpecified,
			}
			order++
			current.slices = append(current.slices, slice)
			if marker != "" {
				doc.existingIssueMarkers++
			}
		}
	}
	if current != nil {
		current.finishLine = len(lines)
	}

	if err := assignMarkers(doc); err != nil {
		return nil, err
	}
	assignIntentsAndRefs(doc)
	return doc, nil
}

func assignMarkers(doc *roadmapDoc) error {
	used := map[string]int{}
	claim := func(marker string, line int) error {
		if marker == "" {
			return nil
		}
		if prior, ok := used[marker]; ok {
			return fmt.Errorf("duplicate roadmap marker %q on lines %d and %d", marker, prior+1, line+1)
		}
		used[marker] = line
		return nil
	}
	for _, unit := range doc.units {
		if err := claim(unit.marker, unit.line); err != nil {
			return err
		}
		for _, slice := range unit.slices {
			if err := claim(slice.marker, slice.line); err != nil {
				return err
			}
		}
	}
	for _, unit := range doc.units {
		if unit.marker == "" {
			unit.marker = uniqueMarker(fmt.Sprintf("unit:%d:%s", unit.order, unit.title), used)
			doc.lines[unit.line] = addMarker(doc.lines[unit.line], unit.marker)
			doc.changed = true
		}
		for _, slice := range unit.slices {
			if slice.marker == "" {
				seed := fmt.Sprintf("slice:%s:%d:%s:%s", unit.marker, slice.order, slice.kind, slice.text)
				slice.marker = uniqueMarker(seed, used)
				doc.lines[slice.line] = addMarker(doc.lines[slice.line], slice.marker)
				doc.changed = true
			}
		}
	}
	return nil
}

func assignIntentsAndRefs(doc *roadmapDoc) {
	for _, unit := range doc.units {
		unit.slug = slugify(unit.title)
		if unit.slug == "" {
			unit.slug = "unit"
		}
		unit.intent = unitIntent(doc.lines, unit)
		docCount := 0
		codeCount := 0
		for _, slice := range unit.slices {
			switch slice.kind {
			case "doc":
				docCount++
				slice.ordinal = docCount
			case "code":
				codeCount++
				slice.ordinal = codeCount
			}
			slice.ref = fmt.Sprintf("%s/%s-%d", unit.slug, slice.kind, slice.ordinal)
		}
	}
}

func parseMarkedText(value string) (string, string) {
	matches := markerPattern.FindStringSubmatch(value)
	marker := ""
	if len(matches) == 2 {
		marker = strings.TrimSpace(matches[1])
	}
	clean := markerPattern.ReplaceAllString(value, "")
	return strings.TrimSpace(clean), marker
}

func parseNeeds(text string) (string, []string, bool) {
	matches := needsPattern.FindStringSubmatch(text)
	if len(matches) != 2 {
		return strings.TrimSpace(text), nil, false
	}
	clean := strings.TrimSpace(needsPattern.ReplaceAllString(text, ""))
	raw := strings.TrimSpace(matches[1])
	if raw == "" || raw == "[]" || strings.EqualFold(raw, "none") {
		return clean, nil, true
	}
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';'
	})
	needs := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			needs = append(needs, trimmed)
		}
	}
	return clean, needs, true
}

func parseSliceTypePrefix(text string) (string, string) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "[") {
		return "", text
	}
	end := strings.Index(text, "]")
	if end <= 1 {
		return "", text
	}
	sliceType := normalizeEpicSliceType(text[1:end])
	if sliceType == "" {
		return "", text
	}
	return sliceType, strings.TrimSpace(text[end+1:])
}

func uniqueMarker(seed string, used map[string]int) string {
	for i := 0; ; i++ {
		candidateSeed := seed
		if i > 0 {
			candidateSeed = fmt.Sprintf("%s:%d", seed, i+1)
		}
		sum := sha1.Sum([]byte(candidateSeed))
		candidate := "lc-" + hex.EncodeToString(sum[:])[:12]
		if _, ok := used[candidate]; !ok {
			used[candidate] = -1
			return candidate
		}
	}
}

func addMarker(line, marker string) string {
	if markerPattern.MatchString(line) {
		return line
	}
	return strings.TrimRight(line, " \t") + " <!-- lc:u=" + marker + " -->"
}

func unitIntent(lines []string, unit *roadmapUnit) string {
	sliceLines := map[int]bool{}
	for _, slice := range unit.slices {
		sliceLines[slice.line] = true
	}
	var out []string
	for i := unit.startLine + 1; i < unit.finishLine && i < len(lines); i++ {
		if sliceLines[i] {
			continue
		}
		clean := strings.TrimRight(markerPattern.ReplaceAllString(lines[i], ""), " \t")
		out = append(out, clean)
	}
	for len(out) > 0 && strings.TrimSpace(out[0]) == "" {
		out = out[1:]
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func (d *roadmapDoc) render() string {
	return strings.Join(d.lines, "\n") + "\n"
}

func buildPlanItems(doc *roadmapDoc) ([]*planItem, error) {
	var items []*planItem
	for _, unit := range doc.units {
		var docs []*roadmapSlice
		var codes []*roadmapSlice
		for _, slice := range unit.slices {
			switch slice.kind {
			case "doc":
				docs = append(docs, slice)
			case "code":
				codes = append(codes, slice)
			}
		}
		if unit.epic && len(docs)+len(codes) == 0 {
			docs = append(docs, fallbackEpicDocSlice(unit))
		}
		migration := isMigrationEpic(unit)
		if unit.epic {
			for _, slice := range append(docs, codes...) {
				slice.sliceType = inferEpicSliceType(unit, slice, migration)
			}
		}
		if migration {
			docs, codes = ensureMigrationDisciplineSlices(unit, docs, codes)
		}
		var unitItems []*planItem
		for _, slice := range append(docs, codes...) {
			item := &planItem{
				id:         slice.marker,
				ref:        slice.ref,
				kind:       slice.kind,
				sliceType:  slice.sliceType,
				title:      slice.text,
				unitTitle:  unit.title,
				unitIntent: unit.intent,
				sliceText:  slice.text,
				order:      slice.order,
			}
			if unit.epic {
				item.epicID = unit.marker
				item.epicRef = unit.slug + "/epic-1"
				item.epicTitle = unit.title
			}
			if slice.needsSpecified {
				item.depIDs = append(item.depIDs, slice.needs...)
			} else if slice.kind == "code" {
				for _, docSlice := range docs {
					item.depIDs = append(item.depIDs, docSlice.marker)
				}
			}
			if item.epicID != "" && item.sliceType == EpicSliceTypeImplementation {
				item.buildTagToggle = buildTagToggleForSlice(item)
			}
			unitItems = append(unitItems, item)
		}
		if migration {
			applyMigrationDisciplineDependencies(unitItems)
		}
		items = append(items, unitItems...)
	}
	return items, nil
}

func fallbackEpicDocSlice(unit *roadmapUnit) *roadmapSlice {
	return &roadmapSlice{
		unit:    unit,
		kind:    "doc",
		text:    "Decompose " + unit.title + " into implementable, testable slices",
		marker:  unit.marker + ":doc-1",
		order:   unit.order,
		ordinal: 1,
		ref:     unit.slug + "/doc-1",
	}
}

func isMigrationEpic(unit *roadmapUnit) bool {
	if unit == nil || !unit.epic {
		return false
	}
	text := strings.ToLower(unit.title + "\n" + unit.intent)
	for _, keyword := range []string{
		"migration",
		"migrate",
		"rewrite",
		"refactor",
		"strangler",
		"branch by abstraction",
		"branch-by-abstraction",
		"go->rust",
		"go to rust",
	} {
		if strings.Contains(text, keyword) {
			return true
		}
	}
	return false
}

func inferEpicSliceType(unit *roadmapUnit, slice *roadmapSlice, migration bool) string {
	if slice == nil {
		return ""
	}
	if normalized := normalizeEpicSliceType(slice.sliceType); normalized != "" {
		return normalized
	}
	if !unit.epic {
		return ""
	}
	if !migration {
		if slice.kind == "code" {
			return EpicSliceTypeImplementation
		}
		return ""
	}
	text := strings.ToLower(slice.text)
	switch {
	case containsAny(text, "cleanup", "clean up", "remove toggle", "remove build tag", "remove abstraction", "abstraction removal", "toggle removal"):
		return EpicSliceTypeCleanup
	case containsAny(text, "flip+delete", "flip and delete", "flip-delete", "flip/delete", "cutover", "switch over", "delete legacy", "remove legacy", "delete old", "remove old"):
		return EpicSliceTypeFlipDelete
	case containsAny(text, "seam", "facade", "adapter", "abstraction", "interface", "strangler"):
		return EpicSliceTypeSeam
	case slice.kind == "code":
		return EpicSliceTypeImplementation
	case migration:
		return EpicSliceTypeSeam
	default:
		return ""
	}
}

func normalizeEpicSliceType(value string) string {
	key := strings.ToLower(strings.TrimSpace(value))
	key = strings.ReplaceAll(key, "_", "-")
	key = strings.ReplaceAll(key, " ", "-")
	switch key {
	case "seam":
		return EpicSliceTypeSeam
	case "impl", "implementation", "implement":
		return EpicSliceTypeImplementation
	case "flip+delete", "flip-delete", "flip/delete", "flip", "delete", "cutover":
		return EpicSliceTypeFlipDelete
	case "cleanup", "clean-up":
		return EpicSliceTypeCleanup
	default:
		return ""
	}
}

func ensureMigrationDisciplineSlices(unit *roadmapUnit, docs, codes []*roadmapSlice) ([]*roadmapSlice, []*roadmapSlice) {
	all := append(append([]*roadmapSlice{}, docs...), codes...)
	present := map[string]bool{}
	maxCodeOrdinal := 0
	maxOrder := unit.order
	for _, slice := range all {
		if slice == nil {
			continue
		}
		present[slice.sliceType] = true
		if slice.kind == "code" && slice.ordinal > maxCodeOrdinal {
			maxCodeOrdinal = slice.ordinal
		}
		if slice.order > maxOrder {
			maxOrder = slice.order
		}
	}
	for _, sliceType := range []string{EpicSliceTypeSeam, EpicSliceTypeImplementation, EpicSliceTypeFlipDelete, EpicSliceTypeCleanup} {
		if present[sliceType] {
			continue
		}
		maxCodeOrdinal++
		maxOrder++
		codes = append(codes, fallbackMigrationCodeSlice(unit, sliceType, maxCodeOrdinal, maxOrder))
		present[sliceType] = true
	}
	return docs, codes
}

func fallbackMigrationCodeSlice(unit *roadmapUnit, sliceType string, ordinal, order int) *roadmapSlice {
	return &roadmapSlice{
		unit:      unit,
		kind:      "code",
		sliceType: sliceType,
		text:      fallbackMigrationSliceTitle(unit, sliceType),
		marker:    fmt.Sprintf("%s:%s-%d", unit.marker, sanitizeFilePart(sliceType, "slice"), ordinal),
		order:     order,
		ordinal:   ordinal,
		ref:       fmt.Sprintf("%s/code-%d", unit.slug, ordinal),
	}
}

func fallbackMigrationSliceTitle(unit *roadmapUnit, sliceType string) string {
	switch sliceType {
	case EpicSliceTypeSeam:
		return "Add Branch-by-Abstraction seam for " + unit.title
	case EpicSliceTypeImplementation:
		return "Implement dark migration slice for " + unit.title
	case EpicSliceTypeFlipDelete:
		return "Flip completed migration and delete superseded path for " + unit.title
	case EpicSliceTypeCleanup:
		return "Remove migration toggle and abstraction cleanup for " + unit.title
	default:
		return "Implement migration slice for " + unit.title
	}
}

func applyMigrationDisciplineDependencies(items []*planItem) {
	byType := map[string][]*planItem{}
	for _, item := range items {
		if item == nil {
			continue
		}
		byType[item.sliceType] = append(byType[item.sliceType], item)
	}
	for _, item := range byType[EpicSliceTypeImplementation] {
		addPlanItemDeps(item, byType[EpicSliceTypeSeam])
	}
	for _, item := range byType[EpicSliceTypeFlipDelete] {
		addPlanItemDeps(item, byType[EpicSliceTypeImplementation])
	}
	cleanupDeps := byType[EpicSliceTypeFlipDelete]
	if len(cleanupDeps) == 0 {
		cleanupDeps = byType[EpicSliceTypeImplementation]
	}
	for _, item := range byType[EpicSliceTypeCleanup] {
		addPlanItemDeps(item, cleanupDeps)
	}
}

func addPlanItemDeps(item *planItem, deps []*planItem) {
	seen := map[string]bool{}
	for _, depID := range item.depIDs {
		seen[depID] = true
	}
	for _, dep := range deps {
		if dep == nil || dep.id == item.id || seen[dep.id] {
			continue
		}
		seen[dep.id] = true
		item.depIDs = append(item.depIDs, dep.id)
	}
}

func buildTagToggleForSlice(item *planItem) *EpicBuildTagToggle {
	tag := buildTagForSliceRef(firstNonEmpty(item.ref, item.id))
	return &EpicBuildTagToggle{
		Name:         firstNonEmpty(item.ref, item.id),
		BuildTag:     tag,
		DefaultState: EpicToggleStateOff,
		State:        EpicToggleStateOff,
	}
}

func buildTagForSliceRef(ref string) string {
	ref = strings.ToLower(strings.TrimSpace(ref))
	var out strings.Builder
	lastUnderscore := false
	for _, r := range ref {
		keep := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if keep {
			out.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if !lastUnderscore {
			out.WriteByte('_')
			lastUnderscore = true
		}
	}
	clean := strings.Trim(out.String(), "_")
	if clean == "" {
		clean = "slice"
	}
	return "lc_" + clean
}

func containsAny(text string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

func patchEpicSliceDAGs(ctx context.Context, opts Options, deps Deps, doc *roadmapDoc, items []*planItem, existingIssues []gh.Issue, roadmapPath string) ([]EpicDAGEntry, bool, error) {
	itemsByEpic := map[string][]*planItem{}
	for _, item := range items {
		if item.epicID == "" {
			continue
		}
		itemsByEpic[item.epicID] = append(itemsByEpic[item.epicID], item)
	}
	if len(itemsByEpic) == 0 {
		return nil, false, nil
	}

	backbone, err := deps.GoListBackbone(ctx, opts.RepoPath)
	if err != nil {
		return nil, false, err
	}
	existingByID := issuesByMarker(existingIssues)
	var entries []EpicDAGEntry
	approvalRequired := false
	for _, unit := range doc.units {
		if !unit.epic {
			continue
		}
		epicItems := append([]*planItem(nil), itemsByEpic[unit.marker]...)
		if len(epicItems) == 0 {
			continue
		}
		sortEpicItemsForArtifact(epicItems)
		path := epicDAGArtifactPath(opts.RepoPath, unit.marker)
		existing, exists, err := readEpicDAGArtifact(path, deps)
		if err != nil {
			return nil, false, fmt.Errorf("read epic DAG artifact for %s: %w", unit.title, err)
		}
		desired, err := buildEpicDAGArtifact(unit, epicItems, roadmapPath, opts.Now, backbone)
		if err != nil {
			return nil, false, fmt.Errorf("order epic DAG artifact for %s: %w", unit.title, err)
		}
		churned := []string{}
		if exists {
			churned = churnedMergedSlices(existing, desired, existingByID)
			if epicDAGArtifactsEqual(existing, desired) {
				desired.GeneratedAt = existing.GeneratedAt
			}
		}
		needsApproval := !exists || len(churned) > 0
		approvalRequired = approvalRequired || needsApproval

		if !exists || !epicDAGArtifactsEqual(existing, desired) {
			desired.GeneratedAt = opts.Now.Format(time.RFC3339)
			data, err := marshalEpicDAGArtifact(desired)
			if err != nil {
				return nil, false, fmt.Errorf("marshal epic DAG artifact for %s: %w", unit.title, err)
			}
			if err := deps.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return nil, false, fmt.Errorf("create epic DAG artifact directory: %w", err)
			}
			if err := deps.WriteFile(path, data, 0o644); err != nil {
				return nil, false, fmt.Errorf("write epic DAG artifact for %s: %w", unit.title, err)
			}
		}

		entries = append(entries, EpicDAGEntry{
			EpicID:               unit.marker,
			EpicRef:              unit.slug + "/epic-1",
			EpicTitle:            unit.title,
			ArtifactPath:         filepath.ToSlash(path),
			PlanApprovalRequired: needsApproval,
			ChurnedMergedSlices:  normalizeStrings(churned),
			Ready:                orderNodeRefs(desired.Ordering.Ready),
			CriticalPath:         normalizeStrings(desired.Ordering.CriticalPath),
			CriticalPathETA:      desired.Ordering.CriticalPathETA,
			AtomicSlices:         atomicSliceRefs(desired.Ordering.AtomicSlices),
		})
	}
	return entries, approvalRequired, nil
}

func buildEpicDAGArtifact(unit *roadmapUnit, items []*planItem, roadmapPath string, now time.Time, backbone GoListBackbone) (EpicSliceDAGArtifact, error) {
	nodes := make([]EpicSliceNode, 0, len(items))
	edges := make([]EpicSliceEdge, 0)
	for _, item := range items {
		node := EpicSliceNode{
			ID:                       item.id,
			Ref:                      item.ref,
			Kind:                     item.kind,
			SliceType:                item.sliceType,
			Title:                    item.title,
			Issue:                    item.issueNumber,
			ImplementableAndTestable: true,
			IsolationNotes:           isolationInvariant,
			BuildTagToggle:           cloneBuildTagToggle(item.buildTagToggle),
			DependsOn:                append([]string(nil), item.depIDs...),
			Atomic:                   item.atomic,
			AtomicMembers:            append([]EpicAtomicMember(nil), item.atomicMembers...),
		}
		sort.Strings(node.DependsOn)
		if item.existing != nil {
			node.State = item.existing.State
			node.StateReason = item.existing.StateReason
			node.Completed = isIssueCompleted(*item.existing)
		}
		nodes = append(nodes, node)
		for _, depID := range item.depIDs {
			edges = append(edges, EpicSliceEdge{
				From:   depID,
				To:     item.id,
				Reason: "roadmap dependency or doc-first dependency",
			})
		}
	}
	applyEpicDarkState(nodes)
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].From == edges[j].From {
			return edges[i].To < edges[j].To
		}
		return edges[i].From < edges[j].From
	})
	ordering, err := computeEpicOrdering(nodes)
	if err != nil {
		return EpicSliceDAGArtifact{}, err
	}
	artifact := EpicSliceDAGArtifact{
		Version:             EpicDAGVersion,
		EpicID:              unit.marker,
		EpicRef:             unit.slug + "/epic-1",
		EpicTitle:           unit.title,
		RoadmapPath:         filepath.ToSlash(roadmapPath),
		GeneratedAt:         now.Format(time.RFC3339),
		ReferenceScheme:     referenceScheme,
		AcceptanceInvariant: isolationInvariant,
		Nodes:               nodes,
		Edges:               edges,
		Ordering:            &ordering,
	}
	if backbone.Tool != "" || backbone.Available || len(backbone.Packages) > 0 || len(backbone.Edges) > 0 {
		copied := backbone
		artifact.GoListBackbone = &copied
	}
	return artifact, nil
}

func readEpicDAGArtifact(path string, deps Deps) (EpicSliceDAGArtifact, bool, error) {
	if _, err := deps.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
			return EpicSliceDAGArtifact{}, false, nil
		}
		return EpicSliceDAGArtifact{}, false, err
	}
	data, err := deps.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
			return EpicSliceDAGArtifact{}, false, nil
		}
		return EpicSliceDAGArtifact{}, false, err
	}
	var artifact EpicSliceDAGArtifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		return EpicSliceDAGArtifact{}, false, err
	}
	return artifact, true, nil
}

func marshalEpicDAGArtifact(artifact EpicSliceDAGArtifact) ([]byte, error) {
	if artifact.Nodes == nil {
		artifact.Nodes = []EpicSliceNode{}
	}
	if artifact.Edges == nil {
		artifact.Edges = []EpicSliceEdge{}
	}
	if artifact.GoListBackbone != nil {
		if artifact.GoListBackbone.Packages == nil {
			artifact.GoListBackbone.Packages = []GoListPackage{}
		}
		if artifact.GoListBackbone.Edges == nil {
			artifact.GoListBackbone.Edges = []GoListEdge{}
		}
	}
	if artifact.Ordering != nil {
		if artifact.Ordering.Ready == nil {
			artifact.Ordering.Ready = []EpicDAGOrderNode{}
		}
		if artifact.Ordering.Layers == nil {
			artifact.Ordering.Layers = []EpicDAGLayer{}
		}
		artifact.Ordering.CriticalPath = normalizeStrings(artifact.Ordering.CriticalPath)
		if artifact.Ordering.AtomicSlices == nil {
			artifact.Ordering.AtomicSlices = []EpicAtomicSlice{}
		}
	}
	data, err := json.MarshalIndent(artifact, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func cloneBuildTagToggle(toggle *EpicBuildTagToggle) *EpicBuildTagToggle {
	if toggle == nil {
		return nil
	}
	copied := *toggle
	return &copied
}

func applyEpicDarkState(nodes []EpicSliceNode) {
	complete := len(nodes) > 0
	for _, node := range nodes {
		if !node.Completed {
			complete = false
			break
		}
	}
	for i := range nodes {
		if nodes[i].BuildTagToggle == nil {
			continue
		}
		toggle := *nodes[i].BuildTagToggle
		if complete {
			toggle.State = EpicToggleStateOn
			nodes[i].BuildTagToggle = &toggle
			nodes[i].Dark = false
			nodes[i].DarkReason = ""
			continue
		}
		toggle.State = EpicToggleStateOff
		nodes[i].BuildTagToggle = &toggle
		nodes[i].Dark = true
		nodes[i].DarkReason = "epic is not complete; implementation slice remains toggled off in pre-prod"
	}
}

func epicDAGArtifactPath(repoPath, epicID string) string {
	return filepath.Join(repoPath, epicDAGRelDir, sanitizeFilePart(epicID, "epic")+".slice_dag.json")
}

func epicDAGArtifactsEqual(a, b EpicSliceDAGArtifact) bool {
	a.GeneratedAt = ""
	b.GeneratedAt = ""
	return reflect.DeepEqual(a, b)
}

func churnedMergedSlices(existing, desired EpicSliceDAGArtifact, existingByID map[string]gh.Issue) []string {
	desiredByID := map[string]EpicSliceNode{}
	for _, node := range desired.Nodes {
		desiredByID[node.ID] = node
	}
	seen := map[string]bool{}
	var churned []string
	for _, oldNode := range existing.Nodes {
		if !nodeWasCompleted(oldNode, existingByID) {
			continue
		}
		newNode, ok := desiredByID[oldNode.ID]
		if !ok || !epicNodeSignatureEqual(oldNode, newNode) {
			ref := firstNonEmpty(oldNode.Ref, oldNode.ID)
			if !seen[ref] {
				seen[ref] = true
				churned = append(churned, ref)
			}
		}
	}
	sort.Strings(churned)
	return churned
}

func nodeWasCompleted(node EpicSliceNode, existingByID map[string]gh.Issue) bool {
	if issue, ok := existingByID[node.ID]; ok {
		return isIssueCompleted(issue)
	}
	return node.Completed
}

func epicNodeSignatureEqual(a, b EpicSliceNode) bool {
	return a.ID == b.ID &&
		a.Ref == b.Ref &&
		a.Kind == b.Kind &&
		a.SliceType == b.SliceType &&
		a.Title == b.Title &&
		buildTagToggleSignatureEqual(a.BuildTagToggle, b.BuildTagToggle) &&
		reflect.DeepEqual(sortedStrings(a.DependsOn), sortedStrings(b.DependsOn)) &&
		a.ImplementableAndTestable == b.ImplementableAndTestable &&
		a.Atomic == b.Atomic &&
		reflect.DeepEqual(sortedAtomicMembers(a.AtomicMembers), sortedAtomicMembers(b.AtomicMembers))
}

func buildTagToggleSignatureEqual(a, b *EpicBuildTagToggle) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Name == b.Name &&
		a.BuildTag == b.BuildTag &&
		a.DefaultState == b.DefaultState
}

func isIssueCompleted(issue gh.Issue) bool {
	stateValue := strings.ToUpper(strings.TrimSpace(issue.State))
	stateReason := strings.ToUpper(strings.TrimSpace(issue.StateReason))
	return stateValue == "CLOSED" && (stateReason == "COMPLETED" || len(issue.ClosedByPullRequestsReferences) > 0)
}

func resolveDependencies(items []*planItem) error {
	byID := map[string]*planItem{}
	byRef := map[string]*planItem{}
	for _, item := range items {
		byID[item.id] = item
		ref := strings.ToLower(item.ref)
		if existing, ok := byRef[ref]; ok {
			return fmt.Errorf("duplicate roadmap slice ref %q for markers %s and %s", item.ref, existing.id, item.id)
		}
		byRef[ref] = item
	}
	for _, item := range items {
		resolved := make([]string, 0, len(item.depIDs))
		seen := map[string]bool{}
		for _, raw := range item.depIDs {
			dep, ok := resolveRef(raw, item, byID, byRef)
			if !ok {
				return fmt.Errorf("resolve needs %q for %s: unknown slice ref", raw, item.ref)
			}
			if dep.id == item.id {
				return fmt.Errorf("resolve needs %q for %s: slice cannot depend on itself", raw, item.ref)
			}
			if !seen[dep.id] {
				seen[dep.id] = true
				resolved = append(resolved, dep.id)
			}
		}
		item.depIDs = resolved
	}
	return nil
}

func resolveRef(raw string, current *planItem, byID map[string]*planItem, byRef map[string]*planItem) (*planItem, bool) {
	ref := strings.TrimSpace(raw)
	if ref == "" {
		return nil, false
	}
	if item, ok := byID[ref]; ok {
		return item, true
	}
	lower := strings.ToLower(ref)
	if item, ok := byRef[lower]; ok {
		return item, true
	}
	if !strings.Contains(ref, "/") {
		unitPrefix := strings.SplitN(current.ref, "/", 2)[0]
		if item, ok := byRef[unitPrefix+"/"+lower]; ok {
			return item, true
		}
	}
	return nil, false
}

func topologicalItems(items []*planItem) ([]*planItem, error) {
	remainingDeps := map[string]int{}
	dependents := map[string][]*planItem{}
	for _, item := range items {
		remainingDeps[item.id] = len(item.depIDs)
		for _, depID := range item.depIDs {
			dependents[depID] = append(dependents[depID], item)
		}
	}
	ready := make([]*planItem, 0)
	for _, item := range items {
		if remainingDeps[item.id] == 0 {
			ready = append(ready, item)
		}
	}
	sortItemsByOrder(ready)
	out := make([]*planItem, 0, len(items))
	for len(ready) > 0 {
		item := ready[0]
		ready = ready[1:]
		out = append(out, item)
		for _, dependent := range dependents[item.id] {
			remainingDeps[dependent.id]--
			if remainingDeps[dependent.id] == 0 {
				ready = append(ready, dependent)
				sortItemsByOrder(ready)
			}
		}
	}
	if len(out) != len(items) {
		return nil, errors.New("roadmap dependencies contain a cycle")
	}
	return out, nil
}

func sortItemsByOrder(items []*planItem) {
	sort.Slice(items, func(i, j int) bool {
		return items[i].order < items[j].order
	})
}

func sortEpicItemsForArtifact(items []*planItem) {
	sort.SliceStable(items, func(i, j int) bool {
		leftRank := epicSliceTypeRank(items[i].sliceType)
		rightRank := epicSliceTypeRank(items[j].sliceType)
		if leftRank != rightRank && (leftRank > 0 || rightRank > 0) {
			return leftRank < rightRank
		}
		if items[i].order != items[j].order {
			return items[i].order < items[j].order
		}
		return items[i].id < items[j].id
	})
}

func epicSliceTypeRank(sliceType string) int {
	switch sliceType {
	case EpicSliceTypeSeam:
		return 1
	case EpicSliceTypeImplementation:
		return 2
	case EpicSliceTypeFlipDelete:
		return 3
	case EpicSliceTypeCleanup:
		return 4
	default:
		return 0
	}
}

func issuesByMarker(issues []gh.Issue) map[string]gh.Issue {
	out := map[string]gh.Issue{}
	for _, issue := range issues {
		marker := markerFromText(issue.Body)
		if marker == "" {
			continue
		}
		if existing, ok := out[marker]; ok && strings.EqualFold(existing.State, "OPEN") {
			continue
		}
		out[marker] = issue
	}
	return out
}

func markerFromText(text string) string {
	matches := markerPattern.FindStringSubmatch(text)
	if len(matches) != 2 {
		return ""
	}
	return strings.TrimSpace(matches[1])
}

func dependencyNumbers(item *planItem, byID map[string]*planItem) []int {
	out := make([]int, 0, len(item.depIDs))
	for _, depID := range item.depIDs {
		if dep, ok := byID[depID]; ok && dep.issueNumber > 0 {
			out = append(out, dep.issueNumber)
		}
	}
	return out
}

func issueTitle(item *planItem) string {
	switch item.kind {
	case "doc":
		return "Doc: " + item.title
	case "code":
		return "Code: " + item.title
	case "epic":
		return "Epic: " + item.title
	default:
		return item.title
	}
}

func issueBody(item *planItem) string {
	var out bytes.Buffer
	fmt.Fprintf(&out, "<!-- lc:u=%s -->\n", item.id)
	fmt.Fprintf(&out, "Roadmap ref: `%s`\n", item.ref)
	fmt.Fprintf(&out, "Kind: %s\n", item.kind)
	if strings.TrimSpace(item.sliceType) != "" {
		fmt.Fprintf(&out, "Slice type: %s\n", item.sliceType)
	}
	fmt.Fprintf(&out, "Unit: %s\n", item.unitTitle)
	if item.epicID != "" {
		fmt.Fprintf(&out, "Epic: %s\n", item.epicTitle)
		fmt.Fprintf(&out, "Epic ref: `%s`\n", item.epicRef)
	}
	if len(item.blockedBy) == 0 {
		fmt.Fprintln(&out, "Blocked by: none")
	} else {
		fmt.Fprintf(&out, "Blocked by: %s\n", formatIssueRefs(item.blockedBy))
	}
	if strings.TrimSpace(item.unitIntent) != "" {
		fmt.Fprintln(&out)
		fmt.Fprintln(&out, "## Intent")
		fmt.Fprintln(&out, strings.TrimSpace(item.unitIntent))
	}
	if item.kind != "epic" {
		fmt.Fprintln(&out)
		if item.atomic {
			fmt.Fprintln(&out, "## Atomic slice")
			fmt.Fprintln(&out, item.sliceText)
		} else {
			fmt.Fprintln(&out, "## Slice")
			fmt.Fprintln(&out, item.sliceText)
		}
		if item.epicID != "" {
			fmt.Fprintln(&out)
			fmt.Fprintln(&out, "## Isolation")
			fmt.Fprintln(&out, isolationInvariant)
		}
		if item.buildTagToggle != nil {
			fmt.Fprintln(&out)
			fmt.Fprintln(&out, "## Build-tag toggle")
			fmt.Fprintf(&out, "Build tag: `%s`\n", item.buildTagToggle.BuildTag)
			fmt.Fprintf(&out, "Default state: %s\n", item.buildTagToggle.DefaultState)
			fmt.Fprintln(&out, "Use this build-tag toggle as the revert net: a bad pre-prod slice is toggled off, not manually rolled back.")
			fmt.Fprintln(&out, "Leave the slice dark while the epic is unfinished; cleanup slices remove this toggle and abstraction after flip+delete.")
		}
	} else {
		fmt.Fprintln(&out)
		fmt.Fprintln(&out, "## Epic")
		fmt.Fprintln(&out, "This roadmap unit is marked as a legacy epic. New epic entries compile into slice DAG artifacts and slice issues.")
	}
	return out.String()
}

func issueLabels(item *planItem) []string {
	labels := []string{baseDeliveryUnitLabel}
	if item.kind == "epic" || item.epicID != "" {
		labels = append(labels, epicLabel)
	}
	for _, dep := range item.blockedBy {
		labels = append(labels, fmt.Sprintf("blocked-by:#%d", dep))
	}
	return labels
}

func labelNames(labels []gh.Label) []string {
	out := make([]string, 0, len(labels))
	for _, label := range labels {
		if strings.TrimSpace(label.Name) != "" {
			out = append(out, label.Name)
		}
	}
	return out
}

func labelDiff(existing, desired []string) ([]string, []string) {
	existingSet := map[string]string{}
	for _, label := range existing {
		existingSet[strings.ToLower(strings.TrimSpace(label))] = strings.TrimSpace(label)
	}
	desiredSet := map[string]string{}
	for _, label := range desired {
		desiredSet[strings.ToLower(strings.TrimSpace(label))] = strings.TrimSpace(label)
	}
	var add []string
	for key, label := range desiredSet {
		if _, ok := existingSet[key]; !ok {
			add = append(add, label)
		}
	}
	var remove []string
	for key, label := range existingSet {
		if strings.HasPrefix(key, "blocked-by:#") {
			if _, ok := desiredSet[key]; !ok {
				remove = append(remove, label)
			}
		}
	}
	sort.Strings(add)
	sort.Strings(remove)
	return add, remove
}

func issueNeedsUpdate(issue gh.Issue, title, body string, addLabels, removeLabels []string) bool {
	if strings.TrimSpace(issue.Title) != strings.TrimSpace(title) {
		return true
	}
	if normalizeBody(issue.Body) != normalizeBody(body) {
		return true
	}
	return len(addLabels) > 0 || len(removeLabels) > 0
}

func normalizeBody(body string) string {
	return strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\r", "\n"))
}

type closeResult struct {
	entries []IssueEntry
	err     error
}

func closeRemovedIssues(ctx context.Context, writer IssueWriter, existing []gh.Issue, desiredIDs map[string]bool) closeResult {
	var entries []IssueEntry
	for _, issue := range existing {
		marker := markerFromText(issue.Body)
		if marker == "" || desiredIDs[marker] {
			continue
		}
		if !strings.EqualFold(issue.State, "OPEN") && strings.TrimSpace(issue.State) != "" {
			continue
		}
		if err := writer.CloseIssue(ctx, issue.Number); err != nil {
			return closeResult{err: fmt.Errorf("close issue #%d: %w", issue.Number, err)}
		}
		entries = append(entries, IssueEntry{
			Issue:     issue.Number,
			ID:        marker,
			Ref:       issueRefFromBody(issue.Body),
			Kind:      issueKindFromBody(issue.Body),
			Title:     issue.Title,
			BlockedBy: blockedByFromLabels(issue.Labels),
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Issue < entries[j].Issue
	})
	return closeResult{entries: entries}
}

func (item *planItem) entry() IssueEntry {
	return IssueEntry{
		Issue:     item.issueNumber,
		ID:        item.id,
		Ref:       item.ref,
		Kind:      item.kind,
		EpicID:    item.epicID,
		Title:     item.desiredTitle,
		BlockedBy: append([]int(nil), item.blockedBy...),
	}
}

func issueRefFromBody(body string) string {
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "Roadmap ref:") {
			value := strings.TrimSpace(strings.TrimPrefix(trimmed, "Roadmap ref:"))
			return strings.Trim(value, "`")
		}
	}
	return ""
}

func issueKindFromBody(body string) string {
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "Kind:") {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, "Kind:"))
		}
	}
	return ""
}

func blockedByFromLabels(labels []gh.Label) []int {
	seen := map[int]bool{}
	for _, label := range labels {
		name := strings.ToLower(strings.TrimSpace(label.Name))
		if !strings.HasPrefix(name, "blocked-by:#") {
			continue
		}
		number, err := strconv.Atoi(strings.TrimPrefix(name, "blocked-by:#"))
		if err == nil && number > 0 {
			seen[number] = true
		}
	}
	out := make([]int, 0, len(seen))
	for number := range seen {
		out = append(out, number)
	}
	sort.Ints(out)
	return out
}

func normalizeEntries(entries []IssueEntry) []IssueEntry {
	if entries == nil {
		return []IssueEntry{}
	}
	for i := range entries {
		if entries[i].BlockedBy == nil {
			entries[i].BlockedBy = []int{}
		}
	}
	return entries
}

func normalizeEpicDAGEntries(entries []EpicDAGEntry) []EpicDAGEntry {
	if entries == nil {
		return []EpicDAGEntry{}
	}
	for i := range entries {
		entries[i].ChurnedMergedSlices = normalizeStrings(entries[i].ChurnedMergedSlices)
		entries[i].Ready = normalizeStrings(entries[i].Ready)
		entries[i].CriticalPath = normalizeStrings(entries[i].CriticalPath)
		entries[i].AtomicSlices = normalizeStrings(entries[i].AtomicSlices)
	}
	return entries
}

func normalizeReport(report Report) Report {
	report.Created = normalizeEntries(report.Created)
	report.Updated = normalizeEntries(report.Updated)
	report.Unchanged = normalizeEntries(report.Unchanged)
	report.Closed = normalizeEntries(report.Closed)
	report.EpicDAGs = normalizeEpicDAGEntries(report.EpicDAGs)
	return report
}

func planApprovalRequired(doc *roadmapDoc, report Report) bool {
	mutations := report.Summary.CreatedCount + report.Summary.UpdatedCount + report.Summary.ClosedCount
	if report.Summary.TotalCount > 0 && doc.existingIssueMarkers == 0 {
		return true
	}
	return mutations >= largeChangeThreshold
}

func formatIssueRefs(numbers []int) string {
	parts := make([]string, 0, len(numbers))
	for _, number := range numbers {
		parts = append(parts, fmt.Sprintf("#%d", number))
	}
	return strings.Join(parts, ", ")
}

func slugify(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var out strings.Builder
	lastDash := false
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			out.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			out.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(out.String(), "-")
}

func sanitizeFilePart(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	var out strings.Builder
	lastDash := false
	for _, r := range value {
		keep := (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '.' || r == '_' || r == '-'
		if keep {
			out.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			out.WriteByte('-')
			lastDash = true
		}
	}
	clean := strings.Trim(out.String(), "-.")
	if clean == "" {
		return fallback
	}
	return clean
}

func sortedStrings(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

func normalizeStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
