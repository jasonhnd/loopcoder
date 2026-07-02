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
	largeChangeThreshold  = 5
	referenceScheme       = "slice refs are <unit-slug>/<kind>-<n>; within the same unit, <kind>-<n> is accepted"
	baseDeliveryUnitLabel = "delivery:unit"
	epicLabel             = "epic"
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
	ReadFile  func(name string) ([]byte, error)
	WriteFile func(name string, data []byte, perm fs.FileMode) error
}

type Report struct {
	Version              int          `json:"version"`
	Repo                 string       `json:"repo"`
	RepoPath             string       `json:"repo_path"`
	RoadmapPath          string       `json:"roadmap_path"`
	GeneratedAt          string       `json:"generated_at"`
	PlanApprovalRequired bool         `json:"plan_approval_required"`
	ReferenceScheme      string       `json:"reference_scheme"`
	Created              []IssueEntry `json:"created"`
	Updated              []IssueEntry `json:"updated"`
	Unchanged            []IssueEntry `json:"unchanged"`
	Closed               []IssueEntry `json:"closed"`
	Summary              Summary      `json:"summary"`
}

type IssueEntry struct {
	Issue     int    `json:"issue"`
	ID        string `json:"id"`
	Ref       string `json:"ref"`
	Kind      string `json:"kind"`
	Title     string `json:"title"`
	BlockedBy []int  `json:"blocked_by"`
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
}

func DefaultDeps() Deps {
	return Deps{
		ReadFile:  os.ReadFile,
		WriteFile: os.WriteFile,
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
	report.PlanApprovalRequired = planApprovalRequired(doc, report)
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

func withDefaults(deps Deps) Deps {
	defaults := DefaultDeps()
	if deps.ReadFile == nil {
		deps.ReadFile = defaults.ReadFile
	}
	if deps.WriteFile == nil {
		deps.WriteFile = defaults.WriteFile
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
		if current == nil || current.epic {
			continue
		}
		if matches := slicePattern.FindStringSubmatch(line); len(matches) == 3 {
			text, marker := parseMarkedText(matches[2])
			text, needs, needsSpecified := parseNeeds(text)
			slice := &roadmapSlice{
				unit:           current,
				kind:           strings.ToLower(matches[1]),
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
		if unit.epic {
			items = append(items, &planItem{
				id:         unit.marker,
				ref:        unit.slug + "/epic-1",
				kind:       "epic",
				title:      unit.title,
				unitTitle:  unit.title,
				unitIntent: unit.intent,
				order:      unit.order,
			})
			continue
		}
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
		for _, slice := range append(docs, codes...) {
			item := &planItem{
				id:         slice.marker,
				ref:        slice.ref,
				kind:       slice.kind,
				title:      slice.text,
				unitTitle:  unit.title,
				unitIntent: unit.intent,
				sliceText:  slice.text,
				order:      slice.order,
			}
			if slice.needsSpecified {
				item.depIDs = append(item.depIDs, slice.needs...)
			} else if slice.kind == "code" {
				for _, docSlice := range docs {
					item.depIDs = append(item.depIDs, docSlice.marker)
				}
			}
			items = append(items, item)
		}
	}
	return items, nil
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
	fmt.Fprintf(&out, "Unit: %s\n", item.unitTitle)
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
		fmt.Fprintln(&out, "## Slice")
		fmt.Fprintln(&out, item.sliceText)
	} else {
		fmt.Fprintln(&out)
		fmt.Fprintln(&out, "## Epic")
		fmt.Fprintln(&out, "This roadmap unit is marked as an epic. Create one epic issue only; decomposition is a later Planner slice.")
	}
	return out.String()
}

func issueLabels(item *planItem) []string {
	labels := []string{baseDeliveryUnitLabel}
	if item.kind == "epic" {
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

func normalizeReport(report Report) Report {
	report.Created = normalizeEntries(report.Created)
	report.Updated = normalizeEntries(report.Updated)
	report.Unchanged = normalizeEntries(report.Unchanged)
	report.Closed = normalizeEntries(report.Closed)
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

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
