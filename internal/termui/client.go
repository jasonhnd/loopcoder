package termui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/jasonhnd/loopcoder/internal/uireport"
	"github.com/jasonhnd/loopcoder/internal/uisub"
)

// Mode selects rendering.
type Mode string

const (
	ModeHuman Mode = "human"
	ModeJSONL Mode = "jsonl"
)

var (
	ErrPartialWrite = errors.New("termui: partial write")
	ErrBrokenPipe   = errors.New("termui: broken pipe")
)

// Client is a terminal protocol client over a uisub.Ledger.
type Client struct {
	ledger   *uisub.Ledger
	clientID string
	mode     Mode
	// reportOut is dedicated operator stream (never machine stdout).
	reportOut io.Writer
	mu        sync.Mutex
	cursor    int64
}

// NewClient binds a registered ledger client to a report stream.
func NewClient(ledger *uisub.Ledger, clientID string, mode Mode, reportOut io.Writer) *Client {
	return &Client{ledger: ledger, clientID: clientID, mode: mode, reportOut: reportOut}
}

// Snapshot replays then returns the number of reports rendered.
func (c *Client) Snapshot(ctx context.Context) (int, error) {
	return c.consume(ctx, false)
}

// Follow replays and continues until ctx cancel or terminal report.
func (c *Client) Follow(ctx context.Context) (int, error) {
	return c.consume(ctx, true)
}

func (c *Client) consume(ctx context.Context, follow bool) (int, error) {
	n := 0
	for {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		c.mu.Lock()
		cur := c.cursor
		c.mu.Unlock()
		reps, err := c.ledger.Replay(c.clientID, cur)
		if err != nil {
			return n, err
		}
		if len(reps) == 0 {
			if !follow {
				return n, nil
			}
			// No busy loop: wait on ctx only (tests cancel promptly).
			select {
			case <-ctx.Done():
				return n, ctx.Err()
			default:
				return n, nil // cooperative: no busy poll; caller re-invokes after publish
			}
		}
		for _, env := range reps {
			if err := ctx.Err(); err != nil {
				return n, err
			}
			if err := c.renderOne(env); err != nil {
				// No rendered ack on failure — replayable.
				return n, err
			}
			// Only after full write:
			if err := c.ledger.Acknowledge(uisub.Ack{
				ClientID: c.clientID,
				EventID:  env.EventID,
				Digest:   env.ContentDigest,
				Stage:    uisub.StageRendered,
			}); err != nil {
				return n, err
			}
			c.mu.Lock()
			c.cursor = env.Sequence
			c.mu.Unlock()
			n++
			if env.ReportKind == uireport.KindTerminal {
				return n, nil
			}
		}
		if !follow {
			return n, nil
		}
	}
}

func (c *Client) renderOne(env uireport.Envelope) error {
	var payload []byte
	var err error
	switch c.mode {
	case ModeJSONL:
		payload, err = json.Marshal(env)
		if err != nil {
			return err
		}
		payload = append(payload, '\n')
	default:
		h := uireport.Human(env)
		line := uireport.PrettyText(h) + "\n"
		payload = []byte(line)
	}
	return writeFull(c.reportOut, payload)
}

func writeFull(w io.Writer, p []byte) error {
	if w == nil {
		return fmt.Errorf("termui: nil report stream")
	}
	n, err := w.Write(p)
	if err != nil {
		// Treat short write / closed as broken pipe class.
		if errors.Is(err, io.ErrClosedPipe) || errors.Is(err, io.ErrShortWrite) {
			return fmt.Errorf("%w: %v", ErrBrokenPipe, err)
		}
		return err
	}
	if n != len(p) {
		return ErrPartialWrite
	}
	return nil
}

// Cursor returns the last fully rendered sequence.
func (c *Client) Cursor() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cursor
}
