package attention_test

import (
	"errors"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/attention"
)

func t0() time.Time { return time.Date(2026, 7, 22, 15, 0, 0, 0, time.UTC) }

func openNeeds(s *attention.Store, rev int64) attention.Attention {
	a, err := s.Open(attention.OpenInput{
		ProjectID:   "proj_a",
		RunID:       "run1",
		AttemptID:   "att1",
		RunRevision: rev,
		Kind:        attention.KindNeedsHuman,
		Severity:    attention.SeverityWarn,
		Reason:      "ci failed",
		PrivateBody: "secret issue body /Users/me/secret",
		Evidence:    map[string]string{"check": "test"},
	})
	if err != nil {
		panic(err)
	}
	return a
}

func baseReq(a attention.Attention, action attention.ActionType, idem string) attention.ActionRequest {
	return attention.ActionRequest{
		Schema:           attention.SchemaAction,
		AttentionID:      a.ID,
		ProjectID:        a.ProjectID,
		ClientID:         "ui1",
		SessionID:        "sess1",
		ExpectedRevision: a.RunRevision,
		IdempotencyKey:   idem,
		Action:           action,
	}
}

func TestIdempotentActionAndEvidenceBeforeEffect(t *testing.T) {
	s := attention.NewStore(t0)
	a := openNeeds(s, 3)
	req := baseReq(a, attention.ActionAcknowledge, "idem-1")
	r1, err := s.Submit(req)
	if err != nil {
		t.Fatal(err)
	}
	if r1.State != attention.StateAcknowledged || r1.EvidenceID == "" {
		t.Fatalf("%+v", r1)
	}
	r2, err := s.Submit(req)
	if err != nil {
		t.Fatal(err)
	}
	if !r2.Duplicate || r2.EvidenceID != r1.EvidenceID {
		t.Fatalf("idempotent want same evidence: %+v", r2)
	}
	ev, err := s.Evidence("proj_a", a.ID)
	if err != nil || len(ev) != 1 {
		t.Fatalf("evidence len=%d err=%v", len(ev), err)
	}
	if ev[0].BeforeState != attention.StateOpen || ev[0].AfterState != attention.StateAcknowledged {
		t.Fatalf("ev=%+v", ev[0])
	}
	// effect maps to runtime transition
	if r1.Effect != "runtime.attention.acknowledged" {
		t.Fatalf("effect=%s", r1.Effect)
	}
}

func TestStaleRevisionAndTerminal(t *testing.T) {
	s := attention.NewStore(t0)
	a := openNeeds(s, 5)
	req := baseReq(a, attention.ActionAcknowledge, "x")
	req.ExpectedRevision = 4
	if _, err := s.Submit(req); !errors.Is(err, attention.ErrStaleRevision) {
		t.Fatalf("err=%v", err)
	}
	req.ExpectedRevision = 5
	if _, err := s.Submit(req); err != nil {
		t.Fatal(err)
	}
	// resolve via cancel
	req2 := baseReq(a, attention.ActionCancel, "cancel-1")
	if _, err := s.Submit(req2); err != nil {
		t.Fatal(err)
	}
	req3 := baseReq(a, attention.ActionAcknowledge, "late")
	if _, err := s.Submit(req3); !errors.Is(err, attention.ErrAlreadyTerminal) {
		t.Fatalf("err=%v", err)
	}
}

