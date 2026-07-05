package cli

import (
	"bytes"
	"testing"
)

func TestReadHookInputCapsOversizedPayload(t *testing.T) {
	oversized := bytes.NewReader(bytes.Repeat([]byte("x"), int(maxHookInputBytes)+1))
	if got := readHookInput(oversized); got != nil {
		t.Fatalf("readHookInput returned %d bytes for oversized payload, want nil fail-open input", len(got))
	}
}

func TestReadHookInputAllowsPayloadAtLimit(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), int(maxHookInputBytes))
	got := readHookInput(bytes.NewReader(payload))
	if !bytes.Equal(got, payload) {
		t.Fatalf("readHookInput at limit returned %d bytes, want %d", len(got), len(payload))
	}
}
