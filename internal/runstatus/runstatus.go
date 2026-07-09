// Package runstatus renders local-only delivery run status from .loopcoder state.
package runstatus

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	lcdefaults "github.com/jasonhnd/loopcoder/internal/defaults"
	"github.com/jasonhnd/loopcoder/internal/reporter"
	"github.com/jasonhnd/loopcoder/internal/state"
)

const NotReported = "not reported"

const (
	maxRunStatusEventBytes       int64 = lcdefaults.RunStatusMaxEventBytes
	maxRunStatusEventLineBytes   int   = lcdefaults.RunStatusMaxEventLineBytes
	maxRunStatusRecordBytes      int64 = lcdefaults.RunStatusMaxRecordBytes
	maxRunStatusDirectoryEntries       = lcdefaults.RunStatusMaxDirectoryEntries
	maxRunStatusFutureSkew             = lcdefaults.RunStatusMaxFutureSkew
)

type Options struct {
	RepoPath string
	RunID    string
}

type Report struct {
	RunID               string
	RunNote             string
	RunPath             string
	EventCount          int
	VerifierRecordCount int
	LifecycleState      string
	LifecycleSource     string
	LifecycleEvents     int
	ParentRunID         string
	ChildRunIDs         []string
	Rows                []Row
}

type Row struct {
	Issue                 string
	WorkerJob             string
	PR                    string
	WorkerProvider        string
	WorkerModel           string
	WorkerModelSource     string
	WorkerEffort          string
	WorkerPermission      string
	WorkerDuration        string
	WorkerInputTokens     string
	WorkerOutputTokens    string
	WorkerTotalTokens     string
	WorkerVerified        string
	Phase                 string
	Status                string
	VerifierVerdict       string
	VerifierProvider      string
	VerifierModel         string
	VerifierModelSource   string
	VerifierEffort        string
	VerifierPermission    string
	VerifierDuration      string
	VerifierInputTokens   string
	VerifierOutputTokens  string
	VerifierTotalTokens   string
	VerifierVerified      string
	issueNumber           int
	attemptNumber         int
	workerJobSort         string
	verifierRecordSortKey string
}

type metadataRecord struct {
	Issue int
	JobID string
	PR    string
	Path  string
}

type verifierRecord struct {
	Issue   int
	JobID   string
	PR      string
	Verdict string
	Report  *reporter.Report
	Path    string
}

func Load(opts Options) (Report, error) {
	repoPath := strings.TrimSpace(opts.RepoPath)
	if repoPath == "" {
		repoPath = "."
	}

	runID := strings.TrimSpace(opts.RunID)
	runNote := "requested run"
	if runID == "" {
		latest, err := state.LatestRunID(repoPath)
		if err != nil {
			return Report{}, err
		}
		if strings.TrimSpace(latest) == "" {
			return Report{}, fmt.Errorf("no local run found under %s", filepath.ToSlash(state.RunsRoot(repoPath)))
		}
		runID = latest
		runNote = "latest modified run selected"
	}

	runPath := state.RunPath(repoPath, runID)
	info, err := os.Stat(runPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Report{}, fmt.Errorf("run %q not found under %s", runID, filepath.ToSlash(state.RunsRoot(repoPath)))
		}
		return Report{}, fmt.Errorf("read run %q: %w", runID, err)
	}
	if !info.IsDir() {
		return Report{}, fmt.Errorf("run path is not a directory: %s", filepath.ToSlash(runPath))
	}

	now := time.Now().UTC()
	if err := validateRunStatusAttemptRecords(runPath, now); err != nil {
		return Report{}, err
	}

	attempts, err := state.LoadAttempts(repoPath, runID)
	if err != nil {
		return Report{}, err
	}
	lifecycle, err := state.LoadLifecycle(repoPath, runID)
	if err != nil {
		return Report{}, err
	}

	eventMetadata, eventVerifiers, eventCount, err := loadEventRecords(repoPath, runID, now)
	if err != nil {
		return Report{}, err
	}
	jsonMetadata, jsonVerifiers, err := scanRunJSONRecords(runPath, now)
	if err != nil {
		return Report{}, err
	}

	metadata := append(eventMetadata, jsonMetadata...)
	verifiers := append(eventVerifiers, jsonVerifiers...)
	verifiers = dedupeVerifierRecords(verifiers)
	sortVerifierRecords(verifiers)

	if len(attempts) == 0 && eventCount == 0 && len(metadata) == 0 && len(verifiers) == 0 {
		return Report{}, fmt.Errorf("run %q has no local status records under %s", runID, filepath.ToSlash(runPath))
	}

	rows := make([]Row, 0, len(attempts))
	for _, attempt := range attempts {
		verifier := matchingVerifier(attempt, metadataPR(attempt, metadata), verifiers)
		row := rowFromAttempt(attempt, metadata, verifier)
		rows = append(rows, row)
	}
	sortRows(rows)

	return Report{
		RunID:               runID,
		RunNote:             runNote,
		RunPath:             runPath,
		EventCount:          eventCount,
		VerifierRecordCount: len(verifiers),
		LifecycleState:      string(lifecycle.State),
		LifecycleSource:     lifecycle.Source,
		LifecycleEvents:     len(lifecycle.History),
		ParentRunID:         lifecycle.ParentRunID,
		ChildRunIDs:         append([]string(nil), lifecycle.ChildRunIDs...),
		Rows:                rows,
	}, nil
}

