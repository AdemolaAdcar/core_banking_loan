package service

import (
	"context"
	"sync"
	"time"

	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/outbox"
	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/store"
)

// fakeStore is an in-memory store.Store/store.Tx double for this
// package's unit tests -- no I/O, mutex-guarded so tests can run with
// -race.
type fakeStore struct {
	mu sync.Mutex

	instructions  map[string]domain.PaymentInstruction
	byRailRef     map[string]string // railReference -> instructionID
	exceptions    []domain.ReconciliationException
	cursors       map[string]time.Time
	outboxEntries []outbox.Entry
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		instructions: map[string]domain.PaymentInstruction{},
		byRailRef:    map[string]string{},
		cursors:      map[string]time.Time{},
	}
}

func (f *fakeStore) WithinTx(_ context.Context, fn func(store.Tx) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fn(&fakeTx{f})
}

type fakeTx struct{ f *fakeStore }

func (t *fakeTx) InsertOutboxEntry(_ context.Context, e outbox.Entry) error {
	t.f.outboxEntries = append(t.f.outboxEntries, e)
	return nil
}

func (t *fakeTx) SavePaymentInstruction(_ context.Context, p domain.PaymentInstruction) error {
	t.f.instructions[p.InstructionID] = p
	if p.RailReference != nil {
		t.f.byRailRef[*p.RailReference] = p.InstructionID
	}
	return nil
}

func (t *fakeTx) SaveReconciliationException(_ context.Context, e domain.ReconciliationException) error {
	t.f.exceptions = append(t.f.exceptions, e)
	return nil
}

func (t *fakeTx) SaveIdempotentResponse(_ context.Context, idempotencyKey, requestHash string, responseJSON []byte) error {
	return nil
}

func (t *fakeTx) SetInboundCursor(_ context.Context, name string, at time.Time) error {
	t.f.cursors[name] = at
	return nil
}

func (f *fakeStore) GetPaymentInstruction(_ context.Context, instructionID string) (domain.PaymentInstruction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.instructions[instructionID]
	if !ok {
		return domain.PaymentInstruction{}, store.ErrNotFound
	}
	return p, nil
}

func (f *fakeStore) GetPaymentInstructionByRailReference(_ context.Context, railReference string) (domain.PaymentInstruction, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.byRailRef[railReference]
	if !ok {
		return domain.PaymentInstruction{}, false, nil
	}
	return f.instructions[id], true, nil
}

func (f *fakeStore) ListSubmittedOutbound(_ context.Context) ([]domain.PaymentInstruction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.PaymentInstruction
	for _, p := range f.instructions {
		if p.Direction == domain.Outbound && p.Status == domain.StatusSubmitted {
			out = append(out, p)
		}
	}
	return out, nil
}

func (f *fakeStore) GetInboundCursor(_ context.Context, name string) (time.Time, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	at, ok := f.cursors[name]
	return at, ok, nil
}

func (f *fakeStore) GetIdempotentResponse(_ context.Context, idempotencyKey string) (bool, []byte, error) {
	return false, nil, nil
}

func (f *fakeStore) ListUnpublished(_ context.Context, limit int) ([]outbox.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.outboxEntries, nil
}

func (f *fakeStore) MarkPublished(_ context.Context, ids []string) error { return nil }
