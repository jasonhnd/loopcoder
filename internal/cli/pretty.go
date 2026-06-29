package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/attestation"
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

func shouldRenderPretty(w io.Writer, explicit bool, deps Deps) bool {
	return explicit || envFlag("LOOPCODER_PRETTY") || prettyTargetInteractive(w, deps)
}

func prettyModeForTarget(w io.Writer, deps Deps) attestation.PrettyMode {
	if plainPrettyForced() || !prettyTargetInteractive(w, deps) {
		return attestation.PrettyModePlain
	}
	return attestation.PrettyModeEmoji
}

func renderPrettyAttestation(w io.Writer, record attestation.AttestationRecord, mode attestation.PrettyMode) error {
	_, err := fmt.Fprintln(w, record.Pretty(attestation.PrettyOptions{Mode: mode}))
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