func Render(report Report) string {
	var out bytes.Buffer
	fmt.Fprintln(&out, "RUN STATUS")
	fmt.Fprintf(&out, "RunId: %s (%s)\n", display(report.RunID), display(report.RunNote))
	fmt.Fprintf(&out, "Source: %s\n", filepath.ToSlash(report.RunPath))
	fmt.Fprintf(&out, "Events: %d\n", report.EventCount)
	fmt.Fprintf(&out, "Verifier records: %d\n", report.VerifierRecordCount)
	fmt.Fprintf(&out, "Lifecycle: %s", display(report.LifecycleState))
	if strings.TrimSpace(report.LifecycleSource) != "" {
		fmt.Fprintf(&out, " (source=%s entries=%d)", display(report.LifecycleSource), report.LifecycleEvents)
	}
	fmt.Fprintln(&out)
	if strings.TrimSpace(report.ParentRunID) != "" {
		fmt.Fprintf(&out, "ParentRunId: %s\n", display(report.ParentRunID))
	}
	if len(report.ChildRunIDs) > 0 {
		fmt.Fprintf(&out, "ChildRunIds: %s\n", strings.Join(report.ChildRunIDs, ","))
	}
	fmt.Fprintln(&out)

	headers := []string{
		"Issue",
		"Worker job",
		"PR",
		"Worker provider",
		"Worker model",
		"Worker model source",
		"Worker effort",
		"Worker permission",
		"Worker duration",
		"Worker tokens in",
		"Worker tokens out",
		"Worker tokens total",
		"Worker verified",
		"Phase",
		"Status",
		"Verifier verdict",
		"Verifier provider",
		"Verifier model",
		"Verifier model source",
		"Verifier effort",
		"Verifier permission",
		"Verifier duration",
		"Verifier tokens in",
		"Verifier tokens out",
		"Verifier tokens total",
		"Verifier verified",
	}
	writeTableRow(&out, headers)
	separators := make([]string, len(headers))
	for i := range separators {
		separators[i] = "---"
	}
	writeTableRow(&out, separators)
	if len(report.Rows) == 0 {
		empty := make([]string, len(headers))
		for i := range empty {
			empty[i] = NotReported
		}
		empty[0] = "none"
		writeTableRow(&out, empty)
	} else {
		for _, row := range report.Rows {
			writeTableRow(&out, []string{
				row.Issue,
				row.WorkerJob,
				row.PR,
				row.WorkerProvider,
				row.WorkerModel,
				row.WorkerModelSource,
				row.WorkerEffort,
				row.WorkerPermission,
				row.WorkerDuration,
				row.WorkerInputTokens,
				row.WorkerOutputTokens,
				row.WorkerTotalTokens,
				row.WorkerVerified,
				row.Phase,
				row.Status,
				row.VerifierVerdict,
				row.VerifierProvider,
				row.VerifierModel,
				row.VerifierModelSource,
				row.VerifierEffort,
				row.VerifierPermission,
				row.VerifierDuration,
				row.VerifierInputTokens,
				row.VerifierOutputTokens,
				row.VerifierTotalTokens,
				row.VerifierVerified,
			})
		}
	}

	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Safety")
	fmt.Fprintf(&out, "- status is read-only and local-only: read %s and wrote no repo, GitHub, PR, issue, commit, merge, doc, or tracked-file surface.\n", filepath.ToSlash(report.RunPath))
	return out.String()
}

