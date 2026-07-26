package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// WithWriteTx runs fn inside a write transaction.
// On failure or cancellation, rollback uses an independent bounded cleanup
// context so a cancelled caller context cannot leave a poisoned connection.
func (s *Store) WithWriteTx(ctx context.Context, fn func(ctx context.Context, tx *sql.Tx) error) error {
	if fn == nil {
		return errors.New("store: WithWriteTx callback is required")
	}
	db, _, err := s.openHandle()
	if err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		if IsBusy(err) {
			return fmt.Errorf("%w: begin: %v", ErrBusy, err)
		}
		return fmt.Errorf("store write tx: begin: %w", err)
	}

	committed := false
	defer func() {
		if !committed {
			_ = rollbackTxIndependent(tx)
		}
	}()

	if err := fn(ctx, tx); err != nil {
		_ = rollbackTxIndependent(tx)
		if IsBusy(err) {
			return fmt.Errorf("%w: %v", ErrBusy, err)
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		_ = rollbackTxIndependent(tx)
		if IsBusy(err) {
			return fmt.Errorf("%w: commit: %v", ErrBusy, err)
		}
		return fmt.Errorf("store write tx: commit: %w", err)
	}
	committed = true
	return nil
}

func rollbackTxIndependent(tx *sql.Tx) error {
	if tx == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), RollbackCleanupTimeout)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- tx.Rollback() }()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, sql.ErrTxDone) {
			return err
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("store: rollback cleanup timeout: %w", ctx.Err())
	}
}

// WithRead runs a read callback. Pool remains MaxOpenConns=1 so concurrent
// readers queue within busy_timeout rather than growing connections.
func (s *Store) WithRead(ctx context.Context, fn func(ctx context.Context, db *sql.DB) error) error {
	if fn == nil {
		return errors.New("store: WithRead callback is required")
	}
	db, _, err := s.openHandle()
	if err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return fn(ctx, db)
}

// JournalMode returns the active journal_mode (expected wal).
func (s *Store) JournalMode(ctx context.Context) (string, error) {
	db, _, err := s.openHandle()
	if err != nil {
		return "", err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var mode string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
		return "", err
	}
	return toLowerASCII(mode), nil
}

func toLowerASCII(s string) string {
	b := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[i] = c
	}
	return string(b)
}

// SetBusyTimeout re-applies busy_timeout (tests may tighten it).
func (s *Store) SetBusyTimeout(ctx context.Context, d time.Duration) error {
	db, _, err := s.openHandle()
	if err != nil {
		return err
	}
	if d <= 0 {
		d = DefaultBusyTimeout
	}
	_, err = db.ExecContext(ctx, fmt.Sprintf(`PRAGMA busy_timeout = %d`, d.Milliseconds()))
	if err == nil {
		s.openReport.BusyTimeout = d
	}
	return err
}
