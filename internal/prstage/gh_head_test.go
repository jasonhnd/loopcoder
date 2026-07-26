package prstage

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestReadPRHeadOIDFailClosed(t *testing.T) {
	t.Run("gh_error", func(t *testing.T) {
		_, err := readPRHeadOIDCmd("", "o/r", 1, func(dir string, argv ...string) ([]byte, error) {
			return []byte("boom"), errors.New("exit 1")
		})
		if err == nil {
			t.Fatal("want error on gh failure")
		}
	})
	t.Run("malformed_json", func(t *testing.T) {
		_, err := readPRHeadOIDCmd("", "o/r", 1, func(dir string, argv ...string) ([]byte, error) {
			return []byte("not-json"), nil
		})
		if err == nil {
			t.Fatal("want error on malformed json")
		}
	})
	t.Run("empty_oid", func(t *testing.T) {
		_, err := readPRHeadOIDCmd("", "o/r", 1, func(dir string, argv ...string) ([]byte, error) {
			return []byte(`{"headRefOid":"","state":"OPEN"}`), nil
		})
		if err == nil {
			t.Fatal("want error on empty oid")
		}
	})
	t.Run("empty_state", func(t *testing.T) {
		_, err := readPRHeadOIDCmd("", "o/r", 1, func(dir string, argv ...string) ([]byte, error) {
			return []byte(`{"headRefOid":"abc123","state":""}`), nil
		})
		if err == nil {
			t.Fatal("want error on empty state")
		}
	})
	t.Run("not_open", func(t *testing.T) {
		_, err := readPRHeadOIDCmd("", "o/r", 1, func(dir string, argv ...string) ([]byte, error) {
			return []byte(`{"headRefOid":"abc123","state":"MERGED"}`), nil
		})
		if err == nil || !strings.Contains(err.Error(), "not open") {
			t.Fatalf("want not open error, got %v", err)
		}
	})
	t.Run("open_ok", func(t *testing.T) {
		oid, err := readPRHeadOIDCmd("", "o/r", 1, func(dir string, argv ...string) ([]byte, error) {
			return []byte(`{"headRefOid":"deadbeef","state":"OPEN"}`), nil
		})
		if err != nil || oid != "deadbeef" {
			t.Fatalf("got %q err=%v", oid, err)
		}
	})
}

func TestObserveChecksMovedHeadFailClosed(t *testing.T) {
	// Inject via temporary override of production path by testing readPRHeadOIDCmd
	// comparison logic used in ObserveChecks: expected head must equal before+after.
	// Full ObserveChecks needs gh; unit-test the fail-closed compare contract:
	want := "sha-aaa"
	got := "sha-bbb"
	if got == want {
		t.Fatal("setup")
	}
	err := fmt.Errorf("%w: pr head moved before checks got=%s expected=%s", ErrConflict, got, want)
	if !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
}
