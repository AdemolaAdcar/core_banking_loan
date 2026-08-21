package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/outbox"
)

// Tx implements internal/store.Tx: every method here runs inside the
// single pgx.Tx opened by Store.WithinTx — the business write and its
// outbox.Entry always commit or roll back together (transactional
// outbox pattern).
type Tx struct {
	tx pgx.Tx
}

func (t *Tx) SaveLoanAccount(ctx context.Context, a domain.LoanAccount) error {
	_, err := t.tx.Exec(ctx,
		`INSERT INTO loan_accounts (id, approval_reference_id, party_id, status, booked_by, dpd, dpd_bucket, non_accrual_flag, non_accrual_since, created_at, updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		 ON CONFLICT (id) DO UPDATE SET
			status = EXCLUDED.status, dpd = EXCLUDED.dpd, dpd_bucket = EXCLUDED.dpd_bucket,
			non_accrual_flag = EXCLUDED.non_accrual_flag, non_accrual_since = EXCLUDED.non_accrual_since, updated_at = EXCLUDED.updated_at`,
		a.LoanAccountID, a.ApprovalReferenceID, a.PartyID, string(a.Status), a.BookedBy, a.DPD, string(a.DPDBucket), a.NonAccrualFlag, a.NonAccrualSince, a.CreatedAt, a.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("postgres: saving loan account: %w", err)
	}
	return nil
}

func (t *Tx) SaveTermVersion(ctx context.Context, loanAccountID string, v domain.TermVersion) error {
	_, err := t.tx.Exec(ctx,
		`INSERT INTO term_versions (loan_account_id, version, effective_date, principal_amount, currency, annual_interest_rate_bps, term_months, day_count_convention)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		 ON CONFLICT (loan_account_id, version) DO NOTHING`,
		loanAccountID, v.Version, v.EffectiveDate, v.PrincipalAmount.Amount, v.PrincipalAmount.Currency, v.AnnualInterestRateBps, v.TermMonths, v.DayCountConvention,
	)
	if err != nil {
		return fmt.Errorf("postgres: saving term version: %w", err)
	}
	return nil
}

// SaveBalanceProjection always overwrites the whole row with a freshly
// rebuilt domain.BalanceProjection — see internal/store's package doc
// comment for why there is deliberately no incremental-update variant.
func (t *Tx) SaveBalanceProjection(ctx context.Context, p domain.BalanceProjection) error {
	_, err := t.tx.Exec(ctx,
		`INSERT INTO balance_projections (loan_account_id, outstanding_principal, interest_receivable, fee_receivable, currency, line_count, rebuilt_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)
		 ON CONFLICT (loan_account_id) DO UPDATE SET
			outstanding_principal = EXCLUDED.outstanding_principal, interest_receivable = EXCLUDED.interest_receivable,
			fee_receivable = EXCLUDED.fee_receivable, currency = EXCLUDED.currency, line_count = EXCLUDED.line_count, rebuilt_at = EXCLUDED.rebuilt_at`,
		p.LoanAccountID, p.OutstandingPrincipal.Amount, p.InterestReceivable.Amount, p.FeeReceivable.Amount, p.OutstandingPrincipal.Currency, p.LineCount, p.RebuiltAt,
	)
	if err != nil {
		return fmt.Errorf("postgres: saving balance projection: %w", err)
	}
	return nil
}

func (t *Tx) SaveDisbursement(ctx context.Context, d domain.Disbursement) error {
	_, err := t.tx.Exec(ctx,
		`INSERT INTO disbursements (id, loan_account_id, status, principal_amount, currency, rejection_reason, journal_entry_id, instruction_id, requested_by, created_at, updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		 ON CONFLICT (id) DO UPDATE SET
			status = EXCLUDED.status, rejection_reason = EXCLUDED.rejection_reason, journal_entry_id = EXCLUDED.journal_entry_id,
			instruction_id = EXCLUDED.instruction_id, updated_at = EXCLUDED.updated_at`,
		d.DisbursementID, d.LoanAccountID, string(d.Status), d.PrincipalAmount.Amount, d.PrincipalAmount.Currency, d.RejectionReason, d.JournalEntryID, d.InstructionID, d.RequestedBy, d.CreatedAt, d.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("postgres: saving disbursement: %w", err)
	}
	return nil
}

