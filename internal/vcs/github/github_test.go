package github

import "testing"

func TestParseJSONOutputTrimsWarnings(t *testing.T) {
	var issues []Issue
	input := []byte("warning: ignored\n[{\"number\":93,\"title\":\"Ready\",\"labels\":[{\"name\":\"blocked-by:#1\"}],\"state\":\"OPEN\"}]\ntrailing")

	if err := parseJSONOutput(input, &issues); err != nil {
		t.Fatalf("parseJSONOutput returned error: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("parsed %d issues, want 1", len(issues))
	}
	if issues[0].Number != 93 || issues[0].Labels[0].Name != "blocked-by:#1" {
		t.Fatalf("parsed issue incorrectly: %#v", issues[0])
	}
}

func TestParseJSONOutputParsesObject(t *testing.T) {
	var payload struct {
		NameWithOwner string `json:"nameWithOwner"`
	}
	if err := parseJSONOutput([]byte("prefix {\"nameWithOwner\":\"owner/repo\"} suffix"), &payload); err != nil {
		t.Fatalf("parseJSONOutput returned error: %v", err)
	}
	if payload.NameWithOwner != "owner/repo" {
		t.Fatalf("NameWithOwner = %q, want owner/repo", payload.NameWithOwner)
	}
}

func TestParseGitHubRemote(t *testing.T) {
	cases := map[string]string{
		"git@github.com:owner/repo.git":          "owner/repo",
		"https://github.com/owner/repo.git":      "owner/repo",
		"https://github.com/owner/repo":          "owner/repo",
		"https://example.com/owner/repo.git":     "",
	}
	for input, want := range cases {
		if got := parseGitHubRemote(input); got != want {
			t.Fatalf("parseGitHubRemote(%q) = %q, want %q", input, got, want)
		}
	}
}
