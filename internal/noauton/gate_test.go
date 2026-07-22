package noauton_test

import (
	"testing"

	"github.com/jasonhnd/loopcoder/internal/noauton"
)

func TestDenyAutonomous(t *testing.T) {
	for _, ep := range []noauton.EntryPoint{
		noauton.EPCompile, noauton.EPTick, noauton.EPTrigger, noauton.EPPromote, noauton.EPIssueSynth,
	} {
		d := noauton.Evaluate(ep, true, true)
		if d.Allowed {
			t.Fatalf("%s allowed", ep)
		}
	}
}

func TestAllowBoundedAndHuman(t *testing.T) {
	d := noauton.Evaluate(noauton.EPBoundedWave, false, true)
	if !d.Allowed {
		t.Fatal(d)
	}
	d2 := noauton.Evaluate(noauton.EPBoundedWave, false, false)
	if d2.Allowed {
		t.Fatal("unbounded wave denied")
	}
	d3 := noauton.Evaluate(noauton.EPHumanGate, true, false)
	if !d3.Allowed {
		t.Fatal(d3)
	}
	d4 := noauton.Evaluate(noauton.EPHumanGate, false, false)
	if d4.Allowed {
		t.Fatal("human gate without human")
	}
}

func TestCLIAndMarkers(t *testing.T) {
	if !noauton.CLIDenied("compile") || !noauton.CLIDenied("tick") {
		t.Fatal()
	}
	if !noauton.RoadmapMarkerInert("V090-076") {
		t.Fatal()
	}
	if len(noauton.Inventory()) < 10 {
		t.Fatal()
	}
}
