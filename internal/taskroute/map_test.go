package taskroute_test

import (
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/capclass"
	"github.com/jasonhnd/loopcoder/internal/depthpolicy"
	"github.com/jasonhnd/loopcoder/internal/taskroute"
)

func t0() time.Time { return time.Date(2026, 7, 22, 18, 0, 0, 0, time.UTC) }

func TestClassifyRunDocsTiny(t *testing.T) {
	rr, err := taskroute.ClassifyRun("proj", "12", "docs: fix README typo", "default", t0())
	if err != nil {
		t.Fatal(err)
	}
	// Documentation scope should not force soul; depth may be standard or tiny
	if rr.TaskClass == capclass.ClassNeedsHuman {
		t.Fatalf("%+v", rr)
	}
	if rr.Difficulty == depthpolicy.DifficultyHard {
		t.Fatalf("docs should not be hard: %+v", rr)
	}
}

func TestClassifyRunSecurityHard(t *testing.T) {
	rr, err := taskroute.ClassifyRun("proj", "99", "security: harden credential handling", "default", t0())
	if err != nil {
		t.Fatal(err)
	}
	if rr.TaskClass != capclass.ClassSoul && rr.Difficulty != depthpolicy.DifficultyHard {
		t.Fatalf("security should elevate class/depth: %+v risk=%s quality=%s", rr, rr.RiskTier, rr.QualityFloor)
	}
}

func TestClassifyRunStandard(t *testing.T) {
	rr, err := taskroute.ClassifyRun("proj", "7", "implement feature X", "default", t0())
	if err != nil {
		t.Fatal(err)
	}
	if rr.TaskClass == "" {
		t.Fatal("empty class")
	}
}
