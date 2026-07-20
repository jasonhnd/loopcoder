package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/routing"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

func TestRouteReadOnlyExplainUsesFailClosedStore(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "route.db")
	seed, err := storage.Open(ctx, storage.Options{Path: path, Now: fixedCLINow})
	if err != nil {
		t.Fatalf("seed route database: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close route seed: %v", err)
	}

	called := false
	deps := Deps{
		Now: fixedCLINow,
		RouteExplain: func(ctx context.Context, store storage.Store, _ routing.StoredRouteRequest) (routing.RouteOperationResult, error) {
			called = true
			if err := store.WithWriteTx(ctx, func(storage.Tx) error { return nil }); !errors.Is(err, storage.ErrReadOnlyStore) {
				t.Fatalf("explain WithWriteTx error = %v, want ErrReadOnlyStore", err)
			}
			if err := store.WithTx(ctx, func(tx storage.Tx) error {
				_, err := tx.Exec(ctx, `DELETE FROM migrations`)
				return err
			}); !errors.Is(err, storage.ErrReadOnlyStore) {
				t.Fatalf("explain Tx.Exec error = %v, want ErrReadOnlyStore", err)
			}
			return routeReadOnlyCLIResult(routing.RouteOperationExplain), nil
		},
	}
	var stdout, stderr bytes.Buffer
	code := RunWithDeps(routeReadOnlyCLIArgs("explain", path), &stdout, &stderr, deps)
	if code != 0 || !called {
		t.Fatalf("route explain = code %d called %t stderr=%s", code, called, stderr.String())
	}
}

func TestRouteReadOnlyExplainDoesNotCreateMissingDatabase(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "missing")
	path := filepath.Join(parent, "route.db")
	called := false
	deps := Deps{
		Now: fixedCLINow,
		RouteExplain: func(context.Context, storage.Store, routing.StoredRouteRequest) (routing.RouteOperationResult, error) {
			called = true
			return routeReadOnlyCLIResult(routing.RouteOperationExplain), nil
		},
	}
	var stdout, stderr bytes.Buffer
	code := RunWithDeps(routeReadOnlyCLIArgs("explain", path), &stdout, &stderr, deps)
	if code != 1 || called || !strings.Contains(stderr.String(), "database does not exist") {
		t.Fatalf("missing route explain = code %d called %t stderr=%s", code, called, stderr.String())
	}
	if _, err := os.Lstat(parent); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("route explain parent stat = %v, want no path creation", err)
	}
}

func TestRouteReadOnlyDecideRetainsWritableOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "route.db")
	called := false
	deps := Deps{
		Now: fixedCLINow,
		RouteDecide: func(ctx context.Context, store storage.Store, _ routing.StoredRouteRequest) (routing.RouteOperationResult, error) {
			called = true
			if err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
				_, err := tx.Exec(ctx, `CREATE TABLE route_decide_write_probe (id TEXT PRIMARY KEY)`)
				return err
			}); err != nil {
				t.Fatalf("decide write transaction: %v", err)
			}
			return routeReadOnlyCLIResult(routing.RouteOperationDecide), nil
		},
	}
	var stdout, stderr bytes.Buffer
	code := RunWithDeps(routeReadOnlyCLIArgs("decide", path), &stdout, &stderr, deps)
	if code != 0 || !called {
		t.Fatalf("route decide = code %d called %t stderr=%s", code, called, stderr.String())
	}
	store, err := storage.OpenReadOnly(context.Background(), storage.Options{Path: path, Now: fixedCLINow})
	if err != nil {
		t.Fatalf("reopen decide database read-only: %v", err)
	}
	defer store.Close()
	var count int
	if err := store.WithTx(context.Background(), func(tx storage.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'route_decide_write_probe'`).Scan(&count)
	}); err != nil {
		t.Fatalf("query decide write probe: %v", err)
	}
	if count != 1 {
		t.Fatalf("decide write probe count = %d, want 1", count)
	}
}

func routeReadOnlyCLIArgs(operation, path string) []string {
	return []string{
		"route", operation, "--db", path, "--project-id", "proj-route", "--run-id", "drun-route",
		"--task-requirement-id", "treq-route", "--decision-key", "attempt-read-only", "--format", "json",
	}
}

func routeReadOnlyCLIResult(operation string) routing.RouteOperationResult {
	return routing.RouteOperationResult{
		SchemaVersion: routing.RouteOperationSchema,
		Operation:     operation,
		Outcome:       routing.RouteOutcomeSelected,
		Decision: routing.RoutingDecision{
			SchemaVersion: routing.DecisionSchema, RecordVersion: 1, RoutingDecisionID: "rdec_route_read_only",
			DecisionKey: "route-read-only", DecisionKind: routing.DecisionKindRouting, DecisionStatus: routing.DecisionStatusSelected,
			ProjectID: "proj-route", DeliveryRunID: "drun-route", TaskID: "task-route", TaskRequirementID: "treq-route",
			ChosenCandidateID: "rcand_route_read_only", ChosenReason: "read-only store verified",
		},
	}
}
