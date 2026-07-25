package artifactqual_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/artifactqual"
)

// fakeGHRunner records argv and returns scripted responses (no network).
type fakeGHRunner struct {
	// responses keyed by joined args after "api "
	byPath map[string][]byte
	// errByPath optional errors
	errByPath map[string]error
	calls     [][]string
	// secret body used only to prove it never appears in error strings
	secretBody string
}

func (f *fakeGHRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	cp := append([]string(nil), args...)
	f.calls = append(f.calls, cp)
	if len(args) < 2 || args[0] != "api" {
		return nil, fmt.Errorf("unexpected argv")
	}
	path := args[1]
	if f.errByPath != nil {
		if err, ok := f.errByPath[path]; ok {
			return nil, err
		}
	}
	if f.byPath != nil {
		if b, ok := f.byPath[path]; ok {
			return b, nil
		}
	}
	if f.secretBody != "" {
		return []byte(f.secretBody), fmt.Errorf("gh exit=1")
	}
	return nil, fmt.Errorf("no scripted response")
}

func TestGitHubEvidenceVerifier_FetchRun_HappyParseAndArgs(t *testing.T) {
	sha := strings.Repeat("a", 40)
	repo := "jasonhnd/loopcoder"
	runID := int64(99)
	attempt := 2
	runPath := fmt.Sprintf("repos/%s/actions/runs/%d", repo, runID)
	jobsPath := fmt.Sprintf("repos/%s/actions/runs/%d/attempts/%d/jobs?per_page=100", repo, runID, attempt)
	runJSON := fmt.Sprintf(`{
		"id": %d,
		"run_attempt": %d,
		"path": %q,
		"event": "push",
		"head_branch": "pre-prod",
		"head_sha": %q,
		"status": "completed",
		"conclusion": "success",
		"repository": {"full_name": %q}
	}`, runID, attempt, artifactqual.PreProdIntegrationWorkflow, sha, repo)
	jobsJSON := `{
		"jobs": [
			{"name": "integration-verify", "status": "completed", "conclusion": "success"},
			{"name": "integration-canary", "status": "completed", "conclusion": "success"}
		]
	}`
	fake := &fakeGHRunner{byPath: map[string][]byte{
		runPath:  []byte(runJSON),
		jobsPath: []byte(jobsJSON),
	}}
	v := &artifactqual.GitHubEvidenceVerifier{Runner: fake}
	got, err := v.FetchRun(context.Background(), repo, runID, attempt)
	if err != nil {
		t.Fatalf("FetchRun: %v", err)
	}
	if got.Schema != artifactqual.SchemaPreProdActionsReceipt {
		t.Fatalf("schema=%q", got.Schema)
	}
	if got.RunID != runID || got.Attempt != attempt || got.HeadSHA != sha {
		t.Fatalf("identity: %+v", got)
	}
	if got.Repository != repo || got.WorkflowPath != artifactqual.PreProdIntegrationWorkflow {
		t.Fatalf("repo/path: %+v", got)
	}
	if got.Event != "push" || got.HeadBranch != "pre-prod" {
		t.Fatalf("event/branch: %+v", got)
	}
	if len(got.Jobs) != 2 {
		t.Fatalf("jobs=%d", len(got.Jobs))
	}
	// Exact API argv order: api <path>
	if len(fake.calls) != 2 {
		t.Fatalf("calls=%d want 2", len(fake.calls))
	}
	if fake.calls[0][0] != "api" || fake.calls[0][1] != runPath {
		t.Fatalf("run args=%v", fake.calls[0])
	}
	if fake.calls[1][0] != "api" || fake.calls[1][1] != jobsPath {
		t.Fatalf("jobs args=%v", fake.calls[1])
	}
	// Dual-green validates.
	vOK, cOK, reasons := artifactqual.ValidatePreProdActionsReceipt(got, sha, repo)
	if !vOK || !cOK || len(reasons) > 0 {
		t.Fatalf("validate: v=%v c=%v %v", vOK, cOK, reasons)
	}
}

