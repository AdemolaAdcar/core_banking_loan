// Package accountclient is the ONLY way this service talks to LAS's
// AccountAPI. Per the design note's receiveRepaymentNotification
// unification ("PaymentAPI now calls this endpoint directly and
// synchronously the moment an inbound payment arrives"), every inbound
// payment this service's rail adapter surfaces is handed to AccountAPI
// through this client — internal/service never builds the HTTP request
// inline, and this is also the ONLY compliant path this service uses to
// trigger the compensating reversal a returned/reversed inbound payment
// requires (ReverseRepayment), matching this role's own ground rule:
// never a manual balance correction, never a silent write-off.
package accountclient

import (
	"context"
	"errors"
	"time"

	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/domain"
)

// TokenSource supplies the bearer token this client attaches to every
// AccountAPI call — mirrors services/las/internal/glclient.TokenSource
// exactly (the requesting side of the same client-credentials grant).
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// ReceiveRepaymentNotificationInput mirrors AccountAPI's
// receiveRepaymentNotification request (POST /repayments:notify) — see
// services/las/internal/api/dto.go's receiveRepaymentNotificationRequestDTO.
type ReceiveRepaymentNotificationInput struct {
	IdempotencyKey string // paymentReferenceId
	LoanAccountRef *string
	PayoffQuoteID  *string
	Amount         domain.Money
	Rail           string
	ReceivedAt     time.Time
}

// Kind mirrors AccountAPI's response oneOf for receiveRepaymentNotification
// — status alone on the underlying resource doesn't tell a caller which
// of the three resource types it's looking at, so this client surfaces
// Kind explicitly rather than making callers re-derive it.
type Kind string

const (
	KindRepayment Kind = "Repayment"
	KindPayoff    Kind = "Payoff"
	KindRecovery  Kind = "Recovery"
)

// ReceiveRepaymentNotificationResult carries just enough of AccountAPI's
// response for this service's own PaymentInstruction bookkeeping — the
// resulting resource's ID/status (for cross-reference and logging) and,
// when present, the GLPostingAPI entry AccountAPI's own posting produced
// (payment-instruction.schema.json's journalEntryId: "...or resulted
// from (INBOUND) this instruction").
type ReceiveRepaymentNotificationResult struct {
	Kind           Kind
	ID             string // repaymentId, payoffId, or recoveryId depending on Kind
	Status         string
	JournalEntryID *string
	Unmatched      bool // true iff Kind == KindRepayment && Status == "Unmatched"
}

// ReverseRepaymentInput mirrors AccountAPI's reverseRepayment request
// (POST /repayments/{id}:reverse).
type ReverseRepaymentInput struct {
	RepaymentID            string
	IdempotencyKey         string
	ConfirmedBy            string
	ReasonCode             string // PAYMENT_RETURNED or MISAPPLIED
	CorrectedLoanAccountID *string
}

type ReverseRepaymentResult struct {
	Status         string
	JournalEntryID *string
}

// Sentinel errors every ReceiveRepaymentNotification/ReverseRepayment
// call maps AccountAPI's HTTP response to, so internal/service can react
// via errors.Is without string-matching a response body — same
// discipline services/las/internal/glclient documents for its own
// GLPostingAPI sentinels.
var (
	// ErrRequestRejected: AccountAPI's 400-class rejection (malformed
	// request).
	ErrRequestRejected = errors.New("accountclient: AccountAPI rejected the request")
	// ErrNotFound: AccountAPI's 404 — e.g. ReverseRepayment referencing
	// a repaymentId AccountAPI has no record of.
	ErrNotFound = errors.New("accountclient: AccountAPI has no record of the referenced resource")
	// ErrConflict: AccountAPI's 409-class response — a terminal account,
	// a not-modifiable resource, an invalid state transition, or an
	// Idempotency-Key reused with a different payload. Every one of
	// these is "AccountAPI refused this on domain-state grounds," never
	// partially applied; callers branch only on this sentinel, the
	// specific reason is preserved in the error string for logging.
	ErrConflict = errors.New("accountclient: AccountAPI rejected the request due to a conflicting state")
	// ErrAccountUnavailable: transport failure, non-2xx/4xx status, or a
	// malformed response body — AccountAPI's outcome for this specific
	// call is UNKNOWN. A caller must not assume nothing happened; see
	// internal/service's own retry/reconciliation discipline.
	ErrAccountUnavailable = errors.New("accountclient: AccountAPI unavailable or returned an unexpected response")
)

// Client is the single abstraction this service uses to reach
// AccountAPI (LAS).
type Client interface {
	// ReceiveRepaymentNotification is the synchronous trigger for
	// repayment/payoff/recovery processing — see this package's doc
	// comment. Idempotent on in.IdempotencyKey, exactly as AccountAPI's
	// own spec documents.
	ReceiveRepaymentNotification(ctx context.Context, in ReceiveRepaymentNotificationInput) (ReceiveRepaymentNotificationResult, error)

	// ReverseRepayment triggers AccountAPI's existing PR-REPAY-02
	// reversal path — the ONLY compliant way this service ever
	// compensates for a returned/reversed inbound payment; see this
	// package's doc comment.
	ReverseRepayment(ctx context.Context, in ReverseRepaymentInput) (ReverseRepaymentResult, error)
}
