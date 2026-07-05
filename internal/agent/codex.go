package agent

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type ExecCodexRunner struct{}

const sensitiveFileMode os.FileMode = 0o600

func createSensitiveFile(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, sensitiveFileMode)
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(sensitiveFileMode); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func writeSensitiveFile(path string, data []byte) error {
	file, err := createSensitiveFile(path)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func ensureSensitiveFile(path string) error {
	file, err := createSensitiveFile(path)
	if err != nil {
		return err
	}
	return file.Close()
}

func BuildCodexArgs(inv Invocation) []string {
	args := []string{
		"exec",
		"--cd", inv.WorktreePath,
	}
	if inv.ReadOnly {
		args = append(args, "-s", "read-only")
	} else {
		args = append(args, "--dangerously-bypass-approvals-and-sandbox")
	}
	args = append(args, "--skip-git-repo-check")
	if mcpArgs := codexMCPArgs(inv); len(mcpArgs) > 0 {
		args = append(args, mcpArgs...)
	}
	if strings.TrimSpace(inv.OutputSchema) != "" {
		schemaArg := strings.TrimSpace(inv.OutputSchema)
		if strings.HasPrefix(schemaArg, "{") || strings.HasPrefix(schemaArg, "[") {
			schemaArg = codexSchemaPath(inv.LogPath)
		}
		args = append(args, "--output-schema", schemaArg)
	}
	if strings.TrimSpace(inv.Model) != "" {
		args = append(args, "-m", inv.Model)
	}
	if strings.TrimSpace(inv.Effort) != "" {
		args = append(args, "-c", "model_reasoning_effort="+inv.Effort)
	}
	args = append(args, "-o", codexSummaryPath(inv.LogPath), "-")
	return args
}

func (ExecCodexRunner) Run(ctx context.Context, inv Invocation) (Result, error) {
	if strings.TrimSpace(inv.LogPath) == "" {
		return Result{ExitCode: -1}, errors.New("codex log path is required")
	}
	if _, err := mcpServersForInvocation(inv); err != nil {
		return Result{ExitCode: -1}, fmt.Errorf("codex MCP configuration: %w", err)
	}
	if strings.TrimSpace(inv.OutputSchema) != "" {
		if err := writeSensitiveFile(codexSchemaPath(inv.LogPath), []byte(inv.OutputSchema)); err != nil {
			return Result{ExitCode: -1}, fmt.Errorf("write output schema: %w", err)
		}
	}
	promptPath := codexPromptPath(inv.LogPath)
	if err := writeSensitiveFile(promptPath, []byte(inv.Prompt)); err != nil {
		return Result{ExitCode: -1}, fmt.Errorf("write prompt: %w", err)
	}
	prompt, err := os.Open(promptPath)
	if err != nil {
		return Result{ExitCode: -1}, fmt.Errorf("open prompt: %w", err)
	}
	defer prompt.Close()

	if err := ensureSensitiveFile(codexSummaryPath(inv.LogPath)); err != nil {
		return Result{ExitCode: -1}, fmt.Errorf("create summary: %w", err)
	}

	logFile, err := createSensitiveFile(inv.LogPath)
	if err != nil {
		return Result{ExitCode: -1}, fmt.Errorf("open codex log: %w", err)
	}
	defer logFile.Close()

	cmd := exec.CommandContext(ctx, "codex", BuildCodexArgs(inv)...)
	cmd.Stdin = prompt
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	startedAt := time.Now()
	supervision, runErr := runProviderCommand(ctx, cmd, inv, "codex")
	endedAt := time.Now()
	_ = logFile.Sync()
	summary := readCodexSummary(inv.LogPath)
	exitCode := supervisedExitCode(supervision, runErr)
	logBytes, logErr := os.ReadFile(inv.LogPath)
	metadata := invocationMetadata{}
	var metadataErr error
	if logErr == nil {
		metadata = parseCodexInvocation(logBytes)
		if runErr == nil && exitCode == 0 {
			metadataErr = validateCodexSuccessMetadata(metadata)
		}
	}
	result := resultWithSupervision(exitCode, summary, metadata, startedAt, endedAt, supervision, runErr, ctx)
	if runErr != nil {
		if logErr != nil {
			return result, errors.Join(runErr, fmt.Errorf("read codex log: %w", logErr))
		}
		return result, runErr
	}
	if logErr != nil {
		return result, fmt.Errorf("read codex log: %w", logErr)
	}
	if metadataErr != nil {
		return result, metadataErr
	}
	return result, nil
}

func codexPromptPath(logPath string) string {
	return filepath.Join(filepath.Dir(logPath), "prompt.txt")
}

func codexSummaryPath(logPath string) string {
	return filepath.Join(filepath.Dir(logPath), "summary.txt")
}

func codexSchemaPath(logPath string) string {
	return filepath.Join(filepath.Dir(logPath), "schema.json")
}

func codexMCPArgs(inv Invocation) []string {
	servers := mcpServersForArgs(inv)
	if len(servers) == 0 {
		return nil
	}

	args := []string{"--ignore-user-config"}
	for _, server := range servers {
		prefix := "mcp_servers." + tomlQuotedKey(server.Name)
		transport, _ := mcpServerTransport(server)
		switch transport {
		case "stdio":
			args = append(args,
				"-c", prefix+".command="+tomlString(strings.TrimSpace(server.Command)),
			)
			if len(server.Args) > 0 {
				args = append(args, "-c", prefix+".args="+tomlStringArray(server.Args))
			}
		case "http":
			args = append(args, "-c", prefix+".url="+tomlString(strings.TrimSpace(server.URL)))
			if mcpAuthConfigured(server.Auth) {
				header := strings.TrimSpace(server.Auth.Header)
				envName := strings.TrimSpace(server.Auth.Env)
				if strings.EqualFold(header, "Authorization") {
					args = append(args, "-c", prefix+".bearer_token_env_var="+tomlString(envName))
				} else {
					args = append(args, "-c", prefix+".env_http_headers."+tomlQuotedKey(header)+"="+tomlString(envName))
				}
			}
		}
	}
	return args
}

func mcpServersForArgs(inv Invocation) []MCPServer {
	servers, err := mcpServersForInvocation(inv)
	if err != nil {
		return nil
	}
	return servers
}

func mcpServersForInvocation(inv Invocation) ([]MCPServer, error) {
	if len(inv.MCPServers) == 0 {
		return nil, nil
	}

	role := normalizeMCPRole(inv.Role)
	if role != "" && !validMCPRole(role) {
		return nil, fmt.Errorf("invalid invocation role %q", inv.Role)
	}

	requiresRole := false
	for index, server := range inv.MCPServers {
		if err := validateMCPRoles(index, server.Roles); err != nil {
			return nil, err
		}
		if len(server.Roles) > 0 {
			requiresRole = true
		}
	}
	if requiresRole && role == "" {
		return nil, errors.New("invocation role is required when MCP servers declare role filters")
	}

	servers := make([]MCPServer, 0, len(inv.MCPServers))
	for index, server := range inv.MCPServers {
		if !mcpRoleAllowed(server.Roles, role) {
			continue
		}
		if err := validateMCPServer(index, server); err != nil {
			return nil, err
		}
		if inv.ReadOnly && !server.ReadOnly {
			return nil, fmt.Errorf("mcp.servers[%d] %q is not locally classified read-only for a read-only invocation", index, server.Name)
		}
		servers = append(servers, server)
	}
	if len(servers) == 0 {
		return nil, nil
	}
	return servers, nil
}

func validateMCPRoles(index int, roles []string) error {
	for _, role := range roles {
		normalized := normalizeMCPRole(role)
		if !validMCPRole(normalized) {
			return fmt.Errorf("mcp.servers[%d].roles contains unknown role %q", index, role)
		}
	}
	return nil
}

func validateMCPServer(index int, server MCPServer) error {
	if !validMCPServerName(server.Name) {
		return fmt.Errorf("mcp.servers[%d].name %q is not a safe provider MCP server name", index, server.Name)
	}

	transport, err := mcpServerTransport(server)
	if err != nil {
		return fmt.Errorf("mcp.servers[%d] %q: %w", index, server.Name, err)
	}
	switch transport {
	case "stdio":
		if strings.TrimSpace(server.Command) == "" {
			return fmt.Errorf("mcp.servers[%d] %q stdio transport requires command", index, server.Name)
		}
		if strings.TrimSpace(server.URL) != "" {
			return fmt.Errorf("mcp.servers[%d] %q stdio transport cannot include url", index, server.Name)
		}
		if mcpAuthConfigured(server.Auth) {
			return fmt.Errorf("mcp.servers[%d] %q stdio transport cannot use HTTP auth", index, server.Name)
		}
	case "http":
		if strings.TrimSpace(server.URL) == "" {
			return fmt.Errorf("mcp.servers[%d] %q http transport requires url", index, server.Name)
		}
		if strings.TrimSpace(server.Command) != "" || len(server.Args) > 0 {
			return fmt.Errorf("mcp.servers[%d] %q http transport cannot include command or args", index, server.Name)
		}
		if !validMCPHTTPURL(server.URL) {
			return fmt.Errorf("mcp.servers[%d] %q http transport requires an http or https url", index, server.Name)
		}
		if err := validateMCPAuth(index, server); err != nil {
			return err
		}
	default:
		return fmt.Errorf("mcp.servers[%d] %q has unsupported transport %q", index, server.Name, server.Transport)
	}
	return nil
}

func validateMCPAuth(index int, server MCPServer) error {
	header := strings.TrimSpace(server.Auth.Header)
	envName := strings.TrimSpace(server.Auth.Env)
	if header == "" && envName == "" {
		return nil
	}
	if header == "" || envName == "" {
		return fmt.Errorf("mcp.servers[%d] %q http auth requires both header and env", index, server.Name)
	}
	if !validHTTPHeaderName(header) {
		return fmt.Errorf("mcp.servers[%d] %q http auth header %q is invalid", index, server.Name, header)
	}
	if !validEnvName(envName) {
		return fmt.Errorf("mcp.servers[%d] %q http auth env %q is invalid", index, server.Name, envName)
	}
	return nil
}

func mcpServerTransport(server MCPServer) (string, error) {
	transport := strings.ToLower(strings.TrimSpace(server.Transport))
	if transport == "" {
		switch {
		case strings.TrimSpace(server.URL) != "":
			return "http", nil
		case strings.TrimSpace(server.Command) != "":
			return "stdio", nil
		default:
			return "", errors.New("transport is required")
		}
	}
	switch transport {
	case "stdio", "http":
		return transport, nil
	default:
		return "", fmt.Errorf("unsupported transport %q", server.Transport)
	}
}

func normalizeMCPRole(role string) string {
	return strings.ToLower(strings.TrimSpace(role))
}

func validMCPRole(role string) bool {
	switch role {
	case "worker", "verifier":
		return true
	default:
		return false
	}
}

func mcpRoleAllowed(roles []string, role string) bool {
	if len(roles) == 0 {
		return true
	}
	for _, candidate := range roles {
		if normalizeMCPRole(candidate) == role {
			return true
		}
	}
	return false
}

func validMCPServerName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}

