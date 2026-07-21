package evidence

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadRequiredChecksFromDeliveryPolicy(t *testing.T) {
	required, err := LoadRequiredChecks([]byte(`
ci:
  checks: [verify, test, race, security]
`))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"race", "security", "test", "verify"}
	if !reflect.DeepEqual(required, want) {
		t.Fatalf("required = %#v, want %#v", required, want)
	}
}

func TestLoadRequiredChecksRejectsEmpty(t *testing.T) {
	if _, err := LoadRequiredChecks([]byte("ci: {}\n")); err == nil {
		t.Fatal("expected empty ci.checks to fail")
	}
}

func TestEvaluateMergeReadinessFixtures(t *testing.T) {
	required := []string{"verify", "test", "race", "security"}

	tests := []struct {
		name    string
		obs     []ObservedCheck
		ready   bool
		wantSub string
	}{
		{
			name: "all required green without greptile",
			obs: []ObservedCheck{
				{Name: "verify", Status: CheckStatusSuccess},
				{Name: "test", Status: CheckStatusSuccess},
				{Name: "race", Status: CheckStatusSuccess},
				{Name: "security", Status: CheckStatusSuccess},
			},
			ready: true,
		},
		{
			name: "optional greptile absent does not block",
			obs: []ObservedCheck{
				{Name: "verify", Status: CheckStatusSuccess},
				{Name: "test", Status: CheckStatusSuccess},
				{Name: "race", Status: CheckStatusSuccess},
				{Name: "security", Status: CheckStatusSuccess},
			},
			ready: true,
		},
		{
			name: "optional greptile failed does not block",
			obs: []ObservedCheck{
				{Name: "verify", Status: CheckStatusSuccess},
				{Name: "test", Status: CheckStatusSuccess},
				{Name: "race", Status: CheckStatusSuccess},
				{Name: "security", Status: CheckStatusSuccess},
				{Name: "Greptile Review", Status: CheckStatusFailed},
			},
			ready: true,
		},
		{
			name: "optional greptile pending does not block",
			obs: []ObservedCheck{
				{Name: "verify", Status: CheckStatusSuccess},
				{Name: "test", Status: CheckStatusSuccess},
				{Name: "race", Status: CheckStatusSuccess},
				{Name: "security", Status: CheckStatusSuccess},
				{Name: "Greptile Review", Status: CheckStatusPending},
			},
			ready: true,
		},
		{
			name: "required pending blocks",
			obs: []ObservedCheck{
				{Name: "verify", Status: CheckStatusSuccess},
				{Name: "test", Status: CheckStatusPending},
				{Name: "race", Status: CheckStatusSuccess},
				{Name: "security", Status: CheckStatusSuccess},
			},
			ready:   false,
			wantSub: "required check pending: test",
		},
		{
			name: "required failed blocks",
			obs: []ObservedCheck{
				{Name: "verify", Status: CheckStatusSuccess},
				{Name: "test", Status: CheckStatusSuccess},
				{Name: "race", Status: CheckStatusFailed},
				{Name: "security", Status: CheckStatusSuccess},
			},
			ready:   false,
			wantSub: "required check failed: race",
		},
		{
			name: "required missing blocks",
			obs: []ObservedCheck{
				{Name: "verify", Status: CheckStatusSuccess},
				{Name: "test", Status: CheckStatusSuccess},
				{Name: "race", Status: CheckStatusSuccess},
			},
			ready:   false,
			wantSub: "required check missing: security",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EvaluateMergeReadiness(required, tt.obs)
			if got.Ready != tt.ready {
				t.Fatalf("Ready = %v, want %v; reasons=%v", got.Ready, tt.ready, got.BlockingReasons)
			}
			if !got.OptionalAbsentOK {
				t.Fatal("OptionalAbsentOK must remain true for optional bots")
			}
			if tt.wantSub != "" {
				joined := strings.Join(got.BlockingReasons, "; ")
				if !strings.Contains(joined, tt.wantSub) {
					t.Fatalf("reasons %q missing %q", joined, tt.wantSub)
				}
			}
			if tt.ready && len(got.BlockingReasons) != 0 {
				t.Fatalf("ready evaluation still has blocking reasons: %v", got.BlockingReasons)
			}
		})
	}
}

func TestOptionalGreptileNeverBecomesRequiredByObservation(t *testing.T) {
	required := []string{"verify", "test", "race", "security"}
	got := EvaluateMergeReadiness(required, []ObservedCheck{
		{Name: "verify", Status: CheckStatusSuccess},
		{Name: "test", Status: CheckStatusSuccess},
		{Name: "race", Status: CheckStatusSuccess},
		{Name: "security", Status: CheckStatusSuccess},
		{Name: "Greptile Review", Status: CheckStatusMissing},
	})
	if !got.Ready {
		t.Fatalf("expected ready when only optional greptile is missing; reasons=%v", got.BlockingReasons)
	}
	if len(got.OptionalObserved) != 1 || got.OptionalObserved[0] != "Greptile Review" {
		t.Fatalf("optional observed = %#v", got.OptionalObserved)
	}
}

func TestLoadRequiredChecksFileMatchesRepositoryPolicy(t *testing.T) {
	root := findModuleRoot(t)
	required, err := LoadRequiredChecksFile(filepath.Join(root, ".delivery.yml"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"race", "security", "test", "verify"}
	if !reflect.DeepEqual(required, want) {
		t.Fatalf("repository required checks = %#v, want %#v", required, want)
	}
	// Greptile must not appear in the policy-derived required set.
	for _, name := range required {
		if strings.Contains(strings.ToLower(name), "greptile") {
			t.Fatalf("policy required checks unexpectedly include %q", name)
		}
	}
}

func findModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("module root not found")
		}
		dir = parent
	}
}
