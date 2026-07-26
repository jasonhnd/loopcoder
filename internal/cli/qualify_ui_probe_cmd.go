package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/directattempt"
	"github.com/jasonhnd/loopcoder/internal/directrun"
	"github.com/jasonhnd/loopcoder/internal/execidentity"
	"github.com/jasonhnd/loopcoder/internal/preflight"
	"github.com/jasonhnd/loopcoder/internal/providerexec"
	"github.com/jasonhnd/loopcoder/internal/termui"
)

const qualifyUIProbeChallengeEnv = "LOOPCODER_QUALIFY_UI_PROBE_CHALLENGE"

// runQualifyUIProbe is intentionally hidden from root help. It exists only so
// the exact downloaded binary can exercise its durable direct-run
// publish->render->ack->terminal path without GitHub, provider credentials, or
// paid execution. Its fixture route and output schema are structurally
// ineligible for canary, PR, or real-runtime qualification evidence.
func runQualifyUIProbe(args []string, stdout, stderr io.Writer, deps Deps) int {
	fs := flag.NewFlagSet("_qualify-ui-probe", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", "", "isolated temporary git repository")
	projectID := fs.String("project-id", "", "isolated qualification project id")
	challenge := fs.String("challenge", "", "per-process qualification challenge")
	if err := fs.Parse(args); err != nil {
		return exitRunUsage
	}
	wantChallenge := strings.TrimSpace(os.Getenv(qualifyUIProbeChallengeEnv))
	if wantChallenge == "" || len(wantChallenge) < 32 || strings.TrimSpace(*challenge) != wantChallenge {
		fmt.Fprintln(stderr, "qualify ui probe: missing or mismatched bounded challenge")
		return exitRunUsage
	}
	repoPath := strings.TrimSpace(*repo)
	proj := strings.TrimSpace(*projectID)
	if repoPath == "" || proj == "" {
		fmt.Fprintln(stderr, "qualify ui probe: --repo and --project-id required")
		return exitRunUsage
	}
	if proj != "acme-qual-latency" || !isolatedQualifyProbePaths(repoPath, os.Getenv("LOOPCODER_HOME")) {
		fmt.Fprintln(stderr, "qualify ui probe: isolated repo/home binding invalid")
		return exitRunPrecondition
	}
	baseSHA, err := gitHead(repoPath)
	if err != nil {
		fmt.Fprintln(stderr, "qualify ui probe: isolated repo invalid")
		return exitRunPrecondition
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	title := "Measure exact-binary durable UI lifecycle"
	body := "Structural qualification probe only; never production runtime evidence."
	dc, err := execidentity.BuildDirectContract(execidentity.DirectContractInput{
		IssueTitle: title, IssueBody: body, BaseSHA: baseSHA,
		TaskClass: "luna", Depth: "medium", Permission: "bounded_write",
		OutputContract: execidentity.DirectRunOutputContract,
		Actor:          "owner", ProjectID: proj, Now: now().UTC(),
	})
	if err != nil {
		fmt.Fprintln(stderr, "qualify ui probe: execution identity invalid")
		return exitRunPrecondition
	}

	controlledProvider := func(_ context.Context, req providerexec.Request) (providerexec.Outcome, error) {
		if req.OnProviderStart != nil {
			if err := req.OnProviderStart(providerexec.ProcessStart{
				PID: os.Getpid(), PGID: os.Getpid(),
				ProcessBirthIdentity: "qualify-ui-probe",
				ExecutableIdentity:   "internal://qualify-ui-probe",
				ObservedAt:           now().UTC(),
			}); err != nil {
				return providerexec.Outcome{}, err
			}
		}
		return providerexec.Outcome{
			Schema: providerexec.SchemaOutcome, RequestID: req.RequestID,
			RequestedRoute: req.Route, ActualRoute: req.Route,
			ActualSources: providerexec.ActualSources{
				Model: "controlled_qualification_probe", Effort: "controlled_qualification_probe",
				Permission: "controlled_qualification_probe",
			},
			RouteDigest: req.RouteDigest, ArgvDigest: "sha256:qualification-probe-no-argv",
			ExitCode: 0, FinishedAt: now().UTC(),
			OutputDigest: "sha256:qualification-probe-structural-output",
			Usage:        providerexec.UsageEvidence{},
		}, nil
	}
	svc := directrun.Service{Deps: directrun.Deps{
		Now: now, HomeDir: strings.TrimSpace(os.Getenv("LOOPCODER_HOME")),
		Preflight: func(_ context.Context, in preflight.Input) (preflight.Snapshot, error) {
			return preflight.Snapshot{
				Decision: "allow", AllowLaunch: true, Provider: in.Provider,
				Repo: in.Repo, Digest: "qualify-ui-probe-preflight",
			}, nil
		},
		Provider: controlledProvider,
	}}
	res, err := svc.Execute(context.Background(), directrun.Request{
		Repo: repoPath, RepoPath: repoPath, Issue: "qualification-probe",
		Prompt:   title + "\n\n" + body,
		Provider: "fixture", Model: "m0", Effort: "medium", Permission: "bounded_write",
		BaseBranch: "pre-prod", RequiredUI: []string{"terminal"},
		ProjectID: proj, RunID: "run_qualify_ui_probe", AttemptID: "att_qualify_ui_probe_g0",
		BaseSHA: baseSHA, PlanDigest: dc.PlanDigest, GraphDigest: dc.GraphDigest,
		TaskClass: dc.TaskClass, ChildContractDigest: dc.ChildContractDigest,
		ReportOut: stderr, ReportMode: termui.ModeJSONL,
	})
	if err != nil || res.State != directattempt.StateCleanupTerminal {
		fmt.Fprintln(stderr, "qualify ui probe: direct lifecycle incomplete")
		return exitRunPrecondition
	}
	return writeQualifyUIProbeResult(stdout, res)
}

func isolatedQualifyProbePaths(repo, home string) bool {
	repo = strings.TrimSpace(repo)
	home = strings.TrimSpace(home)
	if repo == "" || home == "" {
		return false
	}
	realRepo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		return false
	}
	realHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		return false
	}
	realRepo, err = filepath.Abs(realRepo)
	if err != nil {
		return false
	}
	realHome, err = filepath.Abs(realHome)
	if err != nil {
		return false
	}
	if filepath.Base(realRepo) != "latency-repo" || filepath.Base(realHome) != "latency-home" {
		return false
	}
	if filepath.Dir(realRepo) != filepath.Dir(realHome) {
		return false
	}
	info, err := os.Stat(realHome)
	return err == nil && info.IsDir() && info.Mode().Perm() == 0o700
}

func gitHead(repo string) (string, error) {
	cmd := exec.Command("git", "-C", repo, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(string(out))
	if len(sha) != 40 {
		return "", fmt.Errorf("invalid head")
	}
	return sha, nil
}

func writeQualifyUIProbeResult(w io.Writer, res directrun.Result) int {
	payload := struct {
		Schema             string `json:"schema"`
		RunID              string `json:"run_id"`
		State              string `json:"state"`
		ProductionEvidence bool   `json:"production_evidence"`
	}{
		Schema: "loopcoder.qualify_ui_probe.v1", RunID: res.RunID,
		State: string(res.State), ProductionEvidence: false,
	}
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		return exitRunPrecondition
	}
	return exitRunOK
}
