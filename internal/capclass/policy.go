package capclass

// DefaultModelMap returns the reviewed, data-only capability mapping for
// currently observed official models. Adding a newly observed model only
// updates this table and its tests — not scheduler or route engine code
// (acceptance criterion #5).
//
// Classes are based on observed capability policy, not marketing name order
// or quota size. Canonical model IDs only; aliases are out of scope here.
func DefaultModelMap() ModelMap {
	m, err := NormalizeModelMap(ModelMap{
		Version: "model-map-v1",
		Entries: []ModelCapability{
			// Codex / OpenAI family
			{Provider: "codex", ModelID: "gpt-5.3-codex", Class: ClassSoul},
			{Provider: "codex", ModelID: "gpt-5.2-codex", Class: ClassTera},
			{Provider: "codex", ModelID: "gpt-5.1-codex-mini", Class: ClassLuna},
			// Claude
			{Provider: "claude", ModelID: "claude-opus-4-5", Class: ClassSoul},
			{Provider: "claude", ModelID: "claude-sonnet-4-5", Class: ClassTera},
			{Provider: "claude", ModelID: "claude-haiku-4-5", Class: ClassLuna},
			// Gemini
			{Provider: "gemini", ModelID: "gemini-2.5-pro", Class: ClassSoul},
			{Provider: "gemini", ModelID: "gemini-2.5-flash", Class: ClassTera},
			{Provider: "gemini", ModelID: "gemini-2.5-flash-lite", Class: ClassLuna},
			// Antigravity (Google AI Studio surface)
			{Provider: "antigravity", ModelID: "gemini-2.5-pro", Class: ClassSoul},
			{Provider: "antigravity", ModelID: "gemini-2.5-flash", Class: ClassTera},
			// Grok
			{Provider: "grok", ModelID: "grok-4.5", Class: ClassSoul},
			{Provider: "grok", ModelID: "grok-4", Class: ClassTera},
			{Provider: "grok", ModelID: "grok-3-mini", Class: ClassLuna},
		},
	})
	if err != nil {
		// Default table is compile-time constant data; panic on author error.
		panic(err)
	}
	return m
}