func TestPermissionAuthAndUnsupported(t *testing.T) {
	s := attention.NewStore(t0)
	a, err := s.Open(attention.OpenInput{
		ProjectID: "proj_a", RunRevision: 1, Kind: attention.KindPermission,
		Reason: "need network", Evidence: map[string]string{"permission": "net.outbound"},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := baseReq(a, attention.ActionPermissionAllow, "p1")
	if _, err := s.Submit(req); !errors.Is(err, attention.ErrUnauthorized) {
		t.Fatalf("err=%v", err)
	}
	req.Authorization = "wrong"
	if _, err := s.Submit(req); !errors.Is(err, attention.ErrUnauthorized) {
		t.Fatalf("err=%v", err)
	}
	req.Authorization = "net.outbound"
	r, err := s.Submit(req)
	if err != nil || r.Effect != "runtime.permission.approved" {
		t.Fatalf("r=%+v err=%v", r, err)
	}
}

func TestForbiddenControlPlaneMutations(t *testing.T) {
	s := attention.NewStore(t0)
	a := openNeeds(s, 1)
	cases := []attention.ActionRequest{
		{AttentionID: a.ID, ProjectID: a.ProjectID, ClientID: "c", SessionID: "s",
			ExpectedRevision: 1, IdempotencyKey: "f1", Action: attention.ActionAcknowledge,
			Extra: map[string]string{"mutate_route": "true"}},
		{AttentionID: a.ID, ProjectID: a.ProjectID, ClientID: "c", SessionID: "s",
			ExpectedRevision: 1, IdempotencyKey: "f2", Action: attention.ActionType("force_complete")},
		{AttentionID: a.ID, ProjectID: a.ProjectID, ClientID: "c", SessionID: "s",
			ExpectedRevision: 1, IdempotencyKey: "f3", Action: attention.ActionType("bypass_admission")},
		{AttentionID: a.ID, ProjectID: a.ProjectID, ClientID: "c", SessionID: "s",
			ExpectedRevision: 1, IdempotencyKey: "f4", Action: attention.ActionType("kill_unowned")},
	}
	for _, req := range cases {
		if _, err := s.Submit(req); !errors.Is(err, attention.ErrForbidden) && !errors.Is(err, attention.ErrUnsupportedAction) {
			// force_complete etc are both forbidden and unsupported depending on path
			if err == nil {
				t.Fatalf("expected failure for %+v", req.Action)
			}
			// Forbidden detected first for explicit types
			if req.Action == attention.ActionType("force_complete") || req.Action == attention.ActionType("bypass_admission") || req.Action == attention.ActionType("kill_unowned") {
				if !errors.Is(err, attention.ErrForbidden) {
					t.Fatalf("action %s err=%v", req.Action, err)
				}
			}
		}
	}
}

func TestMachineIndexNoPrivateBody(t *testing.T) {
	s := attention.NewStore(t0)
	a := openNeeds(s, 1)
	idx := s.MachineIndex()
	if len(idx) != 1 {
		t.Fatalf("len=%d", len(idx))
	}
	if idx[0].ProjectID != "proj_a" || idx[0].Kind != attention.KindNeedsHuman {
		t.Fatalf("%+v", idx[0])
	}
	// full get keeps private for project
	got, err := s.Get("proj_a", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PrivateBody == "" {
		t.Fatal("project get should include private body")
	}
	// list open strips private
	list := s.ListOpen("proj_a")
	if len(list) != 1 || list[0].PrivateBody != "" {
		t.Fatalf("list private leaked: %+v", list)
	}
	// cross project
	if _, err := s.Get("other", a.ID); !errors.Is(err, attention.ErrCrossProject) {
		t.Fatalf("err=%v", err)
	}
}

func TestBoundedInputAndRecovery(t *testing.T) {
	s := attention.NewStore(t0)
	a, _ := s.Open(attention.OpenInput{
		ProjectID: "p", RunRevision: 2, Kind: attention.KindInputRequired, Reason: "need path",
	})
	req := baseReq(a, attention.ActionBoundedInput, "in1")
	req.ProjectID = "p"
	if _, err := s.Submit(req); !errors.Is(err, attention.ErrInvalid) {
		t.Fatalf("err=%v", err)
	}
	req.Input = "relative/path"
	r, err := s.Submit(req)
	if err != nil || r.State != attention.StateResolved {
		t.Fatalf("r=%+v err=%v", r, err)
	}

	s2 := attention.NewStore(t0)
	ar, _ := s2.Open(attention.OpenInput{
		ProjectID: "p", RunRevision: 1, Kind: attention.KindRecovery, Reason: "ambiguous pid",
	})
	rr := baseReq(ar, attention.ActionRecoverySelect, "rec1")
	rr.ProjectID = "p"
	rr.Recovery = "adopt"
	r2, err := s2.Submit(rr)
	if err != nil || r2.Effect != "runtime.recovery.selected" {
		t.Fatalf("r=%+v err=%v", r2, err)
	}
}

func TestSupersedeAndRebuildIndex(t *testing.T) {
	s := attention.NewStore(t0)
	a := openNeeds(s, 1)
	if err := s.Supersede("proj_a", a.ID, "replaced"); err != nil {
		t.Fatal(err)
	}
	idx := s.RebuildIndex()
	if len(idx) != 0 {
		t.Fatalf("superseded should leave index empty: %+v", idx)
	}
}

func TestUnsupportedNotInAllowed(t *testing.T) {
	s := attention.NewStore(t0)
	a, _ := s.Open(attention.OpenInput{
		ProjectID: "p", RunRevision: 1, Kind: attention.KindNeedsHuman, Reason: "x",
		AllowedActions: []attention.ActionType{attention.ActionAcknowledge},
	})
	req := baseReq(a, attention.ActionCancel, "c1")
	req.ProjectID = "p"
	if _, err := s.Submit(req); !errors.Is(err, attention.ErrUnsupportedAction) {
		t.Fatalf("err=%v", err)
	}
}
