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
		fmt.Fprintln(stdout, "usage: loopcoder workflow run [--fixture one|chain] [--def path.json] [--project-id id] [--format json|human]")
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
	fixture := fs.String("fixture", "one", "built-in fixture: one|chain")
	defPath := fs.String("def", "", "path to workflow definition JSON (overrides --fixture)")
	projectID := fs.String("project-id", "local-project", "project id")
	actor := fs.String("actor", "owner", "approval actor")
	provider := fs.String("provider", "fixture", "child route provider pin")
	model := fs.String("model", "fixture-model", "child route model pin")
	format := fs.String("format", "human", "human|json")
	if err := fs.Parse(args); err != nil {
		return exitRunUsage
	}

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
	} else {
		switch strings.ToLower(strings.TrimSpace(*fixture)) {
		case "one", "one-node", "":
			def = workflowrun.OneNodeDefinition("g-one", "bounded one-node")
		case "chain", "three", "three-node":
			def = workflowrun.ChainDefinition("g-chain")
		default:
			fmt.Fprintf(stderr, "workflow run: unknown fixture %q\n", *fixture)
			return exitRunUsage
		}
	}

	now := deps.Now
	if now == nil {
		now = DefaultDeps().Now
	}
	svc := workflowrun.Service{Now: now}
	res, err := svc.Execute(context.Background(), workflowrun.Request{
		ProjectID: *projectID, Definition: def, Actor: *actor,
		Provider: *provider, Model: *model,
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

	switch strings.ToLower(*format) {
	case "json":
		_, _ = stdout.Write(workflowrun.ResultJSON(res))
	default:
		fmt.Fprintf(stdout, "status=%s graph=%s claims=%d launches=%d\n",
			res.Status, res.GraphID, res.ClaimCount, res.LaunchCount)
		fmt.Fprintf(stdout, "message=%s\n", res.Message)
		if len(res.Integrated) > 0 {
			fmt.Fprintf(stdout, "integrated=%s\n", strings.Join(res.Integrated, ","))
		}
	}
	// operator stream
	fmt.Fprintf(stderr, "workflow: %s auto_merge=%v events=%d\n", res.Status, res.AutoMerge, len(res.Events))
	return exitRunOK
}

// ensure ParseJSON available for empty defs tests
var _ = json.Marshal