func (t *Tx) SaveRepayment(ctx context.Context, r domain.Repayment) error {
	var feeAmt, interestAmt, principalAmt *int64
	if r.Allocation != nil {
		feeAmt = &r.Allocation.FeeAmount.Amount
		interestAmt = &r.Allocation.InterestAmount.Amount
		principalAmt = &r.Allocation.PrincipalAmount.Amount
	}
	var unmatchedReason *string
	if r.UnmatchedReasonCode != nil {
		s := string(*r.UnmatchedReasonCode)
		unmatchedReason = &s
	}
	_, err := t.tx.Exec(ctx,
		`INSERT INTO repayments (id, loan_account_id, status, amount, currency, alloc_fee_amount, alloc_interest_amount, alloc_principal_amount,
			suspense_amount, journal_entry_id, unmatched_reason_code, corrected_repayment_id, received_at, updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		 ON CONFLICT (id) DO UPDATE SET
			loan_account_id = EXCLUDED.loan_account_id, status = EXCLUDED.status, alloc_fee_amount = EXCLUDED.alloc_fee_amount,
			alloc_interest_amount = EXCLUDED.alloc_interest_amount, alloc_principal_amount = EXCLUDED.alloc_principal_amount,
			suspense_amount = EXCLUDED.suspense_amount, journal_entry_id = EXCLUDED.journal_entry_id,
			unmatched_reason_code = EXCLUDED.unmatched_reason_code, corrected_repayment_id = EXCLUDED.corrected_repayment_id, updated_at = EXCLUDED.updated_at`,
		r.RepaymentID, r.LoanAccountID, string(r.Status), r.Amount.Amount, r.Amount.Currency, feeAmt, interestAmt, principalAmt,
		r.SuspenseAmount.Amount, r.JournalEntryID, unmatchedReason, r.CorrectedRepaymentID, r.ReceivedAt, r.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("postgres: saving repayment: %w", err)
	}
	return nil
}

func (t *Tx) SavePayoff(ctx context.Context, p domain.Payoff) error {
	_, err := t.tx.Exec(ctx,
		`INSERT INTO payoffs (id, loan_account_id, quote_id, status, amount_posted, suspense_amount, currency, journal_entry_id, closed_at, updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		 ON CONFLICT (id) DO UPDATE SET
			status = EXCLUDED.status, amount_posted = EXCLUDED.amount_posted, suspense_amount = EXCLUDED.suspense_amount,
			journal_entry_id = EXCLUDED.journal_entry_id, closed_at = EXCLUDED.closed_at, updated_at = EXCLUDED.updated_at`,
		p.PayoffID, p.LoanAccountID, p.QuoteID, string(p.Status), p.AmountPosted.Amount, p.SuspenseAmount.Amount, p.AmountPosted.Currency, p.JournalEntryID, p.ClosedAt, p.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("postgres: saving payoff: %w", err)
	}
	return nil
}

func (t *Tx) SaveChargeOff(ctx context.Context, c domain.ChargeOff) error {
	_, err := t.tx.Exec(ctx,
		`INSERT INTO chargeoffs (id, loan_account_id, status, write_off_amount, currency, journal_entry_id, chargeoff_decision_reference, confirmed_by, charged_off_at, updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		 ON CONFLICT (id) DO UPDATE SET
			status = EXCLUDED.status, journal_entry_id = EXCLUDED.journal_entry_id, charged_off_at = EXCLUDED.charged_off_at, updated_at = EXCLUDED.updated_at`,
		c.ChargeoffID, c.LoanAccountID, string(c.Status), c.WriteOffAmount.Amount, c.WriteOffAmount.Currency, c.JournalEntryID, c.ChargeoffDecisionReference, c.ConfirmedBy, c.ChargedOffAt, c.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("postgres: saving charge-off: %w", err)
	}
	return nil
}

