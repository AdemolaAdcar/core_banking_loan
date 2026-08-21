package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/glclient"
	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/postingrules"
	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/store"
)

// quoteCache retains PayoffQuotes this process has issued, purely so a
// later receiveRepaymentNotification call can validate a payoffQuoteId
// reference (expiry + exact-amount match) against a real quote.
// GetPayoffQuote's actual balance figures are still always computed
// live on every call (never served from this cache) — this only
// remembers WHICH quotes were issued and their terms, not a cached
// balance. In-process only, not persisted: a process restart or a
// second instance behind a load balancer won't see a quote issued by
// another instance, which degrades that quote to an expired/not-found
// lookup (falls back to ordinary repayment processing per branch (e),
// never a silent misapplication) — flagged as a known simplification in
// PR_DESCRIPTION.md, not hidden.
type quoteCache struct {
	mu     sync.Mutex
	quotes map[string]domain.PayoffQuote
}

func newQuoteCache() *quoteCache { return &quoteCache{quotes: map[string]domain.PayoffQuote{}} }

func (c *quoteCache) put(q domain.PayoffQuote) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.quotes[q.QuoteID] = q
}

func (c *quoteCache) get(id string) (domain.PayoffQuote, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	q, ok := c.quotes[id]
	return q, ok
}

type ReceiveRepaymentNotificationInput struct {
	IdempotencyKey string // paymentReferenceId
	LoanAccountRef *string
	PayoffQuoteID  *string
	Amount         domain.Money
	Rail           string
	ReceivedAt     time.Time
}

// RepaymentNotificationKind identifies which resource
// receiveRepaymentNotification produced — GL's response is a oneOf, and
// so is this.
type RepaymentNotificationKind string

const (
	KindRepayment RepaymentNotificationKind = "Repayment"
	KindPayoff    RepaymentNotificationKind = "Payoff"
	KindRecovery  RepaymentNotificationKind = "Recovery"
)

type RepaymentNotificationResult struct {
	Kind      RepaymentNotificationKind
	Repayment *domain.Repayment
	Payoff    *domain.Payoff
	Recovery  *domain.Recovery
}

type loanRepaymentPostedPayload struct {
	LoanAccountID  string `json:"loanAccountId"`
	RepaymentID    string `json:"repaymentId"`
	JournalEntryID string `json:"journalEntryId"`
	PostedAt       string `json:"postedAt"`
}

type paymentMatchFailedPayload struct {
	PaymentReferenceID     string       `json:"paymentReferenceId"`
	LoanAccountRefReceived *string      `json:"loanAccountRefReceived"`
	Amount                 payloadMoney `json:"amount"`
	ReasonCode             string       `json:"reasonCode"`
	UnmatchedAt            string       `json:"unmatchedAt"`
}

type loanAccountClosedPayload struct {
	LoanAccountID  string `json:"loanAccountId"`
	PayoffID       string `json:"payoffId"`
	JournalEntryID string `json:"journalEntryId"`
	ClosedAt       string `json:"closedAt"`
}

