package api

import (
	"context"
	"time"

	"github.com/AdemolaAdcar/core_banking_loan/services/party/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/party/internal/outbox"
	"github.com/AdemolaAdcar/core_banking_loan/services/party/internal/store"
)

// fakeStore is a minimal in-memory store.Store/store.Tx double for
// exercising the HTTP layer (handlers.go, idempotency.go) without a live
// Postgres instance — the same role internal/service's fakeStore plays
// for the service layer, duplicated here (not shared) since it is
// test-only scaffolding private to each package.
type fakeStore struct {
	parties         map[string]domain.Party
	documents       map[string][]domain.IdentityDocument
	idempotencyKeys map[string][]byte
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		parties:         map[string]domain.Party{},
		documents:       map[string][]domain.IdentityDocument{},
		idempotencyKeys: map[string][]byte{},
	}
}

func (f *fakeStore) WithinTx(ctx context.Context, fn func(store.Tx) error) error {
	return fn(f)
}

func (f *fakeStore) GetParty(_ context.Context, partyID string) (domain.Party, error) {
	p, ok := f.parties[partyID]
	if !ok {
		return domain.Party{}, store.ErrNotFound
	}
	return p, nil
}

func (f *fakeStore) ListDedupCandidates(_ context.Context, _ store.DedupCandidateFilter) ([]domain.MatchCandidate, error) {
	return nil, nil
}

func (f *fakeStore) ListIdentityDocuments(_ context.Context, partyID string) ([]domain.IdentityDocument, error) {
	return f.documents[partyID], nil
}

func (f *fakeStore) GetIdentityDocument(_ context.Context, partyID, documentID string) (domain.IdentityDocument, error) {
	for _, d := range f.documents[partyID] {
		if d.ID == documentID {
			return d, nil
		}
	}
	return domain.IdentityDocument{}, store.ErrNotFound
}

func (f *fakeStore) MaxDocumentVersion(_ context.Context, partyID string, docType domain.DocumentType) (int, *string, error) {
	max := 0
	var maxID *string
	for _, d := range f.documents[partyID] {
		if d.DocumentType == docType && d.Version > max {
			max = d.Version
			id := d.ID
			maxID = &id
		}
	}
	return max, maxID, nil
}

func (f *fakeStore) GetIdempotentResponse(_ context.Context, key string) (bool, []byte, error) {
	b, ok := f.idempotencyKeys[key]
	if !ok {
		return false, nil, nil
	}
	return true, b, nil
}

func (f *fakeStore) CreateParty(_ context.Context, p domain.Party) error {
	f.parties[p.ID] = p
	return nil
}

func (f *fakeStore) UpdateParty(_ context.Context, p domain.Party, _ []string) error {
	f.parties[p.ID] = p
	return nil
}

func (f *fakeStore) TombstoneParty(_ context.Context, _, _, _ string, _ time.Time) error {
	return nil
}

func (f *fakeStore) AddIdentityDocument(_ context.Context, d domain.IdentityDocument) error {
	f.documents[d.PartyID] = append(f.documents[d.PartyID], d)
	return nil
}

func (f *fakeStore) RecordDedupAttempt(_ context.Context, _ string, _ domain.MatchResult) error {
	return nil
}

func (f *fakeStore) SaveIdempotentResponse(_ context.Context, key, _ string, responseJSON []byte) error {
	f.idempotencyKeys[key] = responseJSON
	return nil
}

func (f *fakeStore) InsertOutboxEntry(_ context.Context, _ outbox.Entry) error {
	return nil
}
