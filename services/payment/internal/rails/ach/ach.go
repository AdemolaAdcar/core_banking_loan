// Package ach is the concrete railclient.Client adapter for the ACH
// network — the rail selected for this pass because it is the only one
// the design note names explicitly ("cut over per payment rail, ACH
// first"). No vendor/ODFI-specific API documentation exists anywhere in
// this repo; this adapter is built against public, standard NACHA
// batch-file conventions instead (see nacha.go), which is what every
// real ACH origination integration ultimately has to speak regardless
// of which bank or payment processor sits in front of it.
//
// The defining characteristic this adapter exists to demonstrate: ACH
// has NO real-time confirmation of anything. Initiate only appends an
// entry to the currently-open batch — nothing is even "sent" until
// CutBatch runs (an operations/scheduler decision, not something this
// package does on its own timer), and even a cut batch's entries stay
// Pending under Confirm until a SEPARATE, LATER call to
// ApplySettlementFile supplies the outcome a real settlement/return
// report would carry, typically one to two banking days after the file
// was transmitted. See PR_DESCRIPTION.md's "Rail limitations" section
// for the full list this package's own shape surfaces.
package ach

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/railclient"
)

// Config carries this originator's own fixed ACH identity — the
// File/Batch Header fields that don't vary per entry.
type Config struct {
	OriginRoutingNumber      string // 9 digits, our bank's ABA routing number
	OriginName               string
	CompanyID                string
	CompanyName              string
	DestinationRoutingNumber string // the ACH operator/receiving institution's routing number
	DestinationName          string
	// ReturnWindow bounds how long after an inbound credit was received
	// this adapter will still originate a return for it via
	// ReturnPayment — see that method's doc comment for why this exists
	// at all and isn't just "always allowed". Defaults to 48h (a
	// simplification of NACHA's actual "2 BANKING days" rule, which
	// excludes weekends/holidays — this adapter counts wall-clock hours
	// instead; see PR_DESCRIPTION.md).
	ReturnWindow time.Duration
}

// ErrReturnWindowExpired: ReturnPayment was asked to return an inbound
// credit outside this adapter's ReturnWindow. A REAL ACH return can only
// be originated by the receiving bank (the RDFI) within a short window
// after settlement (2 banking days for most reason codes); after that,
// the only remaining path is an entirely new, uninsured outbound credit
// requiring the original sender's independently-obtained banking
// details — which this adapter does not have and does not attempt to
// fabricate. See PR_DESCRIPTION.md's rail-limitations list.
var ErrReturnWindowExpired = errors.New("ach: inbound credit is outside the return window; a same-mechanism return is no longer possible")

var _ railclient.Client = (*Adapter)(nil)

type outboundRecord struct {
	input       railclient.InitiateInput
	sub         railclient.Submission
	traceNumber string
}

type entry struct {
	instructionID string
	traceNumber   string
	payout        PayoutAccount
	amount        domain.Money
}

type cutBatch struct {
	batchID  string
	entries  []entry
	fileText string
	cutAt    time.Time
}

// SettlementResult is one line of a settlement/return report — this
// adapter accepts it as already-structured Go data rather than parsing
// a raw incoming NACHA file; see this package's doc comment for why
// full inbound-file PARSING is out of scope for this pass.
type SettlementResult struct {
	TraceNumber string
	Outcome     railclient.OutcomeStatus // Executed, Failed, or Returned — never Pending
	ReasonCode  string                   // e.g. an ACH return code like "R01"; "" for a plain Executed
}

// IncomingCredit is one already-parsed entry from an incoming ACH
// batch/settlement report — money that arrived, to be surfaced via
// ReceiveInbound.
type IncomingCredit struct {
	RailReference  string // the sender's own trace number for this entry
	LoanAccountRef *string
	Amount         domain.Money
	ReceivedAt     time.Time
}

// IncomingReturnNotice reports that a previously-ingested IncomingCredit
// (identified by its RailReference) was itself returned (e.g. NSF, days
// after the fact) — this is a return the RAIL originated on money we
// already received, distinct from ReturnPayment below (which THIS
// service originates).
type IncomingReturnNotice struct {
	OriginalRailReference string
	ReasonCode            string
	OccurredAt            time.Time
}

// Adapter is the concrete railclient.Client. All state is in-memory —
// there is no real network or file-transmission dependency inside this
// package; CutBatch/ApplySettlementFile/IngestIncomingBatch/
// IngestReturnReport are the seams a real deployment would wire to an
// actual SFTP/bank-API delivery mechanism and an actual incoming-report
// ingestion job, entirely outside this repo's current scope.
type Adapter struct {
	mu sync.Mutex

	cfg     Config
	payouts PayoutAccountResolver

	idCounter   int
	batchNumber int

	byInstructionID map[string]*outboundRecord
	byTraceNumber   map[string]*outboundRecord
	currentBatch    []entry
	cutBatches      map[string]*cutBatch
	outcomes        map[string]railclient.Outcome // instructionID -> outcome; Pending until settlement applied

	inboundEvents     []railclient.InboundEvent
	receivedByRailRef map[string]railclient.InboundEvent

	returns map[string]railclient.Submission // IdempotencyKey -> submission, for idempotent replay
}