// ReceiveRepaymentNotification implements receiveRepaymentNotification
// (REQ-CB-REPAY-001..005; REQ-CB-PAYOFF-003..008; REQ-CB-CHARGEOFF-006)
// — the single unified entry point for repayment, payoff, and recovery
// money. See this method's branch comments for the exact order
// (recovery checked first, unconditionally, ahead of any payoffQuoteId).
func (s *Service) ReceiveRepaymentNotification(ctx context.Context, in ReceiveRepaymentNotificationInput) (RepaymentNotificationResult, error) {
	if existing, found, err := s.findExistingNotificationOutcome(ctx, in.IdempotencyKey); err != nil {
		return RepaymentNotificationResult{}, err
	} else if found {
		return existing, nil
	}

	if in.LoanAccountRef == nil || *in.LoanAccountRef == "" {
		return s.recordUnmatchedRepayment(ctx, in, "NO_MATCH")
	}
	account, err := s.GetLoanAccount(ctx, *in.LoanAccountRef)
	if errors.Is(err, ErrLoanAccountNotFound) {
		return s.recordUnmatchedRepayment(ctx, in, "NO_MATCH")
	}
	if err != nil {
		return RepaymentNotificationResult{}, err
	}

	// Branch (0): matched account already ChargedOff -> ALWAYS a
	// recovery, checked first, regardless of payoffQuoteId
	// (REQ-CB-CHARGEOFF-006).
	if account.Status == domain.StatusChargedOff {
		return s.postRecovery(ctx, account, in)
	}

	// Ground rule: "A posting request against a Closed or WrittenOff
	// account (other than a recovery posting) is rejected, not queued or
	// silently dropped." ChargedOff is handled above via recovery;
	// Closed has no equivalent carve-out anywhere in the approved spec —
	// a matched-but-Closed account rejects unconditionally here, before
	// any payoffQuoteId branching or waterfall computation runs.
	if account.Status == domain.StatusClosed {
		return RepaymentNotificationResult{}, fmt.Errorf("%w: loan account %s is Closed", ErrTerminalAccount, account.LoanAccountID)
	}

	if in.PayoffQuoteID != nil {
		if quote, ok := s.quotes.get(*in.PayoffQuoteID); ok && quote.LoanAccountID == account.LoanAccountID && !in.ReceivedAt.After(endOfDay(quote.GoodThrough)) {
			switch {
			case in.Amount.Amount == quote.TotalAmountDue.Amount:
				return s.postPayoff(ctx, account, quote, in, domain.Money{}) // exact match, no suspense
			case in.Amount.Amount > quote.TotalAmountDue.Amount:
				excess := domain.Money{Amount: in.Amount.Amount - quote.TotalAmountDue.Amount, Currency: in.Amount.Currency}
				return s.postPayoff(ctx, account, quote, in, excess)
			default:
				// underpaid against a valid quote -- degrades to ordinary
				// repayment (branch c).
			}
		}
		// expired / not found / underpaid -> falls through to ordinary
		// repayment, same as if no payoffQuoteId had been sent (branches
		// (c) and (e)).
	}

	return s.postOrdinaryRepayment(ctx, account, in)
}

func endOfDay(d time.Time) time.Time {
	return time.Date(d.Year(), d.Month(), d.Day(), 23, 59, 59, 0, time.UTC)
}

func (s *Service) findExistingNotificationOutcome(ctx context.Context, id string) (RepaymentNotificationResult, bool, error) {
	if r, err := s.store.GetRepayment(ctx, id); err == nil {
		return RepaymentNotificationResult{Kind: KindRepayment, Repayment: &r}, true, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return RepaymentNotificationResult{}, false, err
	}
	if p, err := s.store.GetPayoff(ctx, id); err == nil {
		return RepaymentNotificationResult{Kind: KindPayoff, Payoff: &p}, true, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return RepaymentNotificationResult{}, false, err
	}
	if r, err := s.store.GetRecovery(ctx, id); err == nil {
		return RepaymentNotificationResult{Kind: KindRecovery, Recovery: &r}, true, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return RepaymentNotificationResult{}, false, err
	}
	return RepaymentNotificationResult{}, false, nil
}

func (s *Service) recordUnmatchedRepayment(ctx context.Context, in ReceiveRepaymentNotificationInput, reason string) (RepaymentNotificationResult, error) {
	now := s.now()
	reasonCode := domain.UnmatchedReason(reason)
	r := domain.Repayment{
		RepaymentID: in.IdempotencyKey, Status: domain.RepaymentUnmatched, Amount: in.Amount,
		SuspenseAmount: domain.Money{Currency: in.Amount.Currency}, UnmatchedReasonCode: &reasonCode, ReceivedAt: in.ReceivedAt, UpdatedAt: now,
	}
	err := s.store.WithinTx(ctx, func(tx store.Tx) error {
		if err := tx.SaveRepayment(ctx, r); err != nil {
			return err
		}
		return publishEvent(ctx, tx, in.IdempotencyKey+":unmatched", "payment.match.failed", paymentMatchFailedPayload{
			PaymentReferenceID: in.IdempotencyKey, LoanAccountRefReceived: in.LoanAccountRef, Amount: toPayloadMoney(in.Amount), ReasonCode: reason, UnmatchedAt: now.Format(time.RFC3339),
		})
	})
	if err != nil {
		return RepaymentNotificationResult{}, err
	}
	return RepaymentNotificationResult{Kind: KindRepayment, Repayment: &r}, nil
}

