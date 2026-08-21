// Package service orchestrates the GL Posting Engine's business logic:
// posting validation and commit, live trial-balance/statement/account-
// balance queries, and period close. This is the ONLY package in the
// entire system permitted to construct a JournalEntry for persistence —
// see internal/domain.NewJournalEntry, the single constructor this
// package calls, which makes an unbalanced entry a compile-time
// impossibility (invariant 1, enforced here in memory a second time
// after internal/postingrules already produced balanced lines, and a
// third time by the database's own deferred constraint trigger — three
// independent layers, not one relying on the others never having a
// bug).
package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/coa"
	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/events"
	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/outbox"
	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/postingrules"
	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/store"
)

type Service struct {
	store store.Store
	chart *coa.Chart
	now   func() time.Time // overridable in tests
	newID func() string    // overridable in tests
}

func New(s store.Store, chart *coa.Chart) *Service {
	return &Service{
		store: s,
		chart: chart,
		now:   func() time.Time { return time.Now().UTC() },
		newID: newUUIDv4,
	}
}

// NewWithClock builds a Service with an overridable clock, exported for
// tests in other packages (mirrors services/crm's identical need).
func NewWithClock(s store.Store, chart *coa.Chart, now func() time.Time) *Service {
	svc := New(s, chart)
	svc.now = now
	return svc
}

// Sentinel errors the API layer maps to specific HTTP statuses.
var (
	ErrUnknownRuleCode            = errors.New("service: unknown posting rule code")
	ErrWrongInputShape            = errors.New("service: input shape does not match the posting rule's required shape")
	ErrMissingReversalTarget      = errors.New("service: reversalOfSourceEventId is required for this rule")
	ErrMissingRequiredMetadata    = errors.New("service: missing required metadata for this posting rule")
	ErrReversalTargetNotFound     = errors.New("service: reversalOfSourceEventId does not reference an existing posted entry")
	ErrReversalTargetPeriodClosed = errors.New("service: cannot reverse an entry in a closed period; post a prior-period adjustment instead")
	ErrReversalAmountMismatch     = errors.New("service: reversal amount does not match the original entry")
	ErrAdjustmentPeriodNotClosed  = errors.New("service: priorPeriodAdjustmentForPeriodId does not reference a closed period")
	ErrCurrentPeriodClosed        = errors.New("service: the current period is closed; use priorPeriodAdjustmentForPeriodId to post a correction")
)

// PostJournalEntryInput mirrors PostJournalEntryRequest exactly one of
// Amount / Allocation / Capitalization is set, matching
// postingrules.ShapeFor(PostingRuleCode).
type PostJournalEntryInput struct {
	IdempotencyKey                   string
	PostingRuleCode                  postingrules.RuleCode
	LoanAccountID                    string
	Amount                           *domain.Money
	Allocation                       *postingrules.Allocation
	Capitalization                   *postingrules.Capitalization
	ReversalOfSourceEventID          *string
	PriorPeriodAdjustmentForPeriodID *string
	Metadata                         map[string]any
}

