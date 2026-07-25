package artifactqual

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

type fakePreProdVerifier struct {
	receipt PreProdActionsReceipt
	err     error
	// capture last fetch identity
	lastRepo    string
	lastRunID   int64
	lastAttempt int
	calls       int
}

func (f *fakePreProdVerifier) FetchRun(ctx context.Context, repository string, runID int64, attempt int) (PreProdActionsReceipt, error) {
	f.calls++
	f.lastRepo = repository
	f.lastRunID = runID
	f.lastAttempt = attempt
	if f.err != nil {
		return PreProdActionsReceipt{}, f.err
	}
	return f.receipt, nil
}

type fakeRCVerifier struct {
	binding  RCActionsBinding
	err      error
	lastRepo string
	lastRun  int64
	lastArt  int64
	calls    int
}

func (f *fakeRCVerifier) FetchRCBinding(ctx context.Context, repository string, runID, artifactID int64) (RCActionsBinding, error) {
	f.calls++
	f.lastRepo = repository
	f.lastRun = runID
	f.lastArt = artifactID
	if f.err != nil {
		return RCActionsBinding{}, f.err
	}
	return f.binding, nil
}

func TestResolveReleaseActions_ModeReleaseIgnoresForgedCaller(t *testing.T) {
	sha := strings.Repeat("a", 40)
	repo := "jasonhnd/loopcoder"
	forgedReceipt := &PreProdActionsReceipt{
		Schema: SchemaPreProdActionsReceipt, Repository: repo,
		WorkflowPath: PreProdIntegrationWorkflow, RunID: 1, Attempt: 1,
		Event: "push", HeadBranch: PreProdHeadBranch, HeadSHA: sha,
		Status: "completed", Conclusion: "success",
		Jobs: []PreProdActionsJob{
			{Name: JobIntegrationVerify, Status: "completed", Conclusion: "success"},
			{Name: JobIntegrationCanary, Status: "completed", Conclusion: "success"},
		},
	}
	forgedBind := &RCActionsBinding{
		Repository: repo, WorkflowName: ReleaseCandidateDraftWorkflow,
		RunID: 1, RunAttempt: 1, ArtifactID: 2, ArtifactName: V090RCArtifactName,
		HeadSHA: sha, Status: "completed", Conclusion: "success",
	}
	fetchedReceipt := PreProdActionsReceipt{
		Schema: SchemaPreProdActionsReceipt, Repository: repo,
		WorkflowPath: PreProdIntegrationWorkflow, RunID: 99, Attempt: 2,
		Event: "push", HeadBranch: PreProdHeadBranch, HeadSHA: sha,
		Status: "completed", Conclusion: "success",
		Jobs: []PreProdActionsJob{
			{Name: JobIntegrationVerify, Status: "completed", Conclusion: "success"},
			{Name: JobIntegrationCanary, Status: "completed", Conclusion: "success"},
		},
	}
	fetchedBind := RCActionsBinding{
		Repository: repo, WorkflowName: ReleaseCandidateDraftWorkflow,
		RunID: 77, RunAttempt: 1, ArtifactID: 88, ArtifactName: V090RCArtifactName,
		HeadSHA: sha, Status: "completed", Conclusion: "success",
	}
	piv := &fakePreProdVerifier{receipt: fetchedReceipt}
	rcv := &fakeRCVerifier{binding: fetchedBind}
	got := resolveReleaseActions(context.Background(), Input{
		Mode: ModeRelease, Repository: repo,
		IntegrationReceipt: forgedReceipt, RCActionsBinding: forgedBind,
		IntegrationVerifier: piv, IntegrationRunID: 99, IntegrationRunAttempt: 2,
		RCActionsVerifier: rcv, RCRunID: 77, RCArtifactID: 88,
	})
	if len(got.Reasons) != 0 {
		t.Fatalf("reasons=%v", got.Reasons)
	}
	if got.IntegrationReceipt == nil || got.IntegrationReceipt.RunID != 99 {
		t.Fatalf("want fetched run 99, got %+v", got.IntegrationReceipt)
	}
	if got.RCBinding == nil || got.RCBinding.RunID != 77 || got.RCBinding.ArtifactID != 88 {
		t.Fatalf("want fetched RC 77/88, got %+v", got.RCBinding)
	}
	// Forged pointers must not be returned as-is.
	if got.IntegrationReceipt == forgedReceipt {
		t.Fatal("returned forged receipt pointer")
	}
	if got.RCBinding == forgedBind {
		t.Fatal("returned forged binding pointer")
	}
	if piv.calls != 1 || piv.lastRepo != repo || piv.lastRunID != 99 || piv.lastAttempt != 2 {
		t.Fatalf("preprod fetch identity: calls=%d repo=%s run=%d att=%d", piv.calls, piv.lastRepo, piv.lastRunID, piv.lastAttempt)
	}
	if rcv.calls != 1 || rcv.lastRepo != repo || rcv.lastRun != 77 || rcv.lastArt != 88 {
		t.Fatalf("rc fetch identity: calls=%d repo=%s run=%d art=%d", rcv.calls, rcv.lastRepo, rcv.lastRun, rcv.lastArt)
	}
}