func (s *Service) postRecovery(ctx context.Context, account domain.LoanAccount, in ReceiveRepaymentNotificationInput) (RepaymentNotificationResult, error) {
	entry, err := s.postToGL(ctx, glclient.PostJournalEntryInput{
		IdempotencyKey: in.IdempotencyKey, PostingRuleCode: postingrules.PRCHGOFF02, LoanAccountID: account.LoanAccountID, Amount: &in.Amount,
	})
	if err != nil {
		return RepaymentNotificationResult{}, err
	}
	now := s.now()
	r := domain.Recovery{RecoveryID: in.IdempotencyKey, LoanAccountID: account.LoanAccountID, Amount: in.Amount, JournalEntryID: &entry.JournalEntryID, ReceivedAt: in.ReceivedAt, UpdatedAt: now}
	if err := s.store.WithinTx(ctx, func(tx store.Tx) error { return tx.SaveRecovery(ctx, r) }); err != nil {
		return RepaymentNotificationResult{}, err
	}
	if _, err := s.RefreshBalanceProjection(ctx, account.LoanAccountID); err != nil {
		return RepaymentNotificationResult{}, err
	}
	return RepaymentNotificationResult{Kind: KindRecovery, Recovery: &r}, nil
}

func (s *Service) postPayoff(ctx context.Context, account domain.LoanAccount, quote domain.PayoffQuote, in ReceiveRepaymentNotificationInput, suspense domain.Money) (RepaymentNotificationResult, error) {
	nextAccount, err := account.TransitionTo(domain.StatusClosed, s.now())
	if err != nil {
		return RepaymentNotificationResult{}, err
	}
	alloc := &glclient.Allocation{FeeAmount: quote.FeesAmount, InterestAmount: quote.ProjectedInterestAmount, PrincipalAmount: quote.PrincipalAmount}
	entry, err := s.postToGL(ctx, glclient.PostJournalEntryInput{
		IdempotencyKey: in.IdempotencyKey, PostingRuleCode: postingrules.PRPAYOFF01, LoanAccountID: account.LoanAccountID, Allocation: alloc,
	})
	if err != nil {
		return RepaymentNotificationResult{}, err
	}
	now := s.now()
	p := domain.Payoff{
		PayoffID: in.IdempotencyKey, LoanAccountID: account.LoanAccountID, QuoteID: quote.QuoteID, Status: domain.PayoffClosed,
		AmountPosted: quote.TotalAmountDue, SuspenseAmount: suspense, JournalEntryID: &entry.JournalEntryID, ClosedAt: &now, UpdatedAt: now,
	}
	err = s.store.WithinTx(ctx, func(tx store.Tx) error {
		if err := tx.SaveLoanAccount(ctx, nextAccount); err != nil {
			return err
		}
		if err := tx.SavePayoff(ctx, p); err != nil {
			return err
		}
		return publishEvent(ctx, tx, in.IdempotencyKey+":closed", "loan.account.closed", loanAccountClosedPayload{
			LoanAccountID: account.LoanAccountID, PayoffID: in.IdempotencyKey, JournalEntryID: entry.JournalEntryID, ClosedAt: now.Format(time.RFC3339),
		})
	})
	if err != nil {
		return RepaymentNotificationResult{}, err
	}
	if _, err := s.RefreshBalanceProjection(ctx, account.LoanAccountID); err != nil {
		return RepaymentNotificationResult{}, err
	}
	return RepaymentNotificationResult{Kind: KindPayoff, Payoff: &p}, nil
}

// waterfall applies fees -> interest -> principal against the account's
// current outstanding balance, per REQ-CB-REPAY-002/003. Any amount
// beyond total outstanding is suspense — deliberately NOT included in
// what is posted to GL as principal/interest/fee, since crediting more
// than what is actually owed against a receivable would misstate it.
func waterfall(amount domain.Money, projection domain.BalanceProjection) (alloc domain.Allocation, suspense domain.Money) {
	remaining := amount.Amount
	take := func(owed int64) int64 {
		if remaining <= 0 || owed <= 0 {
			return 0
		}
		if remaining >= owed {
			remaining -= owed
			return owed
		}
		applied := remaining
		remaining = 0
		return applied
	}
	feeAlloc := take(projection.FeeReceivable.Amount)
	interestAlloc := take(projection.InterestReceivable.Amount)
	principalAlloc := take(projection.OutstandingPrincipal.Amount)
	return domain.Allocation{
			FeeAmount:       domain.Money{Amount: feeAlloc, Currency: amount.Currency},
			InterestAmount:  domain.Money{Amount: interestAlloc, Currency: amount.Currency},
			PrincipalAmount: domain.Money{Amount: principalAlloc, Currency: amount.Currency},
		},
		domain.Money{Amount: remaining, Currency: amount.Currency}
}

