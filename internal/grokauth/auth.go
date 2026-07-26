// Package grokauth is the single shared non-secret Grok CLI auth identity parser
// used by providerinventory (auth readiness + billing quota) and agent execution.
// Token material never appears in AccountProfileID / reports / logs.
package grokauth

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Binding is the non-secret active Grok account identity.
type Binding struct {
	// AccountProfileID is the canonical opaque account binding: "acct-" + 64 hex.
	// Empty means authenticated token may exist but the account is not exact-routable.
	AccountProfileID string
	// Principal is a redacted diagnostic (never email/raw secret); empty if unknown.
	PrincipalRedacted string
	// SourcePath is the auth.json path used.
	SourcePath string
	// HasToken is true when a usable access key was found.
	HasToken bool
	// ExactRoutable is true only when AccountProfileID is a full opaque binding.
	ExactRoutable bool
}

// AuthPath resolves ~/.grok/auth.json or GROK_HOME/auth.json.
func AuthPath(home string, getenv func(string) string) string {
	if getenv == nil {
		getenv = os.Getenv
	}
	if gh := strings.TrimSpace(getenv("GROK_HOME")); gh != "" {
		return filepath.Join(gh, "auth.json")
	}
	home = strings.TrimSpace(home)
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		home = h
	}
	return filepath.Join(home, ".grok", "auth.json")
}

// ParseActive reads the official auth.json and returns the selected active
// account binding. Token is never returned.
func ParseActive(home string, getenv func(string) string, now time.Time) (Binding, error) {
	path := AuthPath(home, getenv)
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) == 0 {
		return Binding{SourcePath: path}, fmt.Errorf("grokauth: auth unreadable: %w", err)
	}
	bind, _, err := parseAuthJSON(raw, path, now, false)
	return bind, err
}

// LoadToken returns the bearer token for credential boundaries only (HTTP Authorization).
// The token must never be logged, written to reports, or stored in AccountProfileID.
func LoadToken(home string, getenv func(string) string, now time.Time) (token string, bind Binding, err error) {
	path := AuthPath(home, getenv)
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) == 0 {
		return "", Binding{SourcePath: path}, fmt.Errorf("grokauth: auth unreadable: %w", err)
	}
	bind, tok, err := parseAuthJSON(raw, path, now, true)
	return tok, bind, err
}

// ParseBytes is the pure parser for fixtures/tests (no filesystem).
func ParseBytes(raw []byte, now time.Time) (Binding, error) {
	bind, _, err := parseAuthJSON(raw, "", now, false)
	return bind, err
}

// CanonicalAccountProfileID derives the opaque exact-routable account id from
// stable principal + optional team/tenant + optional issuer.
// Never accepts client_id, email, "root", or map-key-as-client alone.
func CanonicalAccountProfileID(principal, team, issuer string) string {
	principal = strings.TrimSpace(principal)
	team = strings.TrimSpace(team)
	issuer = strings.TrimSpace(issuer)
	if principal == "" {
		return ""
	}
	// Reject email-shaped principal as display identity.
	if strings.Contains(principal, "@") {
		return ""
	}
	if principal == "root" || principal == "cli" || principal == "unknown" || principal == "account" {
		return ""
	}
	h := sha256.New()
	fmt.Fprintf(h, "provider:grok|principal:%s", principal)
	if team != "" {
		fmt.Fprintf(h, "|team:%s", team)
	}
	if issuer != "" {
		fmt.Fprintf(h, "|issuer:%s", issuer)
	}
	return "acct-" + hex.EncodeToString(h.Sum(nil))
}

type candidate struct {
	token        string
	principal    string
	team         string
	issuer       string
	exp          time.Time
	malformedExp bool   // expires_at present but unparseable → reject
	mapKey       string // only for deterministic tie-break, never as identity alone
}

