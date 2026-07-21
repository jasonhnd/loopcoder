package attachowner_test

import (
	"errors"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/attachowner"
	"github.com/jasonhnd/loopcoder/internal/deliverygate"
)

func t0() time.Time { return time.Date(2026, 7, 22, 19, 0, 0, 0, time.UTC) }

func TestForegroundDefaultNoAutoDetach(t *testing.T) {
	s := attachowner.NewStore(t0)
	o, err := s.Start(attachowner.Spec{
		RunID: "r1", ProjectID: "p", AttemptID: "a", InvokerPID: 100,
		// mode empty → foreground
	})
	if err != nil {
		t.Fatal(err)
	}
	if o.Mode != attachowner.ModeForeground || o.Phase != attachowner.PhaseLive {
		t.Fatalf("%+v", o)
	}
	if _, err := s.NoteUIDisconnect("r1"); err != nil {
		t.Fatal(err)
	}
	if err := s.TryAutoDetach("r1"); !errors.Is(err, attachowner.ErrNoAutoDetach) {
		t.Fatalf("err=%v", err)
	}
	got, _ := s.Get("r1")
	if got.Mode != attachowner.ModeForeground || !got.UIDisconnected {
		t.Fatalf("%+v", got)
	}
	if got.ForegroundReturnOK() {
		t.Fatal("should not return until terminal")
	}
	done, err := s.Complete("r1", 0, true)
	if err != nil || !done.ForegroundReturnOK() || !done.ResourcesCleared() {
		t.Fatalf("%+v err=%v", done, err)
	}
}

func TestExplicitDetachRequiresPolicyAndEvidence(t *testing.T) {
	s := attachowner.NewStore(t0)
	_, err := s.Start(attachowner.Spec{
		RunID: "r2", ProjectID: "p", AttemptID: "a", Mode: attachowner.ModeDetached,
	})
	if !errors.Is(err, attachowner.ErrDetachRequiresUI) {
		t.Fatalf("err=%v", err)
	}
	o, err := s.Start(attachowner.Spec{
		RunID: "r3", ProjectID: "p", AttemptID: "a", Mode: attachowner.ModeDetached,
		RequiredClients: []deliverygate.ClientSpec{{ClientID: "term", Required: true, Mode: "terminal"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if o.DetachReady() {
		t.Fatal("not ready without supervisor pid")
	}
	o, err = s.MarkSupervisor("r3", 4242, "host1")
	if err != nil {
		t.Fatal(err)
	}
	if !o.DetachReady() {
		t.Fatalf("should be ready: %+v", o)
	}
	if o.CancelEndpoint == "" || o.Supervisor.Generation == 0 {
		t.Fatal("missing durable evidence")
	}
}

func TestStaleGenerationCannotHeartbeatOrComplete(t *testing.T) {
	s := attachowner.NewStore(t0)
	o, err := s.Start(attachowner.Spec{
		RunID: "r4", ProjectID: "p", AttemptID: "a", Mode: attachowner.ModeDetached,
		RequiredClients: []deliverygate.ClientSpec{{ClientID: "term", Required: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	o, err = s.MarkSupervisor("r4", 9, "h")
	if err != nil {
		t.Fatal(err)
	}
	gen := o.Supervisor.Generation
	if _, err := s.Heartbeat("r4", gen+1, 9); !errors.Is(err, attachowner.ErrStaleGeneration) {
		t.Fatalf("err=%v", err)
	}
	if _, err := s.Heartbeat("r4", gen, 8); !errors.Is(err, attachowner.ErrStaleGeneration) {
		t.Fatalf("err=%v", err)
	}
	if err := s.SignalCancel("r4", gen+1); !errors.Is(err, attachowner.ErrStaleGeneration) {
		t.Fatalf("err=%v", err)
	}
	if err := s.SignalCancel("r4", gen); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Complete("r4", gen+1, true); !errors.Is(err, attachowner.ErrStaleGeneration) {
		t.Fatalf("err=%v", err)
	}
	if _, err := s.Complete("r4", gen, true); err != nil {
		t.Fatal(err)
	}
}

func TestStatusWithoutOriginalUI(t *testing.T) {
	s := attachowner.NewStore(t0)
	_, err := s.Start(attachowner.Spec{
		RunID: "r5", ProjectID: "p", AttemptID: "a", InvokerPID: 1,
		RequiredClients: []deliverygate.ClientSpec{{ClientID: "term", Required: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// original UI gone
	_, _ = s.NoteUIDisconnect("r5")
	got, err := s.Get("r5")
	if err != nil || got.RunID != "r5" {
		t.Fatal(err)
	}
	// cancel through durable authority
	if err := s.SignalCancel("r5", 0); err != nil {
		t.Fatal(err)
	}
}