func rowFromAttempt(attempt state.Attempt, metadata []metadataRecord, verifier *verifierRecord) Row {
	worker := attempt.Report
	usage := attempt.Usage
	if worker != nil {
		if worker.HasUsage() {
			usage = &worker.Usage
		} else {
			usage = nil
		}
	}
	input, output, total := formatUsage(usage)

	row := Row{
		Issue:                formatIssue(attempt.Issue),
		WorkerJob:            display(attempt.JobID),
		PR:                   display(metadataPR(attempt, metadata)),
		WorkerProvider:       display(firstNonEmpty(reportProvider(worker), attempt.Provider)),
		WorkerModel:          display(reportModel(worker)),
		WorkerModelSource:    display(reportModelSource(worker)),
		WorkerEffort:         display(reportEffort(worker)),
		WorkerPermission:     display(reportPermission(worker)),
		WorkerDuration:       formatDuration(worker),
		WorkerInputTokens:    input,
		WorkerOutputTokens:   output,
		WorkerTotalTokens:    total,
		WorkerVerified:       formatVerified(worker),
		Phase:                display(attempt.Phase),
		Status:               display(attempt.Status),
		VerifierVerdict:      NotReported,
		VerifierProvider:     NotReported,
		VerifierModel:        NotReported,
		VerifierModelSource:  NotReported,
		VerifierEffort:       NotReported,
		VerifierPermission:   NotReported,
		VerifierDuration:     NotReported,
		VerifierInputTokens:  NotReported,
		VerifierOutputTokens: NotReported,
		VerifierTotalTokens:  NotReported,
		VerifierVerified:     NotReported,
		issueNumber:          attempt.Issue,
		attemptNumber:        attempt.Attempt,
		workerJobSort:        attempt.JobID,
	}
	if verifier == nil {
		return row
	}

	verifierUsage := (*reporter.Usage)(nil)
	if verifier.Report != nil && verifier.Report.HasUsage() {
		verifierUsage = &verifier.Report.Usage
	}
	vInput, vOutput, vTotal := formatUsage(verifierUsage)
	if row.PR == NotReported {
		row.PR = display(verifier.PR)
	}
	row.VerifierVerdict = display(verifier.Verdict)
	row.VerifierProvider = display(reportProvider(verifier.Report))
	row.VerifierModel = display(reportModel(verifier.Report))
	row.VerifierModelSource = display(reportModelSource(verifier.Report))
	row.VerifierEffort = display(reportEffort(verifier.Report))
	row.VerifierPermission = display(reportPermission(verifier.Report))
	row.VerifierDuration = formatDuration(verifier.Report)
	row.VerifierInputTokens = vInput
	row.VerifierOutputTokens = vOutput
	row.VerifierTotalTokens = vTotal
	row.VerifierVerified = formatVerified(verifier.Report)
	row.verifierRecordSortKey = verifier.Path
	return row
}

