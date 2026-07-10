// Package reportquery lists local-only reporter records without mutating state.
package reportquery

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/home"
	"github.com/jasonhnd/loopcoder/internal/loopreview"
	"github.com/jasonhnd/loopcoder/internal/migration"
	"github.com/jasonhnd/loopcoder/internal/registry"
	"github.com/jasonhnd/loopcoder/internal/relaygate"
	"github.com/jasonhnd/loopcoder/internal/reporter"
	"github.com/jasonhnd/loopcoder/internal/state"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

const (
	DefaultLimit        = 20
	notReported         = "not reported"
	maxReportFileBytes  = 4 * 1024 * 1024
	maxReportLineBytes  = 1024 * 1024
	maxReportDirEntries = 20000
)

var reporterHeaderRe = regexp.MustCompile(fmt.Sprintf(`^\[(?:%s|%s)\]\s+`, regexp.QuoteMeta(migration.ReporterHeaderToken), regexp.QuoteMeta(migration.LegacyReporterHeaderToken)))

type Options struct {
	RepoPath     string
	WorkID       string
	Issue        int
	Role         reporter.Role
	Limit        int
	SkipImported bool
}

type Record struct {
	Report          reporter.Report      `json:"report"`
	Source          string               `json:"source,omitempty"`
	RunID           string               `json:"run_id,omitempty"`
	Path            string               `json:"path,omitempty"`
	Status          string               `json:"status,omitempty"`
	Error           string               `json:"error,omitempty"`
	PR              string               `json:"pr,omitempty"`
	PRNumber        int                  `json:"pr_number,omitempty"`
	Verdict         string               `json:"verdict,omitempty"`
	SpecConformance string               `json:"spec_conformance,omitempty"`
	Evidence        string               `json:"evidence,omitempty"`
	Findings        []loopreview.Finding `json:"findings,omitempty"`
	modTime         time.Time
}

func List(opts Options) ([]Record, error) {
	repoPath := strings.TrimSpace(opts.RepoPath)
	if repoPath == "" {
		repoPath = "."
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}

	var records []Record
	if !opts.SkipImported {
		importedRecords, err := loadImportedReports(repoPath)
		if err != nil {
			return nil, err
		}
		records = append(records, importedRecords...)
	}
	runRecords, err := loadRunReports(repoPath)
	if err != nil {
		return nil, err
	}
	records = append(records, runRecords...)
	relayRecords, err := loadRelayReports(repoPath)
	if err != nil {
		return nil, err
	}
	records = append(records, relayRecords...)

	records = dedupe(records)
	records = filter(records, opts)
	sortRecords(records)
	if len(records) > limit {
		records = records[:limit]
	}
	return records, nil
}

