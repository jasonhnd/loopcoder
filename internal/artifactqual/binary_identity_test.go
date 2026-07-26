package artifactqual_test

import (
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/artifactqual"
)

func TestBinaryIdentity_ParseValidRealFormat(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	out := "loopcoder version=0.9.0-rc.41 commit=" + sha + " date=2026-07-22T12:00:00Z go=go1.22.5 platform=darwin/arm64\n"
	id, err := artifactqual.ParseBinaryIdentity(out)
	if err != nil {
		t.Fatal(err)
	}
	if id.Version != "0.9.0-rc.41" {
		t.Fatalf("version=%q", id.Version)
	}
	if id.Commit != strings.ToLower(sha) {
		t.Fatalf("commit=%q", id.Commit)
	}
	got, err := artifactqual.ValidateBinaryIdentity(out, strings.ToUpper(sha))
	if err != nil {
		t.Fatal(err)
	}
	if got.Commit != strings.ToLower(sha) {
		t.Fatalf("validate commit=%q", got.Commit)
	}
}

func TestBinaryIdentity_MismatchAndShortCommit(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	out := "loopcoder version=0.9.0 commit=" + sha + " date=x"
	_, err := artifactqual.ValidateBinaryIdentity(out, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("want mismatch, got %v", err)
	}
	short := "loopcoder version=0.9.0 commit=abc1234 date=x"
	_, err = artifactqual.ParseBinaryIdentity(short)
	if err == nil || !strings.Contains(err.Error(), "40 hex") {
		t.Fatalf("want 40 hex reject, got %v", err)
	}
}

func TestBinaryIdentity_UnknownAndDev(t *testing.T) {
	_, err := artifactqual.ParseBinaryIdentity("loopcoder version=dev commit=0123456789abcdef0123456789abcdef01234567")
	if err == nil || !strings.Contains(err.Error(), "dev") {
		t.Fatalf("want dev reject, got %v", err)
	}
	_, err = artifactqual.ParseBinaryIdentity("loopcoder version=0.9.0 commit=unknown")
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("want unknown reject, got %v", err)
	}
}

func TestBinaryIdentity_MissingAndDuplicate(t *testing.T) {
	_, err := artifactqual.ParseBinaryIdentity("loopcoder version=0.9.0 date=x")
	if err == nil || !strings.Contains(err.Error(), "commit field missing") {
		t.Fatalf("want commit missing, got %v", err)
	}
	_, err = artifactqual.ParseBinaryIdentity("loopcoder commit=0123456789abcdef0123456789abcdef01234567")
	if err == nil || !strings.Contains(err.Error(), "version field missing") {
		t.Fatalf("want version missing, got %v", err)
	}
	sha := "0123456789abcdef0123456789abcdef01234567"
	_, err = artifactqual.ParseBinaryIdentity("version=0.9.0 version=0.9.1 commit=" + sha)
	if err == nil || !strings.Contains(err.Error(), "version field duplicate") {
		t.Fatalf("want version duplicate, got %v", err)
	}
	_, err = artifactqual.ParseBinaryIdentity("version=0.9.0 commit=" + sha + " commit=" + sha)
	if err == nil || !strings.Contains(err.Error(), "commit field duplicate") {
		t.Fatalf("want commit duplicate, got %v", err)
	}
}

func TestBinaryIdentity_ErrorDoesNotLeakRawOutput(t *testing.T) {
	secret := "SUPER_SECRET_TOKEN_VALUE_xyz"
	out := "loopcoder version=dev commit=unknown token=" + secret
	_, err := artifactqual.ParseBinaryIdentity(out)
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked secret: %v", err)
	}
	_, err = artifactqual.ValidateBinaryIdentity(out, "not-40-hex")
	if err == nil {
		t.Fatal("want expected sha error")
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "not-40-hex") {
		// expected sha may appear? User said error is stable/sanitized — "not-40-hex" is the input
		// but secret must not leak. Allow expected-sha message without raw version output.
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("validate leaked secret: %v", err)
	}
	// Bad expectedSHA alone.
	_, err = artifactqual.ValidateBinaryIdentity("version=0.9.0 commit=0123456789abcdef0123456789abcdef01234567", "short")
	if err == nil || !strings.Contains(err.Error(), "expected sha not 40 hex") {
		t.Fatalf("want expected sha reject, got %v", err)
	}
}
