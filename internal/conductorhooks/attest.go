package conductorhooks

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"
)

const (
	attestScopeEnv    = "LOOPCODER_CONDUCTOR_ATTEST_SCOPE"
	attestStateDirEnv = "LOOPCODER_CONDUCTOR_ATTEST_STATE_DIR"
	attestStateSub    = "conductor-attest"
)

var (
	conductorHeaderRe   = regexp.MustCompile(`\[attestation\]\s+role=conductor\b`)
	roleConductorJSONRe = regexp.MustCompile(`"role"\s*:\s*"conductor"`)
	modelSourceSelfRe   = regexp.MustCompile(`"model_source"\s*:\s*"self-reported"`)
	verifiedFalseJSONRe = regexp.MustCompile(`"verified"\s*:\s*false`)
)

// attestState is the persisted state written after a successful conductor
// attestation is observed.
type attestState struct {
	Attested      bool   `json:"attested"`
	AttestedAt    string `json:"attested_at"`
	SessionIDHash string `json:"session_id_hash"`
	Command       string `json:"command"`
	Header        string `json:"header"`
}

// RunAttest is the Go port of conductor-attest.js runHook. It fails open on any
// error or panic (see the deferred recover).
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

	if in.SessionID == "" || !shouldEnforce(env(attestScopeEnv), in.HookEventName, in.CWD) {
		return allow()
	}

	statePath, err := stateFilePath(in.SessionID, in.CWD, env(attestStateDirEnv), attestStateSub)
	if err != nil {
		return allow()
	}
	pruneStateDir(dirOf(statePath), now())

	if isToolCompleteEvent(in.HookEventName) {
		if isSuccessfulConductorAttest(in) {
			if err := writeAttestState(statePath, in, now()); err != nil {
				return allow()
			}
		}
		return allow()
	}

	if !isStopEvent(in.HookEventName) {
		return allow()
	}

	data, err := readStateBytes(statePath)
	if err != nil {
		return allow()
	}
	if data != nil {
		var state attestState
		if err := json.Unmarshal(data, &state); err == nil && state.Attested {
			return allow()
		}
	}

	return block(strings.Join([]string{
		"loopcoder conductor attestation is required before completing this delivery turn.",
		"Run `loopcoder attest --role conductor --provider <provider> --model <model> --permission orchestrate --action \"<delivery action>\" --duration-ms <ms> --total-tokens <tokens>` with the actual host model and usage, then finish the turn.",
		"Keep the emitted attestation local: use command output and gitignored .loopcoder/ run records for recovery; do not copy it into PR bodies, comments, merge commits, or merge comments.",
	}, "\n"))
}

// writeAttestState persists proof that a conductor attestation was observed.
func writeAttestState(statePath string, in hookInput, now time.Time) error {
	command := in.ToolInput.Command
	responseText := collectStringsFromRaw(in.ToolResponse)
	header := firstMatchingLine(responseText, conductorHeaderRe)

	state := attestState{
		Attested:      true,
		AttestedAt:    isoTimestamp(now),
		SessionIDHash: hashText(in.SessionID)[:32],
		Command:       truncate(command, maxTextField),
		Header:        truncate(header, maxTextField),
	}
	return writeStateJSON(statePath, state)
}

// isSuccessfulConductorAttest reports whether the tool-complete event is a
// successful `loopcoder attest --role conductor` invocation whose output carries
// a conductor attestation.
func isSuccessfulConductorAttest(in hookInput) bool {
	if !isShellTool(in.ToolName) {
		return false
	}
	if !isConductorAttestCommand(in.ToolInput.Command) {
		return false
	}

	if resp := decodeResponseObject(in.ToolResponse); resp != nil {
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
	}

	return containsConductorAttestation(in.ToolResponse)
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

// containsConductorAttestation reports whether the walked response strings carry
// a conductor attestation header or the JSON self-reported/unverified marker.
func containsConductorAttestation(raw json.RawMessage) bool {
	text := collectStringsFromRaw(raw)
	if conductorHeaderRe.MatchString(text) {
		return true
	}
	return roleConductorJSONRe.MatchString(text) &&
		modelSourceSelfRe.MatchString(text) &&
		verifiedFalseJSONRe.MatchString(text)
}