func validMCPHTTPURL(rawURL string) bool {
	rawURL = strings.TrimSpace(rawURL)
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" {
		return false
	}
	return parsed.Scheme == "https" || parsed.Scheme == "http"
}

func mcpAuthConfigured(auth MCPAuth) bool {
	return strings.TrimSpace(auth.Header) != "" || strings.TrimSpace(auth.Env) != ""
}

func mcpAuthHeaderValue(auth MCPAuth) string {
	envName := strings.TrimSpace(auth.Env)
	if envName == "" {
		return ""
	}
	if strings.EqualFold(strings.TrimSpace(auth.Header), "Authorization") {
		return "Bearer ${" + envName + "}"
	}
	return "${" + envName + "}"
}

func validHTTPHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case strings.ContainsRune("!#$%&'*+-.^_`|~", r):
		default:
			return false
		}
	}
	return true
}

func validEnvName(name string) bool {
	if name == "" {
		return false
	}
	for index, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r == '_':
		case index > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

func tomlQuotedKey(value string) string {
	return strconv.Quote(strings.TrimSpace(value))
}

func tomlString(value string) string {
	return strconv.Quote(value)
}

func tomlStringArray(values []string) string {
	var b strings.Builder
	b.WriteByte('[')
	for index, value := range values {
		if index > 0 {
			b.WriteString(", ")
		}
		b.WriteString(tomlString(value))
	}
	b.WriteByte(']')
	return b.String()
}

func readCodexSummary(logPath string) string {
	summaryBytes, err := os.ReadFile(codexSummaryPath(logPath))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(summaryBytes))
}

