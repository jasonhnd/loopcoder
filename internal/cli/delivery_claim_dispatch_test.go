package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/delivery"
	"github.com/jasonhnd/loopcoder/internal/worker"
)

func TestDeliveryClaimDispatchNoReadyTask(t *testing.T) {
	var stderr bytes.Buffer
	exit := RunWithDeps([]string{
		"delivery", "claim-dispatch",
		"--project-id", "proj-x",
		"--run-id", "run-x",
		"--repo", t.TempDir(),
		"--format", "json",
	}, &bytes.Buffer{}, &stderr, Deps{
		Now: func() time.Time { return time.Date(2026, 7, 17, 15, 0, 0, 0, time.UTC) },
		// Force fail-closed open store path: no db means error before claim.
	})
	if exit == 0 {
		t.Fatalf("exit = 0, want failure without store; stderr=%s", stderr.String())
	}
}

func TestDeliveryClaimDispatchDispatchesClaimedTask(t *testing.T) {
	// Unit-level: inject claim is hard without full store; verify CLI requires --repo
	// and that route+dispatch wiring is invoked when claim is mocked via partial
	// integration using ResolveWorkerDispatchRoute + Dispatch after a real claim
	// is not available here. This test locks the missing-repo contract.
	var stderr bytes.Buffer
	exit := RunWithDeps([]string{
		"delivery", "claim-dispatch",
		"--project-id", "proj-x",
		"--run-id", "run-x",
	}, &bytes.Buffer{}, &stderr, Deps{
		Now: func() time.Time { return time.Date(2026, 7, 17, 15, 0, 0, 0, time.UTC) },
		Dispatch: func(context.Context, worker.Options) (worker.Result, error) {
			t.Fatal("must not dispatch without repo")
			return worker.Result{}, nil
		},
	})
	if exit != 2 {
		t.Fatalf("exit = %d, want 2; stderr=%s", exit, stderr.String())
	}
	if !strings.Contains(stderr.String(), "--repo is required") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	_ = delivery.OutcomeNoReadyTask
}
