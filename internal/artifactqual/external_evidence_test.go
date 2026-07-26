package artifactqual_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/artifactqual"
)

func TestPreProdActionsReceipt_SameRunDualGreen(t *testing.T) {
	sha := strings.Repeat("a", 40)
	r := artifactqual.PreProdActionsReceipt{
		Schema:     artifactqual.SchemaPreProdActionsReceipt,
		Repository: "jasonhnd/loopcoder", WorkflowPath: artifactqual.PreProdIntegrationWorkflow,
		RunID: 99, Attempt: 1, Event: "push", HeadBranch: "pre-prod", HeadSHA: sha,
		Status: "completed", Conclusion: "success",
		Jobs: []artifactqual.PreProdActionsJob{
			{Name: "integration-verify", Status: "completed", Conclusion: "success"},
			{Name: "integration-canary", Status: "completed", Conclusion: "success"},
		},
	}
	vOK, cOK, reasons := artifactqual.ValidatePreProdActionsReceipt(r, sha, "jasonhnd/loopcoder")
	if !vOK || !cOK || len(reasons) > 0 {
		t.Fatalf("want dual green, got v=%v c=%v reasons=%v", vOK, cOK, reasons)
	}
}

func TestPreProdActionsReceipt_WrongSHAAndSplitJobsFail(t *testing.T) {
	sha := strings.Repeat("a", 40)
	r := artifactqual.PreProdActionsReceipt{
		Schema:     artifactqual.SchemaPreProdActionsReceipt,
		Repository: "jasonhnd/loopcoder", WorkflowPath: artifactqual.PreProdIntegrationWorkflow,
		RunID: 1, Attempt: 1, Event: "push", HeadBranch: "pre-prod", HeadSHA: strings.Repeat("b", 40),
		Status: "completed", Conclusion: "success",
		Jobs: []artifactqual.PreProdActionsJob{
			{Name: "integration-verify", Status: "completed", Conclusion: "success"},
			// canary missing
		},
	}
	vOK, cOK, reasons := artifactqual.ValidatePreProdActionsReceipt(r, sha, "jasonhnd/loopcoder")
	if vOK || cOK {
		t.Fatal("want fail closed")
	}
	joined := strings.Join(reasons, ";")
	if !strings.Contains(joined, "head_sha") && !strings.Contains(joined, "job_missing") {
		t.Fatalf("reasons=%v", reasons)
	}
}

func TestPreProdActionsReceipt_WrongWorkflowFail(t *testing.T) {
	sha := strings.Repeat("a", 40)
	r := artifactqual.PreProdActionsReceipt{
		Schema:     artifactqual.SchemaPreProdActionsReceipt,
		Repository: "jasonhnd/loopcoder", WorkflowPath: ".github/workflows/other.yml",
		RunID: 1, Attempt: 1, Event: "push", HeadBranch: "pre-prod", HeadSHA: sha,
		Status: "completed", Conclusion: "success",
		Jobs: []artifactqual.PreProdActionsJob{
			{Name: "integration-verify", Status: "completed", Conclusion: "success"},
			{Name: "integration-canary", Status: "completed", Conclusion: "success"},
		},
	}
	_, _, reasons := artifactqual.ValidatePreProdActionsReceipt(r, sha, "jasonhnd/loopcoder")
	if !strings.Contains(strings.Join(reasons, ";"), "workflow_path") {
		t.Fatalf("%v", reasons)
	}
}

