// Package postgres is the only place in this service that imports pgx
// or touches SQL directly. It implements internal/store.Store /
// internal/store.Tx, so internal/service never depends on this package
// or on pgx at all.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/outbox"
	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/store"
)

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) WithinTx(ctx context.Context, fn func(store.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: beginning transaction: %w", err)
	}
	ptx := &Tx{tx: tx}
	if fnErr := fn(ptx); fnErr != nil {
		if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			return fmt.Errorf("postgres: rolling back after %w: %v", fnErr, rbErr)
		}
		return fnErr
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: committing transaction: %w", err)
	}
	return nil
}

func (s *Store) GetLoanAccount(ctx context.Context, loanAccountID string) (domain.LoanAccount, error) {
	a, err := s.scanLoanAccount(ctx, "SELECT id, approval_reference_id, party_id, status, booked_by, dpd, dpd_bucket, non_accrual_flag, non_accrual_since, created_at, updated_at FROM loan_accounts WHERE id = $1", loanAccountID)
	if err != nil {
		return domain.LoanAccount{}, err
	}
	v, found, err := s.getTermVersion(ctx, loanAccountID, 0 /* latest */)
	if err != nil {
		return domain.LoanAccount{}, err
	}
	if found {
		a.CurrentTermVersion = v
	}
	return a, nil
}

func (s *Store) FindLoanAccountByApprovalReference(ctx context.Context, approvalReferenceID string) (domain.LoanAccount, bool, error) {
	a, err := s.scanLoanAccount(ctx, "SELECT id, approval_reference_id, party_id, status, booked_by, dpd, dpd_bucket, non_accrual_flag, non_accrual_since, created_at, updated_at FROM loan_accounts WHERE approval_reference_id = $1", approvalReferenceID)
	if errors.Is(err, store.ErrNotFound) {
		return domain.LoanAccount{}, false, nil
	}
	if err != nil {
		return domain.LoanAccount{}, false, err
	}
	v, found, err := s.getTermVersion(ctx, a.LoanAccountID, 0)
	if err != nil {
		return domain.LoanAccount{}, false, err
	}
	if found {
		a.CurrentTermVersion = v
	}
	return a, true, nil
}

func (s *Store) scanLoanAccount(ctx context.Context, query string, arg string) (domain.LoanAccount, error) {
	row := s.pool.QueryRow(ctx, query, arg)
	var a domain.LoanAccount
	var status, dpdBucket string
	var nonAccrualSince *time.Time
	err := row.Scan(&a.LoanAccountID, &a.ApprovalReferenceID, &a.PartyID, &status, &a.BookedBy, &a.DPD, &dpdBucket, &a.NonAccrualFlag, &nonAccrualSince, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.LoanAccount{}, store.ErrNotFound
	}
	if err != nil {
		return domain.LoanAccount{}, fmt.Errorf("postgres: querying loan account: %w", err)
	}
	a.Status = domain.Status(status)
	a.DPDBucket = domain.DPDBucket(dpdBucket)
	a.NonAccrualSince = nonAccrualSince
	return a, nil
}

// getTermVersion returns the given version, or (version=0) the latest.
func (s *Store) getTermVersion(ctx context.Context, loanAccountID string, version int) (domain.TermVersion, bool, error) {
	var row pgx.Row
	if version == 0 {
		row = s.pool.QueryRow(ctx,
			`SELECT version, effective_date, principal_amount, currency, annual_interest_rate_bps, term_months, day_count_convention
			 FROM term_versions WHERE loan_account_id = $1 ORDER BY version DESC LIMIT 1`, loanAccountID)
	} else {
		row = s.pool.QueryRow(ctx,
			`SELECT version, effective_date, principal_amount, currency, annual_interest_rate_bps, term_months, day_count_convention
			 FROM term_versions WHERE loan_account_id = $1 AND version = $2`, loanAccountID, version)
	}
	v, err := scanTermVersion(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.TermVersion{}, false, nil
	}
	if err != nil {
		return domain.TermVersion{}, false, fmt.Errorf("postgres: querying term version: %w", err)
	}
	return v, true, nil
}

