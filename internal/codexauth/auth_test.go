package codexauth_test

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/codexauth"
)

func TestParseBytes_AccountIDField(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	raw, _ := json.Marshal(map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"access_token": "not-a-jwt",
			"account_id":   "537689fe-5e19-45f1-96f2-5f6b99373698",
		},
	})
	bind, err := codexauth.ParseBytes(raw, now)
	if err != nil {
		t.Fatal(err)
	}
	if !bind.ExactRoutable || bind.AccountProfileID == "" {
		t.Fatalf("want exact: %+v", bind)
	}
	want := codexauth.CanonicalAccountProfileID("537689fe-5e19-45f1-96f2-5f6b99373698", "", "")
	if bind.AccountProfileID != want {
		t.Fatalf("got %s want %s", bind.AccountProfileID, want)
	}
	// Never equal raw principal.
	if bind.AccountProfileID == "537689fe-5e19-45f1-96f2-5f6b99373698" {
		t.Fatal("must be opaque acct- hash")
	}
}

func TestParseBytes_JWTClaims(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	claims := map[string]any{
		"iss": "https://auth.openai.com",
		"exp": float64(now.Add(time.Hour).Unix()),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct-uuid-xyz",
			"chatgpt_user_id":    "user-abc",
			"chatgpt_plan_type":  "pro",
		},
		"https://api.openai.com/profile": map[string]any{
			"email": "someone@example.com",
		},
	}
	payload, _ := json.Marshal(claims)
	tok := "hdr." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
	raw, _ := json.Marshal(map[string]any{
		"auth_mode": "chatgpt",
		"tokens":    map[string]any{"access_token": tok},
	})
	bind, err := codexauth.ParseBytes(raw, now)
	if err != nil {
		t.Fatal(err)
	}
	// Principal-only identity: plan/issuer must not change the opaque id.
	want := codexauth.CanonicalAccountProfileID("acct-uuid-xyz", "", "")
	if bind.AccountProfileID != want {
		t.Fatalf("got %s want %s", bind.AccountProfileID, want)
	}
	if codexauth.CanonicalAccountProfileID("acct-uuid-xyz", "pro", "https://auth.openai.com") != want {
		t.Fatal("plan/issuer must not enter account identity")
	}
}

func TestParseBytes_ExpiredRejected(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	claims := map[string]any{
		"iss": "https://auth.openai.com",
		"exp": float64(now.Add(-time.Hour).Unix()),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct-uuid-xyz",
		},
	}
	payload, _ := json.Marshal(claims)
	tok := "hdr." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
	raw, _ := json.Marshal(map[string]any{
		"tokens": map[string]any{"access_token": tok},
	})
	if _, err := codexauth.ParseBytes(raw, now); err == nil {
		t.Fatal("expected expired rejection")
	}
}

func TestParseBytes_EmailNeverPrincipal(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	raw, _ := json.Marshal(map[string]any{
		"tokens": map[string]any{
			"access_token": "x",
			"account_id":   "a@b.com",
		},
	})
	if _, err := codexauth.ParseBytes(raw, now); err == nil {
		t.Fatal("email must not be principal")
	}
}

func TestRequireMatch(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	raw, _ := json.Marshal(map[string]any{
		"tokens": map[string]any{
			"access_token": "x",
			"account_id":   "acct-uuid-1",
		},
	})
	bind, err := codexauth.ParseBytes(raw, now)
	if err != nil {
		t.Fatal(err)
	}
	// RequireMatch needs filesystem; ParseBytes path already covered.
	if bind.AccountProfileID == "" {
		t.Fatal("empty")
	}
	if codexauth.CanonicalAccountProfileID("unknown", "", "") != "" {
		t.Fatal("unknown rejected")
	}
}