// PostJournalEntry is invariant 1-7's central write path. See the
// package doc comment for the three independent layers that enforce
// invariant 1 specifically.
func (s *Service) PostJournalEntry(ctx context.Context, in PostJournalEntryInput) (domain.JournalEntry, error) {
	if !postingrules.IsKnownRuleCode(string(in.PostingRuleCode)) {
		return domain.JournalEntry{}, ErrUnknownRuleCode
	}
	if err := s.validateShape(in); err != nil {
		return domain.JournalEntry{}, err
	}
	if err := s.validateRequiredMetadata(in); err != nil {
		return domain.JournalEntry{}, err
	}

	var lines []domain.Line
	var err error
	if postingrules.IsReversalRule(in.PostingRuleCode) {
		lines, err = s.buildReversalLines(ctx, in)
	} else {
		lines, err = s.buildForwardLines(in)
	}
	if err != nil {
		return domain.JournalEntry{}, err
	}

	now := s.now()
	if err := s.validatePriorPeriodAdjustment(ctx, in.PriorPeriodAdjustmentForPeriodID); err != nil {
		return domain.JournalEntry{}, err
	}
	// An ordinary posting's own periodId is always derived from now() --
	// the only way it could land in an already-Closed period is a race
	// between a close operation and a straggling request timestamped
	// just before it, or a clock skew. Invariant 7's literal text only
	// names reversal, but "period close is a distinct, auditable
	// operation" clearly implies a closed period never silently
	// receives ANY new posting -- a correction must go through
	// PriorPeriodAdjustmentForPeriodID instead, same as a reversal must.
	if in.PriorPeriodAdjustmentForPeriodID == nil {
		if err := s.rejectIfPeriodClosed(ctx, domain.PeriodID(now), ErrCurrentPeriodClosed); err != nil {
			return domain.JournalEntry{}, err
		}
	}

	journalLines, err := s.attachRunningBalances(ctx, in.LoanAccountID, lines)
	if err != nil {
		return domain.JournalEntry{}, err
	}

	isPriorPeriodAdjustment := in.PriorPeriodAdjustmentForPeriodID != nil
	entry, err := domain.NewJournalEntry(
		s.newID(), in.IdempotencyKey, string(in.PostingRuleCode), postingrules.ManifestVersion, in.LoanAccountID,
		journalLines, now, isPriorPeriodAdjustment, in.PriorPeriodAdjustmentForPeriodID, in.ReversalOfSourceEventID, in.Metadata,
	)
	if err != nil {
		return domain.JournalEntry{}, err
	}

	var out domain.JournalEntry
	writeErr := s.store.WithinTx(ctx, func(tx store.Tx) error {
		if err := tx.CreateJournalEntry(ctx, entry); err != nil {
			return err
		}
		outboxEntry, err := outbox.NewEntry(s.newID(), events.TopicEntryPosted, toEntryPostedPayload(entry))
		if err != nil {
			return fmt.Errorf("building gl.entry.posted outbox entry: %w", err)
		}
		if err := tx.InsertOutboxEntry(ctx, outboxEntry); err != nil {
			return fmt.Errorf("inserting gl.entry.posted outbox entry: %w", err)
		}
		out = entry
		return nil
	})
	if errors.Is(writeErr, store.ErrDuplicateSourceEventID) {
		// Someone else's concurrent call with the same Idempotency-Key
		// won the race and committed first -- return their entry, not
		// an error. See store.ErrDuplicateSourceEventID's doc comment.
		winner, found, findErr := s.store.FindBySourceEventID(ctx, in.IdempotencyKey)
		if findErr != nil {
			return domain.JournalEntry{}, findErr
		}
		if !found {
			// Vanishingly unlikely (the row that caused the unique
			// violation must exist), but never silently succeed with a
			// zero-value entry.
			return domain.JournalEntry{}, fmt.Errorf("service: source event id %q reported as duplicate but not found", in.IdempotencyKey)
		}
		return winner, nil
	}
	if writeErr != nil {
		return domain.JournalEntry{}, writeErr
	}
	return out, nil
}

func (s *Service) validateShape(in PostJournalEntryInput) error {
	want, err := postingrules.ShapeFor(in.PostingRuleCode)
	if err != nil {
		return ErrUnknownRuleCode
	}
	got := 0
	if in.Amount != nil {
		got++
	}
	if in.Allocation != nil {
		got++
	}
	if in.Capitalization != nil {
		got++
	}
	if got != 1 {
		return fmt.Errorf("%w: exactly one of amount/allocation/capitalization must be set, got %d", ErrWrongInputShape, got)
	}
	switch want {
	case postingrules.ShapeAmount:
		if in.Amount == nil {
			return fmt.Errorf("%w: %s requires amount", ErrWrongInputShape, in.PostingRuleCode)
		}
	case postingrules.ShapeAllocation:
		if in.Allocation == nil {
			return fmt.Errorf("%w: %s requires allocation", ErrWrongInputShape, in.PostingRuleCode)
		}
	case postingrules.ShapeCapitalization:
		if in.Capitalization == nil {
			return fmt.Errorf("%w: %s requires capitalization", ErrWrongInputShape, in.PostingRuleCode)
		}
	}
	if postingrules.IsReversalRule(in.PostingRuleCode) && in.ReversalOfSourceEventID == nil {
		return ErrMissingReversalTarget
	}
	return nil
}

func (s *Service) validateRequiredMetadata(in PostJournalEntryInput) error {
	required := postingrules.RequiredMetadataKeys(in.PostingRuleCode)
	if len(required) == 0 {
		return nil
	}
	for _, key := range required {
		if _, ok := in.Metadata[key]; !ok {
			return fmt.Errorf("%w: %s requires metadata.%s", ErrMissingRequiredMetadata, in.PostingRuleCode, key)
		}
	}
	return nil
}