func (s *Service) postOrdinaryRepayment(ctx context.Context, account domain.LoanAccount, in ReceiveRepaymentNotificationInput) (RepaymentNotificationResult, error) {
	projection, err := s.GetBalanceProjection(ctx, account.LoanAccountID)
	if err != nil {
		return RepaymentNotificationResult{}, err
	}
	alloc, suspense := waterfall(in.Amount, projection)

	entry, err := s.postToGL(ctx, glclient.PostJournalEntryInput{
		IdempotencyKey: "repay:" + in.IdempotencyKey, PostingRuleCode: postingrules.PRREPAY01, LoanAccountID: account.LoanAccountID, Allocation: allocationToGL(alloc),
	})
	if err != nil {
		return RepaymentNotificationResult{}, err
	}
	now := s.now()
	loanAccountID := account.LoanAccountID
	r := domain.Repayment{
		RepaymentID: in.IdempotencyKey, LoanAccountID: &loanAccountID, Status: domain.RepaymentPosted, Amount: in.Amount,
		Allocation: &alloc, SuspenseAmount: suspense, JournalEntryID: &entry.JournalEntryID, ReceivedAt: in.ReceivedAt, UpdatedAt: now,
	}
	err = s.store.WithinTx(ctx, func(tx store.Tx) error {
		if err := tx.SaveRepayment(ctx, r); err != nil {
			return err
		}
		return publishEvent(ctx, tx, in.IdempotencyKey+":posted", "loan.repayment.posted", loanRepaymentPostedPayload{
			LoanAccountID: account.LoanAccountID, RepaymentID: in.IdempotencyKey, JournalEntryID: entry.JournalEntryID, PostedAt: now.Format(time.RFC3339),
		})
	})
	if err != nil {
		return RepaymentNotificationResult{}, err
	}
	if _, err := s.RefreshBalanceProjection(ctx, account.LoanAccountID); err != nil {
		return RepaymentNotificationResult{}, err
	}
	return RepaymentNotificationResult{Kind: KindRepayment, Repayment: &r}, nil
}

func (s *Service) GetRepayment(ctx context.Context, repaymentID string) (domain.Repayment, error) {
	r, err := s.store.GetRepayment(ctx, repaymentID)
	if errors.Is(err, store.ErrNotFound) {
		return domain.Repayment{}, ErrNotFound
	}
	return r, err
}

// ReverseRepayment implements reverseRepayment (REQ-CB-REPAY-007).
func (s *Service) ReverseRepayment(ctx context.Context, repaymentID, idempotencyKey, confirmedBy, reasonCode string, correctedLoanAccountID *string) (domain.Repayment, error) {
	r, err := s.GetRepayment(ctx, repaymentID)
	if err != nil {
		return domain.Repayment{}, err
	}
	if r.Status == domain.RepaymentReversed || r.Status == domain.RepaymentCorrected || r.Status == domain.RepaymentReversalPending {
		return r, nil
	}
	if r.Status != domain.RepaymentPosted {
		return domain.Repayment{}, fmt.Errorf("%w: repayment %s is not Posted", ErrNotModifiable, repaymentID)
	}
	if reasonCode == string(domain.ReasonMisapplied) && (correctedLoanAccountID == nil || *correctedLoanAccountID == "") {
		return domain.Repayment{}, fmt.Errorf("%w: correctedLoanAccountId is required when reasonCode is MISAPPLIED", ErrMalformedTerms)
	}

	now := s.now()
	r.Status = domain.RepaymentReversalPending
	r.UpdatedAt = now
	if err := s.store.WithinTx(ctx, func(tx store.Tx) error { return tx.SaveRepayment(ctx, r) }); err != nil {
		return domain.Repayment{}, err
	}

	target := "repay:" + repaymentID
	_, err = s.postToGL(ctx, glclient.PostJournalEntryInput{
		IdempotencyKey: idempotencyKey, PostingRuleCode: postingrules.PRREPAY02, LoanAccountID: *r.LoanAccountID,
		Amount: nil, Allocation: allocationToGL(*r.Allocation), ReversalOfSourceEventID: &target,
	})
	if err != nil {
		return domain.Repayment{}, err
	}

	finalStatus := domain.RepaymentReversed
	if reasonCode == string(domain.ReasonMisapplied) {
		correctionKey := repaymentID + ":correction"
		correctionEntry, err := s.postToGL(ctx, glclient.PostJournalEntryInput{
			IdempotencyKey: correctionKey, PostingRuleCode: postingrules.PRREPAY01, LoanAccountID: *correctedLoanAccountID, Allocation: allocationToGL(*r.Allocation),
		})
		if err != nil {
			return domain.Repayment{}, err
		}
		correctedRepaymentID := correctionKey
		r.CorrectedRepaymentID = &correctedRepaymentID
		finalStatus = domain.RepaymentCorrected

		correctedLoanAccountIDCopy := *correctedLoanAccountID
		correctedRepayment := domain.Repayment{
			RepaymentID: correctionKey, LoanAccountID: &correctedLoanAccountIDCopy, Status: domain.RepaymentPosted, Amount: r.Amount,
			Allocation: r.Allocation, SuspenseAmount: domain.Money{Currency: r.Amount.Currency}, JournalEntryID: &correctionEntry.JournalEntryID, ReceivedAt: now, UpdatedAt: now,
		}
		if err := s.store.WithinTx(ctx, func(tx store.Tx) error { return tx.SaveRepayment(ctx, correctedRepayment) }); err != nil {
			return domain.Repayment{}, err
		}
		if _, err := s.RefreshBalanceProjection(ctx, *correctedLoanAccountID); err != nil {
			return domain.Repayment{}, err
		}
	}

	r.Status = finalStatus
	r.UpdatedAt = now
	if err := s.store.WithinTx(ctx, func(tx store.Tx) error { return tx.SaveRepayment(ctx, r) }); err != nil {
		return domain.Repayment{}, err
	}
	if _, err := s.RefreshBalanceProjection(ctx, *r.LoanAccountID); err != nil {
		return domain.Repayment{}, err
	}
	return r, nil
}