func scanTermVersion(row pgx.Row) (domain.TermVersion, error) {
	var v domain.TermVersion
	var amount int64
	var currency string
	err := row.Scan(&v.Version, &v.EffectiveDate, &amount, &currency, &v.AnnualInterestRateBps, &v.TermMonths, &v.DayCountConvention)
	if err != nil {
		return domain.TermVersion{}, err
	}
	v.PrincipalAmount = domain.Money{Amount: amount, Currency: currency}
	return v, nil
}

func (s *Store) ListTermVersions(ctx context.Context, loanAccountID string) ([]domain.TermVersion, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT version, effective_date, principal_amount, currency, annual_interest_rate_bps, term_months, day_count_convention
		 FROM term_versions WHERE loan_account_id = $1 ORDER BY version ASC`, loanAccountID)
	if err != nil {
		return nil, fmt.Errorf("postgres: querying term versions: %w", err)
	}
	defer rows.Close()
	var out []domain.TermVersion
	for rows.Next() {
		v, err := scanTermVersion(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: scanning term version: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) GetEffectiveTermVersion(ctx context.Context, loanAccountID string, asOf time.Time) (domain.TermVersion, bool, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT version, effective_date, principal_amount, currency, annual_interest_rate_bps, term_months, day_count_convention
		 FROM term_versions WHERE loan_account_id = $1 AND effective_date <= $2 ORDER BY effective_date DESC, version DESC LIMIT 1`,
		loanAccountID, asOf)
	v, err := scanTermVersion(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.TermVersion{}, false, nil
	}
	if err != nil {
		return domain.TermVersion{}, false, fmt.Errorf("postgres: querying effective term version: %w", err)
	}
	return v, true, nil
}

func (s *Store) GetBalanceProjection(ctx context.Context, loanAccountID string) (domain.BalanceProjection, bool, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT outstanding_principal, interest_receivable, fee_receivable, currency, line_count, rebuilt_at
		 FROM balance_projections WHERE loan_account_id = $1`, loanAccountID)
	var p domain.BalanceProjection
	var principal, interest, fee int64
	var currency string
	err := row.Scan(&principal, &interest, &fee, &currency, &p.LineCount, &p.RebuiltAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.BalanceProjection{}, false, nil
	}
	if err != nil {
		return domain.BalanceProjection{}, false, fmt.Errorf("postgres: querying balance projection: %w", err)
	}
	p.LoanAccountID = loanAccountID
	p.OutstandingPrincipal = domain.Money{Amount: principal, Currency: currency}
	p.InterestReceivable = domain.Money{Amount: interest, Currency: currency}
	p.FeeReceivable = domain.Money{Amount: fee, Currency: currency}
	return p, true, nil
}

func (s *Store) GetDisbursement(ctx context.Context, disbursementID string) (domain.Disbursement, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT id, loan_account_id, status, principal_amount, currency, rejection_reason, journal_entry_id, instruction_id, requested_by, created_at, updated_at
		 FROM disbursements WHERE id = $1`, disbursementID)
	return scanDisbursement(row)
}

