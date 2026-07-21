package localverify

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	SchemaPlan   = "loopcoder.localverify.plan.v1"
	SchemaResult = "loopcoder.localverify.result.v1"
)

var (
	ErrDenied  = errors.New("localverify: command denied by policy")
	ErrInvalid = errors.New("localverify: invalid")
)

// Class classifies changed files.
type Class string

const (
	ClassGo        Class = "go"
	ClassDocs      Class = "docs"
	ClassWorkflow  Class = "workflow"
	ClassGenerated Class = "generated"
	ClassOther     Class = "other"
)

// Budgets bound a single command.
type Budgets struct {
	SoftDeadline time.Duration `json:"soft_deadline"`
	HardDeadline time.Duration `json:"hard_deadline"`
	MaxOutputB   int64         `json:"max_output_bytes"`
	MaxRSSMB     int64         `json:"max_rss_mb"`
	MaxProcesses int           `json:"max_processes"`
}

// DefaultBudgets returns ordinary local budgets.
func DefaultBudgets() Budgets {
	return Budgets{
		SoftDeadline: 30 * time.Second,
		HardDeadline: 60 * time.Second,
		MaxOutputB:   256 << 10,
		MaxRSSMB:     1024,
		MaxProcesses: 32,
	}
}

// Command is one planned check.
type Command struct {
	Name    string   `json:"name"`
	Argv    []string `json:"argv"`
	Scope   string   `json:"scope"` // packages or paths
	Budgets Budgets  `json:"budgets"`
	Digest  string   `json:"digest"`
}

// Plan is the deterministic verification plan.
type Plan struct {
	Schema    string            `json:"schema"`
	Included  []Command         `json:"included"`
	Excluded  []string          `json:"excluded"`
	Reasons   map[string]string `json:"reasons"`
	FileClass map[string]Class  `json:"file_class"`
	Digest    string            `json:"digest"`
}

// Result is persisted command evidence.
type Result struct {
	Schema   string        `json:"schema"`
	Command  Command       `json:"command"`
	ExitCode int           `json:"exit_code"`
	Duration time.Duration `json:"duration"`
	// OutputDigest only — body bounded/truncated elsewhere.
	OutputDigest string `json:"output_digest"`
	TimedOut     bool   `json:"timed_out"`
	Blocked      bool   `json:"blocks_delivery"`
}

// Classify maps a path to a class.
func Classify(path string) Class {
	p := filepath.ToSlash(path)
	switch {
	case strings.HasPrefix(p, "docs/") || strings.HasSuffix(p, ".md"):
		return ClassDocs
	case strings.HasPrefix(p, ".github/workflows/"):
		return ClassWorkflow
	case strings.Contains(p, "generated/") || strings.HasSuffix(p, ".gen.go"):
		return ClassGenerated
	case strings.HasSuffix(p, ".go"):
		return ClassGo
	default:
		return ClassOther
	}
}

// deniedPatterns must never appear in default local plans.
var deniedSubstrings = []string{
	"go test ./...",
	"go test -race",
	"-race",
	"govulncheck",
	"gosec",
	"codesign",
	"release smoke",
	"provider probe",
	"packaging",
}

// IsDenied reports if argv is forbidden locally.
func IsDenied(argv []string) bool {
	line := strings.Join(argv, " ")
	lower := strings.ToLower(line)
	for _, d := range deniedSubstrings {
		if strings.Contains(lower, d) {
			return true
		}
	}
	// go test ./... explicit
	for i, a := range argv {
		if a == "./..." {
			return true
		}
		if a == "-race" {
			return true
		}
		if a == "test" && i+1 < len(argv) && argv[i+1] == "./..." {
			return true
		}
	}
	return false
}

