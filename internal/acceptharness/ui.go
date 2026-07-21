package acceptharness

import (
	"context"
	"fmt"
	"sync"
)

// AckStage is the final-mile UI acknowledgement stage.
type AckStage string

const (
	AckPersisted AckStage = "persisted"
	AckStreamed  AckStage = "streamed"
	AckAccepted  AckStage = "accepted"
	AckRendered  AckStage = "rendered"
	AckSeen      AckStage = "seen"
)

// UIMessage is one synthetic report envelope.
type UIMessage struct {
	Sequence int
	Summary  string
}

// FakeUI is an in-memory UI client with disconnect and duplicate controls.
type FakeUI struct {
	mu sync.Mutex

	ClientID     string
	Messages     []UIMessage
	Acks         map[int]AckStage
	Disconnected bool
	// DuplicateAck when true records two acks for the same sequence.
	DuplicateAck bool
	// ReplayCursor is the last fully acked sequence.
	ReplayCursor int
}

// NewFakeUI returns a connected UI client.
func NewFakeUI(clientID string) *FakeUI {
	if clientID == "" {
		clientID = "synthetic-ui"
	}
	return &FakeUI{
		ClientID: clientID,
		Acks:     map[int]AckStage{},
	}
}

// Deliver attempts to deliver a message. Fails when disconnected.
func (u *FakeUI) Deliver(ctx context.Context, msg UIMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.Disconnected {
		return fmt.Errorf("ui disconnect: client %s unavailable", u.ClientID)
	}
	u.Messages = append(u.Messages, msg)
	return nil
}

// Acknowledge records an ack stage for a sequence.
func (u *FakeUI) Acknowledge(seq int, stage AckStage) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.Disconnected {
		return fmt.Errorf("ui disconnect: cannot ack while disconnected")
	}
	u.Acks[seq] = stage
	if u.DuplicateAck {
		// second write is intentional to model duplicate delivery
		u.Acks[seq] = stage
	}
	if int(stageRank(stage)) >= int(stageRank(AckRendered)) && seq > u.ReplayCursor {
		u.ReplayCursor = seq
	}
	return nil
}

// Disconnect marks the client unavailable.
func (u *FakeUI) Disconnect() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.Disconnected = true
}

// Reconnect restores the client and keeps the replay cursor.
func (u *FakeUI) Reconnect() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.Disconnected = false
}

// HighestAck returns the highest ack stage for seq, if any.
func (u *FakeUI) HighestAck(seq int) (AckStage, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	stage, ok := u.Acks[seq]
	return stage, ok
}

func stageRank(s AckStage) int {
	switch s {
	case AckPersisted:
		return 1
	case AckStreamed:
		return 2
	case AckAccepted:
		return 3
	case AckRendered:
		return 4
	case AckSeen:
		return 5
	default:
		return 0
	}
}
