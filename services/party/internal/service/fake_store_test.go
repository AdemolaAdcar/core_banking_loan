package service

import (
	"context"
	"errors"
	"time"

	"github.com/AdemolaAdcar/core_banking_loan/services/party/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/party/internal/outbox"
	"github.com/AdemolaAdcar/core_banking_loan/services/party/internal/store"
)

// fakeStore is an in-memory, non-concurrent-safe implementation of
// store.Store/store.Tx used only in tests. It intentionally has no
// encryption, no SQL, and no real transactional rollback semantics
// beyond "apply everything fn does, or apply nothing if fn errors" —
// enough to verify the SERVICE layer's orchestration logic without a
// live Postgres instance.
type fakeStore struct {
	parties          map[string]domain.Party
	documents        map[string][]domain.IdentityDocument // partyID -> versions, in insertion order
	outboxEntries    []outbox.Entry
	dedupAttempts    []domain.MatchResult
	dedupCandidates  []domain.MatchCandidate
	failNextTx       bool
	nextTxErrMessage string
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		parties:   map[string]domain.Party{},
		documents: map[string][]domain.IdentityDocument{},
	}
}

func (f *fakeStore) WithinTx(ctx context.Context, fn func(store.Tx) error) error {
	if f.failNextTx {
		f.failNextTx = false
		return errors.New(f.nextTxErrMessage)
	}
	// Snapshot for rollback-on-error semantics.
	snapshotParties := cloneParties(f.parties)
	snapshotDocs := cloneDocs(f.documents)
	snapshotOutbox := append([]outbox.Entry(nil), f.outboxEntries...)
	snapshotAttempts := append([]domain.MatchResult(nil), f.dedupAttempts...)

	if err := fn(f); err != nil {
		f.parties = snapshotParties
		f.documents = snapshotDocs
		f.outboxEntries = snapshotOutbox
		f.dedupAttempts = snapshotAttempts
		return err
	}
	return nil
}

func cloneParties(m map[string]domain.Party) map[string]domain.Party {
	out := make(map[string]domain.Party, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func cloneDocs(m map[string][]domain.IdentityDocument) map[string][]domain.IdentityDocument {
	out := make(map[string][]domain.IdentityDocument, len(m))
	for k, v := range m {
		out[k] = append([]domain.IdentityDocument(nil), v...)
	}
	return out
}

func (f *fakeStore) GetParty(_ context.Context, partyID string) (domain.Party, error) {
	p, ok := f.parties[partyID]
	if !ok {
		return domain.Party{}, store.ErrNotFound
	}
	return p, nil
}

func (f *fakeStore) ListDedupCandidates(_ context.Context, _ store.DedupCandidateFilter) ([]domain.MatchCandidate, error) {
	return f.dedupCandidates, nil
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

func (f *fakeStore) GetIdempotentResponse(_ context.Context, _ string) (bool, []byte, error) {
	return false, nil, nil
}

// --- store.Tx methods (fakeStore doubles as its own Tx) --------------------

func (f *fakeStore) CreateParty(_ context.Context, p domain.Party) error {
	f.parties[p.ID] = p
	return nil
}

func (f *fakeStore) UpdateParty(_ context.Context, p domain.Party, _ []string) error {
	f.parties[p.ID] = p
	return nil
}

func (f *fakeStore) TombstoneParty(_ context.Context, partyID, reason, actor string, at time.Time) error {
	return nil // unused directly -- service constructs the updated Party itself
}

func (f *fakeStore) AddIdentityDocument(_ context.Context, d domain.IdentityDocument) error {
	f.documents[d.PartyID] = append(f.documents[d.PartyID], d)
	return nil
}

func (f *fakeStore) RecordDedupAttempt(_ context.Context, _ string, result domain.MatchResult) error {
	f.dedupAttempts = append(f.dedupAttempts, result)
	return nil
}

func (f *fakeStore) SaveIdempotentResponse(_ context.Context, _, _ string, _ []byte) error {
	return nil
}

func (f *fakeStore) InsertOutboxEntry(_ context.Context, e outbox.Entry) error {
	f.outboxEntries = append(f.outboxEntries, e)
	return nil
}