func loadImportedReports(repoPath string) ([]Record, error) {
	layout, err := home.Resolve(home.DefaultDeps())
	if err != nil {
		return nil, nil
	}
	dbPath := layout.DatabasePath()
	if _, err := os.Stat(dbPath); err != nil {
		return nil, nil
	}
	ctx := context.Background()
	show, err := registry.Show(ctx, registry.Options{RepoPath: repoPath, DatabasePath: dbPath}, registry.DefaultDeps())
	if err != nil || !show.Registered {
		return nil, nil
	}
	store, err := storage.Open(ctx, storage.Options{Path: dbPath})
	if err != nil {
		return nil, fmt.Errorf("read imported reports: %w", err)
	}
	defer store.Close()

	var records []Record
	err = store.WithTx(ctx, func(tx storage.Tx) error {
		rows, err := tx.Query(ctx, `SELECT COALESCE(run_id, ''), payload_json, source_kind, source_path, created_at FROM reports WHERE project_id = ? ORDER BY ended_at DESC, started_at DESC, created_at DESC, id DESC`, show.Project.ProjectID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var runID string
			var payload string
			var sourceKind string
			var sourcePath string
			var createdAt string
			if err := rows.Scan(&runID, &payload, &sourceKind, &sourcePath, &createdAt); err != nil {
				return err
			}
			var report reporter.Report
			if err := json.Unmarshal([]byte(payload), &report); err != nil {
				continue
			}
			records = append(records, Record{
				Report:  report,
				Source:  importedSource(sourceKind),
				RunID:   runID,
				Path:    sourcePath,
				modTime: parseTime(createdAt),
			})
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("read imported reports: %w", err)
	}
	return records, nil
}

func importedSource(sourceKind string) string {
	sourceKind = strings.TrimSpace(sourceKind)
	if sourceKind == "" {
		return "imported"
	}
	return "imported:" + sourceKind
}

func RenderText(records []Record) string {
	var out strings.Builder
	if len(records) == 0 {
		fmt.Fprintln(&out, "loopcoder report: no records")
		fmt.Fprintln(&out)
		fmt.Fprintln(&out, "Next")
		fmt.Fprintln(&out, "- run a worker, verifier, or conductor command to create local reporter records")
		return out.String()
	}
	for i, record := range records {
		if i > 0 {
			fmt.Fprintln(&out)
		}
		renderReceipt(&out, record)
	}
	return out.String()
}

func RenderVerboseText(records []Record) string {
	var out strings.Builder
	fmt.Fprintln(&out, "REPORTS")
	if len(records) == 0 {
		fmt.Fprintln(&out, "- none")
		return out.String()
	}
	for _, record := range records {
		r := record.Report
		fmt.Fprintf(&out, "- work_id: %s\n", display(r.WorkID))
		fmt.Fprintf(&out, "  source: %s\n", display(record.Source))
		fmt.Fprintf(&out, "  run_id: %s\n", display(record.RunID))
		fmt.Fprintf(&out, "  path: %s\n", display(record.Path))
		if strings.TrimSpace(record.Status) != "" {
			fmt.Fprintf(&out, "  status: %s\n", record.Status)
		}
		if strings.TrimSpace(record.Error) != "" {
			fmt.Fprintf(&out, "  error: %s\n", record.Error)
		}
		fmt.Fprintf(&out, "  role: %s\n", display(string(r.Role)))
		fmt.Fprintf(&out, "  provider: %s\n", display(r.Provider))
		fmt.Fprintf(&out, "  model: %s\n", display(reporter.ModelDepthDisplay(r.Model, r.Effort)))
		if r.Issue > 0 {
			fmt.Fprintf(&out, "  issue: #%d\n", r.Issue)
		}
		if strings.TrimSpace(r.Branch) != "" {
			fmt.Fprintf(&out, "  branch: %s\n", r.Branch)
		}
		if r.Round > 0 {
			fmt.Fprintf(&out, "  round: %d\n", r.Round)
		}
		if record.PRNumber > 0 {
			fmt.Fprintf(&out, "  pr_number: %d\n", record.PRNumber)
		}
		if strings.TrimSpace(record.PR) != "" {
			fmt.Fprintf(&out, "  pr: %s\n", record.PR)
		}
		if strings.TrimSpace(record.Verdict) != "" {
			fmt.Fprintf(&out, "  verdict: %s\n", record.Verdict)
		}
		if strings.TrimSpace(record.SpecConformance) != "" {
			fmt.Fprintf(&out, "  spec_conformance: %s\n", record.SpecConformance)
		}
		if strings.TrimSpace(record.Evidence) != "" {
			fmt.Fprintf(&out, "  evidence: %s\n", compactLine(record.Evidence))
		}
		if len(record.Findings) > 0 {
			fmt.Fprintf(&out, "  findings: %s\n", formatFindingSummary(record.Findings))
			for i, finding := range record.Findings {
				fmt.Fprintf(&out, "    - %d: %s\n", i+1, formatFindingDetail(finding))
			}
		}
		fmt.Fprintf(&out, "  result: %s\n", resultStatus(r))
		fmt.Fprintf(&out, "  exit: %d\n", r.ExitCode)
		fmt.Fprintf(&out, "  duration: %s\n", formatDuration(r))
		fmt.Fprintf(&out, "  started: %s\n", display(r.StartedAt))
		fmt.Fprintf(&out, "  ended: %s\n", display(r.EndedAt))
		fmt.Fprintf(&out, "  tokens: %s\n", formatUsage(r.Usage))
	}
	return out.String()
}

func renderReceipt(out *strings.Builder, record Record) {
	r := record.Report
	status := receiptStatus(record)
	fmt.Fprintf(out, "loopcoder report: %s %s\n\n", display(string(r.Role)), status)
	fmt.Fprintln(out, "Target")
	fmt.Fprintf(out, "- issue: %s\n", displayIssue(r.Issue))
	fmt.Fprintf(out, "- PR: %s\n", displayPR(record))
	fmt.Fprintf(out, "- branch: %s\n", display(r.Branch))
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Verdict")
	fmt.Fprintf(out, "- status: %s\n", status)
	fmt.Fprintf(out, "- blocking: %s\n", blockingStatus(status))
	fmt.Fprintf(out, "- reason: %s\n", receiptReason(record))
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Review summary")
	if strings.TrimSpace(record.SpecConformance) != "" {
		fmt.Fprintf(out, "- spec conformance: %s\n", record.SpecConformance)
	} else {
		fmt.Fprintln(out, "- spec conformance: not reported")
	}
	fmt.Fprintf(out, "- findings: %s\n", formatFindingSummary(record.Findings))
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Run")
	fmt.Fprintf(out, "- work id: %s\n", display(r.WorkID))
	fmt.Fprintf(out, "- source: %s\n", display(record.Source))
	fmt.Fprintf(out, "- run id: %s\n", display(record.RunID))
	fmt.Fprintf(out, "- %s: %s / %s / %s\n", display(string(r.Role)), display(r.Provider), display(reporter.ModelDepthDisplay(r.Model, r.Effort)), display(string(r.Permission)))
	fmt.Fprintf(out, "- duration: %s\n", formatDuration(r))
	fmt.Fprintf(out, "- tokens: %s\n", formatUsage(r.Usage))
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Next")
	for _, line := range nextActions(record, status) {
		fmt.Fprintf(out, "- %s\n", line)
	}
}

func MarshalJSON(records []Record) ([]byte, error) {
	return MarshalJSONWithRunTree(records, nil)
}

func MarshalJSONWithRunTree(records []Record, runTree any) ([]byte, error) {
	reports := make([]reporter.Report, 0, len(records))
	jsonRecords := make([]jsonRecord, 0, len(records))
	for _, record := range records {
		reports = append(reports, record.Report)
		jsonRecords = append(jsonRecords, jsonRecord{
			Report:          record.Report,
			Source:          record.Source,
			RunID:           record.RunID,
			Path:            record.Path,
			Status:          record.Status,
			Error:           record.Error,
			PR:              record.PR,
			PRNumber:        record.PRNumber,
			Verdict:         record.Verdict,
			SpecConformance: record.SpecConformance,
			Evidence:        record.Evidence,
			Findings:        record.Findings,
		})
	}
	payload := struct {
		Reports []reporter.Report `json:"reports"`
		Records []jsonRecord      `json:"records"`
		RunTree any               `json:"run_tree,omitempty"`
	}{Reports: reports, Records: jsonRecords, RunTree: runTree}
	return json.Marshal(payload)
}

type jsonRecord struct {
	Report          reporter.Report      `json:"report"`
	Source          string               `json:"source"`
	RunID           string               `json:"run_id"`
	Path            string               `json:"path"`
	Status          string               `json:"status,omitempty"`
	Error           string               `json:"error,omitempty"`
	PR              string               `json:"pr,omitempty"`
	PRNumber        int                  `json:"pr_number,omitempty"`
	Verdict         string               `json:"verdict,omitempty"`
	SpecConformance string               `json:"spec_conformance,omitempty"`
	Evidence        string               `json:"evidence,omitempty"`
	Findings        []loopreview.Finding `json:"findings,omitempty"`
}

func loadRunReports(repoPath string) ([]Record, error) {
	runsRoot := state.RunsRoot(repoPath)
	entries, err := os.ReadDir(runsRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read runs directory: %w", err)
	}
	if len(entries) > maxReportDirEntries {
		return nil, fmt.Errorf("too many run entries under %s", filepath.ToSlash(runsRoot))
	}

	var records []Record
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		runID := entry.Name()
		attempts, err := state.LoadAttempts(repoPath, runID)
		if err != nil {
			return nil, err
		}
		for _, attempt := range attempts {
			if attempt.Report == nil {
				continue
			}
			record := *attempt.Report
			enrichFromAttempt(&record, runID, attempt)
			records = append(records, Record{
				Report:  record,
				Source:  "attempt",
				RunID:   runID,
				Path:    attempt.Path,
				Status:  attempt.Status,
				Error:   attempt.Error,
				modTime: attempt.LastWriteUTC,
			})
		}

		generic, err := scanRunJSONReports(state.RunPath(repoPath, runID), runID)
		if err != nil {
			return nil, err
		}
		records = append(records, generic...)
	}
	return records, nil
}

func enrichFromAttempt(record *reporter.Report, runID string, attempt state.Attempt) {
	if strings.TrimSpace(record.WorkID) == "" {
		record.WorkID = runID
	}
	if record.Issue == 0 {
		record.Issue = attempt.Issue
	}
	if strings.TrimSpace(record.Branch) == "" {
		record.Branch = attempt.Branch
	}
	if record.Round == 0 {
		record.Round = attempt.Attempt
	}
}

func scanRunJSONReports(runPath, runID string) ([]Record, error) {
	var records []Record
	err := filepath.WalkDir(runPath, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		name := entry.Name()
		if strings.HasSuffix(name, ".attempt.json") {
			return nil
		}
		if !strings.HasSuffix(name, ".json") && !strings.HasSuffix(name, ".jsonl") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		if info.Size() > maxReportFileBytes {
			return nil
		}
		if strings.HasSuffix(name, ".jsonl") {
			found, err := collectJSONLReports(path, info.ModTime().UTC(), runID)
			if err != nil {
				return err
			}
			records = append(records, found...)
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		records = append(records, collectReportRecords(data, "run-json", runID, path, info.ModTime().UTC())...)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan run reports: %w", err)
	}
	return records, nil
}

func collectJSONLReports(path string, modTime time.Time, runID string) ([]Record, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var records []Record
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024), maxReportLineBytes)
	for scanner.Scan() {
		records = append(records, collectReportRecords(scanner.Bytes(), "run-jsonl", runID, path, modTime)...)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return records, nil
}

func collectReports(data []byte) []reporter.Report {
	records := collectReportRecords(data, "", "", "", time.Time{})
	reports := make([]reporter.Report, 0, len(records))
	for _, record := range records {
		reports = append(reports, record.Report)
	}
	return reports
}

func collectReportRecords(data []byte, source, runID, path string, modTime time.Time) []Record {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil
	}
	var records []Record
	var walk func(any)
	walk = func(current any) {
		switch typed := current.(type) {
		case map[string]any:
			if looksLikeReport(typed) {
				if report, ok := parseReportValue(typed); ok {
					records = append(records, recordFromContext(report, typed, source, runID, path, modTime))
				}
			}
			if raw, ok := typed[migration.ReportStateKey]; ok {
				if report, ok := parseReportValue(raw); ok {
					records = append(records, recordFromContext(report, typed, source, runID, path, modTime))
				}
			} else if raw, ok := typed[migration.LegacyReportStateKey]; ok {
				if report, ok := parseReportValue(raw); ok {
					records = append(records, recordFromContext(report, typed, source, runID, path, modTime))
				}
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
	return records
}

func looksLikeReport(values map[string]any) bool {
	_, hasRole := values["role"]
	_, hasProvider := values["provider"]
	_, hasModel := values["model"]
	return hasRole && hasProvider && hasModel
}

func parseReportValue(value any) (reporter.Report, bool) {
	data, err := json.Marshal(value)
	if err != nil {
		return reporter.Report{}, false
	}
	var report reporter.Report
	if err := json.Unmarshal(data, &report); err != nil {
		return reporter.Report{}, false
	}
	return report, true
}

func recordFromContext(report reporter.Report, context map[string]any, source, runID, path string, modTime time.Time) Record {
	record := Record{
		Report:          report,
		Source:          source,
		RunID:           firstNonEmptyString(runID, stringField(context, "run_id")),
		Path:            path,
		Status:          stringField(context, "status"),
		Error:           stringField(context, "error"),
		PR:              stringField(context, "pr"),
		PRNumber:        intField(context, "pr_number"),
		Verdict:         stringField(context, "verdict"),
		SpecConformance: stringField(context, "spec_conformance"),
		Evidence:        stringField(context, "evidence"),
		Findings:        findingsField(context, "findings"),
		modTime:         modTime,
	}
	if record.PRNumber == 0 {
		record.PRNumber = prNumberFromAction(report.Action)
	}
	return record
}

func stringField(values map[string]any, name string) string {
	value, ok := values[name]
	if !ok || value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case json.Number:
		return strings.TrimSpace(typed.String())
	default:
		return strings.TrimSpace(fmt.Sprint(typed))
	}
}

func intField(values map[string]any, name string) int {
	value, ok := values[name]
	if !ok || value == nil {
		return 0
	}
	switch typed := value.(type) {
	case json.Number:
		parsed, _ := typed.Int64()
		return int(parsed)
	case float64:
		return int(typed)
	case int:
		return typed
	case string:
		parsed, _ := strconv.Atoi(strings.TrimSpace(typed))
		return parsed
	default:
		return 0
	}
}

func findingsField(values map[string]any, name string) []loopreview.Finding {
	value, ok := values[name]
	if !ok || value == nil {
		return nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var findings []loopreview.Finding
	if err := json.Unmarshal(data, &findings); err != nil {
		return nil
	}
	return findings
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func prNumberFromAction(action string) int {
	action = strings.ToLower(action)
	index := strings.Index(action, "pr #")
	if index < 0 {
		return 0
	}
	rest := action[index+len("pr #"):]
	var digits strings.Builder
	for _, r := range rest {
		if r < '0' || r > '9' {
			break
		}
		digits.WriteRune(r)
	}
	if digits.Len() == 0 {
		return 0
	}
	parsed, _ := strconv.Atoi(digits.String())
	return parsed
}

func loadRelayReports(repoPath string) ([]Record, error) {
	var records []Record
	pending, err := loadPendingRelayReports(repoPath)
	if err != nil {
		return nil, err
	}
	records = append(records, pending...)

	relayRoot := filepath.Join(repoPath, ".loopcoder", "relay")
	err = filepath.WalkDir(relayRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".attest") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		if info.Size() > maxReportFileBytes {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		found := collectRelayLedgerReports(data, path, info.ModTime().UTC())
		records = append(records, found...)
		return nil
	})
	if err != nil {
		if os.IsNotExist(err) {
			return records, nil
		}
		return nil, fmt.Errorf("scan relay reports: %w", err)
	}
	return records, nil
}

func loadPendingRelayReports(repoPath string) ([]Record, error) {
	dir := filepath.Join(repoPath, ".loopcoder", "relay", "pending")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read pending relay reports: %w", err)
	}
	var records []Record
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		info, err := entry.Info()
		if err != nil || info.Size() > maxReportFileBytes {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var pending relaygate.Record
		if err := json.Unmarshal(data, &pending); err != nil {
			continue
		}
		if pending.Report != nil {
			records = append(records, Record{
				Report:  *pending.Report,
				Source:  "relay-pending",
				RunID:   pending.RunID,
				Path:    path,
				modTime: info.ModTime().UTC(),
			})
			continue
		}
		if report, ok := parsePrettyHeaderBlock(pending.Block); ok {
			records = append(records, Record{
				Report:  report,
				Source:  "relay-pending",
				RunID:   pending.RunID,
				Path:    path,
				modTime: info.ModTime().UTC(),
			})
		}
	}
	return records, nil
}

func collectRelayLedgerReports(data []byte, path string, modTime time.Time) []Record {
	var jsonRecords []Record
	var headerRecords []Record
	var currentRunID string
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 1024), maxReportLineBytes)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if key, value, ok := relayMeta(line); ok {
			if key == "run_id" {
				currentRunID = value
			}
			if key == "report_json" {
				var report reporter.Report
				if err := json.Unmarshal([]byte(value), &report); err == nil {
					jsonRecords = append(jsonRecords, Record{
						Report:  report,
						Source:  "relay-ledger",
						RunID:   currentRunID,
						Path:    path,
						modTime: modTime,
					})
				}
			}
			continue
		}
		if report, ok := parseHeader(line); ok {
			if report.WorkID == "" {
				report.WorkID = currentRunID
			}
			headerRecords = append(headerRecords, Record{
				Report:  report,
				Source:  "relay-ledger",
				RunID:   currentRunID,
				Path:    path,
				modTime: modTime,
			})
		}
	}
	if len(jsonRecords) > 0 {
		return jsonRecords
	}
	return headerRecords
}