func parseAuthJSON(raw []byte, path string, now time.Time, wantToken bool) (Binding, string, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	var root any
	if err := json.Unmarshal(raw, &root); err != nil {
		return Binding{SourcePath: path}, "", fmt.Errorf("grokauth: malformed json: %w", err)
	}
	m, ok := root.(map[string]any)
	if !ok {
		return Binding{SourcePath: path}, "", fmt.Errorf("grokauth: root not object")
	}

	var cands []candidate

	// Flat token shape: has key/access_token at root.
	if tok := firstString(m, "key", "access_token"); tok != "" {
		c := candidate{
			token:     tok,
			principal: extractPrincipal(m),
			team:      extractTeam(m),
			issuer:    firstString(m, "oidc_issuer", "issuer"),
			mapKey:    "root",
		}
		if exp := firstString(m, "expires_at", "expiresAt"); exp != "" {
			t, ok := parseTimeStrict(exp)
			if !ok {
				c.malformedExp = true
			} else {
				c.exp = t
			}
		}
		cands = append(cands, c)
	}

	// Nested maps: keys may be "issuer::client_id" — never treat client_id as account.
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic scan order
	for _, k := range keys {
		vm, ok := m[k].(map[string]any)
		if !ok {
			continue
		}
		tok := firstString(vm, "key", "access_token")
		if tok == "" {
			continue
		}
		c := candidate{
			token:     tok,
			principal: extractPrincipal(vm),
			team:      extractTeam(vm),
			issuer:    firstString(vm, "oidc_issuer", "issuer"),
			mapKey:    k,
		}
		// Issuer from map key only when nested lacks it and key looks like issuer::client.
		if c.issuer == "" {
			if iss, _, ok := splitIssuerClientKey(k); ok {
				c.issuer = iss
			}
		}
		if exp := firstString(vm, "expires_at", "expiresAt"); exp != "" {
			t, ok := parseTimeStrict(exp)
			if !ok {
				c.malformedExp = true
			} else {
				c.exp = t
			}
		}
		cands = append(cands, c)
	}

	if len(cands) == 0 {
		return Binding{}, "", fmt.Errorf("grokauth: no bearer token")
	}

	// Reject candidates with malformed expires_at (present but unparseable).
	var usable []candidate
	var allExpired bool
	expiredCount := 0
	for _, c := range cands {
		if c.malformedExp {
			// Malformed expires_at must not route as active.
			continue
		}
		if !c.exp.IsZero() && c.exp.Before(now) {
			expiredCount++
			continue
		}
		usable = append(usable, c)
	}
	if len(usable) == 0 {
		allExpired = expiredCount > 0 || len(cands) > 0
		if allExpired {
			return Binding{}, "", fmt.Errorf("grokauth: all candidates expired or malformed expires_at")
		}
		return Binding{}, "", fmt.Errorf("grokauth: no usable bearer token")
	}

	// Selection among non-expired: latest expires_at, then principal, then mapKey.
	best := usable[0]
	for _, c := range usable[1:] {
		if preferCandidate(c, best, now) {
			best = c
		}
	}

	// Identity-less token may authenticate but cannot create exact routable account.
	// Never use map key (oidc_issuer::oidc_client_id), email, root, or client_id.
	// Never expose SourcePath or raw identity on the returned Binding for logging.
	acctID := CanonicalAccountProfileID(best.principal, best.team, best.issuer)
	bind := Binding{
		AccountProfileID:  acctID,
		PrincipalRedacted: redactPrincipal(best.principal),
		// SourcePath intentionally empty in returned binding (never expose).
		SourcePath:    "",
		HasToken:      best.token != "",
		ExactRoutable: acctID != "" && strings.HasPrefix(acctID, "acct-") && len(acctID) == 5+64,
	}
	tok := ""
	if wantToken {
		tok = best.token
	}
	if !bind.HasToken {
		return bind, "", fmt.Errorf("grokauth: no bearer token")
	}
	return bind, tok, nil
}

func preferCandidate(c, best candidate, now time.Time) bool {
	bestExp := !best.exp.IsZero() && best.exp.Before(now)
	cExp := !c.exp.IsZero() && c.exp.Before(now)
	if bestExp && !cExp {
		return true
	}
	if !bestExp && cExp {
		return false
	}
	// Prefer candidates with real principal identity.
	if best.principal == "" && c.principal != "" {
		return true
	}
	if best.principal != "" && c.principal == "" {
		return false
	}
	if c.exp.After(best.exp) {
		return true
	}
	if best.exp.After(c.exp) {
		return false
	}
	// Deterministic tie-break: principal then mapKey (never selection by map iteration).
	if c.principal != best.principal {
		return c.principal < best.principal
	}
	return c.mapKey < best.mapKey
}

// extractPrincipal prefers stable non-email principal fields only.
// Order is fixed: principal_id, user_id, account_id, account, profile_id, sub.
// Email is never principal (display only — rejected by CanonicalAccountProfileID).
func extractPrincipal(m map[string]any) string {
	for _, k := range []string{
		"principal_id", "principalId", "user_id", "userId", "account_id", "accountId",
		"account", "profile_id", "profileId", "sub",
	} {
		if s := firstString(m, k); s != "" && !strings.Contains(s, "@") {
			return s
		}
	}
	return ""
}

func extractTeam(m map[string]any) string {
	return firstString(m, "team_id", "teamId", "tenant_id", "tenantId", "org_id", "orgId")
}

func splitIssuerClientKey(k string) (issuer, client string, ok bool) {
	// Official map keys look like "https://auth.x.ai/oidc::client_uuid"
	if i := strings.Index(k, "::"); i > 0 {
		return k[:i], k[i+2:], true
	}
	return "", "", false
}

func firstString(m map[string]any, keys ...string) string {
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

// parseTimeStrict returns false when expires_at is present but not a valid
// RFC3339 / RFC3339Nano timestamp (malformed → reject candidate).
func parseTimeStrict(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC(), true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), true
	}
	return time.Time{}, false
}

func redactPrincipal(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) <= 8 {
		sum := sha256.Sum256([]byte(s))
		return "p-" + hex.EncodeToString(sum[:4])
	}
	return "p-" + s[:2] + "…" + s[len(s)-2:]
}

// RequireMatch verifies requested AccountProfileID/AccountRef equals active binding.
func RequireMatch(want string, home string, getenv func(string) string, now time.Time) (Binding, error) {
	want = strings.TrimSpace(want)
	if want == "" {
		return Binding{}, fmt.Errorf("grokauth: requested account empty")
	}
	bind, err := ParseActive(home, getenv, now)
	if err != nil {
		return Binding{}, err
	}
	if !bind.ExactRoutable || bind.AccountProfileID == "" {
		return bind, fmt.Errorf("grokauth: active auth has no exact-routable account identity")
	}
	if !strings.EqualFold(bind.AccountProfileID, want) {
		return bind, fmt.Errorf("grokauth: account mismatch requested=%s active=%s", want, bind.AccountProfileID)
	}
	return bind, nil
}
