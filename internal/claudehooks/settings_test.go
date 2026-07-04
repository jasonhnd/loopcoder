package claudehooks

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRequiredHooksUsePowerShellPostToolUseMatcher(t *testing.T) {
	for _, hook := range RequiredHooks() {
		if hook.Event != "PostToolUse" {
			continue
		}
		if hook.Matcher != postToolUseShellMatcher {
			t.Fatalf("PostToolUse matcher = %q, want %q", hook.Matcher, postToolUseShellMatcher)
		}
	}
}

func TestMergeSettingsInstallsPowerShellPostToolUseMatcher(t *testing.T) {
	merged, changed, err := MergeSettings(nil)
	if err != nil {
		t.Fatalf("MergeSettings returned error: %v", err)
	}
	if !changed {
		t.Fatal("MergeSettings changed = false, want true for empty settings")
	}
	if !strings.Contains(string(merged), `"matcher": "Bash|PowerShell|pwsh"`) {
		t.Fatalf("merged settings missing PowerShell/pwsh matcher:\n%s", merged)
	}
	missing, err := MissingHooks(merged)
	if err != nil {
		t.Fatalf("MissingHooks returned error: %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("MissingHooks = %#v, want none", missing)
	}
}

func TestMergeSettingsUpgradesBashOnlyConductorPostToolUseHooks(t *testing.T) {
	input := []byte(`{
  "hooks": {
    "PostToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {
            "type": "command",
            "command": "loopcoder hook conductor-attest",
            "timeout": 10
          },
          {
            "type": "command",
            "command": "node hooks/user-hook.js",
            "timeout": 3
          },
          {
            "type": "command",
            "command": "loopcoder hook conductor-relay-guard",
            "timeout": 10
          }
        ]
      }
    ]
  }
}`)

	merged, changed, err := MergeSettings(input)
	if err != nil {
		t.Fatalf("MergeSettings returned error: %v", err)
	}
	if !changed {
		t.Fatal("MergeSettings changed = false, want true for matcher upgrade")
	}
	entries := postToolUseEntries(t, merged)

	var sawPowerShellMatcher bool
	var sawUserBashHook bool
	for _, entry := range entries {
		switch entry.Matcher {
		case postToolUseShellMatcher:
			sawPowerShellMatcher = true
			for _, want := range []string{"loopcoder hook conductor-attest", "loopcoder hook conductor-relay-guard"} {
				if !entry.HasCommand(want) {
					t.Fatalf("PowerShell matcher entry missing %q:\n%s", want, merged)
				}
			}
		case "Bash":
			if entry.HasCommand("node hooks/user-hook.js") {
				sawUserBashHook = true
			}
			for _, stale := range []string{"loopcoder hook conductor-attest", "loopcoder hook conductor-relay-guard"} {
				if entry.HasCommand(stale) {
					t.Fatalf("stale conductor hook %q left in Bash-only entry:\n%s", stale, merged)
				}
			}
		}
	}
	if !sawPowerShellMatcher {
		t.Fatalf("merged settings missing %q entry:\n%s", postToolUseShellMatcher, merged)
	}
	if !sawUserBashHook {
		t.Fatalf("merged settings did not preserve unrelated Bash user hook:\n%s", merged)
	}
}

type postToolUseEntry struct {
	Matcher string `json:"matcher"`
	Hooks   []struct {
		Command string `json:"command"`
	} `json:"hooks"`
}

func (e postToolUseEntry) HasCommand(command string) bool {
	for _, hook := range e.Hooks {
		if hook.Command == command {
			return true
		}
	}
	return false
}

func postToolUseEntries(t *testing.T, data []byte) []postToolUseEntry {
	t.Helper()
	var parsed struct {
		Hooks map[string][]postToolUseEntry `json:"hooks"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("parse settings JSON: %v\n%s", err, data)
	}
	return parsed.Hooks["PostToolUse"]
}