func relayMeta(line string) (string, string, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "# ") {
		return "", "", false
	}
	key, value, ok := strings.Cut(strings.TrimPrefix(line, "# "), "=")
	if !ok {
		return "", "", false
	}
	return strings.TrimSpace(key), strings.TrimSpace(value), true
}

func parsePrettyHeaderBlock(block string) (reporter.Report, bool) {
	scanner := bufio.NewScanner(strings.NewReader(block))
	scanner.Buffer(make([]byte, 1024), maxReportLineBytes)
	for scanner.Scan() {
		if report, ok := parseHeader(scanner.Text()); ok {
			return report, true
		}
	}
	return reporter.Report{}, false
}

func parseHeader(line string) (reporter.Report, bool) {
	line = strings.TrimSpace(line)
	if !reporterHeaderRe.MatchString(line) {
		return reporter.Report{}, false
	}
	fields := parseHeaderFields(reporterHeaderRe.ReplaceAllString(line, ""))
	if len(fields) == 0 {
		return reporter.Report{}, false
	}
	var report reporter.Report
	report.Role = reporter.Role(fields["role"])
	report.Provider = fields["provider"]
	report.Model, report.ModelSource = parseHeaderModel(fields["model"])
	report.Effort = fields["effort"]
	report.Permission = reporter.Permission(fields["perm"])
	report.Action = unquoteHeaderValue(fields["action"])
	report.ExitCode = parseInt(fields["exit"])
	report.DurationMS = parseDurationMS(fields["dur"])
	report.Usage = parseHeaderUsage(fields["tokens"])
	report.Verified = strings.EqualFold(fields["verified"], "true")
	return report, true
}

