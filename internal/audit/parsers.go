package audit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

func ParseToolOutput(parser, toolID string, data []byte) ([]Finding, error) {
	parser = strings.ToLower(strings.TrimSpace(parser))
	toolID = strings.TrimSpace(toolID)
	switch parser {
	case "govulncheck-json":
		return parseGovulncheck(toolID, data)
	case "staticcheck-json":
		return parseStaticcheck(toolID, data)
	case "gosec-json":
		return parseGosec(toolID, data)
	case "generic-line":
		return parseGenericLine(toolID, data), nil
	default:
		return nil, fmt.Errorf("unsupported parser %q", parser)
	}
}

func parseGovulncheck(toolID string, data []byte) ([]Finding, error) {
	values, err := decodeJSONValues(data)
	if err != nil {
		return nil, err
	}
	findings := []Finding{}
	for _, value := range values {
		var obj map[string]any
		if err := json.Unmarshal(value, &obj); err != nil {
			return nil, err
		}
		candidate := mapValue(obj, "finding")
		if candidate == nil && (mapString(obj, "osv") != "" || mapValue(obj, "osv") != nil) {
			candidate = obj
		}
		if candidate == nil {
			continue
		}

		osv := firstString(
			mapString(candidate, "osv"),
			mapString(mapValue(candidate, "osv"), "id"),
			mapString(candidate, "osv_id"),
		)
		if osv == "" {
			osv = "govulncheck"
		}
		file, line, column := govulncheckLocation(candidate)
		message := firstString(
			mapString(candidate, "message"),
			mapString(mapValue(candidate, "osv"), "summary"),
			"reachable vulnerability reported by govulncheck",
		)
		evidence := firstString(mapString(candidate, "symbol"), mapString(candidate, "package"), message)
		findings = append(findings, NewFinding(
			LayerSAST,
			toolID,
			SeverityHigh,
			file,
			line,
			column,
			osv,
			"vulnerability",
			message,
			evidence,
		))
	}
	return findings, nil
}

func govulncheckLocation(finding map[string]any) (string, int, int) {
	if pos := mapValue(finding, "position"); pos != nil {
		return mapString(pos, "filename"), mapInt(pos, "line"), mapInt(pos, "column")
	}
	if trace := mapArray(finding, "trace"); len(trace) > 0 {
		for _, item := range trace {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if pos := mapValue(entry, "position"); pos != nil {
				return mapString(pos, "filename"), mapInt(pos, "line"), mapInt(pos, "column")
			}
			if file := mapString(entry, "file"); file != "" {
				return file, mapInt(entry, "line"), mapInt(entry, "column")
			}
		}
	}
	return "", 0, 0
}

func parseStaticcheck(toolID string, data []byte) ([]Finding, error) {
	values, err := decodeJSONValues(data)
	if err != nil {
		return nil, err
	}
	findings := []Finding{}
	for _, value := range values {
		items, err := flattenJSONObjects(value)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			code := firstString(mapString(item, "code"), mapString(item, "checker"), mapString(item, "rule"))
			message := firstString(mapString(item, "message"), mapString(item, "text"), mapString(item, "details"))
			if code == "" && message == "" {
				continue
			}
			location := mapValue(item, "location")
			file := firstString(mapString(location, "file"), mapString(item, "file"))
			line := firstNonZero(mapInt(location, "line"), mapInt(item, "line"))
			column := firstNonZero(mapInt(location, "column"), mapInt(item, "column"))
			severity := staticcheckSeverity(mapString(item, "severity"))
			findings = append(findings, NewFinding(
				LayerSAST,
				toolID,
				severity,
				file,
				line,
				column,
				firstString(code, "staticcheck"),
				"static-analysis",
				firstString(message, "staticcheck finding"),
				firstString(message, code),
			))
		}
	}
	return findings, nil
}

