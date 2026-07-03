package process

import (
	"os"
	"testing"
	"time"
)

func TestAliveInvalidPID(t *testing.T) {
	if Alive(0) {
		t.Fatal("Alive(0) = true, want false")
	}
}

func TestAliveCurrentProcessReturnsPromptly(t *testing.T) {
	done := make(chan bool, 1)
	go func() {
		done <- Alive(os.Getpid())
	}()

	select {
	case <-done:
	case <-time.After(6 * time.Second):
		t.Fatal("Alive did not return promptly")
	}
}
