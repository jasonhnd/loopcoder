package obsplan_test

import (
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/obsplan"
	"github.com/jasonhnd/loopcoder/internal/providerdesc"
)

func t0() time.Time { return time.Date(2026, 7, 22, 19, 0, 0, 0, time.UTC) }

func TestOrderedPlanSelectsFirstOK(t *testing.T) {
	plan := obsplan.DefaultPlan("fake", providerdesc.OpDiscover)
	// authority order: api_optional (90) > cli_primary (80) > ...
	if plan.Steps[0].Name != "api_optional" {
		t.Fatalf("first=%s", plan.Steps[0].Name)
	}
	ex := &obsplan.Executor{
		Now: t0, ScrubEnv: true,
		Runner: func(st obsplan.SourceStep) (obsplan.StepOutcome, map[string]string, string, string) {
			if st.Name == "api_optional" {
				return obsplan.OutcomeTimeout, nil, "E_TIMEOUT", "api timed out"
			}
			if st.Name == "cli_primary" {
				return obsplan.OutcomeOK, map[string]string{"present": "true"}, "", "cli ok"
			}
			return obsplan.OutcomeSkipped, nil, "not_reached", ""
		},
	}
	snap, err := ex.Run(plan)
	if err != nil {
		t.Fatal(err)
	}
	if snap.SelectedSource != "cli_primary" {
		t.Fatalf("selected=%s", snap.SelectedSource)
	}
	if snap.Facts["present"] != "true" {
		t.Fatalf("facts=%v", snap.Facts)
	}
	// timeout diagnostic preserved, not a fact
	foundTimeout := false
	for _, d := range snap.Diagnostics {
		if d == "api_optional:E_TIMEOUT" || d == "api_optional:timeout" {
			foundTimeout = true
		}
	}
	if !foundTimeout {
		// also check step outcome
		for _, s := range snap.Steps {
			if s.StepName == "api_optional" && s.Outcome == obsplan.OutcomeTimeout && !s.IsFact {
				foundTimeout = true
			}
		}
	}
	if !foundTimeout {
		t.Fatalf("diagnostics=%v steps=%+v", snap.Diagnostics, snap.Steps)
	}
	if snap.Digest == "" {
		t.Fatal("digest")
	}
}

func TestDistinctFailuresNotNormalized(t *testing.T) {
	for _, oc := range obsplan.DistinctOutcomes() {
		plan := obsplan.Plan{
			Schema: obsplan.SchemaPlan, AdapterID: "x", Capability: providerdesc.OpQuota,
			Steps: []obsplan.SourceStep{{
				Name: "only", Kind: obsplan.SourceCLI, Authority: 1, Safety: 1,
				Bounds: obsplan.DefaultBounds(), Capability: providerdesc.OpQuota,
			}},
			StopOnFirstOK: true,
		}
		ex := &obsplan.Executor{Now: t0, ScrubEnv: true, Runner: func(obsplan.SourceStep) (obsplan.StepOutcome, map[string]string, string, string) {
			return oc, map[string]string{"quota": "0"}, "D", "fail"
		}}
		snap, err := ex.Run(plan)
		if err != nil {
			t.Fatal(err)
		}
		// Failed outcomes must not populate facts from the discarded map
		if len(snap.Facts) != 0 {
			t.Fatalf("outcome %s leaked facts %v", oc, snap.Facts)
		}
		if snap.Steps[0].IsFact {
			t.Fatalf("outcome %s marked as fact", oc)
		}
	}
}

