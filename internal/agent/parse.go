package agent

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/attestation"
)

type invocationMetadata struct {
	Model  string
	Effort string
	Usage  attestation.Usage
}

func resultWithTiming(exitCode int, summary string, metadata invocationMetadata, startedAt, endedAt time.Time) Result {
	return Result{
		ExitCode:   exitCode,
		Summary:    summary,
		Model:      metadata.Model,
		Effort:     metadata.Effort,
		Usage:      metadata.Usage,
		StartedAt:  formatInvocationTimestamp(startedAt),
		EndedAt:    formatInvocationTimestamp(endedAt),
		DurationMS: endedAt.Sub(startedAt).Milliseconds(),
	}
}

func formatInvocationTimestamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func firstSortedKey(values map[string]json.RawMessage) string {
	if len(values) == 0 {
		return ""
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		if strings.TrimSpace(key) != "" {
			keys = append(keys, strings.TrimSpace(key))
		}
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return ""
	}
	return keys[0]
}

func parseRawInt64(raw json.RawMessage) (*int64, bool) {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return nil, false
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, false
	}
	return parseAnyInt64(value)
}

func parseAnyInt64(value any) (*int64, bool) {
	switch v := value.(type) {
	case json.Number:
		parsed, err := strconv.ParseInt(v.String(), 10, 64)
		if err != nil {
			return nil, false
		}
		return &parsed, true
	case float64:
		parsed := int64(v)
		if float64(parsed) != v {
			return nil, false
		}
		return &parsed, true
	case string:
		cleaned := strings.NewReplacer(",", "", " ", "", "\t", "").Replace(strings.TrimSpace(v))
		if cleaned == "" {
			return nil, false
		}
		parsed, err := strconv.ParseInt(cleaned, 10, 64)
		if err != nil {
			return nil, false
		}
		return &parsed, true
	default:
		return nil, false
	}
}
