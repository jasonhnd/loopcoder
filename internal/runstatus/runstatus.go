// Package runstatus renders local-only delivery run status from .loopcoder state.
package runstatus

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/attestation"
	"github.com/jasonhnd/loopcoder/internal/state"
)

const NotReported = "not reported"

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
	Issue       int
	JobID       string
	PR          string
	Verdict     string
	Attestation *attestation.AttestationRecord
	Path        string
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

	attempts, err := state.LoadAttempts(repoPath, runID)
	if err != nil {
		return Report{}, err
	}

	eventMetadata, eventVerifiers, eventCount, err := loadEventRecords(repoPath, runID)
	if err != nil {
		return Report{}, err
	}
	jsonMetadata, jsonVerifiers, err := scanRunJSONRecords(runPath)
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
	worker := attempt.Attestation
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
		WorkerProvider:       display(firstNonEmpty(attestationProvider(worker), attempt.Provider)),
		WorkerModel:          display(attestationModel(worker)),
		WorkerModelSource:    display(attestationModelSource(worker)),
		WorkerEffort:         display(attestationEffort(worker)),
		WorkerPermission:     display(attestationPermission(worker)),
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

	verifierUsage := (*attestation.Usage)(nil)
	if verifier.Attestation != nil && verifier.Attestation.HasUsage() {
		verifierUsage = &verifier.Attestation.Usage
	}
	vInput, vOutput, vTotal := formatUsage(verifierUsage)
	if row.PR == NotReported {
		row.PR = display(verifier.PR)
	}
	row.VerifierVerdict = display(verifier.Verdict)
	row.VerifierProvider = display(attestationProvider(verifier.Attestation))
	row.VerifierModel = display(attestationModel(verifier.Attestation))
	row.VerifierModelSource = display(attestationModelSource(verifier.Attestation))
	row.VerifierEffort = display(attestationEffort(verifier.Attestation))
	row.VerifierPermission = display(attestationPermission(verifier.Attestation))
	row.VerifierDuration = formatDuration(verifier.Attestation)
	row.VerifierInputTokens = vInput
	row.VerifierOutputTokens = vOutput
	row.VerifierTotalTokens = vTotal
	row.VerifierVerified = formatVerified(verifier.Attestation)
	row.verifierRecordSortKey = verifier.Path
	return row
}

func loadEventRecords(repoPath, runID string) ([]metadataRecord, []verifierRecord, int, error) {
	file, err := os.Open(state.EventsPath(repoPath, runID))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, 0, nil
		}
		return nil, nil, 0, fmt.Errorf("read events file: %w", err)
	}
	defer file.Close()

	var metadata []metadataRecord
	var verifiers []verifierRecord
	count := 0
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		count++
		records, verifierRecords := collectRecords([]byte(line), state.EventsPath(repoPath, runID))
		metadata = append(metadata, records...)
		verifiers = append(verifiers, verifierRecords...)
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, 0, fmt.Errorf("read events file: %w", err)
	}
	return metadata, verifiers, count, nil
}

func scanRunJSONRecords(runPath string) ([]metadataRecord, []verifierRecord, error) {
	var metadata []metadataRecord
	var verifiers []verifierRecord
	err := filepath.WalkDir(runPath, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		name := entry.Name()
		if name == "events.jsonl" || (!strings.HasSuffix(name, ".json") && !strings.HasSuffix(name, ".attempt.json")) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		records, verifierRecords := collectRecords(data, path)
		metadata = append(metadata, records...)
		verifiers = append(verifiers, verifierRecords...)
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("scan run records: %w", err)
	}
	return metadata, verifiers, nil
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
	if attestationValue, ok := values["attestation"]; ok {
		record.Attestation = parseAttestation(attestationValue)
	}
	if record.Attestation != nil && record.Attestation.Role != attestation.RoleVerifier {
		record.Attestation = nil
	}
	if record.PR == "" && record.Attestation != nil {
		record.PR = parsePRFromAction(record.Attestation.Action)
	}
	if record.Issue == 0 && record.Attestation != nil {
		record.Issue = parseIssueFromAction(record.Attestation.Action)
	}

	if record.Verdict == "" && record.Attestation == nil {
		return verifierRecord{}, false
	}
	return record, true
}

func parseAttestation(value any) *attestation.AttestationRecord {
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var record attestation.AttestationRecord
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
			attestationHeader(record.Attestation),
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

func attestationProvider(record *attestation.AttestationRecord) string {
	if record == nil {
		return ""
	}
	return record.Provider
}

func attestationModel(record *attestation.AttestationRecord) string {
	if record == nil {
		return ""
	}
	return record.Model
}

func attestationModelSource(record *attestation.AttestationRecord) string {
	if record == nil {
		return ""
	}
	return string(record.ModelSource)
}

func attestationEffort(record *attestation.AttestationRecord) string {
	if record == nil {
		return ""
	}
	return record.Effort
}

func attestationPermission(record *attestation.AttestationRecord) string {
	if record == nil {
		return ""
	}
	return string(record.Permission)
}

func formatDuration(record *attestation.AttestationRecord) string {
	if record == nil || !record.HasDurationMS() {
		return NotReported
	}
	return (time.Duration(record.DurationMS) * time.Millisecond).String()
}

func formatVerified(record *attestation.AttestationRecord) string {
	if record == nil || !record.HasVerified() {
		return NotReported
	}
	return strconv.FormatBool(record.Verified)
}

func formatUsage(usage *attestation.Usage) (string, string, string) {
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

func attestationHeader(record *attestation.AttestationRecord) string {
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