func TestPreProdActionsReceipt_SchemaBranchEventRepoMandatory(t *testing.T) {
	sha := strings.Repeat("a", 40)
	base := artifactqual.PreProdActionsReceipt{
		Schema:     artifactqual.SchemaPreProdActionsReceipt,
		Repository: "jasonhnd/loopcoder", WorkflowPath: artifactqual.PreProdIntegrationWorkflow,
		RunID: 1, Attempt: 1, Event: "push", HeadBranch: "pre-prod", HeadSHA: sha,
		Status: "completed", Conclusion: "success",
		Jobs: []artifactqual.PreProdActionsJob{
			{Name: "integration-verify", Status: "completed", Conclusion: "success"},
			{Name: "integration-canary", Status: "completed", Conclusion: "success"},
		},
	}
	// Empty schema fails (mandatory exact).
	r := base
	r.Schema = ""
	if v, c, reasons := artifactqual.ValidatePreProdActionsReceipt(r, sha, "jasonhnd/loopcoder"); v || c {
		t.Fatal("empty schema must fail")
	} else if !strings.Contains(strings.Join(reasons, ";"), "schema") {
		t.Fatalf("want schema reason: %v", reasons)
	}
	// Wrong branch fails.
	r = base
	r.HeadBranch = "main"
	if v, c, reasons := artifactqual.ValidatePreProdActionsReceipt(r, sha, "jasonhnd/loopcoder"); v || c {
		t.Fatal("wrong branch must fail")
	} else if !strings.Contains(strings.Join(reasons, ";"), "head_branch") {
		t.Fatalf("want head_branch reason: %v", reasons)
	}
	// Wrong event fails.
	r = base
	r.Event = "workflow_dispatch"
	if v, c, reasons := artifactqual.ValidatePreProdActionsReceipt(r, sha, "jasonhnd/loopcoder"); v || c {
		t.Fatal("wrong event must fail")
	} else if !strings.Contains(strings.Join(reasons, ";"), "event") {
		t.Fatalf("want event reason: %v", reasons)
	}
	// Empty repository fails.
	r = base
	r.Repository = ""
	if v, c, reasons := artifactqual.ValidatePreProdActionsReceipt(r, sha, "jasonhnd/loopcoder"); v || c {
		t.Fatal("empty repository must fail")
	} else if !strings.Contains(strings.Join(reasons, ";"), "repository") {
		t.Fatalf("want repository reason: %v", reasons)
	}
	// Repo mismatch fails.
	r = base
	if v, c, reasons := artifactqual.ValidatePreProdActionsReceipt(r, sha, "other/repo"); v || c {
		t.Fatal("repo mismatch must fail")
	} else if !strings.Contains(strings.Join(reasons, ";"), "repository") {
		t.Fatalf("want repository reason: %v", reasons)
	}
}

func TestRCProvenance_LocalArchiveWithoutActionsFail(t *testing.T) {
	dir := t.TempDir()
	archName := "loopcoder_0.9.0_darwin_arm64.tar.gz"
	archPath := filepath.Join(dir, archName)
	payload := []byte("fake-archive-bytes")
	if err := os.WriteFile(archPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	dig := hex.EncodeToString(sum[:])
	_ = os.MkdirAll(filepath.Join(dir, "evidence"), 0o700)
	man := artifactqual.RCManifest{
		Schema: artifactqual.SchemaRCManifest, Version: "0.9.0",
		CommitSHA: strings.Repeat("c", 40), BuildSource: "release-candidate",
		Archive: archName, ArchiveDigestSHA256: dig, PublicRelease: false, DraftOnly: true,
	}
	b, _ := json.Marshal(man)
	_ = os.WriteFile(filepath.Join(dir, "evidence", "rc-manifest.json"), b, 0o600)
	_ = os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(dig+"  "+archName+"\n"), 0o600)
	p, err := artifactqual.LoadRCProvenanceForArchive(archPath)
	if err != nil {
		t.Fatal(err)
	}
	ok, reasons := artifactqual.ValidateRCProvenance(p, man.CommitSHA, dig, man.CommitSHA, "jasonhnd/loopcoder", nil)
	if ok {
		t.Fatal("local archive without actions binding must fail")
	}
	if !strings.Contains(strings.Join(reasons, ";"), "rc_actions_binding_missing") {
		t.Fatalf("%v", reasons)
	}
}

func TestRCProvenance_WithBindingOK(t *testing.T) {
	dir := t.TempDir()
	archName := "loopcoder_0.9.0_darwin_arm64.tar.gz"
	archPath := filepath.Join(dir, archName)
	payload := []byte("rc-ok-bytes")
	_ = os.WriteFile(archPath, payload, 0o600)
	sum := sha256.Sum256(payload)
	dig := hex.EncodeToString(sum[:])
	sha := strings.Repeat("d", 40)
	repo := "jasonhnd/loopcoder"
	_ = os.MkdirAll(filepath.Join(dir, "evidence"), 0o700)
	man := artifactqual.RCManifest{
		Schema: artifactqual.SchemaRCManifest, Version: "0.9.0",
		CommitSHA: sha, BuildSource: "release-candidate",
		Archive: archName, ArchiveDigestSHA256: dig, PublicRelease: false, DraftOnly: true,
		ActionsRunID: 7, ActionsArtifactID: 8,
	}
	b, _ := json.Marshal(man)
	_ = os.WriteFile(filepath.Join(dir, "evidence", "rc-manifest.json"), b, 0o600)
	_ = os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(dig+"  "+archName+"\n"), 0o600)
	p, err := artifactqual.LoadRCProvenanceForArchive(archPath)
	if err != nil {
		t.Fatal(err)
	}
	bind := &artifactqual.RCActionsBinding{
		Repository: repo, WorkflowName: artifactqual.ReleaseCandidateDraftWorkflow,
		RunID: 7, RunAttempt: 1, ArtifactID: 8, ArtifactName: artifactqual.V090RCArtifactName,
		HeadSHA: sha, Status: "completed", Conclusion: "success", ArtifactExpired: false,
	}
	ok, reasons := artifactqual.ValidateRCProvenance(p, sha, dig, sha, repo, bind)
	if !ok {
		t.Fatalf("want ok: %v", reasons)
	}
}

