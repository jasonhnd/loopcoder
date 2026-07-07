package conductorhooks

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"
)

const (
	reporterScopeEnv        = "LOOPCODER_CONDUCTOR_REPORTER_SCOPE"
	reporterStateDirEnv     = "LOOPCODER_CONDUCTOR_REPORTER_STATE_DIR"
	reporterStateSub        = "conductor-reporter"
	legacyAttestScopeEnv    = "LOOPCODER_CONDUCTOR_ATTEST_SCOPE"
	legacyAttestStateDirEnv = "LOOPCODER_CONDUCTOR_ATTEST_STATE_DIR"
	legacyAttestStateSub    = "conductor-attest"
)

var (
	conductorHeaderRe   = regexp.MustCompile(`\[(?:attestation|reporter)\]\s+role=conductor\b`)
	roleConductorJSONRe = regexp.MustCompile(`"role"\s*:\s*"conductor"`)
	modelSourceSelfRe   = regexp.MustCompile(`"model_source"\s*:\s*"self-reported"`)
	verifiedFalseJSONRe = regexp.MustCompile(`"verified"\s*:\s*false`)
)

type attestStatePaths struct {
	primary string
	legacy  string
}

// attestState is the persisted per-session state for the conductor-attest gate.
// The gate only applies to sessions that actually performed a delivery or merge
// (DeliverySeen). Attested records a Conductor self-report. Reminded makes
// the Stop block one-shot so the gate can never loop even without an attest.
type attestState struct {
	Attested      bool   `json:"attested"`
	AttestedAt    string `json:"attested_at,omitempty"`
	DeliverySeen  bool   `json:"delivery_seen"`
	DeliveryAt    string `json:"delivery_at,omitempty"`
	Reminded      bool   `json:"reminded"`
	RemindedAt    string `json:"reminded_at,omitempty"`
	SessionIDHash string `json:"session_id_hash"`
	Command       string `json:"command,omitempty"`
	Header        string `json:"header,omitempty"`
}

// RunAttest is the conductor-attest hook. It fails open on any error or panic.
// The Stop gate blocks at most once, only on a turn that performed a delivery or
// merge without a Conductor self-report, and it honors Claude Code's
// stop_hook_active escape valve — so it can never loop or block planning turns.
func RunAttest(input []byte, opts Options) (res Result) {
	defer func() {
		if r := recover(); r != nil {
			res = allow()
		}
	}()

	env := opts.envLookup()
	now := opts.nowFn()

	in, ok := parseInput(input, env)
	if !ok {
		return allow()
	}

	if in.SessionID == "" || !shouldEnforce(firstEnv(env, reporterScopeEnv, legacyAttestScopeEnv), in.HookEventName, in.CWD) {
		return allow()
	}

	statePaths, err := resolveAttestStatePaths(in.SessionID, in.CWD, env)
	if err != nil {
		return allow()
	}
	pruneStateDir(dirOf(statePaths.primary), now())
	if statePaths.legacy != "" {
		pruneStateDir(dirOf(statePaths.legacy), now())
	}

	if isToolCompleteEvent(in.HookEventName) {
		recordAttestObservations(statePaths, in, now())
		return allow()
	}

	if !isStopEvent(in.HookEventName) {
		return allow()
	}

	// Honor Claude Code's stop-loop escape valve: if we are already inside a
	// Stop-hook block loop, never block again.
	if in.StopHookActive {
		return allow()
	}

	state, err := readAttestState(statePaths)
	if err != nil || state == nil {
		return allow()
	}
	// Only gate turns that actually performed a delivery or merge; leave planning
	// and chat turns untouched. Block at most once (Reminded) so the gate can
	// never loop even when the Conductor does not attest.
	if !state.DeliverySeen || state.Attested || state.Reminded {
		return allow()
	}

	state.Reminded = true
	state.RemindedAt = isoTimestamp(now())
	if err := writeStateJSON(statePaths.primary, state); err != nil {
		return allow()
	}

	return block(strings.Join([]string{
		"loopcoder conductor report is required before completing this delivery or merge turn.",
		"Run `loopcoder attest --role conductor --provider <provider> --model <model> --permission orchestrate --action \"<delivery action>\" --duration-ms <ms> --total-tokens <tokens>` with the actual host model and usage, then finish the turn.",
		"Keep the emitted report local: use command output and gitignored .loopcoder/ run records for recovery; do not copy it into PR bodies, comments, merge commits, or merge comments.",
	}, "\n"))
}

// recordAttestObservations updates per-session state from a completed shell
// tool: it marks a successful Conductor report, and separately marks that a
// delivery or merge command was observed (which is what makes the Stop gate
// apply). Fail-open: any error is swallowed.
func recordAttestObservations(statePaths attestStatePaths, in hookInput, now time.Time) {
	if !isShellTool(in.ToolName) {
		return
	}

	state, err := readAttestState(statePaths)
	if err != nil {
		return
	}
	if state == nil {
		state = &attestState{SessionIDHash: hashText(in.SessionID)[:32]}
	}

	changed := false
	if isSuccessfulConductorAttest(in) {
		state.Attested = true
		state.AttestedAt = isoTimestamp(now)
		state.Command = truncate(in.ToolInput.Command, maxTextField)
		responseText := collectStringsFromRaw(in.ToolResponse)
		state.Header = truncate(firstMatchingLine(responseText, conductorHeaderRe), maxTextField)
		changed = true
	}
	if !state.DeliverySeen && responseSucceeded(in) && deliveryOrMergeCommand(in.ToolInput.Command) {
		state.DeliverySeen = true
		state.DeliveryAt = isoTimestamp(now)
		changed = true
	}
	if changed {
		_ = writeStateJSON(statePaths.primary, state)
	}
}

