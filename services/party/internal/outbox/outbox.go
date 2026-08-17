// Package outbox implements the transactional outbox pattern: every
// domain event this service publishes is written to an outbox table in
// the SAME database transaction as the business row it describes, so a
// publish can never succeed while the write fails, or vice versa. A
// separate relay (Publisher) later reads unpublished rows and actually
// delivers them to the event backbone, decoupled from the request path.
package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Entry is one row in the outbox table — one domain event awaiting
// publish. Inserting an Entry MUST happen inside the same database
// transaction as the business write it describes; this package does not
// enforce that on its own — see internal/service, where every write path
// opens exactly one transaction covering both the domain write and the
// outbox insert.
type Entry struct {
	ID          string
	Topic       string
	PayloadJSON []byte
	CreatedAt   time.Time
}

// NewEntry marshals a typed event payload into an outbox Entry. Returns
// an error rather than silently producing an empty/corrupt row — a
// payload that fails to marshal must fail (and roll back) the whole
// transaction, not be swallowed.
func NewEntry(id, topic string, payload any) (Entry, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return Entry{}, fmt.Errorf("outbox: marshaling payload for topic %s: %w", topic, err)
	}
	return Entry{ID: id, Topic: topic, PayloadJSON: b, CreatedAt: time.Now().UTC()}, nil
}

// Inserter is the minimal capability the service layer needs from
// whatever database transaction type it is using. Satisfied by the
// pgx-backed implementation in internal/store/postgres without this
// package importing pgx — keeps the outbox pattern itself testable with
// a fake, independent of the database driver.
type Inserter interface {
	InsertOutboxEntry(ctx context.Context, e Entry) error
}

// Publisher relays unpublished outbox rows to the event backbone. A
// production implementation polls (or uses logical replication / CDC)
// for unpublished rows, publishes each to Kafka, and marks it published
// in a single follow-up transaction — at-least-once delivery: a crash
// between publish and mark-published means a duplicate publish on the
// next poll, never a lost one. Every consumer of these topics elsewhere
// in this system is already expected to be idempotent, same as every
// other event in the platform.
//
// This package defines the contract only. A concrete Kafka-backed
// implementation is a deployment-time concern outside this change's
// scope — see InMemoryPublisher below for the fake used in tests.
type Publisher interface {
	PublishUnpublished(ctx context.Context) (published int, err error)
}

// InMemoryPublisher is a test/local-dev fake: it publishes by appending
// to Sent rather than talking to a real broker. Not for production use.
type InMemoryPublisher struct {
	Sent []Entry
}

func (p *InMemoryPublisher) PublishUnpublished(_ context.Context) (int, error) {
	// A real implementation reads unpublished rows from the database;
	// this fake has nothing to poll from since InMemoryInserter (in
	// tests) records entries directly into Sent at insert time. Provided
	// so callers can depend on the Publisher interface uniformly.
	return len(p.Sent), nil
}