func parseHeaderFields(value string) map[string]string {
	fields := map[string]string{}
	for len(value) > 0 {
		value = strings.TrimLeft(value, " \t")
		if value == "" {
			break
		}
		key, rest, ok := strings.Cut(value, "=")
		if !ok {
			break
		}
		key = strings.TrimSpace(key)
		rest = strings.TrimLeft(rest, " \t")
		if strings.HasPrefix(rest, `"`) {
			parsed, tail, ok := scanQuoted(rest)
			if !ok {
				break
			}
			fields[key] = parsed
			value = tail
			continue
		}
		next := strings.IndexAny(rest, " \t")
		if next < 0 {
			fields[key] = rest
			break
		}
		fields[key] = rest[:next]
		value = rest[next+1:]
	}
	return fields
}

func scanQuoted(value string) (string, string, bool) {
	for i := 1; i < len(value); i++ {
		if value[i] == '\\' {
			i++
			continue
		}
		if value[i] == '"' {
			raw := value[:i+1]
			parsed, err := strconv.Unquote(raw)
			if err != nil {
				return "", "", false
			}
			return parsed, value[i+1:], true
		}
	}
	return "", "", false
}

func parseHeaderModel(value string) (string, reporter.ModelSource) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", ""
	}
	open := strings.LastIndex(value, "(")
	close := strings.LastIndex(value, ")")
	if open <= 0 || close != len(value)-1 || close <= open {
		return value, ""
	}
	return strings.TrimSpace(value[:open]), reporter.ModelSource(strings.TrimSpace(value[open+1 : close]))
}