func loadEventRecords(repoPath, runID string, now time.Time) ([]metadataRecord, []verifierRecord, int, error) {
	path := state.EventsPath(repoPath, runID)
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, 0, nil
		}
		return nil, nil, 0, fmt.Errorf("read events file: %w", err)
	}
	if err := checkRunStatusFileBounds(path, info, maxRunStatusEventBytes, now); err != nil {
		return nil, nil, 0, fmt.Errorf("read events file: %w", err)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("read events file: %w", err)
	}
	defer file.Close()

	var metadata []metadataRecord
	var verifiers []verifierRecord
	count := 0
	limited := &io.LimitedReader{R: file, N: maxRunStatusEventBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 1024), maxRunStatusEventLineBytes)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		count++
		records, verifierRecords := collectRecords([]byte(line), path)
		metadata = append(metadata, records...)
		verifiers = append(verifiers, verifierRecords...)
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, 0, fmt.Errorf("read events file: %w", err)
	}
	if limited.N <= 0 {
		return nil, nil, 0, fmt.Errorf("read events file: %s: file is too large while reading (%d byte limit)", filepath.ToSlash(path), maxRunStatusEventBytes)
	}
	return metadata, verifiers, count, nil
}

func validateRunStatusAttemptRecords(runPath string, now time.Time) error {
	var diagnostics runStatusDiagnostics
	scanRunStatusDir(runPath, "workers", isWorkerAttemptRecord, maxRunStatusRecordBytes, now, nil, &diagnostics)
	if diagnostics.empty() {
		return nil
	}
	return fmt.Errorf("scan worker records: %w", diagnostics)
}

func scanRunJSONRecords(runPath string, now time.Time) ([]metadataRecord, []verifierRecord, error) {
	var metadata []metadataRecord
	var verifiers []verifierRecord
	var diagnostics runStatusDiagnostics
	collect := func(data []byte, path string) {
		records, verifierRecords := collectRecords(data, path)
		metadata = append(metadata, records...)
		verifiers = append(verifiers, verifierRecords...)
	}

	scanRunStatusFile(runPath, "state.json", maxRunStatusRecordBytes, now, collect, &diagnostics)
	scanRunStatusDir(runPath, "verifiers", isVerifierRunRecord, maxRunStatusRecordBytes, now, collect, &diagnostics)
	scanRunStatusDir(runPath, "guardrails", isGuardrailRunRecord, maxRunStatusRecordBytes, now, collect, &diagnostics)

	if !diagnostics.empty() {
		return nil, nil, fmt.Errorf("scan run records: %w", diagnostics)
	}
	return metadata, verifiers, nil
}

type runStatusDiagnostics []string

func (d runStatusDiagnostics) Error() string {
	return "skipped unsafe local run status record(s): " + strings.Join(d, "; ")
}

func (d runStatusDiagnostics) empty() bool {
	return len(d) == 0
}

func (d *runStatusDiagnostics) add(format string, args ...any) {
	*d = append(*d, fmt.Sprintf(format, args...))
}

func scanRunStatusFile(runPath, rel string, maxBytes int64, now time.Time, collect func([]byte, string), diagnostics *runStatusDiagnostics) {
	path := filepath.Join(runPath, filepath.FromSlash(rel))
	info, err := os.Stat(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			diagnostics.add("%s: %v", filepath.ToSlash(path), err)
		}
		return
	}
	if err := checkRunStatusFileBounds(path, info, maxBytes, now); err != nil {
		diagnostics.add("%v", err)
		return
	}
	if collect == nil {
		return
	}
	data, err := readBoundedRunStatusFile(path, maxBytes)
	if err != nil {
		diagnostics.add("%v", err)
		return
	}
	collect(data, path)
}