func parseCodexInvocation(output []byte) invocationMetadata {
	text := string(output)
	metadata := invocationMetadata{
		Model:  parseCodexHeaderValue(text, "model"),
		Effort: parseCodexHeaderValue(text, "reasoning effort"),
	}
	if totalTokens, ok := parseCodexTotalTokens(text); ok {
		metadata.Usage.TotalTokens = &totalTokens
	}
	return metadata
}

func validateCodexSuccessMetadata(metadata invocationMetadata) error {
	var missing []string
	if strings.TrimSpace(metadata.Model) == "" {
		missing = append(missing, "model")
	}
	if metadata.Usage.TotalTokens == nil && (metadata.Usage.InputTokens == nil || metadata.Usage.OutputTokens == nil) {
		missing = append(missing, "token usage")
	}
	if len(missing) > 0 {
		return fmt.Errorf("codex metadata parse failed: missing %s", strings.Join(missing, ", "))
	}
	return nil
}

func parseCodexHeaderValue(text, label string) string {
	prefix := strings.ToLower(label) + ":"
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(trimmed), prefix) {
			return strings.TrimSpace(trimmed[len(prefix):])
		}
	}
	return ""
}

func parseCodexTotalTokens(text string) (int64, bool) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for index, line := range lines {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)
		if !strings.HasPrefix(lower, "tokens used") {
			continue
		}

		remainder := strings.TrimSpace(trimmed[len("tokens used"):])
		remainder = strings.TrimPrefix(remainder, ":")
		remainder = strings.TrimSpace(remainder)
		if total, ok := parseTokenCount(remainder); ok {
			return total, true
		}

		for next := index + 1; next < len(lines); next++ {
			nextLine := strings.TrimSpace(lines[next])
			if nextLine == "" {
				continue
			}
			return parseTokenCount(nextLine)
		}
	}
	return 0, false
}

func parseTokenCount(text string) (int64, bool) {
	var cleaned strings.Builder
	for _, r := range strings.TrimSpace(text) {
		switch {
		case r >= '0' && r <= '9':
			cleaned.WriteRune(r)
		case r == ',' || r == ' ' || r == '\t':
			continue
		default:
			return 0, false
		}
	}
	if cleaned.Len() == 0 {
		return 0, false
	}
	value, err := strconv.ParseInt(cleaned.String(), 10, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}