// buildReversalLines fetches the ACTUAL posted lines of the referenced
// entry and mirrors them (domain.MirrorForReversal) — it never
// re-derives lines from the reversal request's own amount/allocation.
// This is what makes a reversal of a reversal well-defined for free: the
// entry being mirrored might itself be a reversal, and mirroring it
// again is exactly correct (double negation), with no special-casing
// needed here.
func (s *Service) buildReversalLines(ctx context.Context, in PostJournalEntryInput) ([]domain.Line, error) {
	original, found, err := s.store.FindBySourceEventID(ctx, *in.ReversalOfSourceEventID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrReversalTargetNotFound
	}
	if err := s.rejectIfPeriodClosed(ctx, original.PeriodID, ErrReversalTargetPeriodClosed); err != nil {
		return nil, err
	}

	originalLines := make([]domain.Line, len(original.Lines))
	for i, l := range original.Lines {
		originalLines[i] = l.Line
	}

	if err := s.validateReversalAmountMatches(in, originalLines); err != nil {
		return nil, err
	}

	return domain.MirrorForReversal(originalLines), nil
}

// validateReversalAmountMatches is a client-mistake guard, not a
// structural requirement: the mirrored lines above are already correct
// regardless of what the caller supplied, but a caller-supplied
// amount/allocation that doesn't match what's actually being reversed
// is almost certainly a client bug worth rejecting loudly rather than
// silently posting a technically-balanced but confusingly-labeled
// reversal.
func (s *Service) validateReversalAmountMatches(in PostJournalEntryInput, originalLines []domain.Line) error {
	var originalTotal int64
	for _, l := range originalLines {
		if l.Direction == domain.Debit {
			originalTotal += l.Amount.Amount
		}
	}
	var suppliedTotal int64
	switch {
	case in.Amount != nil:
		suppliedTotal = in.Amount.Amount
	case in.Allocation != nil:
		suppliedTotal = in.Allocation.FeeAmount.Amount + in.Allocation.InterestAmount.Amount + in.Allocation.PrincipalAmount.Amount
	}
	if suppliedTotal != originalTotal {
		return fmt.Errorf("%w: supplied total %d does not match original entry's total %d", ErrReversalAmountMismatch, suppliedTotal, originalTotal)
	}
	return nil
}

func (s *Service) buildForwardLines(in PostJournalEntryInput) ([]domain.Line, error) {
	switch in.PostingRuleCode {
	case postingrules.PRDISB01:
		return postingrules.PRDISB01Lines(*in.Amount), nil
	case postingrules.PRACCR01:
		return postingrules.PRACCR01Lines(*in.Amount), nil
	case postingrules.PRDELINQ01:
		return postingrules.PRDELINQ01Lines(*in.Amount), nil
	case postingrules.PRCHGOFF02:
		return postingrules.PRCHGOFF02Lines(*in.Amount), nil
	case postingrules.PRREPAY01:
		return postingrules.PRREPAY01Lines(*in.Allocation)
	case postingrules.PRPAYOFF01:
		return postingrules.PRPAYOFF01Lines(*in.Allocation)
	case postingrules.PRCHGOFF01:
		return postingrules.PRCHGOFF01Lines(*in.Allocation)
	case postingrules.PRMOD01:
		return postingrules.PRMOD01Lines(*in.Capitalization)
	default:
		return nil, ErrUnknownRuleCode
	}
}

func (s *Service) validatePriorPeriodAdjustment(ctx context.Context, periodID *string) error {
	if periodID == nil {
		return nil
	}
	p, err := s.store.GetPeriod(ctx, *periodID)
	if err != nil {
		return err
	}
	if p.Status != domain.PeriodClosed {
		return ErrAdjustmentPeriodNotClosed
	}
	return nil
}

func (s *Service) rejectIfPeriodClosed(ctx context.Context, periodID string, sentinel error) error {
	p, err := s.store.GetPeriod(ctx, periodID)
	if err != nil {
		return err
	}
	if p.Status == domain.PeriodClosed {
		return sentinel
	}
	return nil
}

// attachRunningBalances computes each line's RunningBalanceAfter from a
// live query (invariant 6 — see store.Store.GetLatestRunningBalance's
// doc comment) and applies subsequent lines within the SAME entry
// cumulatively, so an entry with two lines on the same GL account (rare,
// but not structurally forbidden) reflects both, in order.
func (s *Service) attachRunningBalances(ctx context.Context, loanAccountID string, lines []domain.Line) ([]domain.JournalEntryLine, error) {
	out := make([]domain.JournalEntryLine, len(lines))
	running := map[string]int64{}
	for i, l := range lines {
		balance, cached := running[l.GLAccount]
		if !cached {
			current, err := s.store.GetLatestRunningBalance(ctx, loanAccountID, l.GLAccount, l.Amount.Currency)
			if err != nil {
				return nil, err
			}
			balance = current.Amount
		}
		if l.Direction == domain.Debit {
			balance += l.Amount.Amount
		} else {
			balance -= l.Amount.Amount
		}
		running[l.GLAccount] = balance
		out[i] = domain.JournalEntryLine{Line: l, RunningBalanceAfter: domain.Money{Amount: balance, Currency: l.Amount.Currency}}
	}
	return out, nil
}

