package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type ClaudeRunner struct{}

func init() {
	registry["claude"] = ClaudeRunner{}
}

func BuildClaudeArgs(inv Invocation) []string {
	mcpServers := mcpServersForArgs(inv)
	args := []string{
		"--print",
	}
	if inv.ReadOnly {
		if len(mcpServers) > 0 {
			args = append(args,
				"--permission-mode", "plan",
				"--no-session-persistence",
				"--allowedTools", claudeReadOnlyAllowedTools(mcpServers),
			)
		} else {
			args = append(args,
				"--safe-mode",
				"--no-session-persistence",
				"--allowedTools", "Read Grep Glob",
			)
		}
	} else {
		args = append(args, "--dangerously-skip-permissions")
	}
	args = append(args, "--output-format", "stream-json", "--verbose")
	if len(mcpServers) > 0 {
		args = append(args, "--mcp-config", claudeMCPConfigPath(inv.LogPath), "--strict-mcp-config")
	}
	if strings.TrimSpace(inv.OutputSchema) != "" {
		args = append(args, "--json-schema", inv.OutputSchema)
	}
	if strings.TrimSpace(inv.Model) != "" {
		args = append(args, "--model", inv.Model)
	}
	if strings.TrimSpace(inv.Effort) != "" {
		args = append(args, "--effort", inv.Effort)
	}
	return args
}

func (ClaudeRunner) Run(ctx context.Context, inv Invocation) (Result, error) {
	if strings.TrimSpace(inv.LogPath) == "" {
		return Result{ExitCode: -1}, errors.New("claude log path is required")
	}
	if inv.BoundedWrite {
		return Result{ExitCode: -1}, errors.New("claude bounded-write mode is not registered because project settings and hooks cannot be isolated")
	}
	if err := validateNestedDelegationBoundary(inv); err != nil {
		return Result{ExitCode: -1}, err
	}
	mcpServers, err := mcpServersForInvocation(inv)
	if err != nil {
		return Result{ExitCode: -1}, fmt.Errorf("claude MCP configuration: %w", err)
	}
	if len(mcpServers) > 0 {
		if err := writeClaudeMCPConfig(inv.LogPath, mcpServers); err != nil {
			return Result{ExitCode: -1}, err
		}
	}

	logFile, err := createSensitiveFile(inv.LogPath)
	if err != nil {
		return Result{ExitCode: -1}, fmt.Errorf("open claude log: %w", err)
	}
	defer logFile.Close()

	var stdout bytes.Buffer
	cmd := exec.CommandContext(ctx, "claude", BuildClaudeArgs(inv)...)
	cmd.Dir = inv.WorktreePath
	cmd.Stdin = strings.NewReader(inv.Prompt)
	cmd.Stdout = io.MultiWriter(logFile, &stdout)
	cmd.Stderr = logFile

	startedAt := time.Now()
	supervision, runErr := runProviderCommand(ctx, cmd, inv, "claude")
	endedAt := time.Now()
	summary := parseClaudeSummary(stdout.Bytes())
	metadata := parseClaudeInvocation(stdout.Bytes(), inv)
	result := resultWithSupervision(supervisedExitCode(supervision, runErr), summary, metadata, startedAt, endedAt, supervision, runErr, ctx)
	if runErr != nil {
		return result, runErr
	}
	return result, nil
}

func claudeReadOnlyAllowedTools(servers []MCPServer) string {
	tools := []string{"Read", "Grep", "Glob"}
	for _, server := range servers {
		tools = append(tools, "mcp__"+strings.TrimSpace(server.Name)+"__*")
	}
	return strings.Join(tools, " ")
}

func writeClaudeMCPConfig(logPath string, servers []MCPServer) error {
	config := struct {
		MCPServers map[string]claudeMCPServer `json:"mcpServers"`
	}{
		MCPServers: make(map[string]claudeMCPServer, len(servers)),
	}
	for _, server := range servers {
		converted, err := claudeMCPServerConfig(server)
		if err != nil {
			return fmt.Errorf("render claude MCP config for %q: %w", server.Name, err)
		}
		config.MCPServers[strings.TrimSpace(server.Name)] = converted
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("render claude MCP config: %w", err)
	}
	data = append(data, '\n')
	if err := writeSensitiveFile(claudeMCPConfigPath(logPath), data); err != nil {
		return fmt.Errorf("write claude MCP config: %w", err)
	}
	return nil
}

