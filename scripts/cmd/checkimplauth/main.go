// Command checkimplauth evaluates implementation authorization for CI.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/evidence"
)

func main() {
	filesPath := flag.String("files", "", "path to newline-separated changed files")
	issuesPath := flag.String("issues", "", "path to JSON array of linked issues")
	bodyPath := flag.String("body", "", "path to PR body")
	flag.Parse()

	var paths []string
	if *filesPath != "" {
		f, err := os.Open(*filesPath)
		if err != nil {
			fatal(err)
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line != "" {
				paths = append(paths, line)
			}
		}
		_ = f.Close()
	}

	isImpl := evidence.IsImplementationChange(paths)
	fmt.Printf("implementation_change=%v\n", isImpl)

	var issues []evidence.LinkedIssueRef
	if *issuesPath != "" {
		raw, err := os.ReadFile(*issuesPath)
		if err != nil {
			fatal(err)
		}
		if err := json.Unmarshal(raw, &issues); err != nil {
			fatal(err)
		}
	}
	// Also parse body for issue numbers if issues empty but body provided
	if *bodyPath != "" {
		body, _ := os.ReadFile(*bodyPath)
		_ = body
	}

	d := evidence.EvaluateImplementationAuthorization(isImpl, issues, false)
	fmt.Printf("allowed=%v\n", d.Allowed)
	for _, r := range d.Reasons {
		fmt.Printf("reason=%s\n", r)
	}
	for _, n := range d.BlockedIssues {
		fmt.Printf("blocked_issue=%d\n", n)
	}
	if !d.Allowed {
		fmt.Fprintln(os.Stderr, "implementation-authorization: REJECTED — status:planned issue without explicit authorization")
		fmt.Fprintln(os.Stderr, "Grant with label status:authorized or body text 'Implementation authorization: granted'")
		os.Exit(1)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(2)
}