func scanRunStatusDir(runPath, relDir string, match func(string) bool, maxBytes int64, now time.Time, collect func([]byte, string), diagnostics *runStatusDiagnostics) {
	dirPath := filepath.Join(runPath, filepath.FromSlash(relDir))
	dir, err := os.Open(dirPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			diagnostics.add("%s: %v", filepath.ToSlash(dirPath), err)
		}
		return
	}
	defer dir.Close()

	entries, err := dir.ReadDir(maxRunStatusDirectoryEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		diagnostics.add("%s: %v", filepath.ToSlash(dirPath), err)
		return
	}
	if len(entries) > maxRunStatusDirectoryEntries {
		diagnostics.add("%s: directory entry limit exceeded (%d > %d)", filepath.ToSlash(dirPath), len(entries), maxRunStatusDirectoryEntries)
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	for _, entry := range entries {
		path := filepath.Join(dirPath, entry.Name())
		if entry.IsDir() {
			diagnostics.add("%s: depth limit exceeded under %s", filepath.ToSlash(path), filepath.ToSlash(dirPath))
			continue
		}
		if !match(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			diagnostics.add("%s: %v", filepath.ToSlash(path), err)
			continue
		}
		if err := checkRunStatusFileBounds(path, info, maxBytes, now); err != nil {
			diagnostics.add("%v", err)
			continue
		}
		if collect == nil {
			continue
		}
		data, err := readBoundedRunStatusFile(path, maxBytes)
		if err != nil {
			diagnostics.add("%v", err)
			continue
		}
		collect(data, path)
	}
}

func checkRunStatusFileBounds(path string, info os.FileInfo, maxBytes int64, now time.Time) error {
	slashPath := filepath.ToSlash(path)
	if info.IsDir() {
		return fmt.Errorf("%s: expected file, found directory", slashPath)
	}
	if info.Size() > maxBytes {
		return fmt.Errorf("%s: file is too large (%d bytes > %d byte limit)", slashPath, info.Size(), maxBytes)
	}
	if info.ModTime().UTC().After(now.Add(maxRunStatusFutureSkew)) {
		return fmt.Errorf("%s: file mtime %s exceeds future skew limit %s", slashPath, info.ModTime().UTC().Format(time.RFC3339), maxRunStatusFutureSkew)
	}
	return nil
}

func readBoundedRunStatusFile(path string, maxBytes int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.ToSlash(path), err)
	}
	defer file.Close()

	limited := &io.LimitedReader{R: file, N: maxBytes + 1}
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.ToSlash(path), err)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%s: file is too large while reading (%d bytes > %d byte limit)", filepath.ToSlash(path), len(data), maxBytes)
	}
	return data, nil
}

func isWorkerAttemptRecord(name string) bool {
	return strings.HasSuffix(name, ".attempt.json")
}

func isVerifierRunRecord(name string) bool {
	return strings.HasSuffix(name, ".loopreview.json")
}

func isGuardrailRunRecord(name string) bool {
	trimmed := strings.TrimSuffix(name, ".json")
	if trimmed == name || trimmed == "" {
		return false
	}
	issue, err := strconv.Atoi(trimmed)
	return err == nil && issue > 0
}

