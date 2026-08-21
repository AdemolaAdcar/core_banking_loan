// Package service orchestrates every Loan Account Subledger domain
// intent: it owns the LoanAccount state machine (internal/domain),
// translates each intent into exactly one call to GLPostingAPI's typed
// client (internal/glclient), and rebuilds the read-optimized balance
// projection from GL's posted entries after every posting. Nothing in
// this package constructs a journal entry's lines or picks a GL
// account — that decision belongs entirely to GLPostingAPI.
//
// Deferred integrations (documented in PR_DESCRIPTION.md, same
// "flagged, not silently built" discipline the GL/CRM/Party phases
// established): this service does not yet call PartyAPI (booking/
// disbursement party validation) or PaymentAPI (disbursement funding
// confirmation, payment execution) — both are out of this role's scope
// (state machine, GL client, balance projection). Where a domain flow
// depends on one of those, this package exposes the internal method a
// future consumer of that integration would call (e.g.
// ConfirmDisbursementFunding, which a PaymentAPI
// payment.disbursement.confirmed consumer would invoke), fully
// implemented and tested end to end against a fake GL client, but not
// yet wired to a live HTTP/Kafka trigger.
package service

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/glclient"
	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/outbox"
	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/store"
)

type Service struct {
	store  store.Store
	gl     glclient.Client
	now    func() time.Time
	newID  func() string
	quotes *quoteCache
}

func New(st store.Store, gl glclient.Client) *Service {
	return &Service{
		store: st, gl: gl,
		now:    func() time.Time { return time.Now().UTC() },
		newID:  func() string { return uuid.NewString() },
		quotes: newQuoteCache(),
	}
}

var (
	// ErrMalformedTerms: a required booking term is missing/invalid —
	// 400, no account created (REQ-CB-BOOK-001).
	ErrMalformedTerms = errors.New("service: malformed or missing loan terms")

	// ErrLoanAccountNotFound / ErrNotFound: mapped to 404.
	ErrLoanAccountNotFound = errors.New("service: loan account not found")
	ErrNotFound            = errors.New("service: resource not found")

	// ErrTerminalAccount is returned whenever a posting-triggering
	// operation targets a Closed or ChargedOff account other than a
	// recovery — "A posting request against a Closed or WrittenOff
	// account (other than a recovery posting) is rejected, not queued or
	// silently dropped." Mapped to 409.
	ErrTerminalAccount = errors.New("service: loan account is in a terminal status (Closed or ChargedOff) and cannot accept this posting")

	// ErrNotModifiable: applyModification against an account that isn't
	// Disbursed.
	ErrNotModifiable = errors.New("service: loan account is not in a modifiable status")
)

// postToGL is the single choke point every domain intent in this
// service posts through — see this package's own doc comment and
// internal/glclient's. It does no translation of its own: callers
// branch directly on the glclient sentinel errors
// (glclient.ErrRequestRejected, glclient.ErrIdempotencyKeyReused,
// glclient.ErrGLUnavailable) via errors.Is, which internal/api's error
// mapping also does — one error vocabulary from GL's client all the way
// to the HTTP response, no lossy re-wrapping in between.
func (s *Service) postToGL(ctx context.Context, in glclient.PostJournalEntryInput) (glclient.JournalEntry, error) {
	return s.gl.PostJournalEntry(ctx, in)
}

// RefreshBalanceProjection rebuilds loanAccountID's BalanceProjection
// from GL's current posted lines and persists the result. This is the
// ONLY function in this service that writes a BalanceProjection, and it
// always does so by fetching GL's live statement and calling
// domain.RebuildFromLines — never by adjusting a prior value.
//
// Called synchronously by this service immediately after any of its own
// postings succeed (so a caller reading the projection right after a
// write sees it reflect that write, without waiting on a Kafka
// round-trip), and is also the exact function a future gl.entry.posted
// Kafka consumer would call for cross-service postings this account
// might one day have — e.g. a correction posted directly against GL by
// another module. That consumer is not wired yet (see this package's
// doc comment); this method is what it would invoke.
func (s *Service) RefreshBalanceProjection(ctx context.Context, loanAccountID string) (domain.BalanceProjection, error) {
	lines, err := s.gl.GetStatementOfAccount(ctx, loanAccountID)
	if err != nil {
		return domain.BalanceProjection{}, err
	}
	domainLines := make([]domain.StatementLine, len(lines))
	for i, l := range lines {
		domainLines[i] = domain.StatementLine{JournalEntryID: l.JournalEntryID, GLAccount: l.GLAccount, Direction: l.Direction, Amount: l.Amount, PostedAt: l.PostedAt}
	}
	projection, err := domain.RebuildFromLines(loanAccountID, domainLines, s.now())
	if err != nil {
		return domain.BalanceProjection{}, err
	}
	if err := s.store.WithinTx(ctx, func(tx store.Tx) error {
		return tx.SaveBalanceProjection(ctx, projection)
	}); err != nil {
		return domain.BalanceProjection{}, err
	}
	return projection, nil
}

// GetBalanceProjection returns the freshest available projection: it
// always triggers a live rebuild rather than trusting the cached row,
// since a caller asking for the current balance is exactly the case
// where a stale cache would violate this service's core ground rule.
func (s *Service) GetBalanceProjection(ctx context.Context, loanAccountID string) (domain.BalanceProjection, error) {
	return s.RefreshBalanceProjection(ctx, loanAccountID)
}

func allocationToGL(a domain.Allocation) *glclient.Allocation {
	return &glclient.Allocation{FeeAmount: a.FeeAmount, InterestAmount: a.InterestAmount, PrincipalAmount: a.PrincipalAmount}
}

func capitalizationToGL(c domain.Capitalization) *glclient.Capitalization {
	return &glclient.Capitalization{InterestAmount: c.InterestAmount, FeeAmount: c.FeeAmount}
}

func publishEvent(ctx context.Context, tx store.Tx, id, topic string, payload any) error {
	e, err := outbox.NewEntry(id, topic, payload)
	if err != nil {
		return err
	}
	return tx.InsertOutboxEntry(ctx, e)
}
