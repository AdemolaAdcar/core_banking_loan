// Package sandbox is the deterministic, no-real-rail-dependency
// railclient.Client this role's ground rules require: usable in this
// service's own tests, in CI, and by any other service in this system
// that needs a fake PaymentAPI dependency without standing up a real
// rail or even a real Payment Execution service instance.
//
// Deterministic by design: every outcome is either an explicit,
// caller-armed value (SetOutcome, QueueInbound) or a fixed default
// (Initiate/ReturnPayment succeed immediately; Confirm reports Executed
// on the very first call unless armed otherwise) — never a random delay,
// never a wall-clock-dependent state change. A test that doesn't touch
// the arming methods at all gets the same result every run.
package sandbox

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/railclient"
)

var _ railclient.Client = (*Sandbox)(nil)

type storedSubmission struct {
	input railclient.InitiateInput
	sub   railclient.Submission
}

type storedReturn struct {
	input railclient.ReturnPaymentInput
	sub   railclient.Submission
}

// Sandbox implements railclient.Client entirely in memory, guarded by a
// single mutex so it is safe under `go test -race` when driven by
// concurrent goroutines (mirroring internal/service's own fakeStore
// pattern in every other service in this repo).
type Sandbox struct {
	mu sync.Mutex

	idCounter int

	// Outbound disbursements, keyed by InstructionID (the idempotency
	// key) AND by the rail reference Initiate hands back — two indices
	// over the same underlying record so both Initiate's own dedup check
	// and Confirm's by-rail-reference lookup are O(1).
	byInstructionID map[string]*storedSubmission
	byRailRef       map[string]*storedSubmission

	// outcomes overrides what Confirm reports for a given InstructionID.
	// Not present at all = the deterministic default: Executed,
	// immediately. An explicit railclient.OutcomePending entry here
	// means Confirm keeps reporting Pending until the test calls
	// SetOutcome again with a terminal value — deliberately requires an
	// explicit re-arm rather than "expiring" on a timer, since this
	// package has no wall-clock-dependent behavior at all.
	outcomes map[string]railclient.Outcome

	inbound []railclient.InboundEvent

	// returns, keyed by IdempotencyKey, backs ReturnPayment's own
	// idempotent-replay guarantee — the same "survives retries" promise
	// Initiate makes, for the inbound-return direction.
	returns map[string]*storedReturn

	nextInitiateErr       error
	nextConfirmErr        error
	nextReceiveInboundErr error
	nextReturnErr         error
}

func New() *Sandbox {
	return &Sandbox{
		byInstructionID: map[string]*storedSubmission{},
		byRailRef:       map[string]*storedSubmission{},
		outcomes:        map[string]railclient.Outcome{},
		returns:         map[string]*storedReturn{},
	}
}

// SetOutcome arms what Confirm will report the NEXT time (and every
// time thereafter, until re-armed) it's asked about instructionID —
// call this BEFORE Initiate if the test wants something other than the
// default immediate-Executed behavior.
func (s *Sandbox) SetOutcome(instructionID string, outcome railclient.Outcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outcomes[instructionID] = outcome
}

// QueueInbound seeds an event ReceiveInbound will return once its
// `since` cursor is before event.OccurredAt.
func (s *Sandbox) QueueInbound(event railclient.InboundEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inbound = append(s.inbound, event)
}

// SetNextInitiateErr/SetNextConfirmErr/SetNextReceiveInboundErr/
// SetNextReturnErr each arm exactly ONE error to be returned on the
// next matching call, then reset to nil — mirroring
// glclient.Fake.NextErr's pattern used identically across this repo's
// other typed-client fakes.
func (s *Sandbox) SetNextInitiateErr(err error) { s.mu.Lock(); s.nextInitiateErr = err; s.mu.Unlock() }
func (s *Sandbox) SetNextConfirmErr(err error)  { s.mu.Lock(); s.nextConfirmErr = err; s.mu.Unlock() }
func (s *Sandbox) SetNextReceiveInboundErr(err error) {
	s.mu.Lock()
	s.nextReceiveInboundErr = err
	s.mu.Unlock()
}
func (s *Sandbox) SetNextReturnErr(err error) { s.mu.Lock(); s.nextReturnErr = err; s.mu.Unlock() }

func (s *Sandbox) Initiate(_ context.Context, in railclient.InitiateInput) (railclient.Submission, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.byInstructionID[in.InstructionID]; ok {
		if existing.input != in {
			return railclient.Submission{}, fmt.Errorf("%w: instruction %s", railclient.ErrDuplicateInstruction, in.InstructionID)
		}
		return existing.sub, nil // idempotent replay
	}

	if s.nextInitiateErr != nil {
		err := s.nextInitiateErr
		s.nextInitiateErr = nil
		return railclient.Submission{}, err
	}

	s.idCounter++
	sub := railclient.Submission{RailReference: fmt.Sprintf("sbx-%d", s.idCounter), SubmittedAt: time.Now().UTC()}
	rec := &storedSubmission{input: in, sub: sub}
	s.byInstructionID[in.InstructionID] = rec
	s.byRailRef[sub.RailReference] = rec

	if _, armed := s.outcomes[in.InstructionID]; !armed {
		s.outcomes[in.InstructionID] = railclient.Outcome{Status: railclient.OutcomeExecuted, ConfirmedAt: sub.SubmittedAt}
	}
	return sub, nil
}

func (s *Sandbox) Confirm(_ context.Context, railReference string) (railclient.Outcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.nextConfirmErr != nil {
		err := s.nextConfirmErr
		s.nextConfirmErr = nil
		return railclient.Outcome{}, err
	}

	rec, ok := s.byRailRef[railReference]
	if !ok {
		return railclient.Outcome{}, railclient.ErrNotFound
	}
	outcome, ok := s.outcomes[rec.input.InstructionID]
	if !ok {
		return railclient.Outcome{}, railclient.ErrNotFound
	}
	return outcome, nil
}

func (s *Sandbox) ReceiveInbound(_ context.Context, since time.Time) ([]railclient.InboundEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.nextReceiveInboundErr != nil {
		err := s.nextReceiveInboundErr
		s.nextReceiveInboundErr = nil
		return nil, err
	}

	var out []railclient.InboundEvent
	for _, e := range s.inbound {
		if e.OccurredAt.After(since) {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OccurredAt.Before(out[j].OccurredAt) })
	return out, nil
}

func (s *Sandbox) ReturnPayment(_ context.Context, in railclient.ReturnPaymentInput) (railclient.Submission, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.returns[in.IdempotencyKey]; ok {
		if existing.input != in {
			return railclient.Submission{}, fmt.Errorf("%w: return %s", railclient.ErrDuplicateInstruction, in.IdempotencyKey)
		}
		return existing.sub, nil // idempotent replay
	}

	if s.nextReturnErr != nil {
		err := s.nextReturnErr
		s.nextReturnErr = nil
		return railclient.Submission{}, err
	}

	s.idCounter++
	sub := railclient.Submission{RailReference: fmt.Sprintf("sbx-return-%d", s.idCounter), SubmittedAt: time.Now().UTC()}
	s.returns[in.IdempotencyKey] = &storedReturn{input: in, sub: sub}
	return sub, nil
}