func unquoteHeaderValue(value string) string {
	parsed, err := strconv.Unquote(value)
	if err != nil {
		return value
	}
	return parsed
}

func parseInt(value string) int {
	parsed, _ := strconv.Atoi(strings.TrimSpace(value))
	return parsed
}

func parseDurationMS(value string) int64 {
	duration, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil {
		return 0
	}
	return duration.Milliseconds()
}

func parseHeaderUsage(value string) reporter.Usage {
	var usage reporter.Usage
	value = strings.TrimSpace(value)
	if value == "" || value == "missing" || value == "unset" {
		return usage
	}
	splitPart, totalPart, hasTotal := strings.Cut(value, "|")
	if strings.Contains(splitPart, "/") {
		input, output, ok := strings.Cut(splitPart, "/")
		if ok {
			if parsed, ok := parseInt64Ptr(input); ok {
				usage.InputTokens = parsed
			}
			if parsed, ok := parseInt64Ptr(output); ok {
				usage.OutputTokens = parsed
			}
		}
	} else if parsed, ok := parseInt64Ptr(splitPart); ok && !hasTotal {
		usage.TotalTokens = parsed
	}
	if hasTotal {
		if parsed, ok := parseInt64Ptr(totalPart); ok {
			usage.TotalTokens = parsed
		}
	}
	return usage
}

