package deliverygate_test

import (
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/attention"
	"github.com/jasonhnd/loopcoder/internal/deliverygate"
	"github.com/jasonhnd/loopcoder/internal/uireport"
	"github.com/jasonhnd/loopcoder/internal/uisub"
)

func t0() time.Time { return time.Date(2026, 7, 22, 16, 0, 0, 0, time.UTC) }

func startEnv() uireport.Envelope {
	e, err := uireport.Project(uireport.Input{
		Kind: uireport.KindStart, ProjectID: "proj", AttemptID: "a1", Sequence: 1,
		Stage: "start", Status: "starting", Liveness: "alive", RecordedAt: t0(),
	})
	if err != nil {
		panic(err)
	}
	return e
}

func setupLedger(clients ...string) *uisub.Ledger {
	l := uisub.NewLedger("proj", 32, t0)
	for _, c := range clients {
		_ = l.RegisterClient(uisub.ClientIdentity{
			ClientID: c, SessionID: "s", ProjectID: "proj", Required: true,
		})
	}
	return l
}

func TestPreLaunchRequiresStartRendered(t *testing.T) {
	l := setupLedger("term")
	env := startEnv()
	_ = l.Publish(env)
	g, err := deliverygate.New(deliverygate.Snapshot{
		ProjectID: "proj", AttemptID: "a1",
		RequiredClients:    []deliverygate.ClientSpec{{ClientID: "term", Required: true, Mode: "terminal"}},
		MissedReportPolicy: deliverygate.MissedStop,
	}, l, nil, t0)
	if err != nil {
		t.Fatal(err)
	}
	// no start recorded
	d := g.PreLaunch()
	if d.AllowLaunch || d.Reason != "start_report_missing" {
		t.Fatalf("%+v", d)
	}
	g.RecordStartReport(env.EventID, env.ContentDigest, env.Sequence)
	// connected but not rendered
	d = g.PreLaunch()
	if d.AllowLaunch || d.Reason != "start_not_rendered" {
		t.Fatalf("%+v", d)
	}
	// render ack
	if err := l.Acknowledge(uisub.Ack{
		ClientID: "term", EventID: env.EventID, Digest: env.ContentDigest, Stage: uisub.StageRendered,
	}); err != nil {
		t.Fatal(err)
	}
	d = g.PreLaunch()
	if !d.AllowLaunch || d.State != deliverygate.StateLive || d.ActiveClient != "term" {
		t.Fatalf("%+v", d)
	}
}

func TestMissingRequiredUIBlocksLaunch(t *testing.T) {
	l := setupLedger() // no clients
	env := startEnv()
	_ = l.Publish(env)
	g, _ := deliverygate.New(deliverygate.Snapshot{
		ProjectID: "proj", AttemptID: "a1",
		RequiredClients: []deliverygate.ClientSpec{{ClientID: "term", Required: true, Mode: "terminal"}},
	}, l, nil, t0)
	g.RecordStartReport(env.EventID, env.ContentDigest, 1)
	d := g.PreLaunch()
	if d.AllowLaunch || d.Reason != "no_required_ui_connected" {
		t.Fatalf("%+v", d)
	}
}

func TestFallbackNamedAndRendered(t *testing.T) {
	l := setupLedger("bridge") // only fallback connected
	env := startEnv()
	_ = l.Publish(env)
	g, _ := deliverygate.New(deliverygate.Snapshot{
		ProjectID: "proj", AttemptID: "a1",
		RequiredClients:  []deliverygate.ClientSpec{{ClientID: "term", Required: true, Mode: "terminal"}},
		AllowedFallbacks: []string{"bridge"},
	}, l, nil, t0)
	g.RecordStartReport(env.EventID, env.ContentDigest, 1)
	// fallback connected but not rendered
	d := g.PreLaunch()
	if d.AllowLaunch || d.Reason != "fallback_start_not_rendered" {
		t.Fatalf("%+v", d)
	}
	_ = l.Acknowledge(uisub.Ack{
		ClientID: "bridge", EventID: env.EventID, Digest: env.ContentDigest, Stage: uisub.StageRendered,
	})
	d = g.PreLaunch()
	if !d.AllowLaunch || d.FallbackUsed != "bridge" {
		t.Fatalf("%+v", d)
	}
}