func TestRCProvenance_BinaryCommitRequired(t *testing.T) {
	dir := t.TempDir()
	archName := "loopcoder_0.9.0_darwin_arm64.tar.gz"
	archPath := filepath.Join(dir, archName)
	payload := []byte("rc-bin-commit-bytes")
	_ = os.WriteFile(archPath, payload, 0o600)
	sum := sha256.Sum256(payload)
	dig := hex.EncodeToString(sum[:])
	sha := strings.Repeat("f", 40)
	repo := "jasonhnd/loopcoder"
	_ = os.MkdirAll(filepath.Join(dir, "evidence"), 0o700)
	man := artifactqual.RCManifest{
		Schema: artifactqual.SchemaRCManifest, Version: "0.9.0",
		CommitSHA: sha, BuildSource: "release-candidate",
		Archive: archName, ArchiveDigestSHA256: dig, PublicRelease: false, DraftOnly: true,
	}
	b, _ := json.Marshal(man)
	_ = os.WriteFile(filepath.Join(dir, "evidence", "rc-manifest.json"), b, 0o600)
	_ = os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(dig+"  "+archName+"\n"), 0o600)
	p, err := artifactqual.LoadRCProvenanceForArchive(archPath)
	if err != nil {
		t.Fatal(err)
	}
	bind := &artifactqual.RCActionsBinding{
		Repository: repo, WorkflowName: artifactqual.ReleaseCandidateDraftWorkflow,
		RunID: 7, RunAttempt: 1, ArtifactID: 8, ArtifactName: artifactqual.V090RCArtifactName,
		HeadSHA: sha, Status: "completed", Conclusion: "success",
	}
	// Empty binary commit fails (no skip / no SHA fallback).
	ok, reasons := artifactqual.ValidateRCProvenance(p, sha, dig, "", repo, bind)
	if ok || !strings.Contains(strings.Join(reasons, ";"), "rc_binary_commit_missing_or_invalid") {
		t.Fatalf("empty binary commit: ok=%v reasons=%v", ok, reasons)
	}
	// Short commit fails.
	ok, reasons = artifactqual.ValidateRCProvenance(p, sha, dig, "abc1234", repo, bind)
	if ok || !strings.Contains(strings.Join(reasons, ";"), "rc_binary_commit_missing_or_invalid") {
		t.Fatalf("short binary commit: ok=%v reasons=%v", ok, reasons)
	}
	// Valid length but wrong SHA fails mismatch.
	other := strings.Repeat("1", 40)
	ok, reasons = artifactqual.ValidateRCProvenance(p, sha, dig, other, repo, bind)
	if ok || !strings.Contains(strings.Join(reasons, ";"), "rc_binary_commit_mismatch") {
		t.Fatalf("mismatch binary commit: ok=%v reasons=%v", ok, reasons)
	}
	// Exact match still green.
	ok, reasons = artifactqual.ValidateRCProvenance(p, sha, dig, sha, repo, bind)
	if !ok {
		t.Fatalf("want ok: %v", reasons)
	}
}