func parseGosec(toolID string, data []byte) ([]Finding, error) {
	values, err := decodeJSONValues(data)
	if err != nil {
		return nil, err
	}
	findings := []Finding{}
	for _, value := range values {
		var obj map[string]any
		if err := json.Unmarshal(value, &obj); err != nil {
			return nil, err
		}
		for _, raw := range mapArray(obj, "Issues") {
			issue, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			rule := firstString(mapString(issue, "rule_id"), mapString(issue, "rule"), "gosec")
			file := mapString(issue, "file")
			line := mapInt(issue, "line")
			column := mapInt(issue, "column")
			message := firstString(mapString(issue, "details"), mapString(issue, "message"), rule)
			findings = append(findings, NewFinding(
				LayerSAST,
				toolID,
				gosecSeverity(mapString(issue, "severity")),
				file,
				line,
				column,
				rule,
				"static-analysis",
				message,
				message,
			))
		}
	}
	return findings, nil
}

func parseGenericLine(toolID string, data []byte) []Finding {
	lines := splitTextLines(string(data))
	findings := []Finding{}
	for index, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		findings = append(findings, NewFinding(
			LayerSAST,
			toolID,
			SeverityMedium,
			"",
			index+1,
			0,
			"generic-line",
			"static-analysis",
			line,
			line,
		))
	}
	return findings
}

func decodeJSONValues(data []byte) ([]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	values := []json.RawMessage{}
	for {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("parse JSON: %w", err)
		}
		if len(bytes.TrimSpace(raw)) > 0 {
			values = append(values, raw)
		}
	}
	if len(values) == 0 {
		return nil, nil
	}
	return values, nil
}

func flattenJSONObjects(raw json.RawMessage) ([]map[string]any, error) {
	var array []map[string]any
	if err := json.Unmarshal(raw, &array); err == nil {
		return array, nil
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	if diagnostics := mapArray(obj, "diagnostics"); len(diagnostics) > 0 {
		out := make([]map[string]any, 0, len(diagnostics))
		for _, raw := range diagnostics {
			if item, ok := raw.(map[string]any); ok {
				out = append(out, item)
			}
		}
		return out, nil
	}
	if warnings := mapArray(obj, "warnings"); len(warnings) > 0 {
		out := make([]map[string]any, 0, len(warnings))
		for _, raw := range warnings {
			if item, ok := raw.(map[string]any); ok {
				out = append(out, item)
			}
		}
		return out, nil
	}
	return []map[string]any{obj}, nil
}

func mapValue(values map[string]any, key string) map[string]any {
	if values == nil {
		return nil
	}
	value, ok := values[key].(map[string]any)
	if !ok {
		return nil
	}
	return value
}

func mapArray(values map[string]any, key string) []any {
	if values == nil {
		return nil
	}
	value, ok := values[key].([]any)
	if !ok {
		return nil
	}
	return value
}

func mapString(values map[string]any, key string) string {
	if values == nil {
		return ""
	}
	switch value := values[key].(type) {
	case string:
		return strings.TrimSpace(value)
	case float64:
		if value == float64(int64(value)) {
			return strconv.FormatInt(int64(value), 10)
		}
		return strconv.FormatFloat(value, 'f', -1, 64)
	default:
		return ""
	}
}

func mapInt(values map[string]any, key string) int {
	if values == nil {
		return 0
	}
	switch value := values[key].(type) {
	case float64:
		return int(value)
	case int:
		return value
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(value))
		return n
	default:
		return 0
	}
}

func firstString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func firstNonZero(values ...int) int {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}

func staticcheckSeverity(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "error":
		return SeverityMedium
	case "warning", "warn":
		return SeverityLow
	default:
		return SeverityMedium
	}
}

func gosecSeverity(value string) string {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "CRITICAL":
		return SeverityCritical
	case "HIGH":
		return SeverityHigh
	case "MEDIUM":
		return SeverityMedium
	case "LOW":
		return SeverityLow
	default:
		return SeverityMedium
	}
}

func splitTextLines(text string) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimRight(text, "\n\r")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}
