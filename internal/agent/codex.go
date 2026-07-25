package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/config"
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
		if inv.DisableDelegation {
			args = append(args,
				"--ephemeral",
				"--ignore-user-config",
				"--ignore-rules",
				"--disable", "multi_agent",
			)
		}
	} else if inv.BoundedWrite {
		args = append(args,
			"-s", "workspace-write",
			"--ephemeral",
			"--ignore-user-config",
			"--ignore-rules",
			"--disable", "multi_agent",
			"-c", "sandbox_workspace_write.network_access=false",
			"-c", "sandbox_workspace_write.exclude_tmpdir_env_var=true",
			"-c", "sandbox_workspace_write.exclude_slash_tmp=true",
			"-c", `shell_environment_policy.inherit="core"`,
			"-c", "shell_environment_policy.ignore_default_excludes=false",
			"-c", "allow_login_shell=false",
		)
		args = append(args, codexShellEnvironmentArgs(inv.Environment)...)
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

func codexShellEnvironmentArgs(environment map[string]string) []string {
	keys := make([]string, 0, len(environment))
	for key := range environment {
		if validShellEnvironmentKey(key) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	args := make([]string, 0, len(keys)*2)
	for _, key := range keys {
		args = append(args, "-c", "shell_environment_policy.set."+key+"="+strconv.Quote(environment[key]))
	}
	return args
}

func validShellEnvironmentKey(key string) bool {
	if key == "" {
		return false
	}
	for index, char := range key {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || char == '_' || (index > 0 && char >= '0' && char <= '9') {
			continue
		}
		return false
	}
	return true
}

func (ExecCodexRunner) Run(ctx context.Context, inv Invocation) (Result, error) {
	if strings.TrimSpace(inv.LogPath) == "" {
		return Result{ExitCode: -1}, errors.New("codex log path is required")
	}
	if inv.ReadOnly && inv.BoundedWrite {
		return Result{ExitCode: -1}, errors.New("codex invocation cannot be both read-only and bounded-write")
	}
	if err := validateNestedDelegationBoundary(inv); err != nil {
		return Result{ExitCode: -1}, err
	}
	if _, err := mcpServersForInvocation(inv); err != nil {
		return Result{ExitCode: -1}, fmt.Errorf("codex MCP configuration: %w", err)
	}
	if inv.BoundedWrite && len(mcpServersForArgs(inv)) > 0 {
		return Result{ExitCode: -1}, errors.New("codex bounded-write invocation cannot enable MCP servers")
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

	// Effective execution environment (process + inv.Environment overrides).
	effEnv := environmentWithOverrides(os.Environ(), inv.Environment)
	// Preflight exact account from the SAME effective env before launch —
	// never consume wrong-account capacity then reject afterward.
	bind, aerr := preflightCodexAccountBinding(inv, effEnv)
	if aerr != nil {
		return Result{
			ExitCode: -1, FailureClass: "auth_refusal",
			ActualProvider: "codex",
		}, aerr
	}

	cmd := exec.CommandContext(ctx, "codex", BuildCodexArgs(inv)...)
	cmd.Env = effEnv
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
	exe := ""
	if p, err := exec.LookPath("codex"); err == nil {
		exe = p
	}
	AffirmBasicActual(&result, "codex", exe, inv)
	// Re-affirm from the same effective env after launch (must match preflight).
	post, perr := ParseCodexAuthFromEnv(effEnv)
	if perr != nil {
		result.FailureClass = "auth_refusal"
		if runErr != nil {
			return result, errors.Join(runErr, perr)
		}
		return result, perr
	}
	if post.AccountProfileID != bind.AccountProfileID {
		result.FailureClass = "auth_refusal"
		return result, fmt.Errorf("codex: account drifted during execution preflight=%s post=%s",
			bind.AccountProfileID, post.AccountProfileID)
	}
	result.ActualAccountRef = bind.AccountProfileID
	result.ActualSourceAccount = ActualSourceAuthBinding // auth.json binding, not stream
	if strings.TrimSpace(result.ActualInstallRef) != "" {
		result.ActualSourceInstall = ActualSourceInstallBinding
	}
	argv := append([]string{"codex"}, BuildCodexArgs(inv)...)
	result.ArgvDigest = RedactedArgvDigest(argv)
	if inv.CapabilityProbeOnly && result.ExitCode != 0 && codexLogReportsModelUnavailable(logBytes) {
		result.FailureClass = "model_unavailable"
	}
	// Failures first — never accepted_invocation on partial/failed runs.
	if runErr != nil {
		ClearAcceptedActual(&result)
		if logErr != nil {
			return result, errors.Join(runErr, fmt.Errorf("read codex log: %w", logErr))
		}
		return result, runErr
	}
	if logErr != nil {
		ClearAcceptedActual(&result)
		return result, fmt.Errorf("read codex log: %w", logErr)
	}
	if metadataErr != nil {
		ClearAcceptedActual(&result)
		return result, metadataErr
	}
	if exitCode != 0 {
		ClearAcceptedActual(&result)
		return result, nil
	}
	// FULL success only: exact sandbox / -m / model_reasoning_effort options.
	AffirmAcceptedInvocation(&result, inv, argv, true, AcceptedInvocationOpts{
		PermissionNoFallback: true,
		ModelNoFallback:      true,
		EffortNoFallback:     true,
	})
	if want := strings.TrimSpace(inv.Effort); want != "" && strings.TrimSpace(result.ActualEffort) == "" {
		result.FailureClass = "route_mismatch"
		ClearAcceptedActual(&result)
		return result, fmt.Errorf("codex: depth %q requested but not reported (actual effort unknown)", want)
	}
	return result, nil
}

// codexLogReportsModelUnavailable recognizes only provider/CLI terminal model
// refusal phrases. It must not classify generic process errors or quota/auth
// failures as model_unavailable.
func codexLogReportsModelUnavailable(logBytes []byte) bool {
	s := strings.ToLower(string(logBytes))
	switch {
	case strings.Contains(s, "model is not supported") &&
		strings.Contains(s, "chatgpt account"):
		return true
	case strings.Contains(s, "model is not available") &&
		strings.Contains(s, "chatgpt account"):
		return true
	case strings.Contains(s, "model") &&
		strings.Contains(s, "does not exist or you do not have access"):
		return true
	case strings.Contains(s, "not recognized as a known model"):
		return true
	default:
		return false
	}
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
	return config.MCPServersForInvocation(config.MCP{Servers: inv.MCPServers}, config.MCPInvocationOptions{
		Role:                     mcpInvocationRole(inv.Role),
		ReadOnly:                 inv.ReadOnly,
		InvocationRoleError:      "invalid invocation role",
		InvocationRoleErrorValue: inv.Role,
	})
}

func mcpServerTransport(server MCPServer) (string, error) {
	return config.MCPServerTransport(server)
}

func mcpAuthConfigured(auth MCPAuth) bool {
	return config.MCPAuthConfigured(auth)
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

func stripANSI(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	for i := 0; i < len(text); {
		if text[i] == 0x1b && i+1 < len(text) && text[i+1] == '[' {
			j := i + 2
			for j < len(text) {
				c := text[j]
				j++
				if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
					break
				}
			}
			i = j
			continue
		}
		b.WriteByte(text[i])
		i++
	}
	return b.String()
}

func parseCodexInvocation(output []byte) invocationMetadata {
	text := stripANSI(string(output))
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
