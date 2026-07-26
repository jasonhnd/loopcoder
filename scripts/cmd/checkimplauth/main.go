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
	closingPath := flag.String("closing-issues", "", "JSON array of closing issues with owner evidence")
	baseSHA := flag.String("base-sha", "", "exact pre-prod base SHA")
	verifyOK := flag.String("base-verify-ok", "false", "base SHA passed integration-verify")
	canaryOK := flag.String("base-canary-ok", "false", "base SHA passed integration-canary")
	prNumber := flag.Int("pr-number", 0, "pull request number")
	headBranch := flag.String("head-branch", "", "PR head branch")
	headSHA := flag.String("head-sha", "", "exact PR head SHA")
	promotionAnchorSHA := flag.String("promotion-anchor-sha", "", "frozen promotion merge anchor")
	promotionAnchorOK := flag.String("promotion-anchor-ok", "false", "promotion anchor is live head ancestor")
	baseBranch := flag.String("base-branch", "", "PR base branch")
	flag.Parse()

	if *filesPath == "" || *closingPath == "" {
		fatal(fmt.Errorf("files and closing-issues are required"))
	}

	paths := readLines(*filesPath)
	raw, err := os.ReadFile(*closingPath)
	if err != nil {
		fatal(err)
	}
	var closing []evidence.ClosingIssue
	if err := json.Unmarshal(raw, &closing); err != nil {
		fatal(fmt.Errorf("closing-issues json: %w", err))
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
		os.Exit(1)
	}

	if evidence.IsImplementationChange(paths) {
		issueN := 0
		if len(closing) == 1 {
			issueN = closing[0].Number
		}
		boot := evidence.BootstrapContext{
			PRNumber:           *prNumber,
			HeadBranch:         *headBranch,
			HeadSHA:            *headSHA,
			BaseBranch:         *baseBranch,
			BaseSHA:            *baseSHA,
			IssueNumber:        issueN,
			PromotionAnchorSHA: *promotionAnchorSHA,
			PromotionAnchorOK:  parseBool(*promotionAnchorOK),
		}
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