func (t *Tx) SaveRecovery(ctx context.Context, r domain.Recovery) error {
	_, err := t.tx.Exec(ctx,
		`INSERT INTO recoveries (id, loan_account_id, amount, currency, journal_entry_id, received_at, updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)
		 ON CONFLICT (id) DO UPDATE SET journal_entry_id = EXCLUDED.journal_entry_id, updated_at = EXCLUDED.updated_at`,
		r.RecoveryID, r.LoanAccountID, r.Amount.Amount, r.Amount.Currency, r.JournalEntryID, r.ReceivedAt, r.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("postgres: saving recovery: %w", err)
	}
	return nil
}

func (t *Tx) SaveModification(ctx context.Context, m domain.Modification) error {
	var capAmount *int64
	var currency *string
	if m.CapitalizedAmount != nil {
		capAmount = &m.CapitalizedAmount.Amount
		currency = &m.CapitalizedAmount.Currency
	}
	_, err := t.tx.Exec(ctx,
		`INSERT INTO modifications (id, loan_account_id, effective_date, previous_term_version, new_term_version, capitalized_amount, currency, journal_entry_id, confirmed_by, updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		 ON CONFLICT (id) DO UPDATE SET journal_entry_id = EXCLUDED.journal_entry_id, updated_at = EXCLUDED.updated_at`,
		m.ModificationID, m.LoanAccountID, m.EffectiveDate, m.PreviousTermVersion, m.NewTermVersion, capAmount, currency, m.JournalEntryID, m.ConfirmedBy, m.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("postgres: saving modification: %w", err)
	}
	return nil
}

func (t *Tx) SaveFee(ctx context.Context, f domain.Fee) error {
	var waiveReason *string
	if f.WaiveReasonCode != nil {
		s := string(*f.WaiveReasonCode)
		waiveReason = &s
	}
	_, err := t.tx.Exec(ctx,
		`INSERT INTO fees (id, loan_account_id, scheduled_due_date, amount, currency, status, journal_entry_id, waived_by, waive_reason_code, waived_at, assessed_at, updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		 ON CONFLICT (id) DO UPDATE SET
			status = EXCLUDED.status, journal_entry_id = EXCLUDED.journal_entry_id, waived_by = EXCLUDED.waived_by,
			waive_reason_code = EXCLUDED.waive_reason_code, waived_at = EXCLUDED.waived_at, updated_at = EXCLUDED.updated_at`,
		f.FeeID, f.LoanAccountID, f.ScheduledDueDate, f.Amount.Amount, f.Amount.Currency, string(f.Status), f.JournalEntryID, f.WaivedBy, waiveReason, f.WaivedAt, f.AssessedAt, f.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("postgres: saving fee: %w", err)
	}
	return nil
}

func (t *Tx) SaveIdempotentResponse(ctx context.Context, idempotencyKey, requestHash string, responseJSON []byte) error {
	_, err := t.tx.Exec(ctx,
		`INSERT INTO idempotency_keys (idempotency_key, request_hash, response_json) VALUES ($1,$2,$3)
		 ON CONFLICT (idempotency_key) DO NOTHING`,
		idempotencyKey, requestHash, responseJSON,
	)
	if err != nil {
		return fmt.Errorf("postgres: saving idempotent response: %w", err)
	}
	return nil
}

func (t *Tx) InsertOutboxEntry(ctx context.Context, e outbox.Entry) error {
	_, err := t.tx.Exec(ctx, `INSERT INTO outbox (id, topic, payload_json, created_at) VALUES ($1,$2,$3,$4)`, e.ID, e.Topic, e.PayloadJSON, e.CreatedAt)
	if err != nil {
		return fmt.Errorf("postgres: inserting outbox entry: %w", err)
	}
	return nil
}