func TestRCProvenance_BindingFailures(t *testing.T) {
	dir := t.TempDir()
	archName := "loopcoder_0.9.0_darwin_arm64.tar.gz"
	archPath := filepath.Join(dir, archName)
	payload := []byte("rc-fail-bytes")
	_ = os.WriteFile(archPath, payload, 0o600)
	sum := sha256.Sum256(payload)
	dig := hex.EncodeToString(sum[:])
	sha := strings.Repeat("e", 40)
	repo := "jasonhnd/loopcoder"
	_ = os.MkdirAll(filepath.Join(dir, "evidence"), 0o700)
	man := artifactqual.RCManifest{
		Schema: artifactqual.SchemaRCManifest, Version: "0.9.0",
		CommitSHA: sha, BuildSource: "release-candidate",
		Archive: archName, ArchiveDigestSHA256: dig, PublicRelease: false, DraftOnly: true,
	}
	b, _ := json.Marshal(man)
	_ = os.WriteFile(filepath.Join(dir, "evidence", "rc-manifest.json"), b, 0o600)
	_ = os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(dig+"  "+archName+"\n"), 0o600)
	p, err := artifactqual.LoadRCProvenanceForArchive(archPath)
	if err != nil {
		t.Fatal(err)
	}
	good := artifactqual.RCActionsBinding{
		Repository: repo, WorkflowName: artifactqual.ReleaseCandidateDraftWorkflow,
		RunID: 7, RunAttempt: 1, ArtifactID: 8, ArtifactName: artifactqual.V090RCArtifactName,
		HeadSHA: sha, Status: "completed", Conclusion: "success",
	}
	cases := []struct {
		name    string
		repo    string
		mut     func(*artifactqual.RCActionsBinding)
		wantSub string
	}{
		{"empty_expect_repo", "", nil, "rc_actions_expect_repository_missing"},
		{"wrong_repo", repo, func(b *artifactqual.RCActionsBinding) { b.Repository = "other/repo" }, "rc_actions_repository_mismatch"},
		{"empty_bind_repo", repo, func(b *artifactqual.RCActionsBinding) { b.Repository = "" }, "rc_actions_repository_missing"},
		{"zero_attempt", repo, func(b *artifactqual.RCActionsBinding) { b.RunAttempt = 0 }, "rc_actions_run_attempt_missing"},
		{"wrong_artifact_name", repo, func(b *artifactqual.RCActionsBinding) { b.ArtifactName = "wrong-name" }, "rc_actions_artifact_name_mismatch"},
		{"expired", repo, func(b *artifactqual.RCActionsBinding) { b.ArtifactExpired = true }, "rc_actions_artifact_expired"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bind := good
			if tc.mut != nil {
				tc.mut(&bind)
			}
			ok, reasons := artifactqual.ValidateRCProvenance(p, sha, dig, sha, tc.repo, &bind)
			if ok {
				t.Fatal("want fail")
			}
			if !strings.Contains(strings.Join(reasons, ";"), tc.wantSub) {
				t.Fatalf("want %q in %v", tc.wantSub, reasons)
			}
		})
	}
}

