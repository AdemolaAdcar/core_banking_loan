// Package service orchestrates every Payment Execution intent: it owns
// the PaymentInstruction lifecycle (internal/domain), talks to exactly
// one rail through internal/railclient.Client (never a concrete rail
// SDK), and talks to AccountAPI through internal/accountclient.Client
// for every synchronous inbound-payment/compensating-reversal call this
// role's ground rules require.
//
// The refusal clause this role's system prompt states literally — "If a
// requirement would post a disbursement or repayment without a matched,
// confirmed rail event, refuse and explain the compliant alternative" —
// is satisfied STRUCTURALLY, not by a runtime check sprinkled through
// this package: domain.NewOutboundDisbursement only ever constructs a
// PaymentInstruction in Submitted status, and the ONLY function in this
// entire service that can move one to Executed is
// PaymentInstruction.TransitionTo, called ONLY from reconciliation.go's
// confirmation-matching path. There is no method anywhere in this
// package (and no HTTP handler in internal/api) that lets a caller mark
// a PaymentInstruction Executed directly — the same "the only way to
// construct/transition X is through this one path" discipline
// services/gl's NewJournalEntry and services/las's TransitionTo already
// established for their own roles.
package service

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/accountclient"
	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/outbox"
	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/railclient"
	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/store"
)

type Service struct {
	store   store.Store
	rail    railclient.Client
	account accountclient.Client
	now     func() time.Time
	newID   func() string
}

func New(st store.Store, rail railclient.Client, account accountclient.Client) *Service {
	return &Service{
		store: st, rail: rail, account: account,
		now: func() time.Time { return time.Now().UTC() }, newID: func() string { return uuid.NewString() },
	}
}

var (
	// ErrMissingJournalEntry: InitiateDisbursement called with no
	// journalEntryId — REQ-CB-DISB-004/payment-execution.yaml's own
	// 400 case ("journalEntryId does not correspond to a confirmed
	// PR-DISB-01 entry"). This service never assumes a caller's word
	// that GL has already posted; the field is required and this is as
	// far as this service's own validation goes without a live GL
	// lookup (deferred, same "flagged, not silently invented"
	// discipline every service in this repo applies to a cross-service
	// dependency it doesn't own — see PR_DESCRIPTION.md).
	ErrMissingJournalEntry = errors.New("service: journalEntryId is required — AccountAPI must confirm PR-DISB-01 before calling initiateDisbursement")

	// ErrNotFound: no PaymentInstruction with the given ID.
	ErrNotFound = errors.New("service: payment instruction not found")

	// ErrNoCompliantReversalPath is this role's OWN "refuse and explain
	// the compliant alternative" clause, made concrete: a returned
	// inbound payment whose original PaymentInstruction has
	// Purpose=PAYOFF has NO existing AccountAPI reversal endpoint to
	// call — services/las's own design note documents this explicitly
	// ("There is no reversal endpoint for a Payoff in the shipped
	// contract... open question carried forward unresolved"). Rather
	// than silently no-op (a silent write-off) or call
	// reverseRepayment against a resource it was never built for (which
	// would either 404 or corrupt an unrelated Repayment), this service
	// refuses the automatic path entirely: it still records the return
	// on its OWN PaymentInstruction (see reconcileInboundReturn) and
	// files a domain.ReconciliationException
	// (ExceptionNoCompliantReversalPath) so Ops/Financial-Reporting
	// performs the GL correction through a reviewed manual process —
	// never a silent balance drift, but also never a fabricated
	// automated call to an endpoint that doesn't exist for this case.
	ErrNoCompliantReversalPath = errors.New("service: no compliant AccountAPI reversal path exists for a returned Payoff payment — filed as a reconciliation exception for manual Ops correction")
)

func (s *Service) GetPaymentInstruction(ctx context.Context, instructionID string) (domain.PaymentInstruction, error) {
	p, err := s.store.GetPaymentInstruction(ctx, instructionID)
	if errors.Is(err, store.ErrNotFound) {
		return domain.PaymentInstruction{}, ErrNotFound
	}
	return p, err
}

