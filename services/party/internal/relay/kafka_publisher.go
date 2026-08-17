// Package relay is the concrete Kafka-backed implementation of
// internal/outbox.Publisher: it polls the outbox table (via
// outbox.Reader) for unpublished rows, writes each to its topic on
// Kafka, and only then marks it published.
//
// This is deliberately at-least-once, not exactly-once: a crash between
// a successful Kafka write and the follow-up MarkPublished call means
// the same row is picked up and republished on the next poll. Every
// consumer of party.created/party.updated/party.tombstoned elsewhere in
// this system is already expected to be idempotent (same assumption
// internal/outbox's package doc comment states) — this package does not
// invent a new delivery guarantee the rest of the platform doesn't
// already rely on.
package relay

import (
	"context"
	"fmt"

	kafkago "github.com/segmentio/kafka-go"

	"github.com/AdemolaAdcar/core_banking_loan/services/party/internal/outbox"
)

// Writer is the minimal capability this package needs from a Kafka
// producer — satisfied directly by *kafkago.Writer, and by a fake in
// tests. Kept narrow so nothing outside this package needs to know
// segmentio/kafka-go exists.
type Writer interface {
	WriteMessages(ctx context.Context, msgs ...kafkago.Message) error
}

// Publisher implements outbox.Publisher.
type Publisher struct {
	reader    outbox.Reader
	writer    Writer
	batchSize int
}

// NewPublisher builds a relay Publisher. writer.Topic must be left unset
// on a real *kafkago.Writer — every outbox entry sets its own Topic per
// message (entries for different event types share one Writer), and
// kafka-go rejects a Writer that has both a fixed Topic and per-message
// topics set.
func NewPublisher(reader outbox.Reader, writer Writer, batchSize int) *Publisher {
	if batchSize <= 0 {
		batchSize = 100
	}
	return &Publisher{reader: reader, writer: writer, batchSize: batchSize}
}

func (p *Publisher) PublishUnpublished(ctx context.Context) (int, error) {
	entries, err := p.reader.ListUnpublished(ctx, p.batchSize)
	if err != nil {
		return 0, fmt.Errorf("relay: listing unpublished outbox entries: %w", err)
	}
	if len(entries) == 0 {
		return 0, nil
	}

	msgs := make([]kafkago.Message, len(entries))
	ids := make([]string, len(entries))
	for i, e := range entries {
		msgs[i] = kafkago.Message{
			Topic: e.Topic,
			Key:   []byte(e.ID),
			Value: e.PayloadJSON,
			Time:  e.CreatedAt,
		}
		ids[i] = e.ID
	}

	if err := p.writer.WriteMessages(ctx, msgs...); err != nil {
		// Nothing is marked published -- every one of these entries is
		// picked up again on the next poll. Safe by construction (see
		// package doc comment).
		return 0, fmt.Errorf("relay: writing %d messages to kafka: %w", len(msgs), err)
	}

	if err := p.reader.MarkPublished(ctx, ids); err != nil {
		// The Kafka write already succeeded here -- these entries WILL
		// be republished on the next poll despite already having been
		// delivered once. Still correct under the documented
		// at-least-once contract, just worth surfacing distinctly from
		// a publish failure for anyone reading logs/metrics.
		return 0, fmt.Errorf("relay: marking %d entries published after a successful kafka write: %w", len(ids), err)
	}
	return len(entries), nil
}