func (s *Store) ListDisbursementsByLoanAccount(ctx context.Context, loanAccountID string) ([]domain.Disbursement, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, loan_account_id, status, principal_amount, currency, rejection_reason, journal_entry_id, instruction_id, requested_by, created_at, updated_at
		 FROM disbursements WHERE loan_account_id = $1 ORDER BY created_at ASC`, loanAccountID)
	if err != nil {
		return nil, fmt.Errorf("postgres: querying disbursements: %w", err)
	}
	defer rows.Close()
	var out []domain.Disbursement
	for rows.Next() {
		d, err := scanDisbursement(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func scanDisbursement(row pgx.Row) (domain.Disbursement, error) {
	var d domain.Disbursement
	var status string
	var amount int64
	var currency string
	err := row.Scan(&d.DisbursementID, &d.LoanAccountID, &status, &amount, &currency, &d.RejectionReason, &d.JournalEntryID, &d.InstructionID, &d.RequestedBy, &d.CreatedAt, &d.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Disbursement{}, store.ErrNotFound
	}
	if err != nil {
		return domain.Disbursement{}, fmt.Errorf("postgres: querying disbursement: %w", err)
	}
	d.Status = domain.DisbursementStatus(status)
	d.PrincipalAmount = domain.Money{Amount: amount, Currency: currency}
	return d, nil
}

func (s *Store) GetRepayment(ctx context.Context, repaymentID string) (domain.Repayment, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT id, loan_account_id, status, amount, currency, alloc_fee_amount, alloc_interest_amount, alloc_principal_amount,
			suspense_amount, journal_entry_id, unmatched_reason_code, corrected_repayment_id, received_at, updated_at
		 FROM repayments WHERE id = $1`, repaymentID)
	return scanRepayment(row)
}

func scanRepayment(row pgx.Row) (domain.Repayment, error) {
	var r domain.Repayment
	var status string
	var amount, suspense int64
	var currency string
	var feeAmt, interestAmt, principalAmt *int64
	var unmatchedReason *string
	err := row.Scan(&r.RepaymentID, &r.LoanAccountID, &status, &amount, &currency, &feeAmt, &interestAmt, &principalAmt,
		&suspense, &r.JournalEntryID, &unmatchedReason, &r.CorrectedRepaymentID, &r.ReceivedAt, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Repayment{}, store.ErrNotFound
	}
	if err != nil {
		return domain.Repayment{}, fmt.Errorf("postgres: querying repayment: %w", err)
	}
	r.Status = domain.RepaymentStatus(status)
	r.Amount = domain.Money{Amount: amount, Currency: currency}
	r.SuspenseAmount = domain.Money{Amount: suspense, Currency: currency}
	if feeAmt != nil && interestAmt != nil && principalAmt != nil {
		r.Allocation = &domain.Allocation{
			FeeAmount: domain.Money{Amount: *feeAmt, Currency: currency}, InterestAmount: domain.Money{Amount: *interestAmt, Currency: currency}, PrincipalAmount: domain.Money{Amount: *principalAmt, Currency: currency},
		}
	}
	if unmatchedReason != nil {
		reason := domain.UnmatchedReason(*unmatchedReason)
		r.UnmatchedReasonCode = &reason
	}
	return r, nil
}

func (s *Store) GetPayoff(ctx context.Context, payoffID string) (domain.Payoff, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT id, loan_account_id, quote_id, status, amount_posted, suspense_amount, currency, journal_entry_id, closed_at, updated_at
		 FROM payoffs WHERE id = $1`, payoffID)
	var p domain.Payoff
	var status string
	var amountPosted, suspense int64
	var currency string
	err := row.Scan(&p.PayoffID, &p.LoanAccountID, &p.QuoteID, &status, &amountPosted, &suspense, &currency, &p.JournalEntryID, &p.ClosedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Payoff{}, store.ErrNotFound
	}
	if err != nil {
		return domain.Payoff{}, fmt.Errorf("postgres: querying payoff: %w", err)
	}
	p.Status = domain.PayoffStatus(status)
	p.AmountPosted = domain.Money{Amount: amountPosted, Currency: currency}
	p.SuspenseAmount = domain.Money{Amount: suspense, Currency: currency}
	return p, nil
}

func (s *Store) GetChargeOff(ctx context.Context, chargeoffID string) (domain.ChargeOff, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT id, loan_account_id, status, write_off_amount, currency, journal_entry_id, chargeoff_decision_reference, confirmed_by, charged_off_at, updated_at
		 FROM chargeoffs WHERE id = $1`, chargeoffID)
	return scanChargeOff(row)
}

