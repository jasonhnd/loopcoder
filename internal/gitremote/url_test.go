package gitremote

import "testing"

func TestNormalizeURLSanitizesCredentialBearingRemotes(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		normalized string
		owner      string
		repo       string
		ok         bool
	}{
		{
			name:       "https password userinfo",
			raw:        "https://alice:super-secret-token@github.com/Owner/Repo.git",
			normalized: "https://github.com/Owner/Repo",
			owner:      "owner",
			repo:       "repo",
			ok:         true,
		},
		{
			name:       "https username only userinfo",
			raw:        "https://alice@github.com/Owner/Repo.git",
			normalized: "https://github.com/Owner/Repo",
			owner:      "owner",
			repo:       "repo",
			ok:         true,
		},
		{
			name:       "percent encoded credentials",
			raw:        "https://alice:%73uper%2Dsecret@github.com/Owner/Repo.git",
			normalized: "https://github.com/Owner/Repo",
			owner:      "owner",
			repo:       "repo",
			ok:         true,
		},
		{
			name:       "credential query and fragment removed",
			raw:        "https://github.com/Owner/Repo.git?access_token=super-secret-token&X-Amz-Signature=signed#token=super-secret-token",
			normalized: "https://github.com/Owner/Repo",
			owner:      "owner",
			repo:       "repo",
			ok:         true,
		},
		{
			name:       "ipv6 host",
			raw:        "ssh://git@[2001:db8::1]/team/repo.git",
			normalized: "ssh://[2001:db8::1]/team/repo",
			ok:         true,
		},
		{
			name:       "non default port",
			raw:        "ssh://git@example.com:2222/team/repo.git",
			normalized: "ssh://example.com:2222/team/repo",
			ok:         true,
		},
		{
			name:       "default port removed",
			raw:        "https://github.com:443/Owner/Repo.git",
			normalized: "https://github.com/Owner/Repo",
			owner:      "owner",
			repo:       "repo",
			ok:         true,
		},
		{
			name:       "scp style removes ssh username",
			raw:        "git@github.com:Owner/Repo.git",
			normalized: "ssh://github.com/Owner/Repo",
			owner:      "owner",
			repo:       "repo",
			ok:         true,
		},
		{
			name:       "plain ssh url removes user",
			raw:        "ssh://deploy@git.example.test/team/repo.git",
			normalized: "ssh://git.example.test/team/repo",
			ok:         true,
		},
		{
			name: "bad escape fails closed",
			raw:  "https://github.com/Owner/%zz.git",
			ok:   false,
		},
		{
			name: "unsupported file remote fails closed",
			raw:  "file:///tmp/repo.git",
			ok:   false,
		},
		{
			name: "host whitespace fails closed",
			raw:  "https://exa mple.com/org/repo.git",
			ok:   false,
		},
		{
			name: "path traversal fails closed",
			raw:  "https://github.com/Owner/../Repo.git",
			ok:   false,
		},
		{
			name: "not a remote fails closed",
			raw:  "not a remote",
			ok:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			normalized, owner, repo, ok := NormalizeURL(tt.raw)
			if normalized != tt.normalized || owner != tt.owner || repo != tt.repo || ok != tt.ok {
				t.Fatalf("NormalizeURL() = %q %q %q %t, want %q %q %q %t", normalized, owner, repo, ok, tt.normalized, tt.owner, tt.repo, tt.ok)
			}
			display, displayOK := SanitizeDisplayURL(tt.raw)
			if display != tt.normalized || displayOK != tt.ok {
				t.Fatalf("SanitizeDisplayURL() = %q %t, want %q %t", display, displayOK, tt.normalized, tt.ok)
			}
		})
	}
}
