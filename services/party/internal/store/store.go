// Package store defines the persistence contract the service layer
// depends on. The concrete Postgres implementation lives in
// internal/store/postgres; nothing outside that subpackage imports pgx
// directly, which is what keeps internal/service testable with a fake.
package store

import (
	"context"
	"time"

	"github.com/AdemolaAdcar/core_banking_loan/services/party/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/party/internal/outbox"
)

// DedupCandidateFilter narrows the dedup lookup to an indexed query
// (ssn_hash exact / email exact / phone exact / name+DOB exact) rather
// than a full-table scan compared in application code. The Postgres
// implementation issues one indexed OR query built from these fields.
type DedupCandidateFilter struct {
	SSNHash             string
	Email               string
	Phone               string
	NormalizedFirstName string
	NormalizedLastName  string
	DateOfBirth         time.Time
}

// Tx is a single atomic unit of work: a business write plus its outbox
// entry, committed or rolled back together. Every method here must
// participate in the same database transaction.
type Tx interface {
	outbox.Inserter

	CreateParty(ctx context.Context, p domain.Party) error
	UpdateParty(ctx context.Context, p domain.Party, changedFields []string) error
	TombstoneParty(ctx context.Context, partyID, reason, actor string, at time.Time) error
	AddIdentityDocument(ctx context.Context, d domain.IdentityDocument) error

	// RecordDedupAttempt persists ONE candidate considered during a
	// findOrCreateParty call. Called once per result in
	// domain.EvaluateAll's output — every candidate considered is
	// audited, not only the eventual winner (or lack of one).
	RecordDedupAttempt(ctx context.Context, applicantRequestID string, result domain.MatchResult) error

	SaveIdempotentResponse(ctx context.Context, idempotencyKey, requestHash string, responseJSON []byte) error
}

// Store is the top-level persistence dependency the service layer takes.
// WithinTx is the ONLY way callers can write — it commits on a nil
// return from fn and rolls back otherwise, which is what makes "business
// write + outbox insert, atomically" structurally enforced rather than a
// convention callers might forget to follow.
type Store interface {
	WithinTx(ctx context.Context, fn func(Tx) error) error

	GetParty(ctx context.Context, partyID string) (domain.Party, error)
	ListDedupCandidates(ctx context.Context, filter DedupCandidateFilter) ([]domain.MatchCandidate, error)
	ListIdentityDocuments(ctx context.Context, partyID string) ([]domain.IdentityDocument, error)
	GetIdentityDocument(ctx context.Context, partyID, documentID string) (domain.IdentityDocument, error)

	// MaxDocumentVersion returns the highest existing version for
	// (partyID, docType) and that version's documentID, or (0, nil, nil)
	// if no document of that type exists yet on this party.
	MaxDocumentVersion(ctx context.Context, partyID string, docType domain.DocumentType) (version int, documentID *string, err error)

	GetIdempotentResponse(ctx context.Context, idempotencyKey string) (found bool, responseJSON []byte, err error)
}

// ErrNotFound is returned by Get*/List* methods when the requested
// resource does not exist. Handlers translate this to HTTP 404.
var ErrNotFound = &notFoundError{}

type notFoundError struct{}

func (*notFoundError) Error() string { return "store: resource not found" }
