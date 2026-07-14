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
	"github.com/jasonhnd/loopcoder/internal/migration"
	"github.com/jasonhnd/loopcoder/internal/observability"
	"github.com/jasonhnd/loopcoder/internal/registry"
	"github.com/jasonhnd/loopcoder/internal/relaygate"
	"github.com/jasonhnd/loopcoder/internal/reporter"
	"github.com/jasonhnd/loopcoder/internal/runtimepath"
	"github.com/jasonhnd/loopcoder/internal/state"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

const (
	SchemaVersion       = "loopcoder.report_query.v1"
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
	Report  reporter.Report `json:"report"`
	Source  string          `json:"source,omitempty"`
	RunID   string          `json:"run_id,omitempty"`
	Path    string          `json:"path,omitempty"`
	modTime time.Time
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
	return RenderTextWithOptions(records, RenderOptions{})
}

type RenderOptions struct {
	Verbose bool
}

func RenderTextWithOptions(records []Record, opts RenderOptions) string {
	var out strings.Builder
	fmt.Fprintln(&out, "REPORTS")
	if len(records) == 0 {
		fmt.Fprintln(&out, "- none")
		return out.String()
	}
	for index, record := range records {
		r := record.Report
		if index > 0 {
			fmt.Fprintln(&out)
		}
		fmt.Fprintln(&out, r.Pretty(reporter.PrettyOptions{
			Mode:           reporter.PrettyModePlain,
			DetailCommand:  detailCommand(r),
			RawJSONCommand: rawJSONCommand(r),
		}))
		fmt.Fprintf(&out, "Source\n")
		fmt.Fprintf(&out, "- source: %s\n", display(record.Source))
		fmt.Fprintf(&out, "- run ID: %s\n", display(record.RunID))
		fmt.Fprintf(&out, "- path: %s\n", display(record.Path))
		if opts.Verbose {
			fmt.Fprintf(&out, "\nRaw record\n")
			fmt.Fprintf(&out, "- header: %s\n", r.Header())
			if data, err := r.CanonicalJSON(); err == nil {
				fmt.Fprintf(&out, "- canonical JSON: %s\n", data)
			}
		}
	}
	return out.String()
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
			Report: record.Report,
			Source: record.Source,
			RunID:  record.RunID,
			Path:   record.Path,
		})
	}
	payload := struct {
		SchemaVersion string                 `json:"schema_version"`
		Observability observability.Document `json:"observability"`
		Reports       []reporter.Report      `json:"reports"`
		Records       []jsonRecord           `json:"records"`
		RunTree       any                    `json:"run_tree,omitempty"`
	}{
		SchemaVersion: SchemaVersion,
		Observability: reportQueryObservability(records),
		Reports:       reports,
		Records:       jsonRecords,
		RunTree:       runTree,
	}
	return json.Marshal(payload)
}

type jsonRecord struct {
	Report reporter.Report `json:"report"`
	Source string          `json:"source"`
	RunID  string          `json:"run_id"`
	Path   string          `json:"path"`
}

func loadRunReports(repoPath string) ([]Record, error) {
	var records []Record
	for _, runsRoot := range state.RunsRootsForRead(repoPath) {
		found, err := loadRunReportsFromRoot(repoPath, runsRoot)
		if err != nil {
			return nil, err
		}
		records = append(records, found...)
		if len(found) > 0 {
			break
		}
	}
	return records, nil
}

func loadRunReportsFromRoot(repoPath, runsRoot string) ([]Record, error) {
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
		attempts, err := state.LoadAttemptsFromWorkersDir(filepath.Join(runsRoot, runID, "workers"))
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
				modTime: attempt.LastWriteUTC,
			})
		}

		generic, err := scanRunJSONReports(filepath.Join(runsRoot, runID), runID)
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
		for _, report := range collectReports(data) {
			records = append(records, Record{
				Report:  report,
				Source:  "run-json",
				RunID:   runID,
				Path:    path,
				modTime: info.ModTime().UTC(),
			})
		}
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
		for _, report := range collectReports(scanner.Bytes()) {
			records = append(records, Record{
				Report:  report,
				Source:  "run-jsonl",
				RunID:   runID,
				Path:    path,
				modTime: modTime,
			})
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return records, nil
}

func collectReports(data []byte) []reporter.Report {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil
	}
	var records []reporter.Report
	var walk func(any)
	walk = func(current any) {
		switch typed := current.(type) {
		case map[string]any:
			if looksLikeReport(typed) {
				if report, ok := parseReportValue(typed); ok {
					records = append(records, report)
				}
			}
			if raw, ok := typed[migration.ReportStateKey]; ok {
				if report, ok := parseReportValue(raw); ok {
					records = append(records, report)
				}
			} else if raw, ok := typed[migration.LegacyReportStateKey]; ok {
				if report, ok := parseReportValue(raw); ok {
					records = append(records, report)
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

func loadRelayReports(repoPath string) ([]Record, error) {
	var records []Record
	pending, err := loadPendingRelayReports(repoPath)
	if err != nil {
		return nil, err
	}
	records = append(records, pending...)

	for _, relayRoot := range relayRootsForRead(repoPath) {
		found, err := loadRelayLedgerReportsFromRoot(relayRoot)
		if err != nil {
			return nil, err
		}
		records = append(records, found...)
		if len(found) > 0 {
			break
		}
	}
	return records, nil
}

func loadRelayLedgerReportsFromRoot(relayRoot string) ([]Record, error) {
	var records []Record
	err := filepath.WalkDir(relayRoot, func(path string, entry os.DirEntry, err error) error {
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
		records = append(records, collectRelayLedgerReports(data, path, info.ModTime().UTC())...)
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
	var records []Record
	for _, relayRoot := range relayRootsForRead(repoPath) {
		found, err := loadPendingRelayReportsFromDir(filepath.Join(relayRoot, "pending"))
		if err != nil {
			return nil, err
		}
		records = append(records, found...)
		if len(found) > 0 {
			break
		}
	}
	return records, nil
}

func loadPendingRelayReportsFromDir(dir string) ([]Record, error) {
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

func relayRootsForRead(repoPath string) []string {
	roots, err := runtimepath.Resolve(context.Background(), repoPath)
	if err != nil {
		return []string{filepath.Join(repoPath, ".loopcoder", "relay")}
	}
	out := []string{roots.RelayRoot}
	if roots.Registered && roots.LegacyRelayRoot != "" && roots.LegacyRelayRoot != roots.RelayRoot {
		out = append(out, roots.LegacyRelayRoot)
	}
	return out
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

func display(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return notReported
	}
	return value
}

func detailCommand(report reporter.Report) string {
	if strings.TrimSpace(report.WorkID) == "" {
		return "loopcoder report --verbose"
	}
	return "loopcoder report --work-id " + report.WorkID + " --verbose"
}

func rawJSONCommand(report reporter.Report) string {
	if strings.TrimSpace(report.WorkID) == "" {
		return "loopcoder report --format json"
	}
	return "loopcoder report --work-id " + report.WorkID + " --format json"
}
