package grokauth_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/grokauth"
)

func TestParseActive_RealShapePrincipalUserTeamIssuer(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	// Official nested shape: map key is oidc_issuer::oidc_client_id — NOT account.
	doc := map[string]any{
		"https://auth.x.ai/oidc::client-uuid-aaaa": map[string]any{
			"key":            "tok-a",
			"principal_id":   "prin-alice",
			"user_id":        "user-should-not-win", // principal_id wins
			"team_id":        "team-t1",
			"oidc_issuer":    "https://auth.x.ai/oidc",
			"oidc_client_id": "client-uuid-aaaa",
			"expires_at":     now.Add(2 * time.Hour).Format(time.RFC3339),
		},
	}
	raw, _ := json.Marshal(doc)
	bind, err := grokauth.ParseBytes(raw, now)
	if err != nil {
		t.Fatal(err)
	}
	if !bind.ExactRoutable || bind.AccountProfileID == "" {
		t.Fatalf("want exact routable: %+v", bind)
	}
	want := grokauth.CanonicalAccountProfileID("prin-alice", "team-t1", "https://auth.x.ai/oidc")
	if bind.AccountProfileID != want {
		t.Fatalf("acct=%s want %s", bind.AccountProfileID, want)
	}
	// Never hash client_id as account.
	bad := grokauth.CanonicalAccountProfileID("client-uuid-aaaa", "", "")
	if bind.AccountProfileID == bad {
		t.Fatal("must not use client_id as principal")
	}
}

func TestParseActive_ShuffledMultiAccountDeterministic(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	// Insert B before A in JSON object — selection must be stable by rules not map order.
	mk := func(order []string) []byte {
		m := map[string]any{}
		for _, id := range order {
			m["issuer::"+id] = map[string]any{
				"key":         "tok-" + id,
				"user_id":     "user-" + id,
				"oidc_issuer": "https://auth.example/oidc",
				"expires_at":  now.Add(time.Hour).Format(time.RFC3339),
			}
		}
		// Force non-expired both; principal user-aaa < user-bbb so aaa wins on tie.
		raw, _ := json.Marshal(m)
		return raw
	}
	b1, err := grokauth.ParseBytes(mk([]string{"bbb", "aaa"}), now)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := grokauth.ParseBytes(mk([]string{"aaa", "bbb"}), now)
	if err != nil {
		t.Fatal(err)
	}
	if b1.AccountProfileID != b2.AccountProfileID {
		t.Fatalf("nondeterministic %s vs %s", b1.AccountProfileID, b2.AccountProfileID)
	}
	want := grokauth.CanonicalAccountProfileID("user-aaa", "", "https://auth.example/oidc")
	if b1.AccountProfileID != want {
		t.Fatalf("got %s want %s", b1.AccountProfileID, want)
	}
}

func TestParseActive_PreferNonExpired(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	raw, _ := json.Marshal(map[string]any{
		"issuer::expired": map[string]any{
			"key": "tok-old", "user_id": "user-old", "oidc_issuer": "https://auth.example",
			"expires_at": now.Add(-time.Hour).Format(time.RFC3339),
		},
		"issuer::fresh": map[string]any{
			"key": "tok-new", "user_id": "user-new", "oidc_issuer": "https://auth.example",
			"expires_at": now.Add(time.Hour).Format(time.RFC3339),
		},
	})
	bind, err := grokauth.ParseBytes(raw, now)
	if err != nil {
		t.Fatal(err)
	}
	want := grokauth.CanonicalAccountProfileID("user-new", "", "https://auth.example")
	if bind.AccountProfileID != want {
		t.Fatalf("got %s want %s", bind.AccountProfileID, want)
	}
}

func TestParseActive_MissingIdentityNotRoutable(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	// Flat token only — identity-less.
	raw, _ := json.Marshal(map[string]any{
		"key":            "tok-only",
		"oidc_client_id": "client-x",
	})
	bind, err := grokauth.ParseBytes(raw, now)
	if err != nil {
		t.Fatal(err)
	}
	if bind.AccountProfileID != "" || bind.ExactRoutable {
		t.Fatalf("identity-less must not be exact-routable: %+v", bind)
	}
	if !bind.HasToken {
		t.Fatal("token present")
	}
}

