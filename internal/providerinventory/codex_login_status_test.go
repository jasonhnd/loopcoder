package providerinventory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/codexauth"
	"github.com/jasonhnd/loopcoder/internal/config"
)

// TestParseCodexLoginStatus_SharedCodexAuthID proves inventory AuthReadiness stamps
// the same opaque AccountProfileID as agent preflight (codexauth), and that the
// legacy status-line acct_<base32> → opaqueAccountRef double-hash path is NOT used.
func TestParseCodexLoginStatus_SharedCodexAuthID(t *testing.T) {
	// Isolated CODEX_HOME with non-secret fixture principal.
	codexHome := t.TempDir()
	principal := "537689fe-5e19-45f1-96f2-5f6b99373698"
	raw, _ := json.Marshal(map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"access_token": "not-a-jwt",
			"account_id":   principal,
		},
	})
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	prev := os.Getenv("CODEX_HOME")
	t.Setenv("CODEX_HOME", codexHome)
	_ = prev

	want := codexauth.CanonicalAccountProfileID(principal, "", "")
	if want == "" || !strings.HasPrefix(want, "acct-") || len(want) != 5+64 {
		t.Fatalf("canonical id malformed: %q", want)
	}

	parsed := parseCodexLoginStatus("Logged in using ChatGPT\n", 0)
	if len(parsed) != 1 {
		t.Fatalf("parsed=%d want 1: %+v", len(parsed), parsed)
	}
	if parsed[0].State != ReadinessReady {
		t.Fatalf("state=%q want ready", parsed[0].State)
	}
	if parsed[0].AccountKind != "codexauth-shared" {
		t.Fatalf("AccountKind=%q want codexauth-shared", parsed[0].AccountKind)
	}
	if parsed[0].ReferenceHash != want {
		t.Fatalf("ReferenceHash=%q want shared codexauth %q", parsed[0].ReferenceHash, want)
	}
	// accountProfileID must pass through without re-hashing to acct_base32.
	gotID := accountProfileID("codex", string(ProfileSourceStatusCommand), parsed[0].ReferenceHash)
	if gotID != want {
		t.Fatalf("accountProfileID=%q want pass-through %q", gotID, want)
	}

	// Prove the RC38 dual-hash path is different (must NOT equal shared id).
	legacyRef := "sha256:" + hashHex("codex", "provider-status-command", "Logged in using ChatGPT")
	legacyStatusID := "acct_" + hashBase32("codex", string(ProfileSourceStatusCommand), legacyRef)[:32]
	sum := sha256.Sum256([]byte("acct|" + legacyStatusID))
	legacyOpaque := "acct-" + hex.EncodeToString(sum[:])
	if legacyOpaque == want {
		t.Fatal("legacy dual-hash unexpectedly equals codexauth id — test fixture collision")
	}
	if gotID == legacyOpaque {
		t.Fatal("inventory still produces legacy opaqueAccountRef dual-hash account id")
	}
}

func TestParseCodexLoginStatus_NotAuthenticatedNoAmbientAccount(t *testing.T) {
	// Even if ambient auth.json exists, not-authenticated status must not stamp
	// a routable account from ambient credentials alone when classify says not auth.
	// (parseCodexLoginStatus still prefers codexauth when ParseActive succeeds —
	// that is correct for "Logged in" paths. For not-logged-in text we still
	// return classified state; if auth.json is present identity may stamp while
	// state stays not-authenticated. Capacity routing requires Ready.)
	parsed := parseCodexLoginStatus("Not logged in. Run codex login.\n", 1)
	if len(parsed) != 1 {
		t.Fatalf("parsed=%d: %+v", len(parsed), parsed)
	}
	if parsed[0].State != ReadinessNotAuthenticated {
		t.Fatalf("state=%q want not_authenticated", parsed[0].State)
	}
}

func TestDiscoverCodexAuthReadiness_SharedIDWhenAuthPresent(t *testing.T) {
	// End-to-end Discover: when login status is ready and CODEX_HOME has auth,
	// AuthReadiness.AccountProfileID equals codexauth canonical (not acct_base32).
	codexHome := t.TempDir()
	principal := "fixture-principal-rc38-rebind"
	raw, _ := json.Marshal(map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"access_token": "not-a-jwt",
			"account_id":   principal,
		},
	})
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", codexHome)
	want := codexauth.CanonicalAccountProfileID(principal, "", "")

	dir := t.TempDir()
	exe := filepath.Join(dir, executableName("codex"))
	writeExecutable(t, exe)
	deps := fakeDeps(t, map[string]string{filepath.Clean(exe): "codex 1.2.3"})
	deps.Getenv = func(key string) string {
		if key == "PATH" {
			return dir
		}
		if key == "CODEX_HOME" {
			return codexHome
		}
		return ""
	}
	deps.RunProbe = func(_ context.Context, req ProbeExecution) (ProbeExecutionResult, error) {
		if len(req.Argv) >= 3 && req.Argv[1] == "login" && req.Argv[2] == "status" {
			return ProbeExecutionResult{Stdout: "Logged in using ChatGPT\n", ExitCode: 0}, nil
		}
		return ProbeExecutionResult{Stdout: "codex 1.2.3\n", ExitCode: 0}, nil
	}

	report, err := Discover(context.Background(), Options{
		Config: config.Config{Adapters: config.Adapters{Worker: "codex"}},
		Now:    fixedInventoryNow,
	}, deps)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	got := latestAuthReadinessFor(t, report, "codex")
	if got.ReadinessState != ReadinessReady {
		t.Fatalf("state=%q record=%#v", got.ReadinessState, got)
	}
	if got.AccountProfileID == nil {
		t.Fatal("AccountProfileID nil — inventory not stamping shared codexauth id")
	}
	if *got.AccountProfileID != want {
		t.Fatalf("AccountProfileID=%q want codexauth %q (must not be acct_base32)", *got.AccountProfileID, want)
	}
	if strings.HasPrefix(*got.AccountProfileID, "acct_") {
		t.Fatalf("status-style acct_ id leaked into readiness: %q", *got.AccountProfileID)
	}
	// accountProfileID length: opaque is acct- + 64 hex
	if len(*got.AccountProfileID) != 5+64 {
		t.Fatalf("len=%d want 69 for opaque acct- hex", len(*got.AccountProfileID))
	}
	_ = time.Now // keep import if needed
}