func parseInt64Ptr(value string) (*int64, bool) {
	parsed, err := strconv.ParseInt(strings.TrimSpace(strings.ReplaceAll(value, ",", "")), 10, 64)
	if err != nil {
		return nil, false
	}
	return &parsed, true
}

func dedupe(records []Record) []Record {
	seen := map[string]bool{}
	out := make([]Record, 0, len(records))
	for _, record := range records {
		key := dedupeKey(record.Report)
		if key == "" {
			key = record.Source + "\x00" + record.Path
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, record)
	}
	return out
}

func dedupeKey(report reporter.Report) string {
	return strings.Join([]string{
		strings.TrimSpace(report.WorkID),
		string(report.Role),
		strings.TrimSpace(report.Provider),
		strings.TrimSpace(report.Action),
		strings.TrimSpace(report.StartedAt),
		strings.TrimSpace(report.EndedAt),
		strconv.Itoa(report.ExitCode),
	}, "\x00")
}

func filter(records []Record, opts Options) []Record {
	out := make([]Record, 0, len(records))
	for _, record := range records {
		report := record.Report
		if strings.TrimSpace(opts.WorkID) != "" && report.WorkID != strings.TrimSpace(opts.WorkID) {
			continue
		}
		if opts.Issue > 0 && report.Issue != opts.Issue {
			continue
		}
		if strings.TrimSpace(string(opts.Role)) != "" && report.Role != opts.Role {
			continue
		}
		out = append(out, record)
	}
	return out
}