type claudeMCPServer struct {
	Type    string            `json:"type"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

func claudeMCPServerConfig(server MCPServer) (claudeMCPServer, error) {
	transport, err := mcpServerTransport(server)
	if err != nil {
		return claudeMCPServer{}, err
	}
	switch transport {
	case "stdio":
		return claudeMCPServer{
			Type:    "stdio",
			Command: strings.TrimSpace(server.Command),
			Args:    append([]string(nil), server.Args...),
		}, nil
	case "http":
		config := claudeMCPServer{
			Type: "http",
			URL:  strings.TrimSpace(server.URL),
		}
		if mcpAuthConfigured(server.Auth) {
			config.Headers = map[string]string{
				strings.TrimSpace(server.Auth.Header): mcpAuthHeaderValue(server.Auth),
			}
		}
		return config, nil
	default:
		return claudeMCPServer{}, fmt.Errorf("unsupported transport %q", server.Transport)
	}
}

func claudeMCPConfigPath(logPath string) string {
	return filepath.Join(filepath.Dir(logPath), "claude-mcp.json")
}

func parseClaudeSummary(output []byte) string {
	resultEvent := extractClaudeResultEvent(output)
	if len(resultEvent) == 0 {
		return ""
	}

	var payload struct {
		Result           string          `json:"result"`
		StructuredOutput json.RawMessage `json:"structured_output"`
	}
	if err := json.Unmarshal(resultEvent, &payload); err != nil {
		return ""
	}
	if structured := bytes.TrimSpace(payload.StructuredOutput); len(structured) > 0 && !bytes.Equal(structured, []byte("null")) {
		var compacted bytes.Buffer
		if err := json.Compact(&compacted, structured); err == nil {
			return compacted.String()
		}
		return string(structured)
	}
	return strings.TrimSpace(payload.Result)
}

func parseClaudeInvocation(output []byte, inv Invocation) invocationMetadata {
	metadata := invocationMetadata{
		Effort: strings.TrimSpace(inv.Effort),
	}

	resultEvent := extractClaudeResultEvent(output)
	if len(resultEvent) == 0 {
		return metadata
	}

	var payload struct {
		ModelUsage map[string]json.RawMessage `json:"modelUsage"`
		Usage      struct {
			InputTokens  json.RawMessage `json:"input_tokens"`
			OutputTokens json.RawMessage `json:"output_tokens"`
			TotalTokens  json.RawMessage `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(resultEvent, &payload); err != nil {
		return metadata
	}

	metadata.Model = claudePrimaryModel(payload.ModelUsage)
	if inv.Model != "" {
		if _, ok := payload.ModelUsage[inv.Model]; ok {
			metadata.Model = inv.Model
		}
	}
	if inputTokens, ok := parseRawInt64(payload.Usage.InputTokens); ok {
		metadata.Usage.InputTokens = inputTokens
	}
	if outputTokens, ok := parseRawInt64(payload.Usage.OutputTokens); ok {
		metadata.Usage.OutputTokens = outputTokens
	}
	if totalTokens, ok := parseRawInt64(payload.Usage.TotalTokens); ok {
		metadata.Usage.TotalTokens = totalTokens
	}
	return metadata
}

func extractClaudeResultEvent(output []byte) []byte {
	var result []byte
	for _, line := range bytes.Split(output, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			continue
		}
		var event struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(trimmed, &event); err != nil {
			continue
		}
		if event.Type == "result" {
			result = append([]byte(nil), trimmed...)
		}
	}
	if len(result) > 0 {
		return result
	}

	trimmed := bytes.TrimSpace(output)
	if len(trimmed) == 0 {
		return nil
	}
	var event struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(trimmed, &event); err != nil || event.Type != "result" {
		return nil
	}
	return append([]byte(nil), trimmed...)
}

func claudePrimaryModel(values map[string]json.RawMessage) string {
	if len(values) == 0 {
		return ""
	}
	type candidate struct {
		model  string
		tokens int64
	}
	best := candidate{}
	for model, raw := range values {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		var usage struct {
			InputTokens  json.RawMessage `json:"inputTokens"`
			OutputTokens json.RawMessage `json:"outputTokens"`
		}
		_ = json.Unmarshal(raw, &usage)
		tokens := int64(0)
		if input, ok := parseRawInt64(usage.InputTokens); ok && input != nil {
			tokens += *input
		}
		if output, ok := parseRawInt64(usage.OutputTokens); ok && output != nil {
			tokens += *output
		}
		if best.model == "" || tokens > best.tokens || (tokens == best.tokens && model < best.model) {
			best = candidate{model: model, tokens: tokens}
		}
	}
	if best.model == "" {
		return firstSortedKey(values)
	}
	return best.model
}