func TestPRLive_DisposableHeadOKAndFailClosed(t *testing.T) {
	rcSHA := strings.Repeat("a", 40)
	prHead := strings.Repeat("b", 40) // disposable canary PR head ≠ RC SHA
	if prHead == rcSHA {
		t.Fatal("test setup requires distinct heads")
	}
	pr := artifactqual.CanaryPR{
		URL: "https://github.com/o/r/pull/1", Number: 1, Repository: "o/r",
		BaseRef: "main", HeadOID: prHead, CreatedByLoopCoder: true,
		IndependentVerifier: "codex", VerifierProvider: "codex",
		VerifierEvidenceRef: "sha256:verify@head:" + prHead, VerifierAttemptID: "att-v-1",
		AutoMerge: false, HumanMergeGate: true,
		RequiredChecks: []string{"verify", "test"}, RequiredChecksGreen: true,
	}
	liveOK := &artifactqual.PRLiveState{
		Repository: "o/r", Number: 1, URL: pr.URL, BaseRef: "main",
		HeadOID: prHead, State: "open", AutoMergeEnabled: false,
		RequiredChecksGreen: true, HumanMergeGate: true,
		ChecksAtHead: []artifactqual.PRCheck{
			{Name: "verify", Status: "completed", Conclusion: "success"},
			{Name: "test", Status: "completed", Conclusion: "success"},
		},
	}
	// Pass when expected = disposable head (not RC SHA).
	ok, reasons := artifactqual.ValidatePRLive(pr, liveOK, prHead)
	if !ok {
		t.Fatalf("want pass with disposable head: %v", reasons)
	}
	// Empty expected fails.
	ok, reasons = artifactqual.ValidatePRLive(pr, liveOK, "")
	if ok || !strings.Contains(strings.Join(reasons, ";"), "pr_expected_head_oid_invalid") {
		t.Fatalf("empty expected: ok=%v reasons=%v", ok, reasons)
	}
	// RC SHA as expected while head is disposable fails.
	ok, reasons = artifactqual.ValidatePRLive(pr, liveOK, rcSHA)
	if ok {
		t.Fatalf("RC SHA as expected must fail when head is disposable: %v", reasons)
	}

	// Stale live head.
	stale := *liveOK
	stale.HeadOID = strings.Repeat("c", 40)
	ok, reasons = artifactqual.ValidatePRLive(pr, &stale, prHead)
	if ok || !strings.Contains(strings.Join(reasons, ";"), "pr_live_head") {
		t.Fatalf("stale head: %v", reasons)
	}

	// Repo / base / url / state / human gate mismatches.
	cases := []struct {
		name string
		mut  func(*artifactqual.PRLiveState)
		sub  string
	}{
		{"repo", func(l *artifactqual.PRLiveState) { l.Repository = "other/r" }, "pr_live_repository_mismatch"},
		{"base", func(l *artifactqual.PRLiveState) { l.BaseRef = "develop" }, "pr_live_base_ref_mismatch"},
		{"url", func(l *artifactqual.PRLiveState) { l.URL = "https://github.com/o/r/pull/2" }, "pr_live_url_mismatch"},
		{"state_empty", func(l *artifactqual.PRLiveState) { l.State = "" }, "pr_live_not_open"},
		{"state_closed", func(l *artifactqual.PRLiveState) { l.State = "closed" }, "pr_live_not_open"},
		{"human_gate", func(l *artifactqual.PRLiveState) { l.HumanMergeGate = false }, "pr_live_not_human_gate"},
		{"auto_merge", func(l *artifactqual.PRLiveState) { l.AutoMergeEnabled = true }, "pr_live_auto_merge_enabled"},
		{"checks_green_flag", func(l *artifactqual.PRLiveState) { l.RequiredChecksGreen = false }, "pr_live_required_checks_not_green"},
		{"checks_empty", func(l *artifactqual.PRLiveState) { l.ChecksAtHead = nil }, "pr_live_checks_at_head_missing"},
		{"check_missing", func(l *artifactqual.PRLiveState) {
			l.ChecksAtHead = []artifactqual.PRCheck{{Name: "verify", Status: "completed", Conclusion: "success"}}
		}, "pr_live_required_check_missing"},
		{"check_not_green", func(l *artifactqual.PRLiveState) {
			l.ChecksAtHead = []artifactqual.PRCheck{
				{Name: "verify", Status: "completed", Conclusion: "success"},
				{Name: "test", Status: "completed", Conclusion: "failure"},
			}
		}, "pr_live_required_check_not_green"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := *liveOK
			// deep-copy checks slice
			l.ChecksAtHead = append([]artifactqual.PRCheck(nil), liveOK.ChecksAtHead...)
			tc.mut(&l)
			ok, reasons := artifactqual.ValidatePRLive(pr, &l, prHead)
			if ok || !strings.Contains(strings.Join(reasons, ";"), tc.sub) {
				t.Fatalf("want %q in %v", tc.sub, reasons)
			}
		})
	}

	// Manifest: human gate false / empty required checks.
	badPR := pr
	badPR.HumanMergeGate = false
	ok, reasons = artifactqual.ValidatePRLive(badPR, liveOK, prHead)
	if ok || !strings.Contains(strings.Join(reasons, ";"), "pr_human_merge_gate_false") {
		t.Fatalf("manifest human gate: %v", reasons)
	}
	badPR = pr
	badPR.RequiredChecks = nil
	ok, reasons = artifactqual.ValidatePRLive(badPR, liveOK, prHead)
	if ok || !strings.Contains(strings.Join(reasons, ";"), "pr_required_checks_missing") {
		t.Fatalf("manifest required checks: %v", reasons)
	}

	// Prose verifier without wi_verify child (independent of live PR).
	prose := pr
	prose.VerifierEvidenceRef = "pending-live"
	prose.VerifierAttemptID = ""
	vok, vreasons := artifactqual.ValidateIndependentVerifierFromChildren(prose, nil)
	if vok || len(vreasons) == 0 {
		t.Fatal("prose verifier must fail")
	}
}