func collectRecords(data []byte, path string) ([]metadataRecord, []verifierRecord) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, nil
	}
	var metadata []metadataRecord
	var verifiers []verifierRecord
	var walk func(any)
	walk = func(current any) {
		switch typed := current.(type) {
		case map[string]any:
			if record := metadataFromMap(typed, path); record.Issue > 0 || record.JobID != "" || record.PR != "" {
				metadata = append(metadata, record)
			}
			if verifier, ok := verifierFromMap(typed, path); ok {
				verifiers = append(verifiers, verifier)
			}
			for _, child := range typed {
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(value)
	return metadata, verifiers
}

func metadataFromMap(values map[string]any, path string) metadataRecord {
	return metadataRecord{
		Issue: firstInt(values, "issue", "issue_number"),
		JobID: firstString(values, "job_id", "worker_job_id", "attempt_job_id"),
		PR:    firstPR(values),
		Path:  path,
	}
}

func verifierFromMap(values map[string]any, path string) (verifierRecord, bool) {
	record := verifierRecord{
		Issue:   firstInt(values, "issue", "issue_number"),
		JobID:   firstString(values, "job_id", "worker_job_id", "attempt_job_id"),
		PR:      firstPR(values),
		Verdict: firstString(values, "verdict", "verifier_verdict"),
		Path:    path,
	}
	if reportValue, ok := values["report"]; ok {
		record.Report = parseReport(reportValue)
	} else if reportValue, ok := values["attestation"]; ok {
		record.Report = parseReport(reportValue)
	}
	if record.Report != nil && record.Report.Role != reporter.RoleVerifier {
		record.Report = nil
	}
	if record.PR == "" && record.Report != nil {
		record.PR = parsePRFromAction(record.Report.Action)
	}
	if record.Issue == 0 && record.Report != nil {
		record.Issue = parseIssueFromAction(record.Report.Action)
	}

	if record.Verdict == "" && record.Report == nil {
		return verifierRecord{}, false
	}
	return record, true
}

func parseReport(value any) *reporter.Report {
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var record reporter.Report
	if err := json.Unmarshal(data, &record); err != nil {
		return nil
	}
	return &record
}

func dedupeVerifierRecords(records []verifierRecord) []verifierRecord {
	seen := map[string]bool{}
	out := make([]verifierRecord, 0, len(records))
	for _, record := range records {
		key := strings.Join([]string{
			strconv.Itoa(record.Issue),
			record.JobID,
			normalizePRID(record.PR),
			record.Verdict,
			reportHeader(record.Report),
			filepath.ToSlash(record.Path),
		}, "\x00")
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, record)
	}
	return out
}

func sortVerifierRecords(records []verifierRecord) {
	sort.Slice(records, func(i, j int) bool {
		if records[i].Issue != records[j].Issue {
			return records[i].Issue < records[j].Issue
		}
		if normalizePRID(records[i].PR) != normalizePRID(records[j].PR) {
			return normalizePRID(records[i].PR) < normalizePRID(records[j].PR)
		}
		if records[i].JobID != records[j].JobID {
			return records[i].JobID < records[j].JobID
		}
		if records[i].Verdict != records[j].Verdict {
			return records[i].Verdict < records[j].Verdict
		}
		return filepath.ToSlash(records[i].Path) < filepath.ToSlash(records[j].Path)
	})
}

func sortRows(rows []Row) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].issueNumber != rows[j].issueNumber {
			return rows[i].issueNumber < rows[j].issueNumber
		}
		if rows[i].attemptNumber != rows[j].attemptNumber {
			return rows[i].attemptNumber < rows[j].attemptNumber
		}
		if rows[i].workerJobSort != rows[j].workerJobSort {
			return rows[i].workerJobSort < rows[j].workerJobSort
		}
		return rows[i].verifierRecordSortKey < rows[j].verifierRecordSortKey
	})
}

func matchingVerifier(attempt state.Attempt, pr string, records []verifierRecord) *verifierRecord {
	if len(records) == 0 {
		return nil
	}
	prID := normalizePRID(pr)
	for _, record := range records {
		if record.Issue > 0 && record.Issue == attempt.Issue {
			return &record
		}
	}
	for _, record := range records {
		if record.JobID != "" && record.JobID == attempt.JobID {
			return &record
		}
	}
	if prID != "" {
		for _, record := range records {
			if normalizePRID(record.PR) == prID {
				return &record
			}
		}
	}
	if len(records) == 1 && records[0].Issue == 0 && records[0].JobID == "" && normalizePRID(records[0].PR) == "" {
		return &records[0]
	}
	return nil
}

func metadataPR(attempt state.Attempt, records []metadataRecord) string {
	for _, record := range records {
		if record.PR == "" {
			continue
		}
		if record.Issue > 0 && record.Issue == attempt.Issue {
			return record.PR
		}
	}
	for _, record := range records {
		if record.PR == "" {
			continue
		}
		if record.JobID != "" && record.JobID == attempt.JobID {
			return record.PR
		}
	}
	return ""
}

func firstString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		raw, ok := values[key]
		if !ok {
			continue
		}
		switch typed := raw.(type) {
		case string:
			if trimmed := strings.TrimSpace(typed); trimmed != "" {
				return trimmed
			}
		case json.Number:
			if trimmed := strings.TrimSpace(typed.String()); trimmed != "" {
				return trimmed
			}
		case float64:
			if typed != 0 {
				return strconv.FormatInt(int64(typed), 10)
			}
		}
	}
	return ""
}

