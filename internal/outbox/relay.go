// Package outbox publishes committed outbox rows to JetStream.
package outbox

import (
	"context"
	"log/slog"
	"strconv"
	"time"
)

// Row is one committed stream message waiting for its JetStream acknowledgement.
type Row struct {
	Seq     int64
	Subject string
	Headers map[string]string
	Payload []byte
}

// Store reads pending rows in publisher_seq order and marks them sent.
type Store interface {
	PendingOutbox(ctx context.Context, limit int) ([]Row, error)
	MarkOutboxSent(ctx context.Context, seq int64) error
}

// Publisher publishes one message and returns after the JetStream
// acknowledgement. msgID is the Nats-Msg-Id.
type Publisher interface {
	Publish(ctx context.Context, subject, msgID string, headers map[string]string, data []byte) error
}

// MsgID returns the Nats-Msg-Id of a team-registry message.
func MsgID(seq int64) string { return "team-registry-" + strconv.FormatInt(seq, 10) }

const batch = 100

// Relay publishes outbox rows.
type Relay struct {
	store    Store
	pub      Publisher
	logger   *slog.Logger
	interval time.Duration
}

// NewRelay returns a relay that polls every interval.
func NewRelay(store Store, pub Publisher, logger *slog.Logger, interval time.Duration) *Relay {
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	return &Relay{store: store, pub: pub, logger: logger, interval: interval}
}

// RunOnce publishes every pending row in publisher_seq order. It stops at the
// first row that is not acknowledged or not marked sent, so no later row
// overtakes it; that row is retried on the next run with the same Msg-Id.
func (r *Relay) RunOnce(ctx context.Context) (int, error) {
	n := 0
	for {
		rows, err := r.store.PendingOutbox(ctx, batch)
		if err != nil {
			return n, err
		}
		if len(rows) == 0 {
			return n, nil
		}
		for _, row := range rows {
			if err := r.pub.Publish(ctx, row.Subject, MsgID(row.Seq), row.Headers, row.Payload); err != nil {
				return n, err
			}
			if err := r.store.MarkOutboxSent(ctx, row.Seq); err != nil {
				return n, err
			}
			n++
		}
	}
}

// Run publishes pending rows every interval until ctx ends.
func (r *Relay) Run(ctx context.Context) {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		if _, err := r.RunOnce(ctx); err != nil && ctx.Err() == nil {
			r.logger.Warn("outbox relay paused", "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
