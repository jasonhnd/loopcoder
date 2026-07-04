package claudehooks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
)

const (
	SettingsRelPath = ".claude/settings.json"

	hookTimeout             = 10
	postToolUseShellMatcher = "Bash|PowerShell"
)

// requiredHookNames are the conductor hook subcommands wired into a project's
// Claude Code settings. They run through the loopcoder binary
// (loopcoder hook <name>) so they resolve regardless of the host's working
// directory and stay in lockstep with the installed binary, instead of a
// relative node hooks/*.js path that only resolved inside loopcoder's own repo.
var requiredHookNames = []string{
	"conductor-attest",
	"conductor-relay-guard",
}

// deprecatedCommands are the legacy conductor hook command strings from the
// pre-binary node hooks/*.js mechanism. The settings merge strips them so an
// upgrade does not leave behind broken, unresolved hook entries.
var deprecatedCommands = []string{
	"node hooks/conductor-attest.js",
	"node hooks/conductor-relay-guard.js",
}

// RequiredHook describes one required Claude Code hook registration.
type RequiredHook struct {
	Event   string
	Matcher string
	Script  string
	Command string
}

// SettingsPath returns the project-scoped Claude Code settings path.
func SettingsPath(projectDir string) string {
	projectDir = strings.TrimSpace(projectDir)
	if projectDir == "" {
		projectDir = "."
	}
	return filepath.Join(projectDir, SettingsRelPath)
}

// RequiredHooks returns the hook entries loopcoder needs in project settings.
func RequiredHooks() []RequiredHook {
	hooks := make([]RequiredHook, 0, len(requiredHookNames)*2)
	for _, name := range requiredHookNames {
		command := commandForHook(name)
		hooks = append(hooks, RequiredHook{
			Event:   "PostToolUse",
			Matcher: postToolUseShellMatcher,
			Script:  name,
			Command: command,
		})
		hooks = append(hooks, RequiredHook{
			Event:   "Stop",
			Script:  name,
			Command: command,
		})
	}
	return hooks
}

// MergeSettings structurally merges required loopcoder conductor hooks into
// Claude Code settings JSON. An empty input is treated as an absent file.
func MergeSettings(data []byte) ([]byte, bool, error) {
	settings, err := decodeSettings(data)
	if err != nil {
		return nil, false, err
	}

	changed := false
	hooks, ok, err := objectField(settings, "hooks")
	if err != nil {
		return nil, false, err
	}
	if !ok {
		hooks = map[string]any{}
		settings["hooks"] = hooks
		changed = true
	}

	for _, required := range RequiredHooks() {
		var didChange bool
		switch required.Event {
		case "PostToolUse":
			didChange, err = ensurePostToolUse(hooks, required)
		case "Stop":
			didChange, err = ensureStop(hooks, required)
		default:
			err = fmt.Errorf("unsupported hook event %q", required.Event)
		}
		if err != nil {
			return nil, false, err
		}
		changed = changed || didChange
	}

	if pruneDeprecatedHooks(hooks) {
		changed = true
	}
	if pruneStalePostToolUseMatchers(hooks) {
		changed = true
	}

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return nil, false, fmt.Errorf("render Claude Code settings: %w", err)
	}
	out = append(out, '\n')
	if bytes.Equal(bytes.TrimSpace(data), bytes.TrimSpace(out)) {
		changed = false
	}
	return out, changed, nil
}

// pruneStalePostToolUseMatchers removes loopcoder's own conductor hook commands
// from older Bash-only PostToolUse entries after the current Bash|PowerShell
// entry has been installed. Unrelated user hook commands in those entries are
// preserved.
func pruneStalePostToolUseMatchers(hooks map[string]any) bool {
	entries, ok := hooks["PostToolUse"].([]any)
	if !ok {
		return false
	}
	requiredCommands := map[string]bool{}
	for _, name := range requiredHookNames {
		requiredCommands[commandForHook(name)] = true
	}

	changed := false
	kept := make([]any, 0, len(entries))
	for _, raw := range entries {
		entry, ok := raw.(map[string]any)
		if !ok {
			kept = append(kept, raw)
			continue
		}
		matcher, _ := entry["matcher"].(string)
		if matcher == postToolUseShellMatcher {
			kept = append(kept, entry)
			continue
		}
		if pruneMatchingEntryCommands(entry, requiredCommands) {
			changed = true
		}
		if commands, had := entry["hooks"].([]any); had && len(commands) == 0 {
			continue
		}
		kept = append(kept, entry)
	}
	if len(kept) == 0 {
		delete(hooks, "PostToolUse")
	} else {
		hooks["PostToolUse"] = kept
	}
	return changed
}

// MissingHooks returns required loopcoder conductor hook entries absent from
// settings JSON. An empty input is treated as an absent file.
func MissingHooks(data []byte) ([]RequiredHook, error) {
	settings, err := decodeSettings(data)
	if err != nil {
		return nil, err
	}
	hooks, ok, err := objectField(settings, "hooks")
	if err != nil {
		return nil, err
	}
	if !ok {
		return RequiredHooks(), nil
	}

	var missing []RequiredHook
	for _, required := range RequiredHooks() {
		present, err := hasRequiredHook(hooks, required)
		if err != nil {
			return nil, err
		}
		if !present {
			missing = append(missing, required)
		}
	}
	return missing, nil
}

