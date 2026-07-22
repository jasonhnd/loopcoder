package capclass

// DefaultModelMap returns the reviewed, data-only capability mapping for
// currently observed official models. Adding a newly observed model only
// updates this table and its tests — not scheduler or route engine code
// (acceptance criterion #5).
//
// Classes are based on observed capability policy, not marketing name order
// or quota size. Entries include currently observed static registry IDs and
// prior catalog IDs so routing does not reject live account models (V090-CRO-005).
// A model still must appear in a fresh account catalog to be selected.
func DefaultModelMap() ModelMap {
	m, err := NormalizeModelMap(ModelMap{
		Version: "model-map-v2",
		Entries: []ModelCapability{
			// Codex / OpenAI family (registry + prior IDs)
			{Provider: "codex", ModelID: "gpt-5.5", Class: ClassSoul},
			{Provider: "codex", ModelID: "gpt-5.3-codex", Class: ClassSoul},
			{Provider: "codex", ModelID: "gpt-5.2-codex", Class: ClassTera},
			{Provider: "codex", ModelID: "gpt-5.1-codex-mini", Class: ClassLuna},
			// Claude
			{Provider: "claude", ModelID: "claude-opus-4-8[1m]", Class: ClassSoul},
			{Provider: "claude", ModelID: "claude-opus-4-5", Class: ClassSoul},
			{Provider: "claude", ModelID: "claude-sonnet-4-5", Class: ClassTera},
			{Provider: "claude", ModelID: "claude-haiku-4-5", Class: ClassLuna},
			// Gemini
			{Provider: "gemini", ModelID: "gemini-2.5-pro", Class: ClassSoul},
			{Provider: "gemini", ModelID: "gemini-2.5-flash", Class: ClassTera},
			{Provider: "gemini", ModelID: "gemini-2.5-flash-lite", Class: ClassLuna},
			// Antigravity (Google AI Studio surface + observed registry names)
			{Provider: "antigravity", ModelID: "Gemini 3.1 Pro", Class: ClassSoul},
			{Provider: "antigravity", ModelID: "gemini-2.5-pro", Class: ClassSoul},
			{Provider: "antigravity", ModelID: "gemini-2.5-flash", Class: ClassTera},
			{Provider: "antigravity", ModelID: "Opus 4.6", Class: ClassSoul},
			{Provider: "antigravity", ModelID: "GPT-OSS 120B", Class: ClassTera},
			// Grok (static list is incomplete; dynamic catalog still required)
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
