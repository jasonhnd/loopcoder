package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// injectGrokCLIAuthAsAPIKey loads the official Grok Build CLI login token from
// ~/.grok/auth.json (or GROK_HOME/auth.json) into XAI_API_KEY when that env is
// unset. Isolated worker HOMEs do not inherit the interactive login session;
// XAI_API_KEY is the only documented credential passthrough for headless runs.
// Token is never written to disk or returned in logs.
func injectGrokCLIAuthAsAPIKey(environ []string) []string {
	for _, entry := range environ {
		key, value, ok := strings.Cut(entry, "=")
		if ok && key == "XAI_API_KEY" && strings.TrimSpace(value) != "" {
			return environ
		}
	}
	token := loadGrokCLIAuthTokenForWorker()
	if token == "" {
		return environ
	}
	return append(append([]string(nil), environ...), "XAI_API_KEY="+token)
}

func loadGrokCLIAuthTokenForWorker() string {
	authPath := ""
	if gh := strings.TrimSpace(os.Getenv("GROK_HOME")); gh != "" {
		authPath = filepath.Join(gh, "auth.json")
	} else {
		home, err := os.UserHomeDir()
		if err != nil || strings.TrimSpace(home) == "" {
			return ""
		}
		authPath = filepath.Join(home, ".grok", "auth.json")
	}
	raw, err := os.ReadFile(authPath)
	if err != nil || len(raw) == 0 {
		return ""
	}
	var root any
	if err := json.Unmarshal(raw, &root); err != nil {
		return ""
	}
	m, ok := root.(map[string]any)
	if !ok {
		return ""
	}
	if tok := firstJSONString(m, "key", "access_token"); tok != "" {
		return tok
	}
	type cand struct {
		tok string
		exp time.Time
	}
	var best *cand
	now := time.Now().UTC()
	for _, v := range m {
		vm, ok := v.(map[string]any)
		if !ok {
			continue
		}
		tok := firstJSONString(vm, "key")
		if tok == "" {
			tok = firstJSONString(vm, "access_token")
		}
		if tok == "" {
			continue
		}
		c := cand{tok: tok}
		if exp := firstJSONString(vm, "expires_at", "expiresAt"); exp != "" {
			if t, perr := time.Parse(time.RFC3339Nano, exp); perr == nil {
				c.exp = t
			} else if t, perr := time.Parse(time.RFC3339, exp); perr == nil {
				c.exp = t
			}
		}
		if best == nil {
			best = &c
			continue
		}
		bestExpired := !best.exp.IsZero() && best.exp.Before(now)
		cExpired := !c.exp.IsZero() && c.exp.Before(now)
		if bestExpired && !cExpired {
			best = &c
			continue
		}
		if !bestExpired && cExpired {
			continue
		}
		if c.exp.After(best.exp) {
			best = &c
		}
	}
	if best == nil {
		return ""
	}
	return best.tok
}

func firstJSONString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok {
				s = strings.TrimSpace(s)
				if s != "" {
					return s
				}
			}
		}
	}
	return ""
}