// BuildPlan creates a deterministic plan from changed files.
func BuildPlan(changed []string) (Plan, error) {
	if len(changed) == 0 {
		return Plan{}, fmt.Errorf("%w: no changed files", ErrInvalid)
	}
	fc := map[string]Class{}
	pkgs := map[string]struct{}{}
	hasGo, hasDocs, hasGen := false, false, false
	for _, f := range changed {
		c := Classify(f)
		fc[f] = c
		switch c {
		case ClassGo:
			hasGo = true
			dir := filepath.ToSlash(filepath.Dir(f))
			if dir == "." {
				dir = ""
			}
			pkg := "./" + dir
			if dir == "" {
				pkg = "."
			}
			pkg = strings.TrimSuffix(pkg, "/")
			if pkg == "./" {
				pkg = "."
			}
			pkgs[pkg] = struct{}{}
		case ClassDocs:
			hasDocs = true
		case ClassGenerated:
			hasGen = true
		}
	}

	budgets := DefaultBudgets()
	var included []Command
	reasons := map[string]string{}
	excluded := []string{
		"go test ./... (full suite → remote CI)",
		"full race suite → remote CI",
		"security/govulncheck/gosec → remote CI",
		"packaging/signing/release smoke → remote CI",
		"provider probes → remote/canary",
	}

	// formatting / gofmt for go
	if hasGo {
		var pkgList []string
		for p := range pkgs {
			pkgList = append(pkgList, p)
		}
		sort.Strings(pkgList)
		// focused tests per package (not ./...)
		for _, p := range pkgList {
			if p == "./..." || p == "..." {
				continue
			}
			argv := []string{"go", "test", p, "-count=1"}
			if IsDenied(argv) {
				return Plan{}, fmt.Errorf("%w: %v", ErrDenied, argv)
			}
			cmd := Command{
				Name: "focused_go_test", Argv: argv, Scope: p, Budgets: budgets,
			}
			cmd.Digest = cmdDigest(cmd)
			included = append(included, cmd)
			reasons[cmd.Digest] = "go package changed: " + p
		}
		// gofmt check scoped
		argv := []string{"gofmt", "-l"}
		for _, f := range changed {
			if Classify(f) == ClassGo {
				argv = append(argv, f)
			}
		}
		cmd := Command{Name: "gofmt_list", Argv: argv, Scope: "changed_go", Budgets: budgets}
		cmd.Digest = cmdDigest(cmd)
		included = append(included, cmd)
		reasons[cmd.Digest] = "format check on changed go files"
	}
	if hasDocs {
		cmd := Command{
			Name: "docs_noop_lint", Argv: []string{"true"}, Scope: "docs", Budgets: budgets,
		}
		cmd.Digest = cmdDigest(cmd)
		included = append(included, cmd)
		reasons[cmd.Digest] = "docs-only changes: skip heavy go tests"
	}
	if hasGen {
		cmd := Command{
			Name: "generated_consistency", Argv: []string{"true"}, Scope: "generated", Budgets: budgets,
		}
		cmd.Digest = cmdDigest(cmd)
		included = append(included, cmd)
		reasons[cmd.Digest] = "generated files present: lightweight consistency stub"
	}

	// package build for go packages (bounded)
	if hasGo {
		argv := []string{"go", "build", "-o", "/dev/null"}
		// build only first package to keep budget — still explicit
		var pkgList []string
		for p := range pkgs {
			pkgList = append(pkgList, p)
		}
		sort.Strings(pkgList)
		if len(pkgList) > 0 {
			argv = append(argv, pkgList[0])
			if !IsDenied(argv) {
				cmd := Command{Name: "package_build", Argv: argv, Scope: pkgList[0], Budgets: budgets}
				cmd.Digest = cmdDigest(cmd)
				included = append(included, cmd)
				reasons[cmd.Digest] = "package build for " + pkgList[0]
			}
		}
	}

	sort.Slice(included, func(i, j int) bool { return included[i].Digest < included[j].Digest })
	p := Plan{
		Schema: SchemaPlan, Included: included, Excluded: excluded,
		Reasons: reasons, FileClass: fc,
	}
	p.Digest = planDigest(p)
	// final deny scan
	for _, c := range p.Included {
		if IsDenied(c.Argv) {
			return Plan{}, fmt.Errorf("%w: plan contained denied %v", ErrDenied, c.Argv)
		}
	}
	return p, nil
}

// RecordResult builds evidence for one command execution (caller supplies exit/duration).
func RecordResult(cmd Command, exit int, dur time.Duration, output []byte, timedOut bool) Result {
	h := sha256.Sum256(output)
	blocked := exit != 0 || timedOut
	return Result{
		Schema: SchemaResult, Command: cmd, ExitCode: exit, Duration: dur,
		OutputDigest: "sha256:" + hex.EncodeToString(h[:])[:16],
		TimedOut:     timedOut, Blocked: blocked,
	}
}

// PlanBlocksDelivery is true if any result blocks.
func PlanBlocksDelivery(results []Result) bool {
	for _, r := range results {
		if r.Blocked {
			return true
		}
	}
	return false
}

func cmdDigest(c Command) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%v", c.Name, c.Scope, c.Argv)
	return "sha256:" + hex.EncodeToString(h.Sum(nil))[:16]
}

func planDigest(p Plan) string {
	h := sha256.New()
	for _, c := range p.Included {
		fmt.Fprintf(h, "%s;", c.Digest)
	}
	for _, e := range p.Excluded {
		fmt.Fprintf(h, "x:%s;", e)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))[:20]
}