func TestGitHubEvidenceVerifier_FetchRun_InvalidInputsNoRunner(t *testing.T) {
	v := &artifactqual.GitHubEvidenceVerifier{Runner: ghPanicIfCalled{}}
	if _, err := v.FetchRun(context.Background(), "not a repo", 1, 1); err == nil {
		t.Fatal("invalid repo must fail without runner")
	}
	if _, err := v.FetchRun(context.Background(), "jasonhnd/loopcoder", 0, 1); err == nil {
		t.Fatal("invalid run_id")
	}
	if _, err := v.FetchRun(context.Background(), "jasonhnd/loopcoder", 1, 0); err == nil {
		t.Fatal("invalid attempt")
	}
}

type ghPanicIfCalled struct{}

func (g ghPanicIfCalled) Run(ctx context.Context, args ...string) ([]byte, error) {
	panic("runner must not be called")
}

func TestGitHubEvidenceVerifier_FetchRun_MismatchReject(t *testing.T) {
	repo := "jasonhnd/loopcoder"
	runID := int64(10)
	attempt := 1
	runPath := fmt.Sprintf("repos/%s/actions/runs/%d", repo, runID)
	// Wrong id in body.
	fake := &fakeGHRunner{byPath: map[string][]byte{
		runPath: []byte(`{"id":999,"run_attempt":1,"path":"x","event":"push","head_branch":"pre-prod","head_sha":"a","status":"completed","conclusion":"success","repository":{"full_name":"jasonhnd/loopcoder"}}`),
	}}
	v := &artifactqual.GitHubEvidenceVerifier{Runner: fake}
	if _, err := v.FetchRun(context.Background(), repo, runID, attempt); err == nil || !strings.Contains(err.Error(), "run id mismatch") {
		t.Fatalf("want run id mismatch, got %v", err)
	}
	// Attempt mismatch.
	fake.byPath[runPath] = []byte(`{"id":10,"run_attempt":3,"path":"x","event":"push","head_branch":"pre-prod","head_sha":"a","status":"completed","conclusion":"success","repository":{"full_name":"jasonhnd/loopcoder"}}`)
	if _, err := v.FetchRun(context.Background(), repo, runID, attempt); err == nil || !strings.Contains(err.Error(), "attempt mismatch") {
		t.Fatalf("want attempt mismatch, got %v", err)
	}
	// Repo mismatch.
	fake.byPath[runPath] = []byte(`{"id":10,"run_attempt":1,"path":"x","event":"push","head_branch":"pre-prod","head_sha":"a","status":"completed","conclusion":"success","repository":{"full_name":"other/repo"}}`)
	if _, err := v.FetchRun(context.Background(), repo, runID, attempt); err == nil || !strings.Contains(err.Error(), "repository mismatch") {
		t.Fatalf("want repository mismatch, got %v", err)
	}
}

func TestGitHubEvidenceVerifier_FetchRCBinding_HappyAndMissingArtifact(t *testing.T) {
	repo := "jasonhnd/loopcoder"
	runID := int64(7)
	artID := int64(8)
	sha := strings.Repeat("d", 40)
	runPath := fmt.Sprintf("repos/%s/actions/runs/%d", repo, runID)
	artPath := fmt.Sprintf("repos/%s/actions/runs/%d/artifacts?per_page=100", repo, runID)
	runJSON := fmt.Sprintf(`{
		"id": %d, "run_attempt": 1, "name": %q,
		"head_sha": %q, "status": "completed", "conclusion": "success",
		"repository": {"full_name": %q}
	}`, runID, artifactqual.ReleaseCandidateDraftWorkflow, sha, repo)
	artJSON := fmt.Sprintf(`{
		"artifacts": [
			{"id": %d, "name": "v090-rc-darwin-arm64", "expired": false, "workflow_run": {"id": %d}},
			{"id": 99, "name": "other", "expired": false, "workflow_run": {"id": %d}}
		]
	}`, artID, runID, runID)
	fake := &fakeGHRunner{byPath: map[string][]byte{
		runPath: []byte(runJSON),
		artPath: []byte(artJSON),
	}}
	v := &artifactqual.GitHubEvidenceVerifier{Runner: fake}
	b, err := v.FetchRCBinding(context.Background(), repo, runID, artID)
	if err != nil {
		t.Fatalf("FetchRCBinding: %v", err)
	}
	if b.RunID != runID || b.ArtifactID != artID || b.ArtifactName != "v090-rc-darwin-arm64" {
		t.Fatalf("%+v", b)
	}
	if b.ArtifactExpired || b.RunAttempt != 1 || b.HeadSHA != sha {
		t.Fatalf("%+v", b)
	}
	if b.WorkflowName != artifactqual.ReleaseCandidateDraftWorkflow {
		t.Fatalf("workflow=%q", b.WorkflowName)
	}
	if len(fake.calls) != 2 || fake.calls[0][1] != runPath || fake.calls[1][1] != artPath {
		t.Fatalf("args=%v", fake.calls)
	}
	// Missing artifact id.
	if _, err := v.FetchRCBinding(context.Background(), repo, runID, 404); err == nil || !strings.Contains(err.Error(), "artifact id not found") {
		t.Fatalf("want not found, got %v", err)
	}
	// Wrong workflow_run id on artifact.
	fake.byPath[artPath] = []byte(fmt.Sprintf(`{"artifacts":[{"id":%d,"name":"v090-rc-darwin-arm64","expired":false,"workflow_run":{"id":999}}]}`, artID))
	if _, err := v.FetchRCBinding(context.Background(), repo, runID, artID); err == nil || !strings.Contains(err.Error(), "workflow_run mismatch") {
		t.Fatalf("want workflow_run mismatch, got %v", err)
	}
}