func firstInt(values map[string]any, keys ...string) int {
	for _, key := range keys {
		raw, ok := values[key]
		if !ok {
			continue
		}
		switch typed := raw.(type) {
		case json.Number:
			value, err := strconv.Atoi(typed.String())
			if err == nil && value > 0 {
				return value
			}
		case float64:
			if typed > 0 {
				return int(typed)
			}
		case string:
			value, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(typed, "#")))
			if err == nil && value > 0 {
				return value
			}
		}
	}
	return 0
}

func firstPR(values map[string]any) string {
	if pr := firstString(values, "pr", "pr_url", "pull_request", "pull_request_url"); pr != "" {
		return pr
	}
	if prNumber := firstInt(values, "pr_number", "pull_request_number"); prNumber > 0 {
		return "#" + strconv.Itoa(prNumber)
	}
	return ""
}

func formatIssue(issue int) string {
	if issue <= 0 {
		return NotReported
	}
	return "#" + strconv.Itoa(issue)
}

func display(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return NotReported
	}
	return value
}

func reportProvider(record *reporter.Report) string {
	if record == nil {
		return ""
	}
	return record.Provider
}

func reportModel(record *reporter.Report) string {
	if record == nil {
		return ""
	}
	return record.Model
}

func reportModelSource(record *reporter.Report) string {
	if record == nil {
		return ""
	}
	return string(record.ModelSource)
}

func reportEffort(record *reporter.Report) string {
	if record == nil {
		return ""
	}
	return record.Effort
}

func reportPermission(record *reporter.Report) string {
	if record == nil {
		return ""
	}
	return string(record.Permission)
}

func formatDuration(record *reporter.Report) string {
	if record == nil || !record.HasDurationMS() {
		return NotReported
	}
	return (time.Duration(record.DurationMS) * time.Millisecond).String()
}

func formatVerified(record *reporter.Report) string {
	if record == nil || !record.HasVerified() {
		return NotReported
	}
	return strconv.FormatBool(record.Verified)
}

func formatUsage(usage *reporter.Usage) (string, string, string) {
	if usage == nil {
		return NotReported, NotReported, NotReported
	}
	input := NotReported
	output := NotReported
	total := NotReported
	if usage.InputTokens != nil {
		input = strconv.FormatInt(*usage.InputTokens, 10)
	}
	if usage.OutputTokens != nil {
		output = strconv.FormatInt(*usage.OutputTokens, 10)
	}
	if usage.TotalTokens != nil {
		total = strconv.FormatInt(*usage.TotalTokens, 10)
	}
	return input, output, total
}

func writeTableRow(out *bytes.Buffer, values []string) {
	fmt.Fprint(out, "|")
	for _, value := range values {
		fmt.Fprintf(out, " %s |", escapeTableValue(value))
	}
	fmt.Fprintln(out)
}

func escapeTableValue(value string) string {
	value = display(value)
	value = strings.ReplaceAll(value, "\r\n", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.ReplaceAll(value, "|", `\|`)
	return value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func reportHeader(record *reporter.Report) string {
	if record == nil {
		return ""
	}
	return record.Header()
}

var actionPRPattern = regexp.MustCompile(`(?i)\bPR\s*#?([1-9]\d*)\b`)
var actionIssuePattern = regexp.MustCompile(`(?i)\bissue\s*#?([1-9]\d*)\b`)

func parsePRFromAction(action string) string {
	match := actionPRPattern.FindStringSubmatch(action)
	if len(match) != 2 {
		return ""
	}
	return "#" + match[1]
}

func parseIssueFromAction(action string) int {
	match := actionIssuePattern.FindStringSubmatch(action)
	if len(match) != 2 {
		return 0
	}
	value, err := strconv.Atoi(match[1])
	if err != nil {
		return 0
	}
	return value
}

func normalizePRID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	trimmed := strings.TrimPrefix(value, "#")
	if _, err := strconv.Atoi(trimmed); err == nil {
		return trimmed
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return value
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	for i, part := range parts {
		if part == "pull" && i+1 < len(parts) {
			if _, err := strconv.Atoi(parts[i+1]); err == nil {
				return parts[i+1]
			}
		}
	}
	return value
}
