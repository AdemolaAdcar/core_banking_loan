// Package store defines the persistence contract internal/service
// depends on. The concrete Postgres implementation lives in
// internal/store/postgres; nothing outside that subpackage imports pgx
// directly.
//
// Deliberately, there is no method anywhere on this interface that
// writes a BalanceProjection field independently of
// domain.RebuildFromLines's output — SaveBalanceProjection always takes
// a whole domain.BalanceProjection produced by a fresh rebuild, never a
// delta. That is this package's own structural enforcement of "a loan
// account's balance is never a stored, independently-mutable field":
// the Go interface itself doesn't expose an IncrementBalance or
// SetBalance method for an application author to reach for.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/outbox"
)

// Tx is a single atomic unit of work: a business write plus its outbox
// entry, committed or rolled back together (transactional outbox
// pattern — every state transition publishes its event in the same
// transaction as the state write).
type Tx interface {
	outbox.Inserter

	SaveLoanAccount(ctx context.Context, a domain.LoanAccount) error
	SaveTermVersion(ctx context.Context, loanAccountID string, v domain.TermVersion) error
	SaveBalanceProjection(ctx context.Context, p domain.BalanceProjection) error

	SaveDisbursement(ctx context.Context, d domain.Disbursement) error
	SaveRepayment(ctx context.Context, r domain.Repayment) error
	SavePayoff(ctx context.Context, p domain.Payoff) error
	SaveChargeOff(ctx context.Context, c domain.ChargeOff) error
	SaveRecovery(ctx context.Context, r domain.Recovery) error
	SaveModification(ctx context.Context, m domain.Modification) error
	SaveFee(ctx context.Context, f domain.Fee) error

	SaveIdempotentResponse(ctx context.Context, idempotencyKey, requestHash string, responseJSON []byte) error
}

// Store is the top-level persistence dependency internal/service takes.
// WithinTx is the only way callers can write.
type Store interface {
	WithinTx(ctx context.Context, fn func(Tx) error) error

	GetLoanAccount(ctx context.Context, loanAccountID string) (domain.LoanAccount, error)
	FindLoanAccountByApprovalReference(ctx context.Context, approvalReferenceID string) (domain.LoanAccount, bool, error)
	ListTermVersions(ctx context.Context, loanAccountID string) ([]domain.TermVersion, error)
	GetEffectiveTermVersion(ctx context.Context, loanAccountID string, asOf time.Time) (domain.TermVersion, bool, error)

	// GetBalanceProjection returns the last-rebuilt projection cache —
	// callers that need a guaranteed-fresh value call
	// service.RefreshBalanceProjection first, which always rebuilds from
	// GL before saving. found=false means no rebuild has ever run for
	// this account (e.g. a freshly booked, undisbursed account).
	GetBalanceProjection(ctx context.Context, loanAccountID string) (domain.BalanceProjection, bool, error)

	GetDisbursement(ctx context.Context, disbursementID string) (domain.Disbursement, error)
	ListDisbursementsByLoanAccount(ctx context.Context, loanAccountID string) ([]domain.Disbursement, error)

	GetRepayment(ctx context.Context, repaymentID string) (domain.Repayment, error)
	GetPayoff(ctx context.Context, payoffID string) (domain.Payoff, error)
	GetChargeOff(ctx context.Context, chargeoffID string) (domain.ChargeOff, error)
	FindChargeOffByLoanAccount(ctx context.Context, loanAccountID string) (domain.ChargeOff, bool, error)
	GetRecovery(ctx context.Context, recoveryID string) (domain.Recovery, error)
	GetModification(ctx context.Context, modificationID string) (domain.Modification, error)
	GetFee(ctx context.Context, feeID string) (domain.Fee, error)

	// ListAccrualEligibleAccounts / ListPastDueAccounts back the two
	// batch-eligibility read endpoints and the two daily batch jobs'
	// own internal iteration — always a live query over current
	// LoanAccount state, never a separately maintained eligibility list.
	ListAccrualEligibleAccounts(ctx context.Context, asOf time.Time) ([]domain.LoanAccount, error)
	ListPastDueAccounts(ctx context.Context, asOf time.Time) ([]domain.LoanAccount, error)
	ListAllDisbursedOrChargedOff(ctx context.Context) ([]domain.LoanAccount, error)

	GetIdempotentResponse(ctx context.Context, idempotencyKey string) (found bool, responseJSON []byte, err error)

	ListUnpublished(ctx context.Context, limit int) ([]outbox.Entry, error)
	MarkPublished(ctx context.Context, ids []string) error
}

var ErrNotFound = errors.New("store: resource not found")
