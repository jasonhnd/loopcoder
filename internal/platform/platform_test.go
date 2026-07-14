package platform

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestCheckRuntimeContractTuples(t *testing.T) {
	tests := []struct {
		name    string
		tuple   Tuple
		wantErr bool
	}{
		{name: "supported darwin arm64", tuple: Tuple{GOOS: "darwin", GOARCH: "arm64"}},
		{name: "darwin amd64", tuple: Tuple{GOOS: "darwin", GOARCH: "amd64"}, wantErr: true},
		{name: "linux amd64", tuple: Tuple{GOOS: "linux", GOARCH: "amd64"}, wantErr: true},
		{name: "linux arm64", tuple: Tuple{GOOS: "linux", GOARCH: "arm64"}, wantErr: true},
		{name: "windows amd64", tuple: Tuple{GOOS: "windows", GOARCH: "amd64"}, wantErr: true},
		{name: "wsl class linux", tuple: Tuple{GOOS: "linux", GOARCH: "amd64"}, wantErr: true},
		{name: "unknown tuple", tuple: Tuple{GOOS: "plan9", GOARCH: "riscv64"}, wantErr: true},
		{name: "empty tuple", tuple: Tuple{}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Check(tt.tuple, StartupPhase)
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("Check() error = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, ErrUnsupportedPlatform) {
				t.Fatalf("Check() error = %v, want ErrUnsupportedPlatform", err)
			}
			var unsupported *UnsupportedPlatformError
			if !errors.As(err, &unsupported) {
				t.Fatalf("Check() error type = %T, want *UnsupportedPlatformError", err)
			}
			if unsupported.ExitCode() != UnsupportedExitCode {
				t.Fatalf("ExitCode() = %d, want %d", unsupported.ExitCode(), UnsupportedExitCode)
			}
			diagnostic := unsupported.Diagnostic()
			if diagnostic.ErrorCode != ErrUnsupportedPlatformCode || diagnostic.Message != HumanFirstLine {
				t.Fatalf("diagnostic = %#v", diagnostic)
			}
			if diagnostic.Actual != Normalize(tt.tuple) {
				t.Fatalf("actual = %#v, want %#v", diagnostic.Actual, Normalize(tt.tuple))
			}
			if diagnostic.SideEffectsPerformed {
				t.Fatal("SideEffectsPerformed = true, want false")
			}
		})
	}
}

func TestUnsupportedPlatformDiagnosticJSONShape(t *testing.T) {
	err := Check(Tuple{GOOS: "linux", GOARCH: "amd64"}, StartupPhase)
	data, marshalErr := MarshalDiagnostic(err)
	if marshalErr != nil {
		t.Fatalf("MarshalDiagnostic: %v", marshalErr)
	}

	const want = `{"schema_version":"loopcoder.diagnostic.v1","error_code":"ErrUnsupportedPlatform","message":"LoopCoder v0.8.0 supports macOS Apple Silicon only (darwin/arm64).","supported":[{"goos":"darwin","goarch":"arm64"}],"actual":{"goos":"linux","goarch":"amd64"},"phase":"startup","exit_code":78,"side_effects_performed":false}`
	if string(data) != want {
		t.Fatalf("diagnostic JSON:\n%s\nwant:\n%s", data, want)
	}
	var payload Diagnostic
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if payload.ExitCode != UnsupportedExitCode || len(payload.Supported) != 1 || payload.Supported[0].GOOS != SupportedGOOS || payload.Supported[0].GOARCH != SupportedGOARCH {
		t.Fatalf("payload = %#v", payload)
	}
}