func TestParseActive_EmailNeverPrincipal(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	raw, _ := json.Marshal(map[string]any{
		"issuer::c": map[string]any{
			"key": "tok", "email": "a@example.com", "oidc_issuer": "https://auth.example",
		},
	})
	bind, err := grokauth.ParseBytes(raw, now)
	if err != nil {
		t.Fatal(err)
	}
	if bind.ExactRoutable {
		t.Fatal("email must not create exact account")
	}
}

func TestLoadToken_DoesNotLeakIntoBinding(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(dir, "auth.json")
	raw, _ := json.Marshal(map[string]any{
		"issuer::c": map[string]any{
			"key": "secret-token-xyz", "user_id": "u1", "oidc_issuer": "https://auth.example",
			"expires_at": now.Add(time.Hour).Format(time.RFC3339),
		},
	})
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	tok, bind, err := grokauth.LoadToken(dir, func(string) string { return dir }, now)
	// GROK_HOME=dir → dir/auth.json
	_ = path
	if err != nil {
		// LoadToken uses AuthPath with GROK_HOME
		tok, bind, err = grokauth.LoadToken("", func(k string) string {
			if k == "GROK_HOME" {
				return dir
			}
			return ""
		}, now)
	}
	if err != nil {
		t.Fatal(err)
	}
	if tok != "secret-token-xyz" {
		t.Fatalf("token=%q", tok)
	}
	if bind.AccountProfileID == "" || bind.AccountProfileID == tok {
		t.Fatal("binding must not equal token")
	}
	b, _ := json.Marshal(bind)
	if string(b) != "" && (contains(string(b), "secret-token") || contains(string(b), "tok")) {
		// ensure secret not in marshaled binding fields
		if contains(string(b), "secret-token-xyz") {
			t.Fatal("token leaked into binding JSON")
		}
	}
}

func TestCanonicalRejectsRootCliClient(t *testing.T) {
	if grokauth.CanonicalAccountProfileID("root", "", "") != "" {
		t.Fatal("root")
	}
	if grokauth.CanonicalAccountProfileID("cli", "", "") != "" {
		t.Fatal("cli")
	}
	if grokauth.CanonicalAccountProfileID("a@b.com", "", "") != "" {
		t.Fatal("email")
	}
}

func TestParseActive_AllExpiredRejected(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	raw, _ := json.Marshal(map[string]any{
		"issuer::a": map[string]any{
			"key": "tok-a", "user_id": "user-a", "oidc_issuer": "https://auth.example",
			"expires_at": now.Add(-2 * time.Hour).Format(time.RFC3339),
		},
		"issuer::b": map[string]any{
			"key": "tok-b", "user_id": "user-b", "oidc_issuer": "https://auth.example",
			"expires_at": now.Add(-time.Hour).Format(time.RFC3339),
		},
	})
	if _, err := grokauth.ParseBytes(raw, now); err == nil {
		t.Fatal("expected all-expired rejection")
	}
}

func TestParseActive_MalformedExpiresAtRejected(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	raw, _ := json.Marshal(map[string]any{
		"issuer::bad": map[string]any{
			"key": "tok-bad", "user_id": "user-bad", "oidc_issuer": "https://auth.example",
			"expires_at": "not-a-timestamp",
		},
	})
	if _, err := grokauth.ParseBytes(raw, now); err == nil {
		t.Fatal("expected malformed expires_at rejection")
	}
}

func TestParseActive_NeverExposesSourcePathOnSuccess(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	raw, _ := json.Marshal(map[string]any{
		"issuer::ok": map[string]any{
			"key": "tok", "user_id": "user-ok", "oidc_issuer": "https://auth.example",
			"expires_at": now.Add(time.Hour).Format(time.RFC3339),
		},
	})
	bind, err := grokauth.ParseBytes(raw, now)
	if err != nil {
		t.Fatal(err)
	}
	if bind.SourcePath != "" {
		t.Fatalf("SourcePath must not be exposed: %q", bind.SourcePath)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		(len(s) > 0 && (func() bool {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		})()))
}
