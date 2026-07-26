package agent

import (
	"os"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/grokauth"
	"github.com/jasonhnd/loopcoder/internal/providerinstall"
)

// ParseActiveGrokAuth is the agent-facing wrapper around the shared grokauth parser.
// Never exposes SourcePath or raw identity to callers beyond Binding fields.
func ParseActiveGrokAuth() (grokauth.Binding, error) {
	bind, err := grokauth.ParseActive("", os.Getenv, time.Now().UTC())
	// Redact source path from any return path that might be logged.
	bind.SourcePath = ""
	return bind, err
}

// RequireGrokAccountMatch requires the active official auth account to equal the
// selected exact AccountRef. Shared with providerinventory identity.
func RequireGrokAccountMatch(requestedAccountRef string) (grokauth.Binding, error) {
	bind, err := grokauth.RequireMatch(requestedAccountRef, "", os.Getenv, time.Now().UTC())
	bind.SourcePath = ""
	return bind, err
}

// loadGrokSelectedToken returns the exact token for the active selected account.
func loadGrokSelectedToken() (string, grokauth.Binding, error) {
	tok, bind, err := grokauth.LoadToken("", os.Getenv, time.Now().UTC())
	bind.SourcePath = ""
	return tok, bind, err
}

// forceGrokAPIKey strips any existing XAI_API_KEY and injects the exact selected
// account token. An unrelated pre-set key must never win over the selected account.
func forceGrokAPIKey(environ []string, token string) []string {
	token = strings.TrimSpace(token)
	out := make([]string, 0, len(environ)+1)
	for _, entry := range environ {
		key, _, ok := strings.Cut(entry, "=")
		if ok && key == "XAI_API_KEY" {
			continue
		}
		out = append(out, entry)
	}
	if token != "" {
		out = append(out, "XAI_API_KEY="+token)
	}
	return out
}

// injectGrokCLIAuthAsAPIKey is retained for tests that exercise the inject-when-
// unset path. Production execution uses forceGrokAPIKey with the selected token.
func injectGrokCLIAuthAsAPIKey(environ []string) []string {
	for _, entry := range environ {
		key, value, ok := strings.Cut(entry, "=")
		if ok && key == "XAI_API_KEY" && strings.TrimSpace(value) != "" {
			// Production path never preserves an unrelated key; this helper is
			// test-only compatibility. Callers that need selected-account binding
			// must use forceGrokAPIKey.
			return environ
		}
	}
	tok, _, err := grokauth.LoadToken("", os.Getenv, time.Now().UTC())
	if err != nil || tok == "" {
		return environ
	}
	return append(append([]string(nil), environ...), "XAI_API_KEY="+tok)
}

// resolveGrokInstallID returns the pinst_* for the exact grok executable that
// will be launched, plus redacted path evidence.
func resolveGrokInstallID() (installID, absPath, redacted string, err error) {
	id, path, err := providerinstall.ComputeFromCommand("grok", grokCommand)
	if err != nil {
		return "", "", "", err
	}
	return id, path, providerinstall.RedactedExecutableEvidence(path), nil
}