func sortRecords(records []Record) {
	sort.SliceStable(records, func(i, j int) bool {
		iEnded := parseTime(records[i].Report.EndedAt)
		jEnded := parseTime(records[j].Report.EndedAt)
		if !iEnded.Equal(jEnded) {
			return iEnded.After(jEnded)
		}
		iStarted := parseTime(records[i].Report.StartedAt)
		jStarted := parseTime(records[j].Report.StartedAt)
		if !iStarted.Equal(jStarted) {
			return iStarted.After(jStarted)
		}
		if !records[i].modTime.Equal(records[j].modTime) {
			return records[i].modTime.After(records[j].modTime)
		}
		return records[i].Path > records[j].Path
	})
}

func parseTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}
	}
	return parsed.UTC()
}

func receiptStatus(record Record) string {
	if strings.TrimSpace(record.Verdict) != "" {
		return strings.TrimSpace(record.Verdict)
	}
	switch state.NormalizeStatus(record.Status) {
	case state.StatusSucceeded:
		return "success"
	case state.StatusFailed:
		return "fail"
	case state.StatusNeedsHuman:
		return "needs-human"
	case state.StatusTimedOut:
		return "timeout"
	case state.StatusCancelled:
		return "cancelled"
	case state.StatusAbandoned, state.StatusHung:
		return "partial-child-failure"
	}
	if record.Report.ExitCode != 0 {
		return "fail"
	}
	if record.Report.Verified {
		return "success"
	}
	return "self-reported"
}

func blockingStatus(status string) string {
	switch strings.TrimSpace(status) {
	case "success", "pass":
		return "no"
	case "needs-human", "self-reported":
		return "needs human"
	default:
		return "yes"
	}
}

func receiptReason(record Record) string {
	if strings.TrimSpace(record.Evidence) != "" {
		return compactLine(record.Evidence)
	}
	if strings.TrimSpace(record.Error) != "" {
		return compactLine(record.Error)
	}
	status := receiptStatus(record)
	switch status {
	case "needs-human":
		if len(record.Findings) == 0 {
			return "no concrete blocking defect was reported; human judgment is needed"
		}
		return "human judgment is needed for the reported review finding"
	case "timeout":
		return "run timed out before completion"
	case "cancelled":
		return "run was cancelled before completion"
	case "partial-child-failure":
		return "one or more child runs did not complete successfully"
	case "fail":
		return fmt.Sprintf("process exited with code %d", record.Report.ExitCode)
	case "self-reported":
		return "report was self-reported and not binary verified"
	default:
		return "run completed successfully"
	}
}

func displayIssue(issue int) string {
	if issue <= 0 {
		return notReported
	}
	return "#" + strconv.Itoa(issue)
}

