// Package store defines the persistence contract the service layer
// depends on. The concrete Postgres implementation lives in
// internal/store/postgres; nothing outside that subpackage imports pgx
// directly. Deliberately, no method anywhere on Tx or Store can update
// or delete a journal_entries/journal_entry_lines row -- there is no
// UpdateJournalEntry or DeleteJournalEntry method on this interface at
// all, which is itself part of invariant 3's enforcement: it is not
// just that the database role lacks the grant (see
// internal/store/postgres/migrations), the Go interface an application
// author would even reach for doesn't expose the possibility either.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/outbox"
)

// Tx is a single atomic unit of work: a business write plus its outbox
// entry, committed or rolled back together (invariant 2).
type Tx interface {
	outbox.Inserter

	// CreateJournalEntry inserts the entry row and every line row inside
	// this same transaction -- all lines commit together or none do,
	// structurally (there is no way to call this once per line).
	CreateJournalEntry(ctx context.Context, e domain.JournalEntry) error

	// ClosePeriod upserts a period's Closed status. Chronological-order
	// enforcement happens one level up, in the service layer, via
	// Store.EarliestOpenPeriodBefore -- this method only ever writes the
	// one period it's given.
	ClosePeriod(ctx context.Context, p domain.Period) error

	SaveIdempotentResponse(ctx context.Context, idempotencyKey, requestHash string, responseJSON []byte) error
}

// StatementLine is one posted line referencing a loan account, joined
// with its parent entry's metadata -- the shape getStatementOfAccount
// needs, computed live (invariant 6).
type StatementLine struct {
	JournalEntryID      string
	PostedAt            time.Time
	PostingRuleCode     string
	GLAccount           string
	Direction           domain.Direction
	Amount              domain.Money
	RunningBalanceAfter domain.Money
}

// TrialBalanceLine is one GL account's live-summed debit/credit totals.
type TrialBalanceLine struct {
	GLAccount   string
	DebitTotal  int64
	CreditTotal int64
	Currency    string
}

// Store is the top-level persistence dependency the service layer
// takes. WithinTx is the ONLY way callers can write.
type Store interface {
	WithinTx(ctx context.Context, fn func(Tx) error) error

	GetJournalEntry(ctx context.Context, journalEntryID string) (domain.JournalEntry, error)

	// FindBySourceEventID is used both for the idempotency-lookup path
	// (postJournalEntry, findJournalEntryBySourceEvent) and to fetch the
	// original entry a reversal targets (reversalOfSourceEventId) --
	// reversals mirror the ACTUAL posted lines this returns, never
	// re-derive from the reversal request's own input (see
	// internal/postingrules' package doc comment for why).
	FindBySourceEventID(ctx context.Context, sourceEventID string) (domain.JournalEntry, bool, error)

	// GetLatestRunningBalance returns the current running balance for
	// (loanAccountId, glAccount) -- a live query over posted lines, used
	// to compute the NEXT line's RunningBalanceAfter snapshot at write
	// time. This is not a separately maintained mutable total (invariant
	// 6): it's a live SUM every time, whose result happens to also be
	// stored as a per-line read-convenience annotation, exactly as
	// journal-entry.schema.json's own RunningBalanceAfter field
	// documents. Returns a zero Money (in the given currency) if the
	// account has no prior lines.
	GetLatestRunningBalance(ctx context.Context, loanAccountID, glAccount, currency string) (domain.Money, error)

	// GetTrialBalance and GetStatementOfAccount are both always computed
	// live from journal_entry_lines as of the given instant -- invariant
	// 6 explicitly forbids a separately maintained running total that
	// could drift from the journal.
	GetTrialBalance(ctx context.Context, asOf time.Time) ([]TrialBalanceLine, error)
	GetStatementOfAccount(ctx context.Context, loanAccountID string, asOf time.Time) ([]StatementLine, error)

	// GetAccountBalance is the portfolio-wide control-account read
	// (getGlAccountBalance) -- also always live.
	GetAccountBalance(ctx context.Context, glAccountCode string, asOf time.Time) (domain.Money, error)

	GetPeriod(ctx context.Context, periodID string) (domain.Period, error)

	// EarliestOpenPeriodBefore returns the earliest period ID strictly
	// before periodID that has at least one posted entry and is not
	// Closed, if any -- the chronological-order check ClosePeriod's
	// service-layer caller enforces before calling Tx.ClosePeriod. A
	// period with zero posted entries never blocks a close, since there
	// is nothing in it to protect.
	EarliestOpenPeriodBefore(ctx context.Context, periodID string) (earliestOpenPeriodID string, found bool, err error)

	GetIdempotentResponse(ctx context.Context, idempotencyKey string) (found bool, responseJSON []byte, err error)

	// ListUnpublished / MarkPublished satisfy outbox.Reader for a future
	// Kafka relay (internal/relay, not built in this change -- see
	// PR_DESCRIPTION.md).
	ListUnpublished(ctx context.Context, limit int) ([]outbox.Entry, error)
	MarkPublished(ctx context.Context, ids []string) error
}

var ErrNotFound = errors.New("store: resource not found")

// ErrDuplicateSourceEventID is returned by Tx.CreateJournalEntry when
// the database's UNIQUE constraint on journal_entries.source_event_id
// rejects the insert -- the backstop for a genuine race between two
// concurrent postJournalEntry calls carrying the same Idempotency-Key
// that both reach the database before either's API-layer idempotency
// cache write has landed (see internal/api's idempotency middleware,
// which is the FIRST line of defense; this is the second, DB-level one,
// exactly analogous to how invariant 1's balance check exists in both
// internal/domain AND the database trigger). The service layer treats
// this as "someone else won the race" and returns their entry, not an
// error.
var ErrDuplicateSourceEventID = errors.New("store: source event id already exists")
