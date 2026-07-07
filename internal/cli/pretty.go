package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/reporter"
)

func isTerminalWriter(w io.Writer) bool {
	file, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func shouldRenderPretty(suppress bool) bool {
	return !(suppress || envFlag("LOOPCODER_NO_PRETTY"))
}

func prettyModeForTarget(w io.Writer, deps Deps, forceEmoji bool) reporter.PrettyMode {
	if plainPrettyForced() {
		return reporter.PrettyModePlain
	}
	if forceEmoji || envFlag("LOOPCODER_PRETTY") {
		return reporter.PrettyModeEmoji
	}
	if prettyTargetInteractive(w, deps) {
		return reporter.PrettyModeEmoji
	}
	return reporter.PrettyModePlain
}

func renderPrettyReport(w io.Writer, record reporter.Report, mode reporter.PrettyMode) error {
	_, err := fmt.Fprintln(w, record.Pretty(reporter.PrettyOptions{Mode: mode}))
	return err
}

func prettyTargetInteractive(w io.Writer, deps Deps) bool {
	if deps.IsTerminal == nil {
		deps.IsTerminal = DefaultDeps().IsTerminal
	}
	return deps.IsTerminal(w)
}

func plainPrettyForced() bool {
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return true
	}
	return envSet("LOOPCODER_NO_EMOJI") || envSet("LOOPCODER_PLAIN")
}

func envFlag(name string) bool {
	value := strings.TrimSpace(strings.ToLower(os.Getenv(name)))
	switch value {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func envSet(name string) bool {
	value := strings.TrimSpace(strings.ToLower(os.Getenv(name)))
	switch value {
	case "", "0", "false", "no", "off":
		return false
	default:
		return true
	}
}