func (s *Store) FindChargeOffByLoanAccount(ctx context.Context, loanAccountID string) (domain.ChargeOff, bool, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT id, loan_account_id, status, write_off_amount, currency, journal_entry_id, chargeoff_decision_reference, confirmed_by, charged_off_at, updated_at
		 FROM chargeoffs WHERE loan_account_id = $1 ORDER BY updated_at DESC LIMIT 1`, loanAccountID)
	c, err := scanChargeOff(row)
	if errors.Is(err, store.ErrNotFound) {
		return domain.ChargeOff{}, false, nil
	}
	if err != nil {
		return domain.ChargeOff{}, false, err
	}
	return c, true, nil
}

func scanChargeOff(row pgx.Row) (domain.ChargeOff, error) {
	var c domain.ChargeOff
	var status string
	var amount int64
	var currency string
	err := row.Scan(&c.ChargeoffID, &c.LoanAccountID, &status, &amount, &currency, &c.JournalEntryID, &c.ChargeoffDecisionReference, &c.ConfirmedBy, &c.ChargedOffAt, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ChargeOff{}, store.ErrNotFound
	}
	if err != nil {
		return domain.ChargeOff{}, fmt.Errorf("postgres: querying charge-off: %w", err)
	}
	c.Status = domain.ChargeOffStatus(status)
	c.WriteOffAmount = domain.Money{Amount: amount, Currency: currency}
	return c, nil
}

func (s *Store) GetRecovery(ctx context.Context, recoveryID string) (domain.Recovery, error) {
	row := s.pool.QueryRow(ctx, `SELECT id, loan_account_id, amount, currency, journal_entry_id, received_at, updated_at FROM recoveries WHERE id = $1`, recoveryID)
	var r domain.Recovery
	var amount int64
	var currency string
	err := row.Scan(&r.RecoveryID, &r.LoanAccountID, &amount, &currency, &r.JournalEntryID, &r.ReceivedAt, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Recovery{}, store.ErrNotFound
	}
	if err != nil {
		return domain.Recovery{}, fmt.Errorf("postgres: querying recovery: %w", err)
	}
	r.Amount = domain.Money{Amount: amount, Currency: currency}
	return r, nil
}

func (s *Store) GetModification(ctx context.Context, modificationID string) (domain.Modification, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT id, loan_account_id, effective_date, previous_term_version, new_term_version, capitalized_amount, currency, journal_entry_id, confirmed_by, updated_at
		 FROM modifications WHERE id = $1`, modificationID)
	var m domain.Modification
	var capAmount *int64
	var currency *string
	err := row.Scan(&m.ModificationID, &m.LoanAccountID, &m.EffectiveDate, &m.PreviousTermVersion, &m.NewTermVersion, &capAmount, &currency, &m.JournalEntryID, &m.ConfirmedBy, &m.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Modification{}, store.ErrNotFound
	}
	if err != nil {
		return domain.Modification{}, fmt.Errorf("postgres: querying modification: %w", err)
	}
	if capAmount != nil && currency != nil {
		amt := domain.Money{Amount: *capAmount, Currency: *currency}
		m.CapitalizedAmount = &amt
	}
	return m, nil
}