func TestGitHubEvidenceVerifier_FetchRCBinding_InvalidIDsNoRunner(t *testing.T) {
	v := &artifactqual.GitHubEvidenceVerifier{Runner: ghPanicIfCalled{}}
	if _, err := v.FetchRCBinding(context.Background(), "../evil", 1, 1); err == nil {
		t.Fatal("invalid repo")
	}
	if _, err := v.FetchRCBinding(context.Background(), "a/b", 0, 1); err == nil {
		t.Fatal("invalid run")
	}
	if _, err := v.FetchRCBinding(context.Background(), "a/b", 1, 0); err == nil {
		t.Fatal("invalid artifact")
	}
}

func TestGitHubEvidenceVerifier_MalformedJSON(t *testing.T) {
	repo := "jasonhnd/loopcoder"
	runPath := "repos/jasonhnd/loopcoder/actions/runs/1"
	fake := &fakeGHRunner{byPath: map[string][]byte{
		runPath: []byte(`{not-json`),
	}}
	v := &artifactqual.GitHubEvidenceVerifier{Runner: fake}
	_, err := v.FetchRun(context.Background(), repo, 1, 1)
	if err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("want parse error, got %v", err)
	}
}

func TestGitHubEvidenceVerifier_ErrorDoesNotLeakRawResponse(t *testing.T) {
	secret := "SUPER_SECRET_TOKEN_VALUE_xyz"
	repo := "jasonhnd/loopcoder"
	runPath := "repos/jasonhnd/loopcoder/actions/runs/1"
	// Runner error itself embeds the secret (simulates token/stderr in err text).
	fake := &fakeGHRunner{
		errByPath: map[string]error{
			runPath: fmt.Errorf("gh failed: auth %s raw-stderr=%s", secret, secret),
		},
	}
	v := &artifactqual.GitHubEvidenceVerifier{Runner: fake}
	_, err := v.FetchRun(context.Background(), repo, 1, 1)
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked runner secret: %v", err)
	}
	// Also when parse would see secret as body — use byPath with invalid json containing secret.
	fake2 := &fakeGHRunner{byPath: map[string][]byte{
		runPath: []byte(`{"token":"` + secret + `", broken`),
	}}
	v2 := &artifactqual.GitHubEvidenceVerifier{Runner: fake2}
	_, err = v2.FetchRun(context.Background(), repo, 1, 1)
	if err == nil {
		t.Fatal("want parse error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("parse error leaked secret: %v", err)
	}
}

