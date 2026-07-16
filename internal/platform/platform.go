// Package platform defines LoopCoder's v0.8.0 runtime platform contract.
package platform

import (
	"encoding/json"
	"errors"
	"runtime"
	"strings"
)

const (
	SchemaVersion = "loopcoder.diagnostic.v1"

	ErrUnsupportedPlatformCode = "ErrUnsupportedPlatform"
	HumanFirstLine             = "LoopCoder v0.8.0 supports macOS Apple Silicon only (darwin/arm64)."
	CompatibilityGuidance      = "LoopCoder v0.7.0 is the final legacy multi-platform release for Windows, Linux, WSL, containers, and Intel macOS."
	UnsupportedExitCode        = 78
	StartupPhase               = "startup"

	SupportedGOOS   = "darwin"
	SupportedGOARCH = "arm64"
)

var ErrUnsupportedPlatform = errors.New(ErrUnsupportedPlatformCode)

type Tuple struct {
	GOOS   string `json:"goos"`
	GOARCH string `json:"goarch"`
}

type UnsupportedPlatformError struct {
	Actual Tuple
	Phase  string
}

func (e *UnsupportedPlatformError) Error() string {
	return HumanFirstLine
}

func (e *UnsupportedPlatformError) Is(target error) bool {
	return target == ErrUnsupportedPlatform
}

func (e *UnsupportedPlatformError) ExitCode() int {
	return UnsupportedExitCode
}

func (e *UnsupportedPlatformError) Diagnostic() Diagnostic {
	phase := strings.TrimSpace(e.Phase)
	if phase == "" {
		phase = StartupPhase
	}
	return Diagnostic{
		SchemaVersion:        SchemaVersion,
		ErrorCode:            ErrUnsupportedPlatformCode,
		Message:              HumanFirstLine,
		Supported:            SupportedTuples(),
		Actual:               e.Actual,
		Phase:                phase,
		ExitCode:             UnsupportedExitCode,
		SideEffectsPerformed: false,
	}
}

type Diagnostic struct {
	SchemaVersion        string  `json:"schema_version"`
	ErrorCode            string  `json:"error_code"`
	Message              string  `json:"message"`
	Supported            []Tuple `json:"supported"`
	Actual               Tuple   `json:"actual"`
	Phase                string  `json:"phase"`
	ExitCode             int     `json:"exit_code"`
	SideEffectsPerformed bool    `json:"side_effects_performed"`
}

func RuntimeTuple() Tuple {
	return Tuple{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}
}

func SupportedTuples() []Tuple {
	return []Tuple{{GOOS: SupportedGOOS, GOARCH: SupportedGOARCH}}
}

func CheckRuntime() error {
	return Check(RuntimeTuple(), StartupPhase)
}

func Check(tuple Tuple, phase string) error {
	tuple = Normalize(tuple)
	if tuple.GOOS == SupportedGOOS && tuple.GOARCH == SupportedGOARCH {
		return nil
	}
	return &UnsupportedPlatformError{Actual: tuple, Phase: phase}
}

func Normalize(tuple Tuple) Tuple {
	return Tuple{
		GOOS:   strings.ToLower(strings.TrimSpace(tuple.GOOS)),
		GOARCH: strings.ToLower(strings.TrimSpace(tuple.GOARCH)),
	}
}

func MarshalDiagnostic(err error) ([]byte, error) {
	var unsupported *UnsupportedPlatformError
	if !errors.As(err, &unsupported) {
		return nil, err
	}
	return json.Marshal(unsupported.Diagnostic())
}
