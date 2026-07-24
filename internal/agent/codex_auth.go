package agent

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/codexauth"
)

// ParseActiveCodexAuth returns the active Codex account binding (opaque acct-).
// Never exposes source path or raw identity. Uses process os.Getenv only —
// callers that have an effective execution environment must use
// ParseCodexAuthFromEnv.
func ParseActiveCodexAuth() (codexauth.Binding, error) {
	return codexauth.ParseActive("", os.Getenv, time.Now().UTC())
}

// RequireCodexAccountMatch requires active Codex auth to equal the selected AccountRef.
func RequireCodexAccountMatch(requestedAccountRef string) (codexauth.Binding, error) {
	return codexauth.RequireMatch(requestedAccountRef, "", os.Getenv, time.Now().UTC())
}

// ParseCodexAuthFromEnv parses auth using the effective execution environment
// (after inv.Environment overrides). CODEX_HOME / HOME from the env slice win
// over ambient process env so launch credentials match account affirmation.
func ParseCodexAuthFromEnv(environ []string) (codexauth.Binding, error) {
	getenv := getenvFromEnviron(environ)
	home := strings.TrimSpace(getenv("CODEX_HOME"))
	if home == "" {
		// AuthPath with empty home uses getenv HOME / UserHomeDir.
		return codexauth.ParseActive("", getenv, time.Now().UTC())
	}
	// When CODEX_HOME is set, AuthPath joins CODEX_HOME/auth.json via getenv.
	return codexauth.ParseActive("", getenv, time.Now().UTC())
}

// preflightCodexAccountBinding verifies exact account identity against the
// effective execution environment BEFORE launch. Ambient OPENAI_API_KEY is
// always ambiguous (API key is not auth.json account) — strip or fail closed
// regardless of whether AccountRef is pinned. Never label API-key session as
// an auth.json account.
func preflightCodexAccountBinding(inv Invocation, environ []string) (codexauth.Binding, error) {
	getenv := getenvFromEnviron(environ)
	// Always refuse ambient API key: it is not an auth.json ChatGPT account and
	// must never be stamped as ActualAccountRef from auth.json.
	if k := strings.TrimSpace(getenv("OPENAI_API_KEY")); k != "" && k != "null" {
		return codexauth.Binding{}, fmt.Errorf("codex: OPENAI_API_KEY present in launch env — refuse ambiguous ambient credential (strip key or launch selected install without API key; never label as auth.json account)")
	}
	bind, err := ParseCodexAuthFromEnv(environ)
	if err != nil {
		return codexauth.Binding{}, err
	}
	if !bind.ExactRoutable || bind.AccountProfileID == "" {
		return bind, fmt.Errorf("codex: active auth has no exact-routable account identity")
	}
	if want := strings.TrimSpace(inv.AccountRef); want != "" && !strings.EqualFold(want, bind.AccountProfileID) {
		return bind, fmt.Errorf("codex: account mismatch requested=%s active=%s", want, bind.AccountProfileID)
	}
	return bind, nil
}

func getenvFromEnviron(environ []string) func(string) string {
	m := map[string]string{}
	for _, e := range environ {
		if i := strings.IndexByte(e, '='); i > 0 {
			m[e[:i]] = e[i+1:]
		}
	}
	return func(k string) string {
		if v, ok := m[k]; ok {
			return v
		}
		// Fall back to process env only for keys not in the effective slice.
		return os.Getenv(k)
	}
}