func TestIndependentVerifier_StructuredBinding(t *testing.T) {
	head := strings.Repeat("d", 40)
	after := 0.85
	verify := child("wi_verify", "att-v-1", "codex", "gpt-5.5", "high", "succeeded", 0.96, 0.05, after)
	implement := child("wi_implement", "att-i-1", "antigravity", "GPT-OSS", "medium", "succeeded", 0.9, 0.05, after)
	pr := *validPR(head)
	pr.VerifierAttemptID = verify.AttemptID
	pr.VerifierProvider = verify.Provider
	pr.IndependentVerifier = verify.Provider
	pr.VerifierEvidenceRef = verify.OutputEvidence + "@head:" + head

	ok, reasons := artifactqual.ValidateIndependentVerifierFromChildren(pr, []artifactqual.CanaryChild{implement, verify})
	if !ok {
		t.Fatalf("want pass: %v", reasons)
	}

	// Arbitrary sha256 PR evidence fails (not OutputEvidence@head).
	bad := pr
	bad.VerifierEvidenceRef = "sha256:" + strings.Repeat("a", 64) + "@head:" + head
	ok, reasons = artifactqual.ValidateIndependentVerifierFromChildren(bad, []artifactqual.CanaryChild{implement, verify})
	if ok || !strings.Contains(strings.Join(reasons, ";"), "pr_verifier_evidence_mismatch") {
		t.Fatalf("arbitrary sha256: ok=%v reasons=%v", ok, reasons)
	}

	// ChildID contains verify but WorkItemID is not exact wi_verify.
	fakeVerify := verify
	fakeVerify.ChildID = "child_verify_role"
	fakeVerify.WorkItemID = "wi_other"
	ok, reasons = artifactqual.ValidateIndependentVerifierFromChildren(pr, []artifactqual.CanaryChild{implement, fakeVerify})
	if ok || !strings.Contains(strings.Join(reasons, ";"), "verifier_no_succeeded_wi_verify_child") {
		t.Fatalf("substring ChildID: ok=%v reasons=%v", ok, reasons)
	}

	// Wrong verify class.
	wrongClass := verify
	wrongClass.TaskClass = "tera"
	ok, reasons = artifactqual.ValidateIndependentVerifierFromChildren(pr, []artifactqual.CanaryChild{implement, wrongClass})
	if ok || !strings.Contains(strings.Join(reasons, ";"), "verifier_wi_verify_task_class_not_soul") {
		t.Fatalf("wrong class: ok=%v reasons=%v", ok, reasons)
	}

	// Missing output evidence.
	noEvid := verify
	noEvid.OutputEvidence = ""
	ok, reasons = artifactqual.ValidateIndependentVerifierFromChildren(pr, []artifactqual.CanaryChild{implement, noEvid})
	if ok || !strings.Contains(strings.Join(reasons, ";"), "verifier_output_evidence_invalid") {
		t.Fatalf("missing evidence: ok=%v reasons=%v", ok, reasons)
	}

	// Same implement/verifier provider.
	same := implement
	same.Provider = "codex"
	ok, reasons = artifactqual.ValidateIndependentVerifierFromChildren(pr, []artifactqual.CanaryChild{same, verify})
	if ok || !strings.Contains(strings.Join(reasons, ";"), "verifier_implement_provider_same") {
		t.Fatalf("same provider: ok=%v reasons=%v", ok, reasons)
	}
}

func TestQualify_ForgedBooleansWithoutReceiptNO_GO(t *testing.T) {
	// Nil receipt → dual-green false (caller booleans alone cannot qualify).
	v, c, reasons := artifactqual.PreProdDualGreenFromReceipt(nil, "sha", "repo")
	if v || c {
		t.Fatal("nil receipt must not dual-green")
	}
	if len(reasons) == 0 {
		t.Fatal("want missing receipt reason")
	}
}

