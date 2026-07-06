package audit

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNativeSecretSignatureTierDetectsKnownFamilies(t *testing.T) {
	rawValues := []string{
		"github_token=" + githubClassicTokenForTest(),
		"github_fine_grained=" + githubFineGrainedTokenForTest(),
		"stripe=" + stripeLiveKeyForTest(),
		"aws=" + awsAccessKeyForTest(),
		"pem=" + pemPrivateKeyHeaderForTest(),
		"jwt=" + jwtValueForTest(),
	}
	findings := nativeSecretFindings("secrets.txt", strings.Join(rawValues, "\n"))

	if len(findings) != len(rawValues) {
		t.Fatalf("signature findings len = %d, want %d: %#v", len(findings), len(rawValues), findings)
	}
	for _, finding := range findings {
		if finding.Tier != FindingTierSignature || finding.Gate != FindingGateGate || finding.Severity != SeverityHigh {
			t.Fatalf("signature finding classification = tier %q gate %q severity %q, want signature/gate/high: %#v", finding.Tier, finding.Gate, finding.Severity, finding)
		}
		if finding.Rule != "native:secret" {
			t.Fatalf("signature finding rule = %q, want native:secret", finding.Rule)
		}
	}
	assertNativeFindingsDoNotLeak(t, findings,
		githubClassicTokenForTest(),
		githubFineGrainedTokenForTest(),
		stripeLiveKeyForTest(),
		awsAccessKeyForTest(),
		pemPrivateKeyHeaderForTest(),
		jwtValueForTest(),
	)
}

func TestNativeSecretGenericWarningsDoNotGateDefaultThreshold(t *testing.T) {
	genericValue := "N0tActuallyButHighEntropyA1b2C3d4E5"
	genericFindings := nativeSecretFindings("config.txt", `api_key = "`+genericValue+`"`)
	if len(genericFindings) != 1 {
		t.Fatalf("generic findings len = %d, want 1: %#v", len(genericFindings), genericFindings)
	}
	generic := genericFindings[0]
	if generic.Tier != FindingTierEntropy || generic.Gate != FindingGateWarning || generic.Severity != SeverityLow {
		t.Fatalf("generic finding classification = tier %q gate %q severity %q, want entropy/warning/low: %#v", generic.Tier, generic.Gate, generic.Severity, generic)
	}

	warningOnly := NewResult("repo", []string{LayerSAST}, SeverityMedium)
	warningOnly.Findings = genericFindings
	warningOnly = Finalize(warningOnly)
	if warningOnly.Verdict != VerdictClean || ExitCode(warningOnly) != 0 {
		t.Fatalf("generic warning verdict/exit = %s/%d, want clean/0", warningOnly.Verdict, ExitCode(warningOnly))
	}

	signatureGate := NewResult("repo", []string{LayerSAST}, SeverityMedium)
	signatureGate.Findings = nativeSecretFindings("config.txt", `token = "`+githubClassicTokenForTest()+`"`)
	signatureGate = Finalize(signatureGate)
	if signatureGate.Verdict != VerdictFindings || ExitCode(signatureGate) != 1 {
		t.Fatalf("signature verdict/exit = %s/%d, want findings/1", signatureGate.Verdict, ExitCode(signatureGate))
	}
}

func TestNativeSecretGenericDropsFalsePositiveSources(t *testing.T) {
	highEntropy := "r4nd0mZ9qP2xY8vT3mN7"
	tests := []struct {
		name string
		file string
		line string
	}{
		{name: "process env", file: "app.js", line: `token = process.env.SERVICE_TOKEN_ABC123456789`},
		{name: "go getenv", file: "app.go", line: `token = os.Getenv("SERVICE_TOKEN")`},
		{name: "python environ", file: "app.py", line: `password = os.environ["SERVICE_PASSWORD"]`},
		{name: "java getenv", file: "App.java", line: `password = System.getenv("SERVICE_PASSWORD")`},
		{name: "dollar placeholder", file: "config.yml", line: `api_key = "${SERVICE_API_KEY}"`},
		{name: "mustache placeholder", file: "config.yml", line: `secret = "{{SERVICE_SECRET}}"`},
		{name: "angle placeholder", file: "config.yml", line: `token = "<SERVICE_TOKEN>"`},
		{name: "example file", file: "config.env.example", line: `api_key = "` + highEntropy + `"`},
		{name: "sample file", file: "config.env.sample", line: `api_key = "` + highEntropy + `"`},
		{name: "template file", file: "config.env.template", line: `api_key = "` + highEntropy + `"`},
		{name: "test fixture path", file: "testdata/config.env", line: `api_key = "` + highEntropy + `"`},
		{name: "low entropy", file: "config.env", line: `password = "passwordpasswordpassword"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if findings := nativeSecretFindings(tt.file, tt.line); len(findings) != 0 {
				t.Fatalf("nativeSecretFindings(%q, %q) = %#v, want none", tt.file, tt.line, findings)
			}
		})
	}
}

func TestNativeSecretRedactsEvidenceJSONAndFingerprint(t *testing.T) {
	signature := githubClassicTokenForTest()
	generic := "A1b2C3d4E5f6G7h8I9j0K1"
	findings := nativeSecretFindings("secrets.txt", strings.Join([]string{
		`token = "` + signature + `"`,
		`api_key = "` + generic + `"`,
	}, "\n"))
	if len(findings) != 2 {
		t.Fatalf("native secret findings len = %d, want 2: %#v", len(findings), findings)
	}
	assertNativeFindingsDoNotLeak(t, findings, signature, generic)
}

func assertNativeFindingsDoNotLeak(t *testing.T, findings []Finding, rawValues ...string) {
	t.Helper()
	encoded, err := json.Marshal(findings)
	if err != nil {
		t.Fatalf("marshal findings: %v", err)
	}
	jsonText := string(encoded)
	for _, raw := range rawValues {
		for _, finding := range findings {
			if strings.Contains(finding.Evidence, raw) {
				t.Fatalf("finding evidence leaked raw value %q: %#v", raw, finding)
			}
			if strings.Contains(finding.Fingerprint, raw) {
				t.Fatalf("finding fingerprint leaked raw value %q: %#v", raw, finding)
			}
		}
		if strings.Contains(jsonText, raw) {
			t.Fatalf("finding JSON leaked raw value %q: %s", raw, jsonText)
		}
	}
}

func githubClassicTokenForTest() string {
	return "ghp_" + strings.Repeat("A", 36)
}

func githubFineGrainedTokenForTest() string {
	return "github_pat_" + strings.Repeat("B", 40)
}

func stripeLiveKeyForTest() string {
	return "sk_live_" + strings.Repeat("C", 24)
}

func awsAccessKeyForTest() string {
	return "AKIA" + strings.Repeat("1", 16)
}

func pemPrivateKeyHeaderForTest() string {
	return "-----BEGIN " + "RSA PRIVATE KEY-----"
}

func jwtValueForTest() string {
	return "eyJ" + strings.Repeat("a", 10) + "." + strings.Repeat("b", 10) + "." + strings.Repeat("c", 10)
}