func New(cfg Config, payouts PayoutAccountResolver) *Adapter {
	if cfg.ReturnWindow == 0 {
		cfg.ReturnWindow = 48 * time.Hour
	}
	return &Adapter{
		cfg: cfg, payouts: payouts,
		byInstructionID: map[string]*outboundRecord{}, byTraceNumber: map[string]*outboundRecord{},
		cutBatches: map[string]*cutBatch{}, outcomes: map[string]railclient.Outcome{},
		receivedByRailRef: map[string]railclient.InboundEvent{}, returns: map[string]railclient.Submission{},
	}
}

func (a *Adapter) Initiate(_ context.Context, in railclient.InitiateInput) (railclient.Submission, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if existing, ok := a.byInstructionID[in.InstructionID]; ok {
		if existing.input != in {
			return railclient.Submission{}, fmt.Errorf("%w: instruction %s", railclient.ErrDuplicateInstruction, in.InstructionID)
		}
		return existing.sub, nil // idempotent replay -- no second entry appended to any batch
	}

	// A Resolve failure fails Initiate synchronously (RAIL_REJECTED) --
	// no Outcome record is ever created for this instruction, since it
	// never made it into a batch at all. Contrast with a rail-side
	// rejection reported LATER via ApplySettlementFile, which DOES
	// carry a domain.FailureReason through Confirm's Outcome.
	payout, found := a.payouts.Resolve(in.PartyID)
	if !found {
		return railclient.Submission{}, fmt.Errorf("%w: no ACH payout account on file for party %s", railclient.ErrRailRejected, in.PartyID)
	}

	a.idCounter++
	traceNumber := fmt.Sprintf("%s%07d", padLeftZero(a.cfg.OriginRoutingNumber, 8), a.idCounter)
	sub := railclient.Submission{RailReference: traceNumber, SubmittedAt: time.Now().UTC()}
	rec := &outboundRecord{input: in, sub: sub, traceNumber: traceNumber}
	a.byInstructionID[in.InstructionID] = rec
	a.byTraceNumber[traceNumber] = rec
	a.currentBatch = append(a.currentBatch, entry{instructionID: in.InstructionID, traceNumber: traceNumber, payout: payout, amount: in.Amount})
	a.outcomes[in.InstructionID] = railclient.Outcome{Status: railclient.OutcomePending}
	return sub, nil
}

// CutBatch finalizes every entry Initiate has accumulated since the last
// cut into a single NACHA file and clears the open batch — an
// operations/scheduler decision (see cmd/payment-service), never
// something Initiate triggers on its own. Entries remain Pending under
// Confirm even after being cut; only ApplySettlementFile changes that.
func (a *Adapter) CutBatch(_ context.Context, effectiveDate time.Time) (batchID string, nachaFile []byte, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if len(a.currentBatch) == 0 {
		return "", nil, fmt.Errorf("ach: nothing to cut, the current batch is empty")
	}
	a.batchNumber++
	batchID = fmt.Sprintf("batch-%d", a.batchNumber)
	fileText := buildFile(a.cfg, a.batchNumber, a.currentBatch, effectiveDate, time.Now().UTC())
	a.cutBatches[batchID] = &cutBatch{batchID: batchID, entries: a.currentBatch, fileText: fileText, cutAt: time.Now().UTC()}
	a.currentBatch = nil
	return batchID, []byte(fileText), nil
}

// ApplySettlementFile ingests an already-parsed settlement/return
// report — see SettlementResult's doc comment for why parsing a raw
// incoming file isn't this method's job. Returns the count actually
// applied and the trace numbers this adapter has no record of at all
// (a defensive, adapter-level check — the SERVICE layer's own
// reconciliation against PaymentInstruction records, including its
// unmatched-confirmation exception logging, is a separate and higher-
// level concern; see internal/service).
func (a *Adapter) ApplySettlementFile(_ context.Context, results []SettlementResult) (applied int, unmatched []string, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	for _, r := range results {
		rec, ok := a.byTraceNumber[r.TraceNumber]
		if !ok {
			unmatched = append(unmatched, r.TraceNumber)
			continue
		}
		outcome := railclient.Outcome{Status: r.Outcome, ConfirmedAt: time.Now().UTC()}
		if r.Outcome == railclient.OutcomeFailed || r.Outcome == railclient.OutcomeReturned {
			reason := reasonFromCode(r.ReasonCode, r.Outcome)
			outcome.FailureReason = &reason
		}
		a.outcomes[rec.input.InstructionID] = outcome
		applied++
	}
	return applied, unmatched, nil
}