func TestValidateCanary_RealPRRequiresLiveAndVerifyChild(t *testing.T) {
	now := time.Date(2026, 7, 22, 21, 0, 0, 0, time.UTC)
	digest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	rcSHA := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"  // pre-prod / RC
	prHead := "cccccccccccccccccccccccccccccccccccccccc" // disposable PR head ≠ RC
	rem := 0.9
	after := 0.85
	ev := artifactqual.CanaryEvidence{
		Schema: artifactqual.SchemaCanaryEvidence, ArchiveDigest: digest, PreProdSHA: rcSHA,
		BinaryVersion: "0.9.0", BinaryCommit: rcSHA, ProjectID: "disp-x", RunID: "run-x",
		ProducedAt: now,
		ProviderObservations: []artifactqual.CanaryProviderObs{
			{Provider: "codex", AccountRef: "acct-codex", InstallRef: "pinst_codex", WindowKind: "five_hour", Source: "codexbar", Freshness: "fresh", Confidence: "exact", Remaining: &rem, CapturedAt: now.Add(-time.Minute), ResetAt: ptrTimeCanary(now.Add(2 * time.Hour))},
			{Provider: "antigravity", AccountRef: "acct-antigravity", InstallRef: "pinst_antigravity", WindowKind: "five_hour", Source: "codexbar", Freshness: "fresh", Confidence: "exact", Remaining: &rem, CapturedAt: now.Add(-time.Minute), ResetAt: ptrTimeCanary(now.Add(2 * time.Hour))},
		},
		Children: []artifactqual.CanaryChild{
			child("wi_research", "att-r-1", "codex", "gpt-5.5", "low", "succeeded", 0.96, 0.05, after),
			child("wi_implement", "att-i-1", "antigravity", "GPT-OSS", "medium", "succeeded", 0.9, 0.05, after),
			child("wi_tests", "att-t-1", "antigravity", "GPT-OSS", "medium", "succeeded", 0.9, 0.05, after),
			child("wi_verify", "att-v-1", "codex", "gpt-5.5", "high", "succeeded", 0.96, 0.05, after),
		},
		UnavailableRetry: &artifactqual.CanaryUnavailableRetry{
			ExcludedProvider: "claude", ExcludedReason: "unavailable",
			NoDuplicateClaim: true, NoDuplicateFiles: true, NoDoubleCapacity: true,
			EvidenceRef: "events:x",
		},
		Restart: validRestart(4),
		PR:      validPR(prHead),
	}
	// Bind verifier to wi_verify OutputEvidence at disposable head.
	for i := range ev.Children {
		if ev.Children[i].WorkItemID == "wi_verify" {
			ev.PR.VerifierAttemptID = ev.Children[i].AttemptID
			ev.PR.VerifierProvider = ev.Children[i].Provider
			ev.PR.IndependentVerifier = ev.Children[i].Provider
			ev.PR.VerifierEvidenceRef = ev.Children[i].OutputEvidence + "@head:" + prHead
		}
	}
	ev.ContentDigest = artifactqual.DigestCanaryBody(ev)

	// Without LivePR → RealPROK false.
	v1 := artifactqual.ValidateCanaryEvidence(ev, digest, rcSHA, now)
	if v1.RealPROK {
		t.Fatal("without live PR must not RealPROK")
	}
	// Empty expected head fails RealPROK even with live.
	live := &artifactqual.PRLiveState{
		Repository: "jasonhnd/loopcoder", Number: 9999,
		URL: ev.PR.URL, BaseRef: "main", HeadOID: prHead, State: "open",
		RequiredChecksGreen: true, HumanMergeGate: true,
		ChecksAtHead: []artifactqual.PRCheck{
			{Name: "verify", Status: "completed", Conclusion: "success"},
			{Name: "test", Status: "completed", Conclusion: "success"},
		},
	}
	vEmpty := artifactqual.ValidateCanaryEvidence(ev, digest, rcSHA, now, artifactqual.CanaryValidateOpts{
		ExpectedPRHeadOID: "", LivePR: live,
	})
	if vEmpty.RealPROK {
		t.Fatal("empty expected PR head must not RealPROK")
	}
	// RC SHA as expected (≠ disposable head) fails.
	vRC := artifactqual.ValidateCanaryEvidence(ev, digest, rcSHA, now, artifactqual.CanaryValidateOpts{
		ExpectedPRHeadOID: rcSHA, LivePR: live,
	})
	if vRC.RealPROK {
		t.Fatal("RC SHA expected against disposable head must not RealPROK")
	}
	// Expected = disposable head → RealPROK with live+verify child.
	v2 := artifactqual.ValidateCanaryEvidence(ev, digest, rcSHA, now, artifactqual.CanaryValidateOpts{
		ExpectedPRHeadOID: prHead, LivePR: live,
	})
	if !v2.RealPROK {
		t.Fatalf("want RealPROK with live+verify child: %v", v2.Reasons)
	}
}
