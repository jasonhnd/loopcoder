package audit

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const (
	ParserGovulncheckJSON = "govulncheck-json"
	ParserStaticcheckJSON = "staticcheck-json"
	ParserGosecJSON       = "gosec-json"
)

func KnownParser(parser string) bool {
	switch strings.TrimSpace(parser) {
	case ParserGovulncheckJSON, ParserStaticcheckJSON, ParserGosecJSON:
		return true
	default:
		return false
	}
}

func ParseToolOutput(parser, toolID, output string) ([]Finding, error) {
	switch strings.TrimSpace(parser) {
	case ParserGovulncheckJSON:
		return parseGovulncheck(toolID, output)
	case ParserStaticcheckJSON:
		return parseStaticcheck(toolID, output)
	case ParserGosecJSON:
		return parseGosec(toolID, output)
	default:
		return nil, fmt.Errorf("unknown parser %q", parser)
	}
}

type osvDefinition struct {
	ID      string
	Summary string
	Details string
}

type govulnFindingCandidate struct {
	OSV          string
	FixedVersion string
	Trace        []govulnFrame
	Message      string
}

type govulnFrame struct {
	Module   string
	Package  string
	Function string
	Symbol   string
	File     string
	Line     int
	Column   int
}

func parseGovulncheck(toolID, output string) ([]Finding, error) {
	defs := map[string]osvDefinition{}
	best := map[string]govulnFindingCandidate{}
	linesSeen := 0
	if err := scanJSONLines(output, func(line string) error {
		linesSeen++
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &envelope); err != nil {
			return err
		}
		if raw, ok := envelope["osv"]; ok {
			def := parseOSVDefinition(raw)
			if def.ID != "" {
				defs[def.ID] = def
			}
			return nil
		}
		raw, ok := envelope["finding"]
		if !ok {
			return nil
		}
		candidate, err := parseGovulnFinding(raw)
		if err != nil {
			return err
		}
		if candidate.OSV == "" {
			return errors.New("govulncheck finding missing osv id")
		}
		current, exists := best[candidate.OSV]
		if !exists || govulnSpecificity(candidate) > govulnSpecificity(current) {
			best[candidate.OSV] = candidate
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("parse govulncheck JSON: %w", err)
	}
	if strings.TrimSpace(output) != "" && linesSeen == 0 {
		return nil, errors.New("parse govulncheck JSON: no JSON messages found")
	}

	findings := make([]Finding, 0, len(best))
	for osvID, candidate := range best {
		def := defs[osvID]
		frame := mostSpecificGovulnFrame(candidate.Trace)
		message := firstNonEmpty(def.Summary, candidate.Message, "govulncheck reported vulnerable dependency "+osvID)
		evidence := govulnEvidence(candidate, frame)
		findings = append(findings, makeFinding(Finding{
			Layer:    LayerSAST,
			Tool:     toolID,
			Severity: SeverityHigh,
			File:     frame.File,
			Line:     frame.Line,
			Column:   frame.Column,
			Rule:     osvID,
			Category: "vulnerable-dependency",
			Message:  message,
			Evidence: evidence,
		}))
	}
	return findings, nil
}

func parseOSVDefinition(raw json.RawMessage) osvDefinition {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		var id string
		if err := json.Unmarshal(raw, &id); err == nil {
			return osvDefinition{ID: strings.TrimSpace(id)}
		}
		return osvDefinition{}
	}
	return osvDefinition{
		ID:      jsonString(payload, "id", "osv"),
		Summary: jsonString(payload, "summary"),
		Details: jsonString(payload, "details"),
	}
}

func parseGovulnFinding(raw json.RawMessage) (govulnFindingCandidate, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		return govulnFindingCandidate{}, err
	}
	candidate := govulnFindingCandidate{
		OSV:          jsonString(payload, "osv", "osv_id", "id"),
		FixedVersion: jsonString(payload, "fixed_version", "fixedVersion"),
		Message:      jsonString(payload, "message", "summary"),
	}
	if rawOSV, ok := payload["osv"]; ok && candidate.OSV == "" {
		def := parseOSVDefinition(rawOSV)
		candidate.OSV = def.ID
		if candidate.Message == "" {
			candidate.Message = firstNonEmpty(def.Summary, def.Details)
		}
	}
	if traceRaw, ok := payload["trace"]; ok {
		candidate.Trace = parseGovulnTrace(traceRaw)
	}
	if frame := parseGovulnFrame(payload); frameHasData(frame) {
		candidate.Trace = append(candidate.Trace, frame)
	}
	return candidate, nil
}

