package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/conductorhooks"
)

// runHook runs one embedded conductor hook by name. It is wired into a project's
// Claude Code settings via `loopcoder hook <name>` and MUST fail open: any of our
// own misconfiguration (missing or unknown hook name, nil stdin) returns exit 0,
// because a non-zero exit from a Claude Code hook blocks the session. Only the
// hook logic itself may request a non-zero exit code.
func runHook(args []string, stdout, stderr io.Writer, deps Deps) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "hook: expected a hook name")
		return 0
	}
	name := args[0]

	input := readHookInput(deps.Stdin)

	var result conductorhooks.Result
	switch name {
	case "conductor-attest":
		result = conductorhooks.RunAttest(input, conductorhooks.Options{})
	case "conductor-relay-guard":
		result = conductorhooks.RunRelayGuard(input, conductorhooks.Options{})
	default:
		fmt.Fprintf(stderr, "hook: unknown hook %q\n", name)
		return 0
	}

	if result.Stdout != "" {
		io.WriteString(stdout, result.Stdout)
	}
	if result.Stderr != "" {
		io.WriteString(stderr, result.Stderr)
		if !strings.HasSuffix(result.Stderr, "\n") {
			io.WriteString(stderr, "\n")
		}
	}
	return result.ExitCode
}

// readHookInput reads the entire hook payload from stdin. A nil reader (or any
// read error) yields empty input so the hook still fails open.
func readHookInput(stdin io.Reader) []byte {
	if stdin == nil {
		return nil
	}
	data, err := io.ReadAll(stdin)
	if err != nil {
		return nil
	}
	return data
}