func TestGitHubEvidenceVerifier_FetchPR_HappyPathArgs(t *testing.T) {
	repo := "jasonhnd/loopcoder"
	num := 42
	head := "0123456789abcdef0123456789abcdef01234567"
	prPath := fmt.Sprintf("repos/%s/pulls/%d", repo, num)
	checksPath := fmt.Sprintf("repos/%s/commits/%s/check-runs?per_page=100", repo, head)
	prJSON := fmt.Sprintf(`{
		"number": %d,
		"html_url": "https://github.com/%s/pull/%d",
		"state": "open",
		"auto_merge": null,
		"base": {"ref": "main", "repo": {"full_name": %q}},
		"head": {"sha": %q}
	}`, num, repo, num, repo, head)
	checksJSON := `{
		"check_runs": [
			{"name": "verify", "status": "completed", "conclusion": "success"},
			{"name": "test", "status": "completed", "conclusion": "success"}
		]
	}`
	fake := &fakeGHRunner{byPath: map[string][]byte{
		prPath:     []byte(prJSON),
		checksPath: []byte(checksJSON),
	}}
	v := &artifactqual.GitHubEvidenceVerifier{Runner: fake}
	st, err := v.FetchPR(context.Background(), repo, num)
	if err != nil {
		t.Fatalf("FetchPR: %v", err)
	}
	if st.Number != num || st.Repository != repo || st.BaseRef != "main" {
		t.Fatalf("%+v", st)
	}
	if st.HeadOID != head || st.State != "open" {
		t.Fatalf("%+v", st)
	}
	if st.AutoMergeEnabled || !st.HumanMergeGate {
		t.Fatalf("human gate: auto=%v human=%v", st.AutoMergeEnabled, st.HumanMergeGate)
	}
	if !st.RequiredChecksGreen || len(st.ChecksAtHead) != 2 || len(st.RequiredChecks) != 2 {
		t.Fatalf("checks: %+v", st)
	}
	if len(fake.calls) != 2 || fake.calls[0][1] != prPath || fake.calls[1][1] != checksPath {
		t.Fatalf("args=%v", fake.calls)
	}
}

func TestGitHubEvidenceVerifier_FetchPR_MismatchAndInvalidInputs(t *testing.T) {
	repo := "jasonhnd/loopcoder"
	// Invalid repo / number — no runner call.
	v := &artifactqual.GitHubEvidenceVerifier{Runner: ghPanicIfCalled{}}
	if _, err := v.FetchPR(context.Background(), "../evil", 1); err == nil {
		t.Fatal("invalid repo")
	}
	if _, err := v.FetchPR(context.Background(), repo, 0); err == nil {
		t.Fatal("invalid number")
	}
	// Number mismatch.
	prPath := "repos/jasonhnd/loopcoder/pulls/1"
	fake := &fakeGHRunner{byPath: map[string][]byte{
		prPath: []byte(`{"number":99,"html_url":"u","state":"open","auto_merge":null,"base":{"ref":"main","repo":{"full_name":"jasonhnd/loopcoder"}},"head":{"sha":"0123456789abcdef0123456789abcdef01234567"}}`),
	}}
	v2 := &artifactqual.GitHubEvidenceVerifier{Runner: fake}
	if _, err := v2.FetchPR(context.Background(), repo, 1); err == nil || !strings.Contains(err.Error(), "number mismatch") {
		t.Fatalf("want number mismatch, got %v", err)
	}
	// Repo mismatch.
	fake.byPath[prPath] = []byte(`{"number":1,"html_url":"u","state":"open","auto_merge":null,"base":{"ref":"main","repo":{"full_name":"other/repo"}},"head":{"sha":"0123456789abcdef0123456789abcdef01234567"}}`)
	if _, err := v2.FetchPR(context.Background(), repo, 1); err == nil || !strings.Contains(err.Error(), "repository mismatch") {
		t.Fatalf("want repository mismatch, got %v", err)
	}
	// Non-40-hex head.
	fake.byPath[prPath] = []byte(`{"number":1,"html_url":"u","state":"open","auto_merge":null,"base":{"ref":"main","repo":{"full_name":"jasonhnd/loopcoder"}},"head":{"sha":"abc1234"}}`)
	if _, err := v2.FetchPR(context.Background(), repo, 1); err == nil || !strings.Contains(err.Error(), "head oid invalid") {
		t.Fatalf("want head oid invalid, got %v", err)
	}
}