func parseGovulnTrace(raw json.RawMessage) []govulnFrame {
	var entries []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil
	}
	frames := make([]govulnFrame, 0, len(entries))
	for _, entry := range entries {
		frame := parseGovulnFrame(entry)
		if frameHasData(frame) {
			frames = append(frames, frame)
		}
	}
	return frames
}

func parseGovulnFrame(payload map[string]json.RawMessage) govulnFrame {
	frame := govulnFrame{
		Module:   jsonString(payload, "module"),
		Package:  jsonString(payload, "package", "import_path"),
		Function: jsonString(payload, "function", "func"),
		Symbol:   jsonString(payload, "symbol"),
		File:     normalizeRepoPath(jsonString(payload, "file", "filename")),
		Line:     jsonInt(payload, "line"),
		Column:   jsonInt(payload, "column"),
	}
	if positionRaw, ok := payload["position"]; ok {
		var position map[string]json.RawMessage
		if err := json.Unmarshal(positionRaw, &position); err == nil {
			if frame.File == "" {
				frame.File = normalizeRepoPath(jsonString(position, "filename", "file"))
			}
			if frame.Line == 0 {
				frame.Line = jsonInt(position, "line")
			}
			if frame.Column == 0 {
				frame.Column = jsonInt(position, "column")
			}
		}
	}
	return frame
}

func frameHasData(frame govulnFrame) bool {
	return frame.Module != "" || frame.Package != "" || frame.Function != "" || frame.Symbol != "" || frame.File != "" || frame.Line > 0
}

func govulnSpecificity(candidate govulnFindingCandidate) int {
	frame := mostSpecificGovulnFrame(candidate.Trace)
	score := 0
	if frame.File != "" {
		score += 100
	}
	if frame.Line > 0 {
		score += 50
	}
	if frame.Function != "" || frame.Symbol != "" {
		score += 25
	}
	if frame.Package != "" {
		score += 10
	}
	score += len(candidate.Trace)
	return score
}

func mostSpecificGovulnFrame(trace []govulnFrame) govulnFrame {
	best := govulnFrame{}
	bestScore := -1
	for index, frame := range trace {
		score := 0
		if frame.File != "" {
			score += 100
		}
		if frame.Line > 0 {
			score += 50
		}
		if frame.Function != "" || frame.Symbol != "" {
			score += 25
		}
		if frame.Package != "" {
			score += 10
		}
		score += index
		if score >= bestScore {
			best = frame
			bestScore = score
		}
	}
	return best
}

func govulnEvidence(candidate govulnFindingCandidate, frame govulnFrame) string {
	parts := []string{"osv=" + candidate.OSV}
	if candidate.FixedVersion != "" {
		parts = append(parts, "fixed_version="+candidate.FixedVersion)
	}
	if frame.Module != "" {
		parts = append(parts, "module="+frame.Module)
	}
	if frame.Package != "" {
		parts = append(parts, "package="+frame.Package)
	}
	if callable := firstNonEmpty(frame.Function, frame.Symbol); callable != "" {
		parts = append(parts, "callable="+callable)
	}
	if frame.File == "" {
		parts = append(parts, "reachability=imported")
	}
	return strings.Join(parts, " ")
}