// FormatMissing renders missing hook entries for human diagnostics.
func FormatMissing(missing []RequiredHook) string {
	if len(missing) == 0 {
		return ""
	}
	parts := make([]string, 0, len(missing))
	for _, hook := range missing {
		if hook.Matcher != "" {
			parts = append(parts, fmt.Sprintf("%s %s matcher=%s", hook.Script, hook.Event, hook.Matcher))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %s", hook.Script, hook.Event))
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

func decodeSettings(data []byte) (map[string]any, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return map[string]any{}, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var settings map[string]any
	if err := decoder.Decode(&settings); err != nil {
		return nil, fmt.Errorf("parse Claude Code settings JSON: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return nil, errors.New("parse Claude Code settings JSON: multiple JSON values")
	} else if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse Claude Code settings JSON: %w", err)
	}
	if settings == nil {
		return nil, errors.New("Claude Code settings must be a JSON object")
	}
	return settings, nil
}

func objectField(parent map[string]any, name string) (map[string]any, bool, error) {
	value, ok := parent[name]
	if !ok {
		return nil, false, nil
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, false, fmt.Errorf("Claude Code settings field %q must be an object", name)
	}
	return object, true, nil
}

func ensurePostToolUse(hooks map[string]any, required RequiredHook) (bool, error) {
	entries, existed, err := eventEntries(hooks, required.Event)
	if err != nil {
		return false, err
	}

	changed := !existed
	found := false
	target := -1
	for index := range entries {
		entry, ok := entries[index].(map[string]any)
		if !ok {
			return false, fmt.Errorf("Claude Code hook event %q entry %d must be an object", required.Event, index)
		}
		if matcher, _ := entry["matcher"].(string); matcher != required.Matcher {
			continue
		}
		if target < 0 {
			target = index
		}
		didChange, present, err := ensureCommandHook(entry, required.Command, false)
		if err != nil {
			return false, err
		}
		changed = changed || didChange
		found = found || present
	}

	if !found {
		if target >= 0 {
			entry := entries[target].(map[string]any)
			didChange, _, err := ensureCommandHook(entry, required.Command, true)
			if err != nil {
				return false, err
			}
			changed = changed || didChange
		} else {
			entries = append(entries, map[string]any{
				"matcher": required.Matcher,
				"hooks":   []any{commandHook(required.Command)},
			})
			changed = true
		}
	}
	hooks[required.Event] = entries
	return changed, nil
}

func ensureStop(hooks map[string]any, required RequiredHook) (bool, error) {
	entries, existed, err := eventEntries(hooks, required.Event)
	if err != nil {
		return false, err
	}

	changed := !existed
	found := false
	target := -1
	for index := range entries {
		entry, ok := entries[index].(map[string]any)
		if !ok {
			return false, fmt.Errorf("Claude Code hook event %q entry %d must be an object", required.Event, index)
		}
		if target < 0 {
			target = index
		}
		didChange, present, err := ensureCommandHook(entry, required.Command, false)
		if err != nil {
			return false, err
		}
		changed = changed || didChange
		found = found || present
	}

	if !found {
		if target >= 0 {
			entry := entries[target].(map[string]any)
			didChange, _, err := ensureCommandHook(entry, required.Command, true)
			if err != nil {
				return false, err
			}
			changed = changed || didChange
		} else {
			entries = append(entries, map[string]any{
				"hooks": []any{commandHook(required.Command)},
			})
			changed = true
		}
	}
	hooks[required.Event] = entries
	return changed, nil
}

func eventEntries(hooks map[string]any, event string) ([]any, bool, error) {
	value, ok := hooks[event]
	if !ok {
		return []any{}, false, nil
	}
	entries, ok := value.([]any)
	if !ok {
		return nil, false, fmt.Errorf("Claude Code hooks.%s must be an array", event)
	}
	return entries, true, nil
}

func ensureCommandHook(entry map[string]any, command string, appendIfMissing bool) (bool, bool, error) {
	hooksValue, ok := entry["hooks"]
	if !ok {
		if !appendIfMissing {
			return false, false, nil
		}
		entry["hooks"] = []any{commandHook(command)}
		return true, true, nil
	}
	hooks, ok := hooksValue.([]any)
	if !ok {
		return false, false, errors.New("Claude Code hook entry field \"hooks\" must be an array")
	}

	changed := false
	for index := range hooks {
		hook, ok := hooks[index].(map[string]any)
		if !ok {
			return false, false, fmt.Errorf("Claude Code command hook %d must be an object", index)
		}
		if hookCommand, _ := hook["command"].(string); hookCommand != command {
			continue
		}
		didChange := normalizeCommandHook(hook, command)
		hooks[index] = hook
		entry["hooks"] = hooks
		return didChange, true, nil
	}
	if !appendIfMissing {
		return changed, false, nil
	}
	hooks = append(hooks, commandHook(command))
	entry["hooks"] = hooks
	return true, true, nil
}

func hasRequiredHook(hooks map[string]any, required RequiredHook) (bool, error) {
	entries, _, err := eventEntries(hooks, required.Event)
	if err != nil {
		return false, err
	}
	for index := range entries {
		entry, ok := entries[index].(map[string]any)
		if !ok {
			return false, fmt.Errorf("Claude Code hook event %q entry %d must be an object", required.Event, index)
		}
		if required.Matcher != "" {
			matcher, _ := entry["matcher"].(string)
			if matcher != required.Matcher {
				continue
			}
		}
		present, err := hasCommandHook(entry, required.Command)
		if err != nil {
			return false, err
		}
		if present {
			return true, nil
		}
	}
	return false, nil
}

func hasCommandHook(entry map[string]any, command string) (bool, error) {
	hooksValue, ok := entry["hooks"]
	if !ok {
		return false, nil
	}
	hooks, ok := hooksValue.([]any)
	if !ok {
		return false, errors.New("Claude Code hook entry field \"hooks\" must be an array")
	}
	for index := range hooks {
		hook, ok := hooks[index].(map[string]any)
		if !ok {
			return false, fmt.Errorf("Claude Code command hook %d must be an object", index)
		}
		if hookCommand, _ := hook["command"].(string); hookCommand != command {
			continue
		}
		if hookType, _ := hook["type"].(string); hookType != "command" {
			return false, nil
		}
		if !timeoutEquals(hook["timeout"], hookTimeout) {
			return false, nil
		}
		return true, nil
	}
	return false, nil
}

func normalizeCommandHook(hook map[string]any, command string) bool {
	changed := false
	if hookType, _ := hook["type"].(string); hookType != "command" {
		hook["type"] = "command"
		changed = true
	}
	if hookCommand, _ := hook["command"].(string); hookCommand != command {
		hook["command"] = command
		changed = true
	}
	if !timeoutEquals(hook["timeout"], hookTimeout) {
		hook["timeout"] = hookTimeout
		changed = true
	}
	return changed
}

func timeoutEquals(value any, want int) bool {
	switch typed := value.(type) {
	case int:
		return typed == want
	case int64:
		return typed == int64(want)
	case float64:
		return typed == float64(want)
	case json.Number:
		if parsed, err := typed.Int64(); err == nil {
			return parsed == int64(want)
		}
		parsed, err := typed.Float64()
		return err == nil && parsed == float64(want)
	default:
		return false
	}
}

func commandHook(command string) map[string]any {
	return map[string]any{
		"type":    "command",
		"command": command,
		"timeout": hookTimeout,
	}
}

func commandForHook(name string) string {
	return "loopcoder hook " + name
}

// pruneDeprecatedHooks removes legacy conductor hook command entries (the
// pre-binary "node hooks/*.js" commands) from the PostToolUse and Stop events.
// A hook entry whose command list becomes empty as a result is dropped, and an
// event whose entry list becomes empty is removed. It reports whether anything
// changed. This makes reinstalling over an old install a clean upgrade instead
// of leaving broken, unresolved commands alongside the new ones.
func pruneDeprecatedHooks(hooks map[string]any) bool {
	changed := false
	for _, event := range []string{"PostToolUse", "Stop"} {
		entries, ok := hooks[event].([]any)
		if !ok {
			continue
		}
		kept := make([]any, 0, len(entries))
		for _, raw := range entries {
			entry, ok := raw.(map[string]any)
			if !ok {
				kept = append(kept, raw)
				continue
			}
			if pruneEntryCommands(entry) {
				changed = true
			}
			if commands, had := entry["hooks"].([]any); had && len(commands) == 0 {
				// We emptied an entry by removing its deprecated commands; drop it.
				continue
			}
			kept = append(kept, entry)
		}
		if len(kept) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = kept
		}
	}
	return changed
}

func pruneEntryCommands(entry map[string]any) bool {
	commands, ok := entry["hooks"].([]any)
	if !ok {
		return false
	}
	changed := false
	filtered := make([]any, 0, len(commands))
	for _, raw := range commands {
		command, ok := raw.(map[string]any)
		if !ok {
			filtered = append(filtered, raw)
			continue
		}
		if value, _ := command["command"].(string); isDeprecatedCommand(value) {
			changed = true
			continue
		}
		filtered = append(filtered, raw)
	}
	if changed {
		entry["hooks"] = filtered
	}
	return changed
}

func pruneMatchingEntryCommands(entry map[string]any, remove map[string]bool) bool {
	commands, ok := entry["hooks"].([]any)
	if !ok {
		return false
	}
	changed := false
	filtered := make([]any, 0, len(commands))
	for _, raw := range commands {
		command, ok := raw.(map[string]any)
		if !ok {
			filtered = append(filtered, raw)
			continue
		}
		value, _ := command["command"].(string)
		if remove[value] {
			changed = true
			continue
		}
		filtered = append(filtered, raw)
	}
	if changed {
		entry["hooks"] = filtered
	}
	return changed
}

func isDeprecatedCommand(command string) bool {
	for _, deprecated := range deprecatedCommands {
		if command == deprecated {
			return true
		}
	}
	return false
}
