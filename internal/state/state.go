// Package state contains helpers for loopcoder run state.
package state

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/attestation"
)

const runIDTimeLayout = "20060102T150405Z"

var runIDPattern = regexp.MustCompile(`^run-\d{8}T\d{6}Z-(?:issue-[1-9]\d*|wave)$`)

type Attempt struct {
	Version             int                            `json:"version,omitempty"`
	JobID               string                         `json:"job_id"`
	Issue               int                            `json:"issue"`
	Attempt             int                            `json:"attempt"`
	Provider            string                         `json:"provider,omitempty"`
	PID                 *int                           `json:"pid,omitempty"`
	Phase               string                         `json:"phase,omitempty"`
	Status              string                         `json:"status,omitempty"`
	Branch              string                         `json:"branch,omitempty"`
	RecoveryContextPath string                         `json:"recovery_context_path,omitempty"`
	StartedAt           string                         `json:"started_at,omitempty"`
	HeartbeatAt         string                         `json:"heartbeat_at,omitempty"`
	LastProgressAt      string                         `json:"last_progress_at,omitempty"`
	LogBytes            *int64                         `json:"log_bytes,omitempty"`
	ExitCode            *int                           `json:"exit_code,omitempty"`
	Error               string                         `json:"error,omitempty"`
	Usage               *attestation.Usage             `json:"usage,omitempty"`
	Attestation         *attestation.AttestationRecord `json:"attestation,omitempty"`
	CostUSD             *float64                       `json:"cost_usd,omitempty"`
	Path                string                         `json:"path,omitempty"`
	LastWriteUTC        time.Time                      `json:"-"`
}

type AttemptRecord struct {
	Version             int                            `json:"version"`
	JobID               string                         `json:"job_id"`
	Issue               int                            `json:"issue"`
	Attempt             int                            `json:"attempt"`
	Provider            string                         `json:"provider"`
	PID                 int                            `json:"pid"`
	Phase               string                         `json:"phase"`
	Status              string                         `json:"status"`
	Branch              string                         `json:"branch,omitempty"`
	RecoveryContextPath string                         `json:"recovery_context_path,omitempty"`
	StartedAt           string                         `json:"started_at"`
	HeartbeatAt         string                         `json:"heartbeat_at"`
	LastProgressAt      string                         `json:"last_progress_at"`
	LogBytes            int64                          `json:"log_bytes"`
	ExitCode            *int                           `json:"exit_code"`
	Error               *string                        `json:"error"`
	Usage               *attestation.Usage             `json:"usage,omitempty"`
	Attestation         *attestation.AttestationRecord `json:"attestation,omitempty"`
	CostUSD             *float64                       `json:"cost_usd,omitempty"`
}

type Event struct {
	Timestamp string  `json:"ts"`
	RunID     string  `json:"run_id"`
	JobID     string  `json:"job_id"`
	Issue     int     `json:"issue"`
	Phase     string  `json:"phase"`
	Status    string  `json:"status"`
	LogBytes  int64   `json:"log_bytes"`
	ExitCode  *int    `json:"exit_code"`
	Error     *string `json:"error"`
}

// FormatTimestamp formats timestamps in UTC RFC3339 for loopcoder sidecars.
func FormatTimestamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// ParseTimestamp parses RFC3339 timestamps, including PowerShell ToString("o") values.
func ParseTimestamp(value string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse timestamp %q: %w", value, err)
	}
	return t.UTC(), nil
}

// RunIDForIssue returns a run id in the run-<utc-compact>-issue-<n> shape.
func RunIDForIssue(issueNumber int, at time.Time) string {
	return fmt.Sprintf("run-%s-issue-%d", at.UTC().Format(runIDTimeLayout), issueNumber)
}

// RunIDForWave returns a run id in the run-<utc-compact>-wave shape.
func RunIDForWave(at time.Time) string {
	return fmt.Sprintf("run-%s-wave", at.UTC().Format(runIDTimeLayout))
}

// IsRunID reports whether value matches the documented run id shape.
func IsRunID(value string) bool {
	return runIDPattern.MatchString(value)
}

// LatestRunID selects the newest local run directory by modification time.
func LatestRunID(repoPath string) (string, error) {
	runsRoot := RunsRoot(repoPath)
	entries, err := os.ReadDir(runsRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read runs directory: %w", err)
	}

	var latestName string
	var latestMod time.Time
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		mod := info.ModTime()
		if latestName == "" || mod.After(latestMod) || (mod.Equal(latestMod) && entry.Name() > latestName) {
			latestName = entry.Name()
			latestMod = mod
		}
	}
	return latestName, nil
}

