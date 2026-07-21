// Command checkimplauth evaluates fail-closed implementation authorization.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/evidence"
)

func main() {
	filesPath := flag.String("files", "", "newline-separated changed paths")
	closingPath := flag.String("closing-issues", "", "JSON array of closingIssuesReferences")
	baseSHA := flag.String("base-sha", "", "exact pre-prod base SHA for integration gate")
	verifyOK := flag.String("base-verify-ok", "false", "base SHA passed integration-verify")
	canaryOK := flag.String("base-canary-ok", "false", "base SHA passed integration-canary")
	bootstrap1092 := flag.String("bootstrap-1092", "false", "one-time exception for stabilization PR closing #1092 only")
	flag.Parse()

	paths := readLines(*filesPath)
	var closing []evidence.ClosingIssue
	if *closingPath != "" {
		raw, err := os.ReadFile(*closingPath)
		if err != nil {
			fatal(err)
		}
		if err := json.Unmarshal(raw, &closing); err != nil {
			fatal(fmt.Errorf("closing-issues json: %w", err))
		}
	}

	fmt.Printf("documentation_only=%v\n", evidence.IsDocumentationOnly(paths))
	fmt.Printf("implementation_change=%v\n", evidence.IsImplementationChange(paths))

	d := evidence.EvaluateImplementationAuthorization(paths, closing)
	fmt.Printf("allowed=%v\n", d.Allowed)
	for _, r := range d.Reasons {
		fmt.Printf("reason=%s\n", r)
	}
	for _, n := range d.BlockedIssues {
		fmt.Printf("blocked_issue=%d\n", n)
	}
	if !d.Allowed {
		fmt.Fprintln(os.Stderr, "implementation-authorization: REJECTED")
		fmt.Fprintln(os.Stderr, "Require exactly one closingIssuesReference with label implementation-authorized.")
		os.Exit(1)
	}

	if evidence.IsImplementationChange(paths) {
		boot := parseBool(*bootstrap1092) && len(closing) == 1 && closing[0].Number == 1092
		g := evidence.EvaluateBaseSHAGate(*baseSHA, parseBool(*verifyOK), parseBool(*canaryOK), boot)
		fmt.Printf("base_sha_allowed=%v\n", g.Allowed)
		for _, r := range g.Reasons {
			fmt.Printf("base_reason=%s\n", r)
		}
		if !g.Allowed {
			fmt.Fprintln(os.Stderr, "implementation-authorization: base pre-prod SHA integration not green")
			os.Exit(1)
		}
	}
}

func parseBool(s string) bool {
	b, err := strconv.ParseBool(strings.TrimSpace(s))
	return err == nil && b
}

func readLines(path string) []string {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		fatal(err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(2)
}
