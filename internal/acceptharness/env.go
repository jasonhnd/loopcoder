package acceptharness

import (
	"os"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/gitutil"
)

// CleanProcessEnv returns an environment suitable for fixture subprocesses:
// Git scoping vars are stripped via gitutil.CleanEnv, and common provider
// credential variables are removed so fixtures cannot inherit host secrets.
func CleanProcessEnv(base []string) []string {
	if base == nil {
		base = os.Environ()
	}
	cleaned := gitutil.CleanEnv(base)
	out := make([]string, 0, len(cleaned))
	for _, entry := range cleaned {
		key, _, _ := strings.Cut(entry, "=")
		upper := strings.ToUpper(key)
		switch {
		case strings.HasPrefix(upper, "GITHUB_TOKEN"),
			upper == "GH_TOKEN",
			strings.HasPrefix(upper, "OPENAI_"),
			strings.HasPrefix(upper, "ANTHROPIC_"),
			strings.HasPrefix(upper, "XAI_"),
			strings.Contains(upper, "API_KEY"),
			strings.Contains(upper, "SECRET"),
			upper == "SSH_AUTH_SOCK":
			continue
		default:
			out = append(out, entry)
		}
	}
	return out
}