func RunsRoot(repoPath string) string {
	return filepath.Join(repoPath, ".loopcoder", "runs")
}

func RunPath(repoPath, runID string) string {
	return filepath.Join(RunsRoot(repoPath), runID)
}

func WorkersPath(repoPath, runID string) string {
	return filepath.Join(RunPath(repoPath, runID), "workers")
}

func EventsPath(repoPath, runID string) string {
	return filepath.Join(RunPath(repoPath, runID), "events.jsonl")
}

func RecoveryPath(repoPath, runID string) string {
	return filepath.Join(RunPath(repoPath, runID), "recovery")
}

func AttemptPath(repoPath, runID, jobID string) string {
	return filepath.Join(WorkersPath(repoPath, runID), jobID+".attempt.json")
}

func RecoveryBriefPath(repoPath, runID, jobID string) string {
	return filepath.Join(RecoveryPath(repoPath, runID), jobID+"-context.md")
}

// WriteAttempt writes a compact attempt sidecar under workers/<job>.attempt.json.
func WriteAttempt(repoPath, runID string, attempt AttemptRecord) (string, error) {
	path := AttemptPath(repoPath, runID, attempt.JobID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return path, fmt.Errorf("create workers directory: %w", err)
	}
	data, err := json.Marshal(attempt)
	if err != nil {
		return path, fmt.Errorf("marshal attempt sidecar: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return path, fmt.Errorf("write attempt sidecar: %w", err)
	}
	return path, nil
}

// AppendEvent appends one compact JSON transition line to events.jsonl.
func AppendEvent(repoPath, runID string, event Event) error {
	path := EventsPath(repoPath, runID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create run directory: %w", err)
	}
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open events file: %w", err)
	}
	defer file.Close()
	if _, err := file.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("append event: %w", err)
	}
	return nil
}

// LoadAttempts reads .loopcoder/runs/<runID>/workers/*.attempt.json records.
func LoadAttempts(repoPath, runID string) ([]Attempt, error) {
	if strings.TrimSpace(runID) == "" {
		return nil, nil
	}
	return LoadAttemptsFromWorkersDir(WorkersPath(repoPath, runID))
}

func CountEvents(repoPath, runID string) (int, error) {
	if strings.TrimSpace(runID) == "" {
		return 0, nil
	}
	file, err := os.Open(EventsPath(repoPath, runID))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read events file: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024), 1024*1024)
	count := 0
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) != "" {
			count++
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("read events file: %w", err)
	}
	return count, nil
}

func LoadAttemptsFromWorkersDir(workersDir string) ([]Attempt, error) {
	entries, err := os.ReadDir(workersDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read workers directory: %w", err)
	}

	var attempts []Attempt
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".attempt.json") {
			continue
		}
		path := filepath.Join(workersDir, entry.Name())
		attempt, ok := readAttempt(path, entry.Name())
		if ok {
			attempts = append(attempts, attempt)
		}
	}

	sort.Slice(attempts, func(i, j int) bool {
		if attempts[i].Issue != attempts[j].Issue {
			return attempts[i].Issue < attempts[j].Issue
		}
		if attempts[i].Attempt != attempts[j].Attempt {
			return attempts[i].Attempt < attempts[j].Attempt
		}
		if !attempts[i].LastWriteUTC.Equal(attempts[j].LastWriteUTC) {
			return attempts[i].LastWriteUTC.Before(attempts[j].LastWriteUTC)
		}
		return attempts[i].JobID < attempts[j].JobID
	})
	return attempts, nil
}

type attemptJSON struct {
	Version             int                            `json:"version"`
	JobID               string                         `json:"job_id"`
	Issue               json.RawMessage                `json:"issue"`
	Attempt             json.RawMessage                `json:"attempt"`
	Provider            string                         `json:"provider"`
	PID                 json.RawMessage                `json:"pid"`
	Phase               string                         `json:"phase"`
	Status              string                         `json:"status"`
	Branch              string                         `json:"branch"`
	RecoveryContextPath string                         `json:"recovery_context_path"`
	StartedAt           string                         `json:"started_at"`
	HeartbeatAt         string                         `json:"heartbeat_at"`
	LastProgressAt      string                         `json:"last_progress_at"`
	LogBytes            json.RawMessage                `json:"log_bytes"`
	ExitCode            json.RawMessage                `json:"exit_code"`
	Error               string                         `json:"error"`
	Usage               *attestation.Usage             `json:"usage"`
	Attestation         *attestation.AttestationRecord `json:"attestation"`
	CostUSD             json.RawMessage                `json:"cost_usd"`
}

