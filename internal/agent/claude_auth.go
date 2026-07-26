package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/providerinstall"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
)

const (
	claudeAuthPreflightTimeout = 15 * time.Second
	claudeAuthOutputLimit      = 64 * 1024
)

type claudeInvocationBinding struct {
	Auth       providerinventory.ClaudeAuthBinding
	Executable string
}

func preflightClaudeInvocationBinding(ctx context.Context, inv Invocation, environ []string) (claudeInvocationBinding, error) {
	installID, executable, err := providerinstall.ComputeFromCommand("claude", "claude")
	if err != nil {
		return claudeInvocationBinding{}, fmt.Errorf("claude: resolve exact executable: %w", err)
	}
	if want := strings.TrimSpace(inv.InstallRef); want != "" && want != installID {
		return claudeInvocationBinding{}, fmt.Errorf("claude: install_ref mismatch requested=%s actual=%s", want, installID)
	}
	auth, err := runClaudeAuthObservation(ctx, executable, environ)
	if err != nil {
		return claudeInvocationBinding{}, err
	}
	if auth.ProviderInstallationID != installID {
		return claudeInvocationBinding{}, errors.New("claude: auth observation installation mismatch")
	}
	if want := strings.TrimSpace(inv.AccountRef); want != "" && want != auth.AccountProfileID {
		return claudeInvocationBinding{}, fmt.Errorf("claude: account mismatch requested=%s active=%s", want, auth.AccountProfileID)
	}
	return claudeInvocationBinding{Auth: auth, Executable: executable}, nil
}

func confirmClaudeInvocationBinding(ctx context.Context, before claudeInvocationBinding, environ []string) error {
	after, err := runClaudeAuthObservation(ctx, before.Executable, environ)
	if err != nil {
		return err
	}
	if after.ProviderInstallationID != before.Auth.ProviderInstallationID || after.AccountProfileID != before.Auth.AccountProfileID {
		return errors.New("claude: active installation/account changed during invocation")
	}
	return nil
}

func runClaudeAuthObservation(ctx context.Context, executable string, environ []string) (providerinventory.ClaudeAuthBinding, error) {
	probeCtx, cancel := context.WithTimeout(ctx, claudeAuthPreflightTimeout)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, executable, "auth", "status", "--json")
	cmd.Env = environ
	var stdout, stderr claudeAuthBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	if probeCtx.Err() != nil {
		stdout.Zero()
		stderr.Zero()
		return providerinventory.ClaudeAuthBinding{}, errors.New("claude: bounded auth observation timed out")
	}
	if stdout.overflow || stderr.overflow {
		stdout.Zero()
		stderr.Zero()
		return providerinventory.ClaudeAuthBinding{}, errors.New("claude: bounded auth observation exceeded output limit")
	}
	if err != nil || exitCode != 0 {
		stdout.Zero()
		stderr.Zero()
		return providerinventory.ClaudeAuthBinding{}, fmt.Errorf("claude: bounded auth observation failed with exit %d", exitCode)
	}
	binding, parseErr := providerinventory.ParseClaudeAuthBinding(executable, stdout.Bytes(), exitCode, time.Now().UTC())
	stdout.Zero()
	stderr.Zero()
	if parseErr != nil {
		return providerinventory.ClaudeAuthBinding{}, fmt.Errorf("claude: auth identity unavailable: %w", parseErr)
	}
	return binding, nil
}

type claudeAuthBuffer struct {
	buf      bytes.Buffer
	overflow bool
}

func (b *claudeAuthBuffer) Write(p []byte) (int, error) {
	original := len(p)
	remaining := claudeAuthOutputLimit - b.buf.Len()
	if remaining <= 0 {
		b.overflow = true
		return original, nil
	}
	if len(p) > remaining {
		_, _ = b.buf.Write(p[:remaining])
		b.overflow = true
		return original, nil
	}
	_, _ = b.buf.Write(p)
	return original, nil
}

func (b *claudeAuthBuffer) Bytes() []byte { return b.buf.Bytes() }

func (b *claudeAuthBuffer) Zero() {
	data := b.buf.Bytes()
	for i := range data {
		data[i] = 0
	}
	b.buf.Reset()
}
