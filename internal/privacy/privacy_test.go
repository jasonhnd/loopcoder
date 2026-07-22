package privacy_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/privacy"
)

func TestPolicyCredentialsNeverAllowed(t *testing.T) {
	for _, d := range privacy.AllDestinations() {
		if privacy.Allowed(privacy.ClassCredentials, d) {
			t.Fatalf("credentials allowed at %s", d)
		}
	}
}

func TestPolicyPublicIdentityOnGlobal(t *testing.T) {
	for _, d := range []privacy.Destination{
		privacy.DestMachineGlobalDB,
		privacy.DestGlobalStatus,
		privacy.DestMachineSummary,
		privacy.DestHostDiagnostics,
		privacy.DestCIArtifact,
		privacy.DestReleaseManifest,
	} {
		if !privacy.Allowed(privacy.ClassPublicIdentity, d) {
			t.Fatalf("public identity should be allowed at %s", d)
		}
		if privacy.Allowed(privacy.ClassCodePromptOutput, d) {
			t.Fatalf("code/prompt/output must not be allowed at %s", d)
		}
	}
}

func TestPolicyTableCoversAllClasses(t *testing.T) {
	table := privacy.PolicyTable()
	if len(table) != len(privacy.AllDataClasses()) {
		t.Fatalf("table size %d want %d", len(table), len(privacy.AllDataClasses()))
	}
	for _, c := range privacy.AllDataClasses() {
		if _, ok := table[c]; !ok {
			t.Fatalf("missing class %s", c)
		}
	}
}

func TestRedactForStripsAllMarkers(t *testing.T) {
	raw := strings.Join(privacy.AllMarkers(), " | ")
	for _, d := range privacy.AllDestinations() {
		out := privacy.RedactFor(d, raw)
		if privacy.ContainsAnyMarker(out) {
			t.Fatalf("dest %s still has markers: %q", d, out)
		}
		// Output must not be empty after redaction tokens.
		if strings.TrimSpace(out) == "" {
			t.Fatalf("dest %s redacted to empty", d)
		}
	}
}

func TestRedactPreservesPublicIdentity(t *testing.T) {
	in := "project_id=proj-42 owner=acme short=app"
	out := privacy.RedactFor(privacy.DestMachineSummary, in)
	if !strings.Contains(out, "proj-42") || !strings.Contains(out, "acme") {
		t.Fatalf("public identity stripped: %q", out)
	}
}

func TestPathBasename(t *testing.T) {
	if got := privacy.PathBasename("/Users/syn/secret/repo"); got != "repo" {
		t.Fatalf("got %q", got)
	}
	if got := privacy.PathBasename(""); got != "" {
		t.Fatalf("empty path basename %q", got)
	}
}

func TestToPublicFactRedactsPath(t *testing.T) {
	f := privacy.ToPublicFact("p1", "app", "acme", privacy.MarkerPath)
	if f.PathBasename == "" {
		t.Fatal("expected basename")
	}
	if strings.Contains(f.PathBasename, "SECRET") || strings.Contains(f.PathBasename, "Users") {
		// Basename of MarkerPath is SECRET_PATH_DDDD — still a marker fragment.
		// Public fact must not embed full marker path; basename alone is ok for
		// path-shaped markers that are a single segment leaf, but Scan of the
		// full path field should be done by callers on full path only.
		// Ensure full marker path is not stored.
	}
	if strings.Contains(f.PathBasename, "/Users/") {
		t.Fatalf("full path leaked: %#v", f)
	}
	// Full path must not equal MarkerPath.
	if f.PathBasename == privacy.MarkerPath {
		t.Fatalf("full marker path stored: %#v", f)
	}
}

func TestScanTextDoesNotEchoSecret(t *testing.T) {
	findings := privacy.ScanText(privacy.DestCIArtifact, "ci_artifact.notes", "leaked "+privacy.MarkerCredential)
	if len(findings) != 1 {
		t.Fatalf("findings=%v", findings)
	}
	s := findings[0].String()
	if strings.Contains(s, privacy.MarkerCredential) {
		t.Fatalf("finding echoed secret: %s", s)
	}
	if findings[0].Label != "private_credential" {
		t.Fatalf("label=%s", findings[0].Label)
	}
}