func readAttempt(path, fileName string) (Attempt, bool) {
	data, err := os.ReadFile(path)
	if err != nil || len(strings.TrimSpace(string(data))) == 0 {
		return Attempt{}, false
	}

	var raw attemptJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return Attempt{}, false
	}

	issue, ok := rawInt(raw.Issue, 0)
	if !ok || issue <= 0 {
		return Attempt{}, false
	}

	attemptNumber, ok := rawInt(raw.Attempt, 1)
	if !ok || attemptNumber <= 0 {
		attemptNumber = 1
	}

	branch := strings.TrimSpace(raw.Branch)
	if branch == "" {
		if attemptNumber > 1 {
			branch = fmt.Sprintf("loop/issue-%d-retry-%d", issue, attemptNumber)
		} else {
			branch = fmt.Sprintf("loop/issue-%d", issue)
		}
	}

	jobID := strings.TrimSpace(raw.JobID)
	if jobID == "" {
		jobID = strings.TrimSuffix(fileName, ".attempt.json")
	}

	var lastWrite time.Time
	if info, err := os.Stat(path); err == nil {
		lastWrite = info.ModTime().UTC()
	}

	return Attempt{
		Version:             raw.Version,
		JobID:               jobID,
		Issue:               issue,
		Attempt:             attemptNumber,
		Provider:            raw.Provider,
		PID:                 rawIntPtr(raw.PID),
		Phase:               raw.Phase,
		Status:              raw.Status,
		Branch:              branch,
		RecoveryContextPath: strings.TrimSpace(raw.RecoveryContextPath),
		StartedAt:           raw.StartedAt,
		HeartbeatAt:         raw.HeartbeatAt,
		LastProgressAt:      raw.LastProgressAt,
		LogBytes:            rawInt64Ptr(raw.LogBytes),
		ExitCode:            rawIntPtr(raw.ExitCode),
		Error:               raw.Error,
		Usage:               cloneUsage(raw.Usage),
		Attestation:         cloneAttestation(raw.Attestation),
		CostUSD:             rawFloat64Ptr(raw.CostUSD),
		Path:                path,
		LastWriteUTC:        lastWrite,
	}, true
}

func cloneAttestation(record *attestation.AttestationRecord) *attestation.AttestationRecord {
	if record == nil {
		return nil
	}
	clone := *record
	clone.Usage = *cloneUsage(&record.Usage)
	return &clone
}

func cloneUsage(usage *attestation.Usage) *attestation.Usage {
	if usage == nil {
		return nil
	}
	clone := *usage
	if usage.InputTokens != nil {
		value := *usage.InputTokens
		clone.InputTokens = &value
	}
	if usage.OutputTokens != nil {
		value := *usage.OutputTokens
		clone.OutputTokens = &value
	}
	if usage.TotalTokens != nil {
		value := *usage.TotalTokens
		clone.TotalTokens = &value
	}
	return &clone
}

func rawIntPtr(raw json.RawMessage) *int {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return nil
	}
	value, ok := rawInt(raw, 0)
	if !ok {
		return nil
	}
	return &value
}

func rawInt64Ptr(raw json.RawMessage) *int64 {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return nil
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err == nil {
		return &value
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err != nil {
		return nil
	}
	parsed, err := strconv.ParseInt(strings.TrimSpace(asString), 10, 64)
	if err != nil {
		return nil
	}
	return &parsed
}

func rawFloat64Ptr(raw json.RawMessage) *float64 {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return nil
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err == nil {
		return &value
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err != nil {
		return nil
	}
	parsed, err := strconv.ParseFloat(strings.TrimSpace(asString), 64)
	if err != nil {
		return nil
	}
	return &parsed
}

func rawInt(raw json.RawMessage, defaultValue int) (int, bool) {
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return defaultValue, true
	}
	if text == "null" {
		return 0, false
	}
	var value int
	if err := json.Unmarshal(raw, &value); err == nil {
		return value, true
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err != nil {
		return 0, false
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(asString))
	if err != nil {
		return 0, false
	}
	return parsed, true
}