func TestGitHubEvidenceVerifier_FetchPR_AutoMergeAndChecks(t *testing.T) {
	repo := "jasonhnd/loopcoder"
	head := "0123456789abcdef0123456789abcdef01234567"
	prPath := "repos/jasonhnd/loopcoder/pulls/3"
	checksPath := fmt.Sprintf("repos/%s/commits/%s/check-runs?per_page=100", repo, head)
	prAuto := fmt.Sprintf(`{
		"number": 3, "html_url": "https://github.com/%s/pull/3", "state": "open",
		"auto_merge": {"enabled_by": {"login": "bot"}},
		"base": {"ref": "main", "repo": {"full_name": %q}},
		"head": {"sha": %q}
	}`, repo, repo, head)
	// Zero checks cannot green.
	fake := &fakeGHRunner{byPath: map[string][]byte{
		prPath:     []byte(prAuto),
		checksPath: []byte(`{"check_runs":[]}`),
	}}
	v := &artifactqual.GitHubEvidenceVerifier{Runner: fake}
	st, err := v.FetchPR(context.Background(), repo, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !st.AutoMergeEnabled || st.HumanMergeGate {
		t.Fatalf("auto_merge must disable human gate: %+v", st)
	}
	if st.RequiredChecksGreen {
		t.Fatal("zero checks cannot green")
	}
	// Non-green check.
	prOpen := fmt.Sprintf(`{
		"number": 3, "html_url": "u", "state": "open", "auto_merge": null,
		"base": {"ref": "main", "repo": {"full_name": %q}},
		"head": {"sha": %q}
	}`, repo, head)
	fake.byPath[prPath] = []byte(prOpen)
	fake.byPath[checksPath] = []byte(`{"check_runs":[{"name":"verify","status":"completed","conclusion":"failure"}]}`)
	st, err = v.FetchPR(context.Background(), repo, 3)
	if err != nil {
		t.Fatal(err)
	}
	if st.RequiredChecksGreen {
		t.Fatal("failed check cannot green")
	}
	if st.AutoMergeEnabled || !st.HumanMergeGate {
		t.Fatalf("open+null auto_merge should human gate: %+v", st)
	}
}

func TestGitHubEvidenceVerifier_FetchPR_MalformedAndSecret(t *testing.T) {
	secret := "SUPER_SECRET_TOKEN_VALUE_xyz"
	repo := "jasonhnd/loopcoder"
	prPath := "repos/jasonhnd/loopcoder/pulls/1"
	fake := &fakeGHRunner{byPath: map[string][]byte{
		prPath: []byte(`{not-json`),
	}}
	v := &artifactqual.GitHubEvidenceVerifier{Runner: fake}
	if _, err := v.FetchPR(context.Background(), repo, 1); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("want parse error, got %v", err)
	}
	// Malformed checks after valid PR.
	head := "0123456789abcdef0123456789abcdef01234567"
	checksPath := fmt.Sprintf("repos/%s/commits/%s/check-runs?per_page=100", repo, head)
	fake.byPath[prPath] = []byte(fmt.Sprintf(`{"number":1,"html_url":"u","state":"open","auto_merge":null,"base":{"ref":"main","repo":{"full_name":%q}},"head":{"sha":%q}}`, repo, head))
	fake.byPath[checksPath] = []byte(`{broken`)
	if _, err := v.FetchPR(context.Background(), repo, 1); err == nil || !strings.Contains(err.Error(), "check-runs") {
		t.Fatalf("want check-runs parse error, got %v", err)
	}
	// Runner error with secret never leaks.
	fake2 := &fakeGHRunner{errByPath: map[string]error{
		prPath: fmt.Errorf("gh failed: %s", secret),
	}}
	v2 := &artifactqual.GitHubEvidenceVerifier{Runner: fake2}
	_, err := v2.FetchPR(context.Background(), repo, 1)
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("leaked secret: %v", err)
	}
	// Body with secret malformed.
	fake3 := &fakeGHRunner{byPath: map[string][]byte{
		prPath: []byte(`{"token":"` + secret + `", broken`),
	}}
	v3 := &artifactqual.GitHubEvidenceVerifier{Runner: fake3}
	_, err = v3.FetchPR(context.Background(), repo, 1)
	if err == nil {
		t.Fatal("want parse error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("parse leaked secret: %v", err)
	}
}
