package depthpolicy_test

import (
	"testing"

	"github.com/jasonhnd/loopcoder/internal/depthpolicy"
)

func TestSelectNeverForcesHighForTiny(t *testing.T) {
	d, err := depthpolicy.Select(depthpolicy.DifficultyTiny, []string{"low", "medium", "high"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if d != "low" {
		t.Fatalf("got %s want low", d)
	}
}

func TestSelectStandardMedium(t *testing.T) {
	d, err := depthpolicy.Select(depthpolicy.DifficultyStandard, []string{"low", "medium", "high", "xhigh"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if d != "medium" {
		t.Fatalf("got %s", d)
	}
}

func TestSelectHardHigh(t *testing.T) {
	d, err := depthpolicy.Select(depthpolicy.DifficultyHard, []string{"low", "medium", "high"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if d != "high" {
		t.Fatalf("got %s", d)
	}
}

func TestExplicitPinFailClosedWhenUnsupported(t *testing.T) {
	_, err := depthpolicy.Select(depthpolicy.DifficultyStandard, []string{"low", "medium"}, "xhigh")
	if err == nil {
		t.Fatal("expected fail closed on unsupported pin")
	}
}

func TestNearestLowerWhenPreferredMissing(t *testing.T) {
	d, err := depthpolicy.Select(depthpolicy.DifficultyHard, []string{"low", "medium"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if d != "medium" {
		t.Fatalf("got %s want medium (nearest lower than high)", d)
	}
}

func TestClassifyDifficulty(t *testing.T) {
	if depthpolicy.ClassifyDifficulty("fix README typo") != depthpolicy.DifficultyTiny {
		t.Fatal("docs/typo")
	}
	if depthpolicy.ClassifyDifficulty("security audit of auth") != depthpolicy.DifficultyHard {
		t.Fatal("security")
	}
	if depthpolicy.ClassifyDifficulty("implement feature X") != depthpolicy.DifficultyStandard {
		t.Fatal("standard")
	}
}