func resolveAttestStatePaths(sessionID, cwd string, env func(string) string) (attestStatePaths, error) {
	if newDir := strings.TrimSpace(env(reporterStateDirEnv)); newDir != "" {
		primary, err := stateFilePath(sessionID, cwd, newDir, reporterStateSub)
		return attestStatePaths{primary: primary}, err
	}
	if oldDir := strings.TrimSpace(env(legacyAttestStateDirEnv)); oldDir != "" {
		primary, err := stateFilePath(sessionID, cwd, oldDir, legacyAttestStateSub)
		return attestStatePaths{primary: primary}, err
	}
	primary, err := stateFilePath(sessionID, cwd, "", reporterStateSub)
	if err != nil {
		return attestStatePaths{}, err
	}
	legacy, err := stateFilePath(sessionID, cwd, "", legacyAttestStateSub)
	if err != nil {
		return attestStatePaths{}, err
	}
	return attestStatePaths{primary: primary, legacy: legacy}, nil
}

func firstEnv(env func(string) string, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(env(key)); value != "" {
			return value
		}
	}
	return ""
}

func readAttestState(statePaths attestStatePaths) (*attestState, error) {
	state, err := readAttestStateFile(statePaths.primary)
	if err != nil || state != nil || statePaths.legacy == "" {
		return state, err
	}
	return readAttestStateFile(statePaths.legacy)
}

func readAttestStateFile(statePath string) (*attestState, error) {
	data, err := readStateBytes(statePath)
	if err != nil {
		return nil, err
	}
	if data == nil {
		return nil, nil
	}
	var state attestState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

// isSuccessfulConductorAttest reports whether the tool-complete event is a
// successful `loopcoder attest --role conductor` invocation whose output carries
// a conductor report.
func isSuccessfulConductorAttest(in hookInput) bool {
	if !isShellTool(in.ToolName) {
		return false
	}
	if !isConductorAttestCommand(in.ToolInput.Command) {
		return false
	}
	if !responseSucceeded(in) {
		return false
	}
	return containsConductorReport(in.ToolResponse)
}

// responseSucceeded reports whether a tool response indicates success: not
// interrupted, no error field, and a zero or absent exit code. A missing
// response object is treated as success.
func responseSucceeded(in hookInput) bool {
	resp := decodeResponseObject(in.ToolResponse)
	if resp == nil {
		return true
	}
	if resp["interrupted"] == true {
		return false
	}
	if truthy(resp["error"]) {
		return false
	}
	exit := firstDefined(resp, "exit_code", "exitCode", "status")
	if present, nonZero := numericExitIsNonZero(exit); present && nonZero {
		return false
	}
	return true
}

// deliveryOrMergeCommand reports whether a shell command performs a loopcoder
// delivery (dispatch / dispatch-wave / loopreview) or a GitHub PR merge — the
// actions that make a turn a "delivery or merge turn" for the conductor gate.
func deliveryOrMergeCommand(command string) bool {
	words := shellWords(command)
	for i := 0; i < len(words); i++ {
		// Only treat a loopcoder/gh token as the command being run when it is at
		// a command position (first word, or right after a shell separator), so a
		// mention in an argument like `echo run loopcoder dispatch later` does not
		// arm the gate.
		if !atCommandPosition(words, i) {
			continue
		}
		if isLoopcoderToken(words[i]) && i+1 < len(words) {
			switch words[i+1] {
			case "dispatch", "dispatch-wave", "loopreview":
				return true
			}
		}
		if isGhToken(words[i]) {
			rest := words[i+1 : nextSeparatorIndex(words, i+1)]
			if containsAdjacent(rest, "pr", "merge") {
				return true
			}
		}
	}
	return false
}

// atCommandPosition reports whether words[i] begins a command: it is the first
// token, or the previous token is a shell separator.
func atCommandPosition(words []string, i int) bool {
	if i == 0 {
		return true
	}
	switch words[i-1] {
	case ";", "|", "&&", "||", "&":
		return true
	}
	return false
}

func isGhToken(token string) bool {
	normalized := strings.ReplaceAll(token, "\\", "/")
	base := strings.ToLower(normalized[strings.LastIndex(normalized, "/")+1:])
	return base == "gh" || base == "gh.exe"
}

// containsAdjacent reports whether first is immediately followed by second in args.
func containsAdjacent(args []string, first, second string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == first && args[i+1] == second {
			return true
		}
	}
	return false
}

// isConductorAttestCommand reports whether the command contains a
// `loopcoder attest ... --role conductor` invocation.
func isConductorAttestCommand(command string) bool {
	words := shellWords(command)
	for i := 0; i < len(words)-1; i++ {
		if !isLoopcoderToken(words[i]) || words[i+1] != "attest" {
			continue
		}
		args := words[i+2 : nextSeparatorIndex(words, i+2)]
		if hasRoleConductor(args) {
			return true
		}
	}
	return false
}

func hasRoleConductor(args []string) bool {
	for i := 0; i < len(args); i++ {
		arg := strings.ToLower(args[i])
		if arg == "--role" || arg == "-role" {
			next := ""
			if i+1 < len(args) {
				next = strings.ToLower(args[i+1])
			}
			if next == "conductor" {
				return true
			}
		}
		if arg == "--role=conductor" || arg == "-role=conductor" {
			return true
		}
	}
	return false
}

// containsConductorReport reports whether the walked response strings carry
// a conductor report header or the JSON self-reported/unverified marker.
func containsConductorReport(raw json.RawMessage) bool {
	text := collectStringsFromRaw(raw)
	if conductorHeaderRe.MatchString(text) {
		return true
	}
	return roleConductorJSONRe.MatchString(text) &&
		modelSourceSelfRe.MatchString(text) &&
		verifiedFalseJSONRe.MatchString(text)
}
