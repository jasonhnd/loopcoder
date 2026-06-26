package report

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRenderReadySetTextIncludesDocumentedSections(t *testing.T) {
	runID := "run-20260626T120000Z-wave"
	text := RenderReadySetText(ReadySetReport{
		Version:     1,
		Repo:        "owner/repo",
		BaseBranch:  "main",
		RunID:       &runID,
		GeneratedAt: "2026-06-26T12:00:00Z",
		Ready: []ReadyIssue{{
			Issue:  81,
			Title:  "Ready issue",
			Reason: "dependencies completed; no open PR; no live local attempt",
		}},
		Blocked: []BlockedIssue{{
			Issue:          82,
			Title:          "Blocked issue",
			Classification: "blocked-by-unmerged-dep",
			Reason:         "blocked by #81 is still open",
		}},
	})

	for _, want := range []string{
		"READY SET",
		"Repo: owner/repo",
		"Ready",
		"Non-ready",
		"classification: blocked-by-unmerged-dep",
		"Safety",
		"ready-set is read-only: no dispatch, no merge, no push, and no GitHub mutation was attempted.",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("rendered text missing %q:\n%s", want, text)
		}
	}
}

func TestMarshalReadySetJSONUsesDocumentedShape(t *testing.T) {
	data, err := MarshalReadySetJSON(ReadySetReport{
		Version:     1,
		Repo:        "owner/repo",
		RepoPath:    "C:/repo",
		BaseBranch:  "main",
		GeneratedAt: "2026-06-26T12:00:00Z",
		Summary: ReadySetSummary{
			ReadyCount:   0,
			BlockedCount: 0,
		},
	})
	if err != nil {
		t.Fatalf("MarshalReadySetJSON returned error: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("JSON did not unmarshal: %v\n%s", err, data)
	}
	for _, key := range []string{"version", "repo", "repo_path", "base_branch", "run_id", "generated_at", "ready", "blocked", "summary"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("JSON missing key %q: %s", key, data)
		}
	}
	if got["run_id"] != nil {
		t.Fatalf("run_id = %#v, want null", got["run_id"])
	}
	if ready, ok := got["ready"].([]any); !ok || len(ready) != 0 {
		t.Fatalf("ready = %#v, want empty array", got["ready"])
	}
	if blocked, ok := got["blocked"].([]any); !ok || len(blocked) != 0 {
		t.Fatalf("blocked = %#v, want empty array", got["blocked"])
	}
}