func displayPR(record Record) string {
	if strings.TrimSpace(record.PR) != "" {
		return record.PR
	}
	if record.PRNumber > 0 {
		return "#" + strconv.Itoa(record.PRNumber)
	}
	if parsed := prNumberFromAction(record.Report.Action); parsed > 0 {
		return "#" + strconv.Itoa(parsed)
	}
	return notReported
}

func nextActions(record Record, status string) []string {
	details := detailsHint(record)
	raw := rawJSONHint(record)
	switch status {
	case "success", "pass":
		if record.Report.Role == reporter.RoleVerifier {
			return append([]string{"conductor may use this verifier result as one gate input"}, details, raw)
		}
		return append([]string{"verify the resulting PR before calling it merge-eligible"}, details, raw)
	case "needs-human", "self-reported":
		return append([]string{"human should decide whether the unresolved evidence is acceptable"}, details, raw)
	case "timeout", "cancelled", "partial-child-failure":
		return append([]string{"resume or recover the run before dispatching dependent work"}, details, raw)
	default:
		return append([]string{"recover or retry after reviewing the failure evidence"}, details, raw)
	}
}

func detailsHint(record Record) string {
	if strings.TrimSpace(record.RunID) != "" {
		return "details: loopcoder report --run " + record.RunID + " --verbose"
	}
	if strings.TrimSpace(record.Report.WorkID) != "" {
		return "details: loopcoder report --work-id " + record.Report.WorkID + " --verbose"
	}
	return "details: loopcoder report --verbose"
}

func rawJSONHint(record Record) string {
	if strings.TrimSpace(record.RunID) != "" {
		return "raw JSON: loopcoder report --run " + record.RunID + " --format json"
	}
	if strings.TrimSpace(record.Report.WorkID) != "" {
		return "raw JSON: loopcoder report --work-id " + record.Report.WorkID + " --format json"
	}
	return "raw JSON: loopcoder report --format json"
}

func formatFindingSummary(findings []loopreview.Finding) string {
	if len(findings) == 0 {
		return "none"
	}
	order := []string{"error", "warning", "low", "info"}
	counts := map[string]int{}
	for _, finding := range findings {
		severity := strings.ToLower(strings.TrimSpace(finding.Severity))
		if severity == "" {
			severity = "unspecified"
		}
		counts[severity]++
	}
	var parts []string
	for _, severity := range order {
		if count := counts[severity]; count > 0 {
			parts = append(parts, pluralCount(count, severity))
			delete(counts, severity)
		}
	}
	var extras []string
	for severity := range counts {
		extras = append(extras, severity)
	}
	sort.Strings(extras)
	for _, severity := range extras {
		parts = append(parts, pluralCount(counts[severity], severity))
	}
	return strings.Join(parts, ", ")
}

func formatFindingDetail(finding loopreview.Finding) string {
	severity := strings.TrimSpace(finding.Severity)
	if severity == "" {
		severity = "unspecified"
	}
	note := compactLine(finding.Note)
	if strings.TrimSpace(finding.File) == "" {
		return severity + ": " + note
	}
	return severity + " " + strings.TrimSpace(finding.File) + ": " + note
}

func pluralCount(count int, label string) string {
	if count == 1 {
		return "1 " + label
	}
	return strconv.Itoa(count) + " " + label
}

func compactLine(value string) string {
	fields := strings.Fields(strings.TrimSpace(value))
	if len(fields) == 0 {
		return notReported
	}
	return strings.Join(fields, " ")
}

func resultStatus(report reporter.Report) string {
	if report.ExitCode == 0 {
		return "ok"
	}
	return "failed"
}

func formatDuration(report reporter.Report) string {
	if !report.HasDurationMS() {
		return notReported
	}
	return (time.Duration(report.DurationMS) * time.Millisecond).String()
}

func formatUsage(usage reporter.Usage) string {
	var parts []string
	if usage.InputTokens != nil {
		parts = append(parts, "input="+strconv.FormatInt(*usage.InputTokens, 10))
	}
	if usage.OutputTokens != nil {
		parts = append(parts, "output="+strconv.FormatInt(*usage.OutputTokens, 10))
	}
	if usage.TotalTokens != nil {
		parts = append(parts, "total="+strconv.FormatInt(*usage.TotalTokens, 10))
	}
	if len(parts) == 0 {
		return notReported
	}
	return strings.Join(parts, " ")
}

func display(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return notReported
	}
	return value
}