func TestResolveReleaseActions_ModeReleaseMissingAndFetchErrors(t *testing.T) {
	secret := "SUPER_SECRET_TOKEN_VALUE_xyz"
	repo := "jasonhnd/loopcoder"

	// Empty repository.
	got := resolveReleaseActions(context.Background(), Input{
		Mode: ModeRelease, Repository: "  ",
		IntegrationVerifier: &fakePreProdVerifier{}, IntegrationRunID: 1, IntegrationRunAttempt: 1,
		RCActionsVerifier: &fakeRCVerifier{}, RCRunID: 1, RCArtifactID: 1,
	})
	if !hasReason(got.Reasons, "release_actions_repository_missing") {
		t.Fatalf("want repository missing: %v", got.Reasons)
	}
	if got.IntegrationReceipt != nil || got.RCBinding != nil {
		t.Fatal("no fetch without repository")
	}

	// Nil verifiers + zero IDs.
	got = resolveReleaseActions(context.Background(), Input{
		Mode: ModeRelease, Repository: repo,
	})
	for _, want := range []string{
		"preprod_actions_verifier_missing",
		"rc_actions_verifier_missing",
	} {
		if !hasReason(got.Reasons, want) {
			t.Fatalf("want %s in %v", want, got.Reasons)
		}
	}

	// Identity missing (verifiers present, bad IDs).
	got = resolveReleaseActions(context.Background(), Input{
		Mode: ModeRelease, Repository: repo,
		IntegrationVerifier: &fakePreProdVerifier{}, IntegrationRunID: 0, IntegrationRunAttempt: 0,
		RCActionsVerifier: &fakeRCVerifier{}, RCRunID: 0, RCArtifactID: 0,
	})
	if !hasReason(got.Reasons, "preprod_actions_identity_missing") || !hasReason(got.Reasons, "rc_actions_identity_missing") {
		t.Fatalf("want identity missing: %v", got.Reasons)
	}

	// Fetch errors — reason only, no secret leakage.
	piv := &fakePreProdVerifier{err: fmt.Errorf("gh failed %s", secret)}
	rcv := &fakeRCVerifier{err: fmt.Errorf("gh failed %s", secret)}
	got = resolveReleaseActions(context.Background(), Input{
		Mode: ModeRelease, Repository: repo,
		IntegrationVerifier: piv, IntegrationRunID: 1, IntegrationRunAttempt: 1,
		RCActionsVerifier: rcv, RCRunID: 2, RCArtifactID: 3,
	})
	if !hasReason(got.Reasons, "preprod_actions_fetch_failed") || !hasReason(got.Reasons, "rc_actions_fetch_failed") {
		t.Fatalf("want fetch failed: %v", got.Reasons)
	}
	joined := strings.Join(got.Reasons, ";")
	if strings.Contains(joined, secret) {
		t.Fatalf("reasons leaked secret: %v", got.Reasons)
	}
	if got.IntegrationReceipt != nil || got.RCBinding != nil {
		t.Fatal("failed fetch must not set records")
	}
}

func TestResolveReleaseActions_ModeUnitPreservesInjected(t *testing.T) {
	r := &PreProdActionsReceipt{RunID: 42, Attempt: 1}
	b := &RCActionsBinding{RunID: 7, ArtifactID: 8}
	// Verifiers present but ModeUnit must not call them / must keep injected.
	piv := &fakePreProdVerifier{receipt: PreProdActionsReceipt{RunID: 999}}
	rcv := &fakeRCVerifier{binding: RCActionsBinding{RunID: 888}}
	got := resolveReleaseActions(context.Background(), Input{
		Mode: ModeUnit, Repository: "jasonhnd/loopcoder",
		IntegrationReceipt: r, RCActionsBinding: b,
		IntegrationVerifier: piv, IntegrationRunID: 1, IntegrationRunAttempt: 1,
		RCActionsVerifier: rcv, RCRunID: 1, RCArtifactID: 1,
	})
	if got.IntegrationReceipt != r || got.RCBinding != b {
		t.Fatalf("ModeUnit must preserve injected pointers")
	}
	if piv.calls != 0 || rcv.calls != 0 {
		t.Fatal("ModeUnit must not fetch")
	}
	if len(got.Reasons) != 0 {
		t.Fatalf("reasons=%v", got.Reasons)
	}
}

func hasReason(reasons []string, id string) bool {
	for _, r := range reasons {
		if r == id {
			return true
		}
	}
	return false
}
