// Package store defines the persistence contract the service layer
// depends on. The concrete Postgres implementation lives in
// internal/store/postgres; nothing outside that subpackage imports pgx
// directly, which is what keeps internal/service testable with a fake.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/AdemolaAdcar/core_banking_loan/services/crm/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/crm/internal/outbox"
)

// Tx is a single atomic unit of work: a business write plus its outbox
// entry, committed or rolled back together. Every method here must
// participate in the same database transaction.
type Tx interface {
	outbox.Inserter

	CreateInteraction(ctx context.Context, i domain.Interaction) error
	CreateCase(ctx context.Context, c domain.ServiceCase) error

	// UpdateCaseConditional writes c only if the stored row's version
	// still equals priorVersion (the version the caller loaded and
	// derived c from) -- a DB-level guard against a race between two
	// transactions both loading the same case and both writing, which
	// the domain-level in-memory version check alone cannot prevent.
	// Returns ErrStaleVersion (not a generic error) if zero rows matched.
	UpdateCaseConditional(ctx context.Context, c domain.ServiceCase, priorVersion int) error

	AddCaseNote(ctx context.Context, n domain.CaseNote) error

	// RecordAccess writes one row to the read-audit-log every read of
	// PII-adjacent content (case notes, interaction notes surfaced via
	// Customer360) must produce, per this service's ground rules.
	RecordAccess(ctx context.Context, actorSubject, resourceType, resourceID string, at time.Time) error

	UpsertCommunicationPreferences(ctx context.Context, p domain.CommunicationPreferences) error
	AssignRelationshipManager(ctx context.Context, a domain.RelationshipManagerAssignment) error

	// LinkLoanAccountToParty records the (loanAccountId, partyId)
	// association the first time CRM learns of it (via openCase supplying
	// both). First link wins and is never overwritten -- see this
	// service's PR description for why this exists and its documented
	// limitation (LogInteractionRequest does not carry partyId, so an
	// account CRM has only ever seen through logInteraction, never
	// through a case opened against it, cannot appear in
	// getCustomer360's loanAccountSummaries; flagged back to the Ledger
	// & Solution Architect Agent as a spec gap, not silently worked
	// around further than this).
	LinkLoanAccountToParty(ctx context.Context, loanAccountID, partyID string) error

	SaveIdempotentResponse(ctx context.Context, idempotencyKey, requestHash string, responseJSON []byte) error
}

// Store is the top-level persistence dependency the service layer takes.
// WithinTx is the ONLY way callers can write.
type Store interface {
	WithinTx(ctx context.Context, fn func(Tx) error) error

	GetCase(ctx context.Context, caseID string) (domain.ServiceCase, error)
	ListCaseNotes(ctx context.Context, caseID string) ([]domain.CaseNote, error)

	// GetCommunicationPreferences returns found=false (not an error) if
	// the party has never had preferences explicitly set -- the caller
	// falls back to domain.DefaultCommunicationPreferences.
	GetCommunicationPreferences(ctx context.Context, partyID string) (prefs domain.CommunicationPreferences, found bool, err error)

	// GetRelationshipManagerAssignment returns a zero-RM assignment
	// (RelationshipManagerID == nil), not an error, if none exists.
	GetRelationshipManagerAssignment(ctx context.Context, partyID string) (domain.RelationshipManagerAssignment, error)

	ListLoanAccountIDsForParty(ctx context.Context, partyID string) ([]string, error)

	// LatestInteractionPerLoanAccount returns, for each of the given
	// loan account IDs that has at least one logged interaction, that
	// account's single most recent Interaction -- the input to
	// domain.InferLoanAccountStatus for Customer360's status-only
	// summaries.
	LatestInteractionPerLoanAccount(ctx context.Context, loanAccountIDs []string) (map[string]domain.Interaction, error)

	ListRecentInteractionsForLoanAccounts(ctx context.Context, loanAccountIDs []string, limit int) ([]domain.Interaction, error)
	ListOpenCasesForParty(ctx context.Context, partyID string) ([]domain.ServiceCase, error)

	// ListCasesPastSLA is the SLA sweep's read: every Open/InProgress,
	// not-yet-escalated case whose slaDueAt is before now, capped at
	// limit.
	ListCasesPastSLA(ctx context.Context, now time.Time, limit int) ([]domain.ServiceCase, error)

	GetIdempotentResponse(ctx context.Context, idempotencyKey string) (found bool, responseJSON []byte, err error)

	// ListUnpublished / MarkPublished satisfy outbox.Reader, structurally
	// -- see internal/relay for the concrete Kafka publisher.
	ListUnpublished(ctx context.Context, limit int) ([]outbox.Entry, error)
	MarkPublished(ctx context.Context, ids []string) error
}

// ErrNotFound is returned by Get* methods when the requested resource
// does not exist. Handlers translate this to HTTP 404.
var ErrNotFound = errors.New("store: resource not found")

// ErrStaleVersion is returned by UpdateCaseConditional when the stored
// row's version no longer matches what the caller expected -- see its
// doc comment. Distinct from domain.ErrStaleVersion (the in-memory,
// pre-write check); this is the DB-level guard against the race that
// check alone cannot close.
var ErrStaleVersion = errors.New("store: stale version (concurrent update)")

// ErrCaseNotClosed is returned by a reopen write path when the DB's
// current row is not Closed -- mirrors domain.ErrInvalidTransition for
// the same DB-level race-safety reason ErrStaleVersion exists.
var ErrCaseNotClosed = errors.New("store: case is not closed")
