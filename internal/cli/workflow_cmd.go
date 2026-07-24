package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/workflowdef"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

func runWorkflow(args []string, stdout, stderr io.Writer, deps Deps) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "workflow: subcommand required (run|plan|goal)")
		return exitRunUsage
	}
	switch args[0] {
	case "run":
		return runWorkflowRun(args[1:], stdout, stderr, deps)
	case "plan":
		return runWorkflowPlan(args[1:], stdout, stderr, deps)
	case "goal":
		return runWorkflowGoal(args[1:], stdout, stderr, deps)
	case "help", "-h", "--help":
		fmt.Fprintln(stdout, "usage: loopcoder workflow run [--def path.json] [--provider p] [--model m] [--project-id id] [--format json|human]")
		fmt.Fprintln(stdout, "       loopcoder workflow run --dev-fixture one|chain  (test-only; never human_gate/PR/canary)")
		fmt.Fprintln(stdout, "       loopcoder workflow plan --goal TEXT [--issue N] [--format json|human]")
		fmt.Fprintln(stdout, "       loopcoder workflow goal --goal TEXT [--issue N] [--provider p] [--model m]")
		return exitRunOK
	default:
		fmt.Fprintf(stderr, "workflow: unknown subcommand %q\n", args[0])
		return exitRunUsage
	}
}

