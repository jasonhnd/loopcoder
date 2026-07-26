package goalrun

import (
	"testing"

	"github.com/jasonhnd/loopcoder/internal/routedecision"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
)

func TestValidateCanaryUnavailableProbeRequestFailsClosed(t *testing.T) {
	valid := Request{
		CanaryUnavailableProbeProvider: "codex",
		CanaryUnavailableProbeModel:    "gpt-5.3-codex",
		CanaryEmit:                     &CanaryEmitOptions{OutPath: "canary.json"},
	}
	if err := validateCanaryUnavailableProbeRequest(valid); err != nil {
		t.Fatalf("valid request: %v", err)
	}
	cases := []Request{
		{CanaryUnavailableProbeProvider: "codex"},
		{CanaryUnavailableProbeProvider: "codex", CanaryUnavailableProbeModel: "invented", CanaryEmit: valid.CanaryEmit},
		{CanaryUnavailableProbeProvider: "codex", CanaryUnavailableProbeModel: "gpt-5.3-codex"},
		{CanaryUnavailableProbeProvider: "codex", CanaryUnavailableProbeModel: "gpt-5.3-codex", CanaryEmit: valid.CanaryEmit, Provider: "codex", Model: "gpt-5.5"},
		{CanaryUnavailableProbeProvider: " codex", CanaryUnavailableProbeModel: "gpt-5.3-codex", CanaryEmit: valid.CanaryEmit},
		{CanaryUnavailableProbeProvider: "codex", CanaryUnavailableProbeModel: "gpt-5.3-codex ", CanaryEmit: valid.CanaryEmit},
	}
	dry := true
	dryReq := valid
	dryReq.DryRun = &dry
	cases = append(cases, dryReq)
	for i, req := range cases {
		if err := validateCanaryUnavailableProbeRequest(req); err == nil {
			t.Fatalf("case %d must fail closed", i)
		}
	}
}

func TestCanaryUnavailableProbeRequiresFilteredDeclaredRoute(t *testing.T) {
	if !declaredModelSupports("codex", "gpt-5.3-codex", "low") {
		t.Fatal("declared codex candidate should support low")
	}
	decision := &routedecision.Decision{Candidates: []routedecision.CandidateView{{
		Provider: "codex", Model: "gpt-5.3-codex", Effort: "low",
		Permission: "read-only", HardEligible: true,
	}}}
	if !decisionHasHardEligibleRoute(decision, "codex", "gpt-5.3-codex", "low", "read-only") {
		t.Fatal("hard-eligible live route must be detected and rejected as probe")
	}
}

func TestPrependAlternateUniqueKeepsVerifiedWinnerFirst(t *testing.T) {
	preferred := workflowrun.AlternateCandidate{
		Provider: "codex", Model: "gpt-5.3-codex-spark", Effort: "low",
		Permission: "read-only", AccountRef: "acct-a", InstallRef: "pinst-a",
		WindowKind: "weekly", HardEligible: true,
	}
	got := prependAlternateUnique([]workflowrun.AlternateCandidate{preferred, {
		Provider: "codex", Model: "gpt-5.5", Effort: "low",
		Permission: "read-only", AccountRef: "acct-a", InstallRef: "pinst-a",
		WindowKind: "weekly", HardEligible: true,
	}}, preferred)
	if len(got) != 2 || got[0].Model != preferred.Model || got[1].Model != "gpt-5.5" {
		t.Fatalf("unexpected alternates: %+v", got)
	}
}