func parseStaticcheck(toolID, output string) ([]Finding, error) {
	findings := []Finding{}
	if err := scanJSONLines(output, func(line string) error {
		var payload struct {
			Code     string `json:"code"`
			Severity string `json:"severity"`
			Message  string `json:"message"`
			Location struct {
				File   string `json:"file"`
				Line   int    `json:"line"`
				Column int    `json:"column"`
			} `json:"location"`
		}
		if err := json.Unmarshal([]byte(line), &payload); err != nil {
			return err
		}
		if strings.TrimSpace(payload.Code) == "" && strings.TrimSpace(payload.Message) == "" {
			return nil
		}
		findings = append(findings, makeFinding(Finding{
			Layer:    LayerSAST,
			Tool:     toolID,
			Severity: staticcheckSeverity(payload.Severity),
			File:     payload.Location.File,
			Line:     payload.Location.Line,
			Column:   payload.Location.Column,
			Rule:     firstNonEmpty(payload.Code, "staticcheck"),
			Category: "static-analysis",
			Message:  strings.TrimSpace(payload.Message),
			Evidence: strings.TrimSpace(payload.Message),
		}))
		return nil
	}); err != nil {
		return nil, fmt.Errorf("parse staticcheck JSON: %w", err)
	}
	return findings, nil
}

func staticcheckSeverity(severity string) string {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case "error":
		return SeverityMedium
	case "warning":
		return SeverityLow
	default:
		return SeverityInfo
	}
}

func parseGosec(toolID, output string) ([]Finding, error) {
	if strings.TrimSpace(output) == "" {
		return nil, nil
	}
	type gosecIssue struct {
		Severity string `json:"severity"`
		RuleID   string `json:"rule_id"`
		Details  string `json:"details"`
		File     string `json:"file"`
		Line     string `json:"line"`
		Column   string `json:"column"`
		Code     string `json:"code"`
	}
	var payload struct {
		Issues      []gosecIssue `json:"Issues"`
		LowerIssues []gosecIssue `json:"issues"`
	}
	if err := json.Unmarshal([]byte(output), &payload); err != nil {
		return nil, fmt.Errorf("parse gosec JSON: %w", err)
	}
	issues := payload.Issues
	if len(issues) == 0 {
		issues = payload.LowerIssues
	}
	findings := make([]Finding, 0, len(issues))
	for _, issue := range issues {
		findings = append(findings, makeFinding(Finding{
			Layer:    LayerSAST,
			Tool:     toolID,
			Severity: gosecSeverity(issue.Severity),
			File:     issue.File,
			Line:     atoi(issue.Line),
			Column:   atoi(issue.Column),
			Rule:     firstNonEmpty(issue.RuleID, "gosec"),
			Category: "security",
			Message:  strings.TrimSpace(issue.Details),
			Evidence: strings.TrimSpace(firstNonEmpty(issue.Code, issue.Details)),
		}))
	}
	return findings, nil
}

func gosecSeverity(severity string) string {
	switch strings.ToUpper(strings.TrimSpace(severity)) {
	case "CRITICAL":
		return SeverityCritical
	case "HIGH":
		return SeverityHigh
	case "MEDIUM":
		return SeverityMedium
	case "LOW":
		return SeverityLow
	default:
		return SeverityInfo
	}
}

func scanJSONLines(output string, fn func(line string) error) error {
	output = strings.TrimSpace(output)
	if output == "" {
		return nil
	}
	reader := strings.NewReader(output)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if err := fn(line); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func makeFinding(finding Finding) Finding {
	finding.Layer = LayerSAST
	finding.Severity = NormalizeSeverity(finding.Severity)
	finding.File = normalizeRepoPath(finding.File)
	if strings.TrimSpace(finding.Message) == "" {
		finding.Message = finding.Rule
	}
	if strings.TrimSpace(finding.Evidence) == "" {
		finding.Evidence = finding.Message
	}
	finding.Fingerprint = FindingFingerprint(finding)
	finding.ID = findingID(finding)
	return finding
}

func jsonString(payload map[string]json.RawMessage, names ...string) string {
	for _, name := range names {
		raw, ok := payload[name]
		if !ok {
			continue
		}
		var value string
		if err := json.Unmarshal(raw, &value); err == nil {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func jsonInt(payload map[string]json.RawMessage, names ...string) int {
	for _, name := range names {
		raw, ok := payload[name]
		if !ok {
			continue
		}
		var number int
		if err := json.Unmarshal(raw, &number); err == nil {
			return number
		}
		var text string
		if err := json.Unmarshal(raw, &text); err == nil {
			return atoi(text)
		}
	}
	return 0
}

func atoi(value string) int {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	number, err := strconv.Atoi(value)
	if err != nil || number < 0 {
		return 0
	}
	return number
}