func runWorkflowRun(args []string, stdout, stderr io.Writer, deps Deps) int {
	fs := flag.NewFlagSet("workflow run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	// Public product path: no default fixture. Explicit --dev-fixture is test-only.
	devFixture := fs.String("dev-fixture", "", "TEST-ONLY: one|chain fixture path (never production evidence)")
	// Legacy --fixture removed from product path: refuse with migration hint.
	legacyFixture := fs.String("fixture", "", "REMOVED: use --dev-fixture (test-only)")
	defPath := fs.String("def", "", "path to workflow definition JSON")
	projectID := fs.String("project-id", "local-project", "project id")
	actor := fs.String("actor", "owner", "approval actor")
	// Empty provider/model → fail closed unless --dev-fixture (no silent fixture-model).
	provider := fs.String("provider", "", "child route provider pin (required unless --dev-fixture)")
	model := fs.String("model", "", "child route model pin (required unless --dev-fixture)")
	format := fs.String("format", "human", "human|json")
	if err := fs.Parse(args); err != nil {
		return exitRunUsage
	}

	if strings.TrimSpace(*legacyFixture) != "" {
		fmt.Fprintln(stderr, "workflow run: --fixture removed; use explicit --dev-fixture (test-only, non-production schema)")
		return exitRunUsage
	}
	fixtureName := strings.TrimSpace(*devFixture)
	isDevFixture := fixtureName != ""

	var def workflowdef.Definition
	if strings.TrimSpace(*defPath) != "" {
		raw, err := os.ReadFile(*defPath)
		if err != nil {
			fmt.Fprintf(stderr, "workflow run: read def: %v\n", err)
			return exitRunPrecondition
		}
		def, err = workflowdef.ParseJSON(raw)
		if err != nil {
			fmt.Fprintf(stderr, "workflow run: parse def: %v\n", err)
			return exitRunUsage
		}
	} else if isDevFixture {
		switch strings.ToLower(fixtureName) {
		case "one", "one-node":
			def = workflowrun.OneNodeDefinition("g-one", "bounded one-node")
		case "chain", "three", "three-node":
			def = workflowrun.ChainDefinition("g-chain")
		default:
			fmt.Fprintf(stderr, "workflow run: unknown --dev-fixture %q\n", fixtureName)
			return exitRunUsage
		}
	} else {
		fmt.Fprintln(stderr, "workflow run: require --def path.json or explicit --dev-fixture (no silent fixture default)")
		return exitRunUsage
	}

	prov := strings.TrimSpace(*provider)
	mod := strings.TrimSpace(*model)
	if isDevFixture {
		if prov == "" {
			prov = "fixture"
		}
		if mod == "" {
			mod = "fixture-model"
		}
	} else {
		if prov == "" || mod == "" {
			fmt.Fprintln(stderr, "workflow run: --provider and --model required on product path (or use --dev-fixture for tests only)")
			return exitRunUsage
		}
		if strings.EqualFold(prov, "fixture") || strings.EqualFold(mod, "fixture-model") {
			fmt.Fprintln(stderr, "workflow run: fixture provider forbidden without --dev-fixture")
			return exitRunUsage
		}
	}

	now := deps.Now
	if now == nil {
		now = DefaultDeps().Now
	}
	homeDir, herr := workflowrun.ResolveDurableHome("")
	if herr != nil {
		fmt.Fprintf(stderr, "workflow run: durable home: %v\n", herr)
		return exitRunPrecondition
	}

	var exec workflowrun.ChildExecutor
	if isDevFixture {
		// Explicit test-only AllowFixture — results are marked non-production.
		exec = workflowrun.ProductionChildExecutor{HomeDir: homeDir, Now: now, AllowFixture: true}
	}
	// Freeze expected digests from Normalize + Materialize (no silent local-only plan/graph).
	if def.SchemaVersion == 0 {
		def.SchemaVersion = 1
	}
	if strings.TrimSpace(def.Source) == "" {
		def.Source = "explicit_definition"
	}
	plan, perr := workflowdef.Normalize(def)
	if perr != nil {
		fmt.Fprintf(stderr, "workflow run: normalize: %v\n", perr)
		return exitRunUsage
	}
	actorID := strings.TrimSpace(*actor)
	if actorID == "" {
		actorID = "owner"
	}
	proj := strings.TrimSpace(*projectID)
	if proj == "" {
		proj = "local-project"
	}
	ap, aerr := workflowdef.Approve(plan.Digest, actorID, "workflow run", now().UTC())
	if aerr != nil {
		fmt.Fprintf(stderr, "workflow run: approve: %v\n", aerr)
		return exitRunPrecondition
	}
	mat, merr := workflowdef.NewRegistry().Materialize(proj, def, ap, now().UTC())
	if merr != nil {
		fmt.Fprintf(stderr, "workflow run: materialize: %v\n", merr)
		return exitRunPrecondition
	}
	graphDig := workgraph.DigestGraph(mat.Graph)
	if graphDig == "" {
		fmt.Fprintln(stderr, "workflow run: empty graph digest after materialize")
		return exitRunPrecondition
	}

	svc := workflowrun.Service{Now: now, HomeDir: homeDir, Executor: exec}
	res, err := svc.Execute(context.Background(), workflowrun.Request{
		ProjectID: proj, Definition: def, Actor: actorID,
		Provider: prov, Model: mod,
		ExpectedPlanDigest:  plan.Digest,
		ExpectedGraphDigest: graphDig,
	})
	if err != nil {
		fmt.Fprintf(stderr, "workflow run: %v\n", err)
		if strings.ToLower(*format) == "json" {
			_, _ = stdout.Write(workflowrun.ResultJSON(res))
		}
		if res.Status == workflowrun.StatusInvalid {
			return exitRunUsage
		}
		return exitRunPrecondition
	}

	// Dev-fixture path must never claim human_gate production evidence.
	statusOut := res.Status
	if isDevFixture {
		if statusOut == workflowrun.StatusHumanGate {
			statusOut = "dev_fixture_complete"
		}
		fmt.Fprintln(stderr, "workflow: DEV_FIXTURE path — not production human_gate/PR/canary evidence")
	}

	switch strings.ToLower(*format) {
	case "json":
		if isDevFixture {
			// Separate dev schema: never embed production Result with human_gate.
			// Only non-production fields are exported.
			type devFixtureResult struct {
				Schema      string   `json:"schema"`
				Status      string   `json:"status"`
				DevFixture  bool     `json:"dev_fixture"`
				SchemaNote  string   `json:"schema_note"`
				GraphID     string   `json:"graph_id,omitempty"`
				ClaimCount  int      `json:"claim_count"`
				LaunchCount int      `json:"launch_count"`
				Integrated  []string `json:"integrated,omitempty"`
				// Production-status fields intentionally omitted (no human_gate/PR/canary).
			}
			enc := json.NewEncoder(stdout)
			_ = enc.Encode(devFixtureResult{
				Schema: "loopcoder.workflow.dev_fixture.v1",
				Status: statusOut, DevFixture: true,
				SchemaNote: "never scorecard/canary/human_gate evidence",
				GraphID:    res.GraphID, ClaimCount: res.ClaimCount, LaunchCount: res.LaunchCount,
				Integrated: append([]string(nil), res.Integrated...),
			})
		} else {
			_, _ = stdout.Write(workflowrun.ResultJSON(res))
		}
	default:
		fmt.Fprintf(stdout, "status=%s graph=%s claims=%d launches=%d\n",
			statusOut, res.GraphID, res.ClaimCount, res.LaunchCount)
		fmt.Fprintf(stdout, "message=%s\n", res.Message)
		if len(res.Integrated) > 0 {
			fmt.Fprintf(stdout, "integrated=%s\n", strings.Join(res.Integrated, ","))
		}
	}
	fmt.Fprintf(stderr, "workflow: %s auto_merge=%v events=%d\n", statusOut, res.AutoMerge, len(res.Events))
	return exitRunOK
}

// ensure ParseJSON available for empty defs tests
var _ = json.Marshal
