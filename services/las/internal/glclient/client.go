// Package glclient is the ONLY way anything in this service talks to
// GLPostingAPI. Every domain intent that moves money (disburse, post
// repayment, reclassify/capitalize, write off, assess a fee, waive a
// fee, accrue interest) is translated here into exactly one call to
// Client.PostJournalEntry, using the posting rule named in
// internal/postingrules — nothing in this service ever builds a journal
// entry's lines, picks a GL account, or decides a debit/credit
// direction inline; that is GLPostingAPI's job alone, invoked through
// this typed client and nothing else.
package glclient

import (
	"context"
	"errors"
	"time"

	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/postingrules"
)

// Allocation mirrors GLPostingAPI's PostJournalEntryRequest.allocation
// shape exactly (see services/gl's postJournalEntryRequest DTO).
type Allocation struct {
	FeeAmount       domain.Money
	InterestAmount  domain.Money
	PrincipalAmount domain.Money
}

// Capitalization mirrors PostJournalEntryRequest.capitalization exactly.
type Capitalization struct {
	InterestAmount domain.Money
	FeeAmount      domain.Money
}

// PostJournalEntryInput mirrors PostJournalEntryRequest. Exactly one of
// Amount, Allocation, or Capitalization must be set — GL itself rejects
// any other combination (WRONG_INPUT_SHAPE, 400); this client does not
// re-validate that here, it passes the caller's shape through as-is so
// GL remains the single source of truth for which shape a rule requires.
type PostJournalEntryInput struct {
	IdempotencyKey          string
	PostingRuleCode         postingrules.RuleCode
	LoanAccountID           string
	Amount                  *domain.Money
	Allocation              *Allocation
	Capitalization          *Capitalization
	ReversalOfSourceEventID *string
	Metadata                map[string]any
}

// JournalEntryLine mirrors journal-entry.schema.json#/$defs/JournalEntryLine.
type JournalEntryLine struct {
	GLAccount           string
	Direction           domain.Direction
	Amount              domain.Money
	RunningBalanceAfter domain.Money
}

// JournalEntry mirrors journal-entry.schema.json#/$defs/JournalEntry —
// GL's response to a successful (or idempotently-replayed) posting.
type JournalEntry struct {
	JournalEntryID     string
	SourceEventID      string
	PostingRuleCode    string
	PostingRuleVersion string
	LoanAccountID      string
	Lines              []JournalEntryLine
	Balanced           bool
	Immutable          bool
	PostedAt           time.Time
	PeriodID           string
}

// Sentinel errors PostJournalEntry maps GL's error responses to, so
// internal/service can react without string-matching. Anything GL
// returns that isn't one of these is wrapped as ErrGLUnavailable — a
// transport/5xx-class failure the caller should treat as "GL's outcome
// is unknown," never as "GL rejected this."
var (
	// ErrIdempotencyKeyReused: GL's 409 IDEMPOTENCY_KEY_REUSED — the same
	// Idempotency-Key was already used with a materially different
	// payload. This should be structurally unreachable in this service
	// (every Idempotency-Key this client sends is derived deterministically
	// from the same business reference every time), so a caller seeing
	// this indicates a bug upstream, not a normal race.
	ErrIdempotencyKeyReused = errors.New("glclient: GL rejected the request — Idempotency-Key reused with a different payload")

	// ErrRequestRejected: GL's 400/422-class rejection (unknown rule
	// code, wrong input shape, unbalanced entry, missing metadata,
	// reversal target not found, reversal amount mismatch, current
	// period closed). The specific reason is preserved in the error
	// string for logging, but callers branch only on this sentinel —
	// every one of these is "GL refused to post this," never partially
	// applied.
	ErrRequestRejected = errors.New("glclient: GL rejected the posting request")

	// ErrGLUnavailable: transport failure, non-2xx/4xx status, or a
	// malformed response body — GL's outcome for this specific call is
	// UNKNOWN. Deliberately distinct from ErrRequestRejected: a caller
	// must not assume "no entry was posted" here the way it safely can
	// for ErrRequestRejected — see internal/service's retry/reconciliation
	// notes in PR_DESCRIPTION.md for why no in-process retry is
	// attempted automatically.
	ErrGLUnavailable = errors.New("glclient: GL Posting Engine unavailable or returned an unexpected response")
)

// Client is the typed Posting Engine client — the sole integration
// point with GLPostingAPI this service is permitted to use.
type Client interface {
	// PostJournalEntry posts exactly one journal entry via GL's
	// postJournalEntry operation, using in.IdempotencyKey as the
	// Idempotency-Key header (GL persists this as the entry's
	// sourceEventId). A retried call with the same key and an identical
	// payload replays GL's original response.
	PostJournalEntry(ctx context.Context, in PostJournalEntryInput) (JournalEntry, error)

	// GetStatementOfAccount fetches every posted line referencing
	// loanAccountID, live from GL — the ONLY data
	// domain.RebuildFromLines is ever fed. This client performs no
	// caching of its own; whatever caching/read-optimization exists
	// lives entirely in the resulting domain.BalanceProjection, which is
	// always rebuilt from a fresh call to this method, never from a
	// previous call's result.
	GetStatementOfAccount(ctx context.Context, loanAccountID string) ([]domain.StatementLine, error)
}
