package relay

import (
	"context"
	"errors"
	"testing"
	"time"

	kafkago "github.com/segmentio/kafka-go"

	"github.com/AdemolaAdcar/core_banking_loan/services/party/internal/outbox"
)

type fakeReader struct {
	unpublished []outbox.Entry
	published   []string
	listErr     error
	markErr     error
}

func (f *fakeReader) ListUnpublished(_ context.Context, limit int) ([]outbox.Entry, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	if limit < len(f.unpublished) {
		return f.unpublished[:limit], nil
	}
	return f.unpublished, nil
}

func (f *fakeReader) MarkPublished(_ context.Context, ids []string) error {
	if f.markErr != nil {
		return f.markErr
	}
	f.published = append(f.published, ids...)
	return nil
}

type fakeWriter struct {
	written  []kafkago.Message
	writeErr error
}

func (f *fakeWriter) WriteMessages(_ context.Context, msgs ...kafkago.Message) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.written = append(f.written, msgs...)
	return nil
}

func TestPublishUnpublished_NoEntries_NoOp(t *testing.T) {
	reader := &fakeReader{}
	writer := &fakeWriter{}
	p := NewPublisher(reader, writer, 10)

	n, err := p.PublishUnpublished(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 published, got %d", n)
	}
	if len(writer.written) != 0 {
		t.Fatalf("expected no kafka writes for an empty outbox, got %d", len(writer.written))
	}
}

func TestPublishUnpublished_WritesEachEntryToItsOwnTopic_ThenMarksPublished(t *testing.T) {
	entries := []outbox.Entry{
		{ID: "e1", Topic: "party.created", PayloadJSON: []byte(`{"partyId":"p1"}`), CreatedAt: time.Now()},
		{ID: "e2", Topic: "party.updated", PayloadJSON: []byte(`{"partyId":"p2"}`), CreatedAt: time.Now()},
	}
	reader := &fakeReader{unpublished: entries}
	writer := &fakeWriter{}
	p := NewPublisher(reader, writer, 10)

	n, err := p.PublishUnpublished(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 published, got %d", n)
	}
	if len(writer.written) != 2 {
		t.Fatalf("expected 2 kafka messages written, got %d", len(writer.written))
	}
	if writer.written[0].Topic != "party.created" || writer.written[1].Topic != "party.updated" {
		t.Fatalf("expected each message on its own entry's topic, got %+v", writer.written)
	}
	if len(reader.published) != 2 || reader.published[0] != "e1" || reader.published[1] != "e2" {
		t.Fatalf("expected both entry IDs marked published, got %v", reader.published)
	}
}

func TestPublishUnpublished_WriteFails_NothingMarkedPublished(t *testing.T) {
	entries := []outbox.Entry{{ID: "e1", Topic: "party.created", PayloadJSON: []byte(`{}`), CreatedAt: time.Now()}}
	reader := &fakeReader{unpublished: entries}
	writer := &fakeWriter{writeErr: errors.New("broker unavailable")}
	p := NewPublisher(reader, writer, 10)

	if _, err := p.PublishUnpublished(context.Background()); err == nil {
		t.Fatalf("expected an error when the kafka write fails")
	}
	if len(reader.published) != 0 {
		t.Fatalf("expected nothing marked published after a failed write, got %v", reader.published)
	}
}

func TestPublishUnpublished_MarkPublishedFails_ErrorSurfaced(t *testing.T) {
	entries := []outbox.Entry{{ID: "e1", Topic: "party.created", PayloadJSON: []byte(`{}`), CreatedAt: time.Now()}}
	reader := &fakeReader{unpublished: entries, markErr: errors.New("db unavailable")}
	writer := &fakeWriter{}
	p := NewPublisher(reader, writer, 10)

	if _, err := p.PublishUnpublished(context.Background()); err == nil {
		t.Fatalf("expected an error when marking published fails, even though the kafka write itself succeeded")
	}
	if len(writer.written) != 1 {
		t.Fatalf("expected the kafka write to have already happened, got %d messages", len(writer.written))
	}
}

func TestPublishUnpublished_RespectsBatchSize(t *testing.T) {
	entries := make([]outbox.Entry, 5)
	for i := range entries {
		entries[i] = outbox.Entry{ID: string(rune('a' + i)), Topic: "party.created", PayloadJSON: []byte(`{}`), CreatedAt: time.Now()}
	}
	reader := &fakeReader{unpublished: entries}
	writer := &fakeWriter{}
	p := NewPublisher(reader, writer, 2)

	n, err := p.PublishUnpublished(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected batch size to cap this call at 2, got %d", n)
	}
}