func TestDedupAndNovelSnapshot(t *testing.T) {
	plan := obsplan.Plan{
		AdapterID: "fake", Capability: providerdesc.OpCatalog, StopOnFirstOK: true,
		Steps: []obsplan.SourceStep{{
			Name: "cli", Kind: obsplan.SourceCLI, Authority: 1, Safety: 1,
			Bounds: obsplan.DefaultBounds(), Capability: providerdesc.OpCatalog,
		}},
	}
	mk := func(model string) obsplan.Snapshot {
		ex := &obsplan.Executor{Now: t0, ScrubEnv: true, Runner: func(obsplan.SourceStep) (obsplan.StepOutcome, map[string]string, string, string) {
			return obsplan.OutcomeOK, map[string]string{"model": model}, "", "ok"
		}}
		s, err := ex.Run(plan)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	store := obsplan.NewStore()
	s1, novel, err := store.Persist(mk("m1"))
	if err != nil || !novel {
		t.Fatalf("n1 %v %v", novel, err)
	}
	s2, novel, err := store.Persist(mk("m1"))
	if err != nil || novel {
		t.Fatalf("dedup expected novel=false got %v err=%v", novel, err)
	}
	if s1.Digest != s2.Digest {
		t.Fatal("digest mismatch on dedup")
	}
	s3, novel, err := store.Persist(mk("m2"))
	if err != nil || !novel {
		t.Fatalf("n3 %v %v", novel, err)
	}
	if s3.Digest == s1.Digest {
		t.Fatal("expected new digest")
	}
	// digest can be copied into route event
	if len(s3.Digest) < 10 {
		t.Fatal(s3.Digest)
	}
}

func TestByteStableReplay(t *testing.T) {
	plan := obsplan.DefaultPlan("fake", providerdesc.OpDiscover)
	runner := func(st obsplan.SourceStep) (obsplan.StepOutcome, map[string]string, string, string) {
		if st.Kind == obsplan.SourceCLI || st.Name == "cli_primary" {
			return obsplan.OutcomeOK, map[string]string{"present": "true", "install": "fixture"}, "", "cli"
		}
		if st.Kind == obsplan.SourceAPI {
			return obsplan.OutcomeTimeout, nil, "E_TIMEOUT", "timeout"
		}
		return obsplan.OutcomeSkipped, nil, "skip", "skip"
	}
	ex := &obsplan.Executor{Now: t0, ScrubEnv: true, Runner: runner}
	a, err := ex.Run(plan)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ex.Run(plan)
	if err != nil {
		t.Fatal(err)
	}
	if a.Digest != b.Digest {
		t.Fatalf("digest %s vs %s", a.Digest, b.Digest)
	}
	if a.Explanation != b.Explanation {
		t.Fatalf("expl %q vs %q", a.Explanation, b.Explanation)
	}
}

func TestSecretFactsRejected(t *testing.T) {
	plan := obsplan.Plan{
		AdapterID: "fake", Capability: providerdesc.OpAuthStatus, StopOnFirstOK: true,
		Steps: []obsplan.SourceStep{{
			Name: "auth", Kind: obsplan.SourceAuthMeta, Authority: 1, Safety: 1,
			Bounds: obsplan.DefaultBounds(), Capability: providerdesc.OpAuthStatus,
		}},
	}
	ex := &obsplan.Executor{Now: t0, ScrubEnv: true, Runner: func(obsplan.SourceStep) (obsplan.StepOutcome, map[string]string, string, string) {
		return obsplan.OutcomeOK, map[string]string{"token": "sk-abc123456789"}, "", "leak"
	}}
	snap, err := ex.Run(plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Facts) != 0 {
		t.Fatalf("secret fact stored: %v", snap.Facts)
	}
	if snap.Steps[0].Outcome != obsplan.OutcomeMalformed {
		t.Fatalf("outcome=%s", snap.Steps[0].Outcome)
	}
}

func TestScrubEnv(t *testing.T) {
	in := []string{"PATH=/usr/bin", "GITHUB_TOKEN=ghp_xxx", "GIT_DIR=/tmp/x", "FOO=bar"}
	out := obsplan.ScrubEnv(in)
	joined := ""
	for _, e := range out {
		joined += e + ";"
		if e == "GITHUB_TOKEN=ghp_xxx" || e == "GIT_DIR=/tmp/x" {
			t.Fatalf("not scrubbed: %v", out)
		}
	}
	if joined == "" || !contains(out, "PATH=/usr/bin") {
		t.Fatalf("%v", out)
	}
}

func TestRedirectPolicy(t *testing.T) {
	plan := obsplan.Plan{
		AdapterID: "x", Capability: providerdesc.OpDiscover, StopOnFirstOK: true,
		Steps: []obsplan.SourceStep{{
			Name: "bad", Kind: obsplan.SourceAPI, Authority: 1, Safety: 1,
			Bounds:     obsplan.Bounds{Timeout: time.Second, MaxOutputB: 10, AllowNetwork: true, AllowRedirects: true},
			Capability: providerdesc.OpDiscover,
		}},
	}
	ex := &obsplan.Executor{Now: t0, ScrubEnv: true, Runner: func(obsplan.SourceStep) (obsplan.StepOutcome, map[string]string, string, string) {
		return obsplan.OutcomeOK, map[string]string{"x": "y"}, "", ""
	}}
	if _, err := ex.Run(plan); err == nil {
		t.Fatal("expected redirect policy error")
	}
}

func contains(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}