// reasonFromCode maps an ACH return/reject reason code onto this
// system's own, much coarser domain.FailureReason vocabulary — a real
// integration would keep the raw R-code for audit; this adapter keeps
// only the coarse mapping, since domain.FailureReason is a shared,
// rail-agnostic enum every rail adapter must map onto (see
// payment-instruction.schema.json).
func reasonFromCode(code string, outcome railclient.OutcomeStatus) domain.FailureReason {
	if outcome == railclient.OutcomeReturned {
		return domain.ReasonPaymentReturned
	}
	if code == "" {
		return domain.ReasonPaymentFailed
	}
	return domain.ReasonRailRejected
}

func (a *Adapter) Confirm(_ context.Context, railReference string) (railclient.Outcome, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	rec, ok := a.byTraceNumber[railReference]
	if !ok {
		return railclient.Outcome{}, railclient.ErrNotFound
	}
	return a.outcomes[rec.input.InstructionID], nil
}

// IngestIncomingBatch simulates processing an incoming ACH
// batch/settlement report of credits received on this account —
// see IncomingCredit's doc comment for why this takes already-parsed
// data. Each credit becomes an InboundEvent ReceiveInbound will later
// surface.
func (a *Adapter) IngestIncomingBatch(_ context.Context, credits []IncomingCredit) int {
	a.mu.Lock()
	defer a.mu.Unlock()

	for _, c := range credits {
		event := railclient.InboundEvent{
			Kind: railclient.InboundReceived, RailReference: c.RailReference, LoanAccountRef: c.LoanAccountRef,
			Amount: c.Amount, Rail: "ACH", OccurredAt: c.ReceivedAt,
		}
		a.inboundEvents = append(a.inboundEvents, event)
		a.receivedByRailRef[c.RailReference] = event
	}
	return len(credits)
}

// IngestReturnReport simulates processing an ACH return file entry for
// a PREVIOUSLY-ingested IncomingCredit — the rail-originated half of
// "returned"; ReturnPayment below is the service-originated half.
// Notices whose OriginalRailReference was never ingested via
// IngestIncomingBatch are reported back as unmatched (same defensive,
// adapter-level check ApplySettlementFile makes).
func (a *Adapter) IngestReturnReport(_ context.Context, notices []IncomingReturnNotice) (applied int, unmatched []string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	for i, n := range notices {
		original, ok := a.receivedByRailRef[n.OriginalRailReference]
		if !ok {
			unmatched = append(unmatched, n.OriginalRailReference)
			continue
		}
		reason := domain.ReasonPaymentReturned
		a.inboundEvents = append(a.inboundEvents, railclient.InboundEvent{
			Kind: railclient.InboundReturned, RailReference: fmt.Sprintf("%s-ret-%d", n.OriginalRailReference, i),
			OriginalRailReference: n.OriginalRailReference, LoanAccountRef: original.LoanAccountRef,
			Amount: original.Amount, Rail: "ACH", FailureReason: &reason, OccurredAt: n.OccurredAt,
		})
		applied++
	}
	return applied, unmatched
}

func (a *Adapter) ReceiveInbound(_ context.Context, since time.Time) ([]railclient.InboundEvent, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	var out []railclient.InboundEvent
	for _, e := range a.inboundEvents {
		if e.OccurredAt.After(since) {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OccurredAt.Before(out[j].OccurredAt) })
	return out, nil
}

// ReturnPayment originates a return of a specific, previously-received
// inbound credit — but ONLY within cfg.ReturnWindow of when it was
// received (ErrReturnWindowExpired otherwise). This is a real ACH
// constraint, not an arbitrary one this adapter invented: see
// ErrReturnWindowExpired's doc comment. No separate NACHA return-batch
// file is generated for this pass (deferred, see PR_DESCRIPTION.md) —
// this method only records the return and hands back a Submission,
// idempotent on in.IdempotencyKey.
func (a *Adapter) ReturnPayment(_ context.Context, in railclient.ReturnPaymentInput) (railclient.Submission, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if existing, ok := a.returns[in.IdempotencyKey]; ok {
		return existing, nil // idempotent replay
	}

	original, ok := a.receivedByRailRef[in.OriginalRailReference]
	if !ok {
		return railclient.Submission{}, railclient.ErrNotFound
	}
	if time.Since(original.OccurredAt) > a.cfg.ReturnWindow {
		return railclient.Submission{}, ErrReturnWindowExpired
	}

	a.idCounter++
	sub := railclient.Submission{RailReference: fmt.Sprintf("ach-return-%d", a.idCounter), SubmittedAt: time.Now().UTC()}
	a.returns[in.IdempotencyKey] = sub
	return sub, nil
}