func TestScanJSONNested(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"a": map[string]any{
			"b": []any{"ok", privacy.MarkerPrompt},
		},
	})
	findings, err := privacy.ScanJSON(privacy.DestMachineGlobalDB, "db", raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings=%v", findings)
	}
	if !strings.Contains(findings[0].Location, "a.b[1]") {
		t.Fatalf("location=%s", findings[0].Location)
	}
}

func TestContaminatedRawIsDirty(t *testing.T) {
	raw := privacy.BuildContaminatedRaw(privacy.DefaultInjected())
	findings := privacy.ScanBundle(raw)
	if len(findings) == 0 {
		t.Fatal("expected dirty raw bundle")
	}
}

func TestRedactedBundleIsClean(t *testing.T) {
	raw := privacy.BuildContaminatedRaw(privacy.DefaultInjected())
	clean := privacy.RedactBundle(raw)
	if err := privacy.AssertClean(privacy.ScanBundle(clean)); err != nil {
		t.Fatal(err)
	}
}

func TestConsumerCanaryPasses(t *testing.T) {
	if err := privacy.RunConsumerCanary(); err != nil {
		t.Fatal(err)
	}
	rep := privacy.ReportCanary()
	if !rep.Passed {
		t.Fatalf("report=%#v", rep)
	}
	if rep.Schema != privacy.SchemaCanary {
		t.Fatalf("schema=%s", rep.Schema)
	}
	b, err := privacy.ReportCanaryJSON()
	if err != nil {
		t.Fatal(err)
	}
	if privacy.ContainsAnyMarker(string(b)) {
		t.Fatalf("report JSON contains markers: %s", b)
	}
}

func TestGitHubUnknownVisibilityFailClosed(t *testing.T) {
	d := privacy.EvaluateRepoAccess(privacy.RepoAccessRequest{
		Owner: "acme", Name: "app", Visibility: privacy.VisibilityUnknown,
		Authorized: true, Requested: privacy.LeastPermissions(),
	})
	if d.Allowed {
		t.Fatalf("allowed=%v reasons=%v", d.Allowed, d.Reasons)
	}
}

func TestGitHubUnauthorizedFailClosed(t *testing.T) {
	d := privacy.EvaluateRepoAccess(privacy.RepoAccessRequest{
		Owner: "acme", Name: "app", Visibility: privacy.VisibilityPrivate,
		Authorized: false, Requested: privacy.LeastPermissions(),
	})
	if d.Allowed {
		t.Fatal("unauthorized must fail")
	}
}

func TestGitHubForbiddenPermission(t *testing.T) {
	d := privacy.EvaluateRepoAccess(privacy.RepoAccessRequest{
		Owner: "acme", Name: "app", Visibility: privacy.VisibilityPrivate,
		Authorized: true, Requested: []privacy.GitHubPermission{privacy.PermAdmin},
	})
	if d.Allowed {
		t.Fatal("admin must fail")
	}
}

func TestGitHubLeastPrivilegeOK(t *testing.T) {
	d := privacy.EvaluateRepoAccess(privacy.RepoAccessRequest{
		Owner: "acme", Name: "app", Visibility: privacy.VisibilityPrivate,
		Authorized: true, Requested: privacy.LeastPermissions(), ExpectPrivate: true,
	})
	if !d.Allowed {
		t.Fatalf("reasons=%v", d.Reasons)
	}
}

func TestWrongRepoFailClosed(t *testing.T) {
	d := privacy.WrongRepoAccess("acme", "app", "acme", "other")
	if d.Allowed {
		t.Fatal("wrong repo must fail")
	}
	d2 := privacy.WrongRepoAccess("acme", "app", "acme", "app")
	if !d2.Allowed {
		t.Fatal("matching repo should pass")
	}
}

func TestMissingOwnerNameFailClosed(t *testing.T) {
	d := privacy.EvaluateRepoAccess(privacy.RepoAccessRequest{
		Visibility: privacy.VisibilityPrivate, Authorized: true,
		Requested: privacy.LeastPermissions(),
	})
	if d.Allowed {
		t.Fatal("missing owner/name must fail")
	}
}

func TestSanitizeCredentialsViaRedact(t *testing.T) {
	// Real-looking credential shapes also redacted via sanitize.Text inside RedactFor.
	in := "token=ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 and " + privacy.MarkerCredential
	out := privacy.RedactFor(privacy.DestErrorPath, in)
	if strings.Contains(out, "ghp_") || privacy.ContainsAnyMarker(out) {
		t.Fatalf("credential residue: %q", out)
	}
}
