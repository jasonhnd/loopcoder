package artifactqual_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/artifactqual"
)

func TestValidateCanaryEvidenceRequiresRealRuntime(t *testing.T) {
	now := time.Date(2026, 7, 22, 21, 0, 0, 0, time.UTC)
	digest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	sha := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	rem := 0.9
	after := 0.85
	ev := artifactqual.CanaryEvidence{
		Schema:        artifactqual.SchemaCanaryEvidence,
		ArchiveDigest: digest,
		PreProdSHA:    sha,
		BinaryVersion: "0.9.0-rc.9",
		BinaryCommit:  sha,
		ProjectID:     "disp-test123",
		RunID:         "run_test1",
		ProducedAt:    now,
		ProviderObservations: []artifactqual.CanaryProviderObs{
			{Provider: "codex", Source: "codexbar", Freshness: "fresh", Remaining: &rem, CapturedAt: now},
			{Provider: "antigravity", Source: "codexbar", Freshness: "fresh", Remaining: &rem, CapturedAt: now},
		},
		Children: []artifactqual.CanaryChild{
			child("wi_research", "att-r-1", "codex", "gpt-5.5", "low", "succeeded", 0.96, 0.05, after),
			child("wi_implement", "att-i-1", "antigravity", "GPT-OSS", "medium", "succeeded", 0.9, 0.05, after),
			child("wi_tests", "att-t-1", "antigravity", "GPT-OSS", "medium", "succeeded", 0.9, 0.05, after),
			child("wi_verify", "att-v-1", "codex", "gpt-5.5", "high", "succeeded", 0.96, 0.05, after),
		},
		UnavailableRetry: &artifactqual.CanaryUnavailableRetry{
			ExcludedProvider: "codex-exhausted", ExcludedReason: "exhausted",
			NoDuplicateClaim: true, NoDuplicateFiles: true, NoDoubleCapacity: true,
		},
		Restart: &artifactqual.CanaryRestart{
			Interrupted: true, ResumedFromDurable: true, ExactlyOnce: true,
			ChildCountUseful: 4, ProcessCeilingOK: true, WorktreeCeilingOK: true,
			NoLeakedProcesses: true, NoRepoLocalRuntime: true,
		},
		PR: &artifactqual.CanaryPR{
			URL: "https://github.com/jasonhnd/loopcoder/pull/9999", Number: 9999,
			RequiredChecks: []string{"verify", "test"}, RequiredChecksGreen: true,
			IndependentVerifier: "loopreview", CreatedByLoopCoder: true,
		},
	}
	v := artifactqual.ValidateCanaryEvidence(ev, digest, sha, now)
	if !v.Valid {
		t.Fatalf("want valid: %v", v.Reasons)
	}
	if !v.MultiDepthOK || !v.MultiProviderOK || !v.CapacityAfterOK ||
		!v.UnavailableRetryOK || !v.RestartOK || !v.RealPROK {
		t.Fatalf("%+v", v)
	}
}

func TestValidateCanaryRejectsLocalProjectAndDigestMismatch(t *testing.T) {
	now := time.Now().UTC()
	ev := artifactqual.CanaryEvidence{
		Schema: artifactqual.SchemaCanaryEvidence, ArchiveDigest: "aa", PreProdSHA: "bb",
		ProjectID: "local-project", RunID: "r1", ProducedAt: now,
		BinaryVersion: "x",
	}
	v := artifactqual.ValidateCanaryEvidence(ev, "cc", "bb", now)
	if v.Valid {
		t.Fatal("must reject")
	}
	joined := ""
	for _, r := range v.Reasons {
		joined += r + ";"
	}
	if !contains(joined, "archive_digest_mismatch") || !contains(joined, "project_id_not_unique") {
		t.Fatalf("reasons=%v", v.Reasons)
	}
}

func TestLoadCanaryEvidenceSchema(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.json")
	_ = os.WriteFile(p, []byte(`{"schema":"wrong"}`), 0o600)
	if _, err := artifactqual.LoadCanaryEvidence(p); err == nil {
		t.Fatal("want schema error")
	}
	good := artifactqual.CanaryEvidence{Schema: artifactqual.SchemaCanaryEvidence, ProjectID: "disp-x", RunID: "r"}
	b, _ := json.Marshal(good)
	_ = os.WriteFile(p, b, 0o600)
	if _, err := artifactqual.LoadCanaryEvidence(p); err != nil {
		t.Fatal(err)
	}
}

func child(id, att, prov, model, depth, term string, before, reserved, after float64) artifactqual.CanaryChild {
	b, r, a := before, reserved, after
	return artifactqual.CanaryChild{
		ChildID: id, AttemptID: att, Provider: prov, Model: model,
		DepthRequired: depth, DepthSelected: depth, DepthInvocation: depth,
		Terminal: term, CapacityBefore: &b, CapacityReserved: &r, CapacityAfter: &a,
		ActualSource: "unknown", AfterSource: "codexbar", AfterFreshness: "fresh",
		RealProviderExecuted: true, WorktreePath: "/tmp/wt/" + id,
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