func toEntryPostedPayload(e domain.JournalEntry) events.EntryPostedPayload {
	lines := make([]events.LinePayload, len(e.Lines))
	for i, l := range e.Lines {
		lines[i] = events.LinePayload{
			GLAccount:           l.GLAccount,
			Direction:           string(l.Direction),
			Amount:              events.Money{Amount: l.Amount.Amount, Currency: l.Amount.Currency},
			RunningBalanceAfter: events.Money{Amount: l.RunningBalanceAfter.Amount, Currency: l.RunningBalanceAfter.Currency},
		}
	}
	return events.EntryPostedPayload{
		JournalEntryID: e.ID, SourceEventID: e.SourceEventID, PostingRuleCode: e.PostingRuleCode,
		PostingRuleVersion: e.PostingRuleVersion, LoanAccountID: e.LoanAccountID, Lines: lines,
		Balanced: e.Balanced(), Immutable: e.Immutable(), PostedAt: e.PostedAt, PeriodID: e.PeriodID,
		IsPriorPeriodAdjustment: e.IsPriorPeriodAdjustment, AdjustmentForPeriodID: e.AdjustmentForPeriodID, Metadata: e.Metadata,
	}
}

// --- Reads --------------------------------------------------------------

func (s *Service) GetJournalEntry(ctx context.Context, journalEntryID string) (domain.JournalEntry, error) {
	return s.store.GetJournalEntry(ctx, journalEntryID)
}

func (s *Service) FindBySourceEventID(ctx context.Context, sourceEventID string) (domain.JournalEntry, bool, error) {
	return s.store.FindBySourceEventID(ctx, sourceEventID)
}

// GetAccountBalance, GetTrialBalance, and GetStatementOfAccount are all
// thin pass-throughs to store methods that are themselves always live
// queries — invariant 6.
func (s *Service) GetAccountBalance(ctx context.Context, glAccountCode string, asOf time.Time) (domain.Money, error) {
	if !s.chart.IsValidCode(glAccountCode) {
		return domain.Money{}, store.ErrNotFound
	}
	return s.store.GetAccountBalance(ctx, glAccountCode, asOf)
}

func (s *Service) GetTrialBalance(ctx context.Context, asOf time.Time) ([]store.TrialBalanceLine, error) {
	return s.store.GetTrialBalance(ctx, asOf)
}

func (s *Service) GetStatementOfAccount(ctx context.Context, loanAccountID string, asOf time.Time) ([]store.StatementLine, error) {
	return s.store.GetStatementOfAccount(ctx, loanAccountID, asOf)
}

// --- Period close ---------------------------------------------------------

// ClosePeriod enforces invariant 7: chronological order (no earlier
// period may still be Open), and idempotent on a repeat close.
func (s *Service) ClosePeriod(ctx context.Context, periodID, closedBy string) (domain.Period, error) {
	earliest, found, err := s.store.EarliestOpenPeriodBefore(ctx, periodID)
	if err != nil {
		return domain.Period{}, err
	}
	if found {
		return domain.Period{}, &domain.ErrEarlierPeriodOpen{RequestedPeriodID: periodID, EarliestOpenPeriodID: earliest}
	}

	current, err := s.store.GetPeriod(ctx, periodID)
	if err != nil {
		return domain.Period{}, err
	}
	updated, changed := current.Close(closedBy, s.now())
	if !changed {
		return updated, nil
	}

	var out domain.Period
	writeErr := s.store.WithinTx(ctx, func(tx store.Tx) error {
		if err := tx.ClosePeriod(ctx, updated); err != nil {
			return err
		}
		entry, err := outbox.NewEntry(s.newID(), events.TopicPeriodClosed, events.PeriodClosedPayload{
			PeriodID: updated.ID, ClosedAt: *updated.ClosedAt, ClosedBy: *updated.ClosedBy,
		})
		if err != nil {
			return fmt.Errorf("building gl.period.closed outbox entry: %w", err)
		}
		if err := tx.InsertOutboxEntry(ctx, entry); err != nil {
			return fmt.Errorf("inserting gl.period.closed outbox entry: %w", err)
		}
		out = updated
		return nil
	})
	return out, writeErr
}

func (s *Service) GetPeriod(ctx context.Context, periodID string) (domain.Period, error) {
	return s.store.GetPeriod(ctx, periodID)
}