type InitiateDisbursementInput struct {
	IdempotencyKey string // becomes PaymentInstruction.InstructionID
	LoanAccountID  string
	PartyID        string
	JournalEntryID string
	Amount         domain.Money
}

// InitiateDisbursement implements initiateDisbursement (REQ-CB-DISB-004).
// Idempotent on in.IdempotencyKey at TWO independent layers: this
// method's own lookup-before-initiate check, AND railclient.Client's own
// dedup ledger (every adapter in internal/rails enforces this itself) —
// the second layer is what makes it safe to call rail.Initiate again
// after a crash between a successful rail submission and this method's
// own SavePaymentInstruction, exactly the "retried call with the same
// key must never result in two outbound payments, even if the first
// attempt's response was lost" ground rule.
func (s *Service) InitiateDisbursement(ctx context.Context, in InitiateDisbursementInput) (domain.PaymentInstruction, error) {
	if existing, err := s.store.GetPaymentInstruction(ctx, in.IdempotencyKey); err == nil {
		return existing, nil // idempotent replay
	} else if !errors.Is(err, store.ErrNotFound) {
		return domain.PaymentInstruction{}, err
	}

	if in.JournalEntryID == "" {
		return domain.PaymentInstruction{}, ErrMissingJournalEntry
	}

	now := s.now()
	instruction := domain.NewOutboundDisbursement(in.IdempotencyKey, in.LoanAccountID, in.PartyID, in.JournalEntryID, in.Amount, now)

	sub, err := s.rail.Initiate(ctx, railclient.InitiateInput{
		InstructionID: in.IdempotencyKey, LoanAccountID: in.LoanAccountID, PartyID: in.PartyID,
		JournalEntryID: in.JournalEntryID, Amount: in.Amount,
	})
	if err != nil {
		return domain.PaymentInstruction{}, err
	}
	instruction = instruction.WithRailReference(sub.RailReference, now)

	if err := s.store.WithinTx(ctx, func(tx store.Tx) error {
		return tx.SavePaymentInstruction(ctx, instruction)
	}); err != nil {
		return domain.PaymentInstruction{}, err
	}
	return instruction, nil
}

// paymentInstructionPayload mirrors
// payment-instruction.schema.json#/$defs/PaymentInstruction
// field-for-field — payment.disbursement.confirmed and
// payment.disbursement.failed both use the shared PaymentInstruction
// object as their payload (status alone distinguishes them), per
// payment-execution-events.yaml.
type paymentInstructionPayload struct {
	InstructionID  string       `json:"instructionId"`
	LoanAccountID  string       `json:"loanAccountId"`
	Direction      string       `json:"direction"`
	Purpose        string       `json:"purpose"`
	Amount         moneyPayload `json:"amount"`
	PartyID        *string      `json:"partyId"`
	JournalEntryID *string      `json:"journalEntryId"`
	Status         string       `json:"status"`
	Rail           *string      `json:"rail"`
	FailureReason  *string      `json:"failureReason"`
}

type moneyPayload struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

func toPaymentInstructionPayload(p domain.PaymentInstruction) paymentInstructionPayload {
	var failureReason *string
	if p.FailureReason != nil {
		s := string(*p.FailureReason)
		failureReason = &s
	}
	return paymentInstructionPayload{
		InstructionID: p.InstructionID, LoanAccountID: p.LoanAccountID, Direction: string(p.Direction), Purpose: string(p.Purpose),
		Amount: moneyPayload{Amount: p.Amount.Amount, Currency: p.Amount.Currency}, PartyID: p.PartyID, JournalEntryID: p.JournalEntryID,
		Status: string(p.Status), Rail: p.Rail, FailureReason: failureReason,
	}
}

func publishEvent(ctx context.Context, tx store.Tx, id, topic string, payload any) error {
	e, err := outbox.NewEntry(id, topic, payload)
	if err != nil {
		return err
	}
	return tx.InsertOutboxEntry(ctx, e)
}
