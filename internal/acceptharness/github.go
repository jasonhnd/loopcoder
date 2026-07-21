package acceptharness

import (
	"context"
	"fmt"
	"sync"
)

// CheckStatus is a simplified hosted check state.
type CheckStatus string

const (
	CheckSuccess CheckStatus = "success"
	CheckPending CheckStatus = "pending"
	CheckFailed  CheckStatus = "failed"
)

// IssueFixture is synthetic GitHub issue data.
type IssueFixture struct {
	Number int
	Title  string
	Body   string
}

// PRFixture is synthetic pull request data.
type PRFixture struct {
	Number int
	Title  string
	Head   string
	Base   string
	SHA    string
}

// CheckFixture is one named check on a PR head.
type CheckFixture struct {
	Name   string
	Status CheckStatus
}

// FakeGitHub is an in-memory GitHub client with ordered evidence and fault
// injection. It never performs network IO.
type FakeGitHub struct {
	mu sync.Mutex

	Issues map[int]IssueFixture
	PRs    map[int]PRFixture
	Checks map[int][]CheckFixture // keyed by PR number

	// PushTimeout when true causes CreatePR to fail with a push timeout once.
	PushTimeout bool
	// DuplicateCreate when true returns the same PR for repeated CreatePR.
	DuplicateCreate bool
	// nextPR is the next PR number.
	nextPR int
	// createCount counts CreatePR calls.
	createCount int
	// lastError is sticky for assertions.
	lastError error
}

// NewFakeGitHub returns an empty client.
func NewFakeGitHub() *FakeGitHub {
	return &FakeGitHub{
		Issues: map[int]IssueFixture{},
		PRs:    map[int]PRFixture{},
		Checks: map[int][]CheckFixture{},
		nextPR: 1,
	}
}

// SeedIssue stores a synthetic issue.
func (g *FakeGitHub) SeedIssue(issue IssueFixture) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.Issues[issue.Number] = issue
}

// CreatePR records a PR for head/base/sha. Honors push timeout and duplicate modes.
func (g *FakeGitHub) CreatePR(ctx context.Context, title, head, base, sha string) (PRFixture, error) {
	if err := ctx.Err(); err != nil {
		return PRFixture{}, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.createCount++
	if g.PushTimeout {
		g.PushTimeout = false
		g.lastError = fmt.Errorf("push timeout: synthetic fixture timeout")
		return PRFixture{}, g.lastError
	}
	if g.DuplicateCreate && g.createCount > 1 {
		for _, pr := range g.PRs {
			if pr.Head == head && pr.Base == base {
				return pr, nil
			}
		}
	}
	pr := PRFixture{
		Number: g.nextPR,
		Title:  title,
		Head:   head,
		Base:   base,
		SHA:    sha,
	}
	g.nextPR++
	g.PRs[pr.Number] = pr
	g.Checks[pr.Number] = []CheckFixture{
		{Name: "verify", Status: CheckPending},
		{Name: "test", Status: CheckPending},
		{Name: "race", Status: CheckPending},
		{Name: "security", Status: CheckPending},
	}
	return pr, nil
}

// SetChecks replaces check states for a PR.
func (g *FakeGitHub) SetChecks(prNumber int, checks []CheckFixture) {
	g.mu.Lock()
	defer g.mu.Unlock()
	cp := make([]CheckFixture, len(checks))
	copy(cp, checks)
	g.Checks[prNumber] = cp
}

// ListChecks returns check fixtures for a PR.
func (g *FakeGitHub) ListChecks(prNumber int) []CheckFixture {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]CheckFixture, len(g.Checks[prNumber]))
	copy(out, g.Checks[prNumber])
	return out
}

// CreateCount returns how many CreatePR calls occurred.
func (g *FakeGitHub) CreateCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.createCount
}