func (s *Store) GetFee(ctx context.Context, feeID string) (domain.Fee, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT id, loan_account_id, scheduled_due_date, amount, currency, status, journal_entry_id, waived_by, waive_reason_code, waived_at, assessed_at, updated_at
		 FROM fees WHERE id = $1`, feeID)
	var f domain.Fee
	var status string
	var amount int64
	var currency string
	var waiveReason *string
	err := row.Scan(&f.FeeID, &f.LoanAccountID, &f.ScheduledDueDate, &amount, &currency, &status, &f.JournalEntryID, &f.WaivedBy, &waiveReason, &f.WaivedAt, &f.AssessedAt, &f.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Fee{}, store.ErrNotFound
	}
	if err != nil {
		return domain.Fee{}, fmt.Errorf("postgres: querying fee: %w", err)
	}
	f.Status = domain.FeeStatus(status)
	f.Amount = domain.Money{Amount: amount, Currency: currency}
	if waiveReason != nil {
		r := domain.WaiveReasonCode(*waiveReason)
		f.WaiveReasonCode = &r
	}
	return f, nil
}

func (s *Store) ListAccrualEligibleAccounts(ctx context.Context, asOf time.Time) ([]domain.LoanAccount, error) {
	return s.listLoanAccountsByStatus(ctx, "SELECT id, approval_reference_id, party_id, status, booked_by, dpd, dpd_bucket, non_accrual_flag, non_accrual_since, created_at, updated_at FROM loan_accounts WHERE status = 'Disbursed' AND non_accrual_flag = FALSE ORDER BY id")
}

func (s *Store) ListPastDueAccounts(ctx context.Context, asOf time.Time) ([]domain.LoanAccount, error) {
	return s.listLoanAccountsByStatus(ctx, "SELECT id, approval_reference_id, party_id, status, booked_by, dpd, dpd_bucket, non_accrual_flag, non_accrual_since, created_at, updated_at FROM loan_accounts WHERE status = 'Disbursed' AND dpd_bucket <> 'Current' ORDER BY id")
}

func (s *Store) ListAllDisbursedOrChargedOff(ctx context.Context) ([]domain.LoanAccount, error) {
	return s.listLoanAccountsByStatus(ctx, "SELECT id, approval_reference_id, party_id, status, booked_by, dpd, dpd_bucket, non_accrual_flag, non_accrual_since, created_at, updated_at FROM loan_accounts WHERE status IN ('Disbursed', 'ChargedOff') ORDER BY id")
}

func (s *Store) listLoanAccountsByStatus(ctx context.Context, query string) ([]domain.LoanAccount, error) {
	rows, err := s.pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("postgres: querying loan accounts: %w", err)
	}
	defer rows.Close()
	var out []domain.LoanAccount
	for rows.Next() {
		var a domain.LoanAccount
		var status, dpdBucket string
		var nonAccrualSince *time.Time
		if err := rows.Scan(&a.LoanAccountID, &a.ApprovalReferenceID, &a.PartyID, &status, &a.BookedBy, &a.DPD, &dpdBucket, &a.NonAccrualFlag, &nonAccrualSince, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, fmt.Errorf("postgres: scanning loan account: %w", err)
		}
		a.Status = domain.Status(status)
		a.DPDBucket = domain.DPDBucket(dpdBucket)
		a.NonAccrualSince = nonAccrualSince
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) GetIdempotentResponse(ctx context.Context, idempotencyKey string) (bool, []byte, error) {
	row := s.pool.QueryRow(ctx, "SELECT response_json FROM idempotency_keys WHERE idempotency_key = $1", idempotencyKey)
	var payload []byte
	err := row.Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, fmt.Errorf("postgres: querying idempotent response: %w", err)
	}
	return true, payload, nil
}

func (s *Store) ListUnpublished(ctx context.Context, limit int) ([]outbox.Entry, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT id, topic, payload_json, created_at FROM outbox WHERE published_at IS NULL ORDER BY created_at ASC LIMIT $1", limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: querying unpublished outbox entries: %w", err)
	}
	defer rows.Close()
	var out []outbox.Entry
	for rows.Next() {
		var e outbox.Entry
		if err := rows.Scan(&e.ID, &e.Topic, &e.PayloadJSON, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("postgres: scanning outbox entry: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) MarkPublished(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, "UPDATE outbox SET published_at = now() WHERE id = ANY($1)", ids)
	if err != nil {
		return fmt.Errorf("postgres: marking outbox entries published: %w", err)
	}
	return nil
}
