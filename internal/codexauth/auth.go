// Package codexauth is the shared non-secret Codex/ChatGPT account identity
// parser used by providerinventory (quota/auth readiness) and agent execution.
// Token material never appears in AccountProfileID / reports / logs.
package codexauth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Binding is the non-secret active Codex account identity.
type Binding struct {
	// AccountProfileID is the canonical opaque account binding: "acct-" + 64 hex.
	AccountProfileID string
	// PrincipalRedacted is a redacted diagnostic (never email/raw secret).
	PrincipalRedacted string
	// HasToken is true when a usable access token was found.
	HasToken bool
	// ExactRoutable is true only when AccountProfileID is a full opaque binding.
	ExactRoutable bool
	// AuthMode is a non-secret label (e.g. chatgpt).
	AuthMode string
}

// AuthPath resolves ~/.codex/auth.json or CODEX_HOME/auth.json.
func AuthPath(home string, getenv func(string) string) string {
	if getenv == nil {
		getenv = os.Getenv
	}
	if ch := strings.TrimSpace(getenv("CODEX_HOME")); ch != "" {
		return filepath.Join(ch, "auth.json")
	}
	home = strings.TrimSpace(home)
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		home = h
	}
	return filepath.Join(home, ".codex", "auth.json")
}

// ParseActive reads the official Codex auth.json and returns the selected
// active account binding. Token is never returned.
func ParseActive(home string, getenv func(string) string, now time.Time) (Binding, error) {
	path := AuthPath(home, getenv)
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) == 0 {
		return Binding{}, fmt.Errorf("codexauth: auth unreadable: %w", err)
	}
	return ParseBytes(raw, now)
}

// ParseBytes is the pure parser for fixtures/tests (no filesystem).
func ParseBytes(raw []byte, now time.Time) (Binding, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return Binding{}, fmt.Errorf("codexauth: malformed json: %w", err)
	}
	authMode := firstString(root, "auth_mode", "authMode")
	tokens, _ := root["tokens"].(map[string]any)
	if tokens == nil {
		// Some fixtures put account fields at root.
		tokens = root
	}
	access := firstString(tokens, "access_token", "accessToken")
	if access == "" {
		// API key only sessions may still have account_id.
		if firstString(root, "OPENAI_API_KEY") == "" && firstString(tokens, "api_key", "apiKey") == "" {
			return Binding{AuthMode: authMode}, fmt.Errorf("codexauth: no access token")
		}
	}

	// Prefer explicit non-secret account_id field on tokens.
	principal := firstString(tokens, "account_id", "accountId", "chatgpt_account_id")
	userID := ""
	issuer := "https://auth.openai.com"
	plan := ""

	// Supplement from JWT claims when present (never store token).
	if access != "" && strings.Count(access, ".") == 2 {
		claims, cerr := decodeJWTClaims(access)
		if cerr == nil {
			if exp, ok := claims["exp"].(float64); ok {
				if time.Unix(int64(exp), 0).UTC().Before(now) {
					return Binding{AuthMode: authMode, HasToken: true}, fmt.Errorf("codexauth: access token expired")
				}
			}
			if iss := firstString(claims, "iss"); iss != "" {
				issuer = iss
			}
			if authObj, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
				if principal == "" {
					principal = firstString(authObj, "chatgpt_account_id", "account_id")
				}
				userID = firstString(authObj, "chatgpt_user_id", "user_id")
				plan = firstString(authObj, "chatgpt_plan_type", "plan_type")
			}
			// Never use email from profile claims as principal.
		}
	}

	// Reject email-shaped principal.
	if strings.Contains(principal, "@") {
		principal = ""
	}
	if principal == "" && userID != "" && !strings.Contains(userID, "@") {
		// Fallback: user id only when account id missing (still exact-routable).
		principal = userID
	}
	if principal == "" {
		return Binding{
			AuthMode: authMode, HasToken: access != "",
		}, fmt.Errorf("codexauth: no exact account identity")
	}

	acctID := CanonicalAccountProfileID(principal, plan, issuer)
	bind := Binding{
		AccountProfileID:  acctID,
		PrincipalRedacted: redactPrincipal(principal),
		HasToken:          access != "",
		ExactRoutable:     acctID != "" && strings.HasPrefix(acctID, "acct-") && len(acctID) == 5+64,
		AuthMode:          authMode,
	}
	if !bind.ExactRoutable {
		return bind, fmt.Errorf("codexauth: account not exact-routable")
	}
	return bind, nil
}

// CanonicalAccountProfileID derives the opaque exact-routable account id from
// provider + stable principal only. Plan and issuer are separate evidence and
// MUST NOT enter the identity hash (mutable plan would drift inventory vs
// execution bindings for the same ChatGPT account).
// Never accepts email, client_id alone, or raw tokens.
//
// The plan/issuer parameters are retained for API stability but ignored.
func CanonicalAccountProfileID(principal, plan, issuer string) string {
	principal = strings.TrimSpace(principal)
	_ = plan
	_ = issuer
	if principal == "" || strings.Contains(principal, "@") {
		return ""
	}
	if principal == "unknown" || principal == "root" || principal == "account" {
		return ""
	}
	h := sha256.New()
	fmt.Fprintf(h, "provider:codex|principal:%s", principal)
	return "acct-" + hexEncode(h.Sum(nil))
}

// RequireMatch verifies requested AccountProfileID equals active binding.
func RequireMatch(want string, home string, getenv func(string) string, now time.Time) (Binding, error) {
	want = strings.TrimSpace(want)
	if want == "" {
		return Binding{}, fmt.Errorf("codexauth: requested account empty")
	}
	bind, err := ParseActive(home, getenv, now)
	if err != nil {
		return Binding{}, err
	}
	if !bind.ExactRoutable || bind.AccountProfileID == "" {
		return bind, fmt.Errorf("codexauth: active auth has no exact-routable account identity")
	}
	if !strings.EqualFold(bind.AccountProfileID, want) {
		return bind, fmt.Errorf("codexauth: account mismatch requested=%s active=%s", want, bind.AccountProfileID)
	}
	return bind, nil
}

func decodeJWTClaims(token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("not a jwt")
	}
	payload := parts[1]
	// Pad base64url.
	if m := len(payload) % 4; m != 0 {
		payload += strings.Repeat("=", 4-m)
	}
	raw, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		// Try raw std encoding without padding issues.
		raw, err = base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			return nil, err
		}
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		return nil, err
	}
	return claims, nil
}

func firstString(m map[string]any, keys ...string) string {
	if m == nil {
		return ""
	}
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

func redactPrincipal(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) <= 8 {
		sum := sha256.Sum256([]byte(s))
		return "p-" + hexEncode(sum[:4])
	}
	return "p-" + s[:2] + "…" + s[len(s)-2:]
}

func hexEncode(b []byte) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = hexdigits[v>>4]
		out[i*2+1] = hexdigits[v&0x0f]
	}
	return string(out)
}