func TestMissedIntervalsDegradeThenStop(t *testing.T) {
	l := setupLedger("term")
	env := startEnv()
	_ = l.Publish(env)
	att := attention.NewStore(t0)
	g, _ := deliverygate.New(deliverygate.Snapshot{
		ProjectID: "proj", RunID: "r1", AttemptID: "a1",
		RequiredClients:    []deliverygate.ClientSpec{{ClientID: "term", Required: true, Mode: "terminal"}},
		MissedReportPolicy: deliverygate.MissedStop,
	}, l, att, t0)
	g.RecordStartReport(env.EventID, env.ContentDigest, 1)
	_ = l.Acknowledge(uisub.Ack{
		ClientID: "term", EventID: env.EventID, Digest: env.ContentDigest, Stage: uisub.StageRendered,
	})
	if d := g.PreLaunch(); !d.AllowLaunch {
		t.Fatal(d)
	}
	// next mandatory without ack
	g.NoteReportGenerated(2)
	d1 := g.OnMandatoryInterval()
	if d1.State != deliverygate.StateDegraded {
		t.Fatalf("%+v", d1)
	}
	if len(att.ListOpen("proj")) != 1 {
		t.Fatal("expected attention on first miss")
	}
	g.NoteReportGenerated(3)
	d2 := g.OnMandatoryInterval()
	if d2.State != deliverygate.StateStopped || d2.Reason != "two_missed_stop" {
		t.Fatalf("%+v", d2)
	}
}

func TestMissedDetachPolicy(t *testing.T) {
	l := setupLedger("term")
	env := startEnv()
	_ = l.Publish(env)
	g, _ := deliverygate.New(deliverygate.Snapshot{
		ProjectID: "proj", AttemptID: "a1",
		RequiredClients:    []deliverygate.ClientSpec{{ClientID: "term", Required: true}},
		MissedReportPolicy: deliverygate.MissedDetach,
	}, l, nil, t0)
	g.RecordStartReport(env.EventID, env.ContentDigest, 1)
	_ = l.Acknowledge(uisub.Ack{
		ClientID: "term", EventID: env.EventID, Digest: env.ContentDigest, Stage: uisub.StageRendered,
	})
	_ = g.PreLaunch()
	g.NoteReportGenerated(2)
	_ = g.OnMandatoryInterval()
	g.NoteReportGenerated(3)
	d := g.OnMandatoryInterval()
	if d.State != deliverygate.StateDetached {
		t.Fatalf("%+v", d)
	}
}

func TestReportsAndCleanupIndependentOfUI(t *testing.T) {
	l := setupLedger("term")
	env := startEnv()
	_ = l.Publish(env)
	g, _ := deliverygate.New(deliverygate.Snapshot{
		ProjectID: "proj", AttemptID: "a1",
		RequiredClients: []deliverygate.ClientSpec{{ClientID: "term", Required: true}},
	}, l, nil, t0)
	g.RecordStartReport(env.EventID, env.ContentDigest, 1)
	g.NoteReportGenerated(2)
	g.NoteReportGenerated(3)
	if g.ReportsGenerated() != 3 {
		t.Fatalf("reports=%d", g.ReportsGenerated())
	}
	g.MarkCleanupDone()
	if !g.CleanupDone() {
		t.Fatal("cleanup should be independent of UI acks")
	}
}

func TestReconnectClearsDegradation(t *testing.T) {
	l := setupLedger("term")
	env := startEnv()
	_ = l.Publish(env)
	g, _ := deliverygate.New(deliverygate.Snapshot{
		ProjectID: "proj", AttemptID: "a1",
		RequiredClients: []deliverygate.ClientSpec{{ClientID: "term", Required: true}},
	}, l, nil, t0)
	g.RecordStartReport(env.EventID, env.ContentDigest, 1)
	_ = l.Acknowledge(uisub.Ack{
		ClientID: "term", EventID: env.EventID, Digest: env.ContentDigest, Stage: uisub.StageRendered,
	})
	_ = g.PreLaunch()
	g.NoteReportGenerated(2)
	_ = g.OnMandatoryInterval() // degrade
	// reconnect with matching start rendered evidence
	d := g.Reconnect("term", env.EventID, env.ContentDigest)
	if d.State != deliverygate.StateLive || d.Reason != "reconnect_ack_cleared" {
		t.Fatalf("%+v", d)
	}
}

func TestNoInventedFallback(t *testing.T) {
	l := setupLedger() // nothing connected
	env := startEnv()
	_ = l.Publish(env)
	g, _ := deliverygate.New(deliverygate.Snapshot{
		ProjectID: "proj", AttemptID: "a1",
		RequiredClients:  []deliverygate.ClientSpec{{ClientID: "term", Required: true}},
		AllowedFallbacks: []string{"bridge"}, // named but not connected
	}, l, nil, t0)
	g.RecordStartReport(env.EventID, env.ContentDigest, 1)
	d := g.PreLaunch()
	if d.AllowLaunch {
		t.Fatal("must not invent fallback from host")
	}
}