func (s *Service) GetPayoff(ctx context.Context, payoffID string) (domain.Payoff, error) {
	p, err := s.store.GetPayoff(ctx, payoffID)
	if errors.Is(err, store.ErrNotFound) {
		return domain.Payoff{}, ErrNotFound
	}
	return p, err
}

func (s *Service) GetRecovery(ctx context.Context, recoveryID string) (domain.Recovery, error) {
	r, err := s.store.GetRecovery(ctx, recoveryID)
	if errors.Is(err, store.ErrNotFound) {
		return domain.Recovery{}, ErrNotFound
	}
	return r, err
}

// GetPayoffQuote implements getPayoffQuote (REQ-CB-PAYOFF-001..003) — a
// live calculation from the current BalanceProjection, never cached as
// the resource itself. See quoteCache's doc comment for the narrow,
// flagged exception: the ISSUED quote's terms (not its balance figures)
// are retained in-process so a later receiveRepaymentNotification call
// can validate a payoffQuoteId reference.
//
// KNOWN SIMPLIFICATION (flagged in PR_DESCRIPTION.md, not silently
// implemented as if complete): projectedInterestAmount is the account's
// currently-outstanding InterestReceivable as of the last posted
// PR-ACCR-01, NOT forward-projected day-by-day through goodThrough —
// a full day-count projection engine is out of this role's scope
// (state machine, GL client, balance projection).
func (s *Service) GetPayoffQuote(ctx context.Context, loanAccountID string, goodThrough time.Time) (domain.PayoffQuote, error) {
	account, err := s.GetLoanAccount(ctx, loanAccountID)
	if err != nil {
		return domain.PayoffQuote{}, err
	}
	if account.IsTerminal() {
		return domain.PayoffQuote{}, fmt.Errorf("%w: loan account %s is not eligible for payoff (status %s)", ErrNotFound, loanAccountID, account.Status)
	}
	projection, err := s.GetBalanceProjection(ctx, loanAccountID)
	if err != nil {
		return domain.PayoffQuote{}, err
	}
	total, err := projection.TotalOutstanding()
	if err != nil {
		return domain.PayoffQuote{}, err
	}
	now := s.now()
	q := domain.PayoffQuote{
		QuoteID: s.newID(), LoanAccountID: loanAccountID, GoodThrough: goodThrough,
		PrincipalAmount: projection.OutstandingPrincipal, ProjectedInterestAmount: projection.InterestReceivable, FeesAmount: projection.FeeReceivable,
		TotalAmountDue: total, GeneratedAt: now,
	}
	s.quotes.put(q)
	return q, nil
}
