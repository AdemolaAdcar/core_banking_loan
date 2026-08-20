// Package service orchestrates the CRM business logic: interaction
// logging, service-case lifecycle (open/update/close/reopen), case
// notes, relationship-manager assignment, and communication preferences.
// It writes through internal/store's transactional Store interface so
// every business write and its outbox entry commit atomically. Nothing
// here ever calls, imports, or references GLPostingAPI or a Money
// value — CRM has no balance-affecting operation, ever.
package service

import (
	"context"
	"fmt"
	"time"

	"github.com/AdemolaAdcar/core_banking_loan/services/crm/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/crm/internal/events"
	"github.com/AdemolaAdcar/core_banking_loan/services/crm/internal/outbox"
	"github.com/AdemolaAdcar/core_banking_loan/services/crm/internal/store"
)

type Service struct {
	store store.Store
	now   func() time.Time // overridable in tests
	newID func() string    // overridable in tests
}

func New(s store.Store) *Service {
	return &Service{
		store: s,
		now:   func() time.Time { return time.Now().UTC() },
		newID: newUUIDv4,
	}
}

// NewWithClock builds a Service with an overridable clock. Exported
// specifically so tests in other packages (see
// internal/integration/integration_test.go) can fast-forward time to
// exercise SLA-escalation logic against a real store without an actual
// 24h+ wait — internal/service's own tests already do this by setting
// the unexported `now` field directly, which only works from inside this
// package. Production code should always use New, never this.
func NewWithClock(s store.Store, now func() time.Time) *Service {
	svc := New(s)
	svc.now = now
	return svc
}

// --- Interaction logging --------------------------------------------------

type LogInteractionInput struct {
	LoanAccountID string
	EventType     domain.EventType
	OccurredAt    time.Time
	Notes         string
}

func (s *Service) LogInteraction(ctx context.Context, in LogInteractionInput) (domain.Interaction, error) {
	var out domain.Interaction
	err := s.store.WithinTx(ctx, func(tx store.Tx) error {
		i := domain.Interaction{
			ID:            s.newID(),
			LoanAccountID: in.LoanAccountID,
			EventType:     in.EventType,
			OccurredAt:    in.OccurredAt,
			Notes:         in.Notes,
			CreatedAt:     s.now(),
		}
		if err := tx.CreateInteraction(ctx, i); err != nil {
			return fmt.Errorf("creating interaction: %w", err)
		}
		entry, err := outbox.NewEntry(s.newID(), events.TopicInteractionLogged, events.InteractionLoggedPayload{
			InteractionID: i.ID, LoanAccountID: i.LoanAccountID, EventType: string(i.EventType), OccurredAt: i.OccurredAt,
		})
		if err != nil {
			return fmt.Errorf("building crm.interaction.logged outbox entry: %w", err)
		}
		if err := tx.InsertOutboxEntry(ctx, entry); err != nil {
			return fmt.Errorf("inserting crm.interaction.logged outbox entry: %w", err)
		}
		out = i
		return nil
	})
	return out, err
}

// --- Case lifecycle ---------------------------------------------------

// OpenCaseInput.CaseID mirrors the spec's convention for this operation:
// the Idempotency-Key header value IS the resulting case's ID, supplied
// by the caller rather than generated here.
type OpenCaseInput struct {
	CaseID        string
	PartyID       string
	LoanAccountID *string
	ReasonCode    domain.ReasonCode
	AssignedTo    *string
}

func (s *Service) OpenCase(ctx context.Context, in OpenCaseInput) (domain.ServiceCase, error) {
	var out domain.ServiceCase
	err := s.store.WithinTx(ctx, func(tx store.Tx) error {
		c := domain.NewCase(in.CaseID, in.PartyID, in.LoanAccountID, in.AssignedTo, in.ReasonCode, s.now())

		if in.LoanAccountID != nil {
			if err := tx.LinkLoanAccountToParty(ctx, *in.LoanAccountID, in.PartyID); err != nil {
				return fmt.Errorf("linking loan account to party: %w", err)
			}
		}
		if err := tx.CreateCase(ctx, c); err != nil {
			return fmt.Errorf("creating case: %w", err)
		}

		entry, err := outbox.NewEntry(s.newID(), events.TopicCaseOpened, events.CaseOpenedPayload{
			CaseID: c.ID, PartyID: c.PartyID, LoanAccountID: c.LoanAccountID,
			ReasonCode: string(c.ReasonCode), Status: string(c.Status), OpenedAt: c.OpenedAt,
		})
		if err != nil {
			return fmt.Errorf("building crm.case.opened outbox entry: %w", err)
		}
		if err := tx.InsertOutboxEntry(ctx, entry); err != nil {
			return fmt.Errorf("inserting crm.case.opened outbox entry: %w", err)
		}
		out = c
		return nil
	})
	return out, err
}

type UpdateCaseInput struct {
	CaseID          string
	ExpectedVersion int
	NewStatus       *domain.CaseStatus
	NewAssignedTo   *string
}

// UpdateCase is optimistic-concurrency-checked twice: once in memory
// (domain.ServiceCase.Update, against the version this call loaded) and
// once again by the database write itself (store.Tx.UpdateCaseConditional,
// against whatever version is ACTUALLY stored at write time) -- the
// database check is what actually closes the race two concurrent
// UpdateCase calls can hit between this method's read and its write; the
// in-memory check alone cannot.
func (s *Service) UpdateCase(ctx context.Context, in UpdateCaseInput) (domain.ServiceCase, error) {
	current, err := s.store.GetCase(ctx, in.CaseID)
	if err != nil {
		return domain.ServiceCase{}, fmt.Errorf("loading case %s: %w", in.CaseID, err)
	}

	updated, changedFields, err := current.Update(in.ExpectedVersion, in.NewStatus, in.NewAssignedTo, s.now())
	if err != nil {
		return domain.ServiceCase{}, err
	}
	if len(changedFields) == 0 {
		return current, nil
	}

	var out domain.ServiceCase
	err = s.store.WithinTx(ctx, func(tx store.Tx) error {
		if err := tx.UpdateCaseConditional(ctx, updated, current.Version); err != nil {
			return err
		}
		entry, err := outbox.NewEntry(s.newID(), events.TopicCaseUpdated, events.CaseUpdatedPayload{
			CaseID: updated.ID, ChangedFields: changedFields, UpdatedAt: updated.UpdatedAt,
		})
		if err != nil {
			return fmt.Errorf("building crm.case.updated outbox entry: %w", err)
		}
		if err := tx.InsertOutboxEntry(ctx, entry); err != nil {
			return fmt.Errorf("inserting crm.case.updated outbox entry: %w", err)
		}
		out = updated
		return nil
	})
	return out, err
}

type CloseCaseInput struct {
	CaseID string
	Reason string
}

// CloseCase is idempotent: closing an already-closed case is a no-op
// success (returns the case unchanged, publishes nothing) rather than an
// error -- a retried request must not fail just because it already
// succeeded once.
func (s *Service) CloseCase(ctx context.Context, in CloseCaseInput) (domain.ServiceCase, error) {
	current, err := s.store.GetCase(ctx, in.CaseID)
	if err != nil {
		return domain.ServiceCase{}, fmt.Errorf("loading case %s: %w", in.CaseID, err)
	}

	updated, changed, err := current.Close(in.Reason, s.now())
	if err != nil {
		return domain.ServiceCase{}, err
	}
	if !changed {
		return updated, nil
	}

	var out domain.ServiceCase
	err = s.store.WithinTx(ctx, func(tx store.Tx) error {
		if err := tx.UpdateCaseConditional(ctx, updated, current.Version); err != nil {
			return err
		}
		entry, err := outbox.NewEntry(s.newID(), events.TopicCaseClosed, events.CaseClosedPayload{
			CaseID: updated.ID, ClosedAt: updated.UpdatedAt,
		})
		if err != nil {
			return fmt.Errorf("building crm.case.closed outbox entry: %w", err)
		}
		if err := tx.InsertOutboxEntry(ctx, entry); err != nil {
			return fmt.Errorf("inserting crm.case.closed outbox entry: %w", err)
		}
		out = updated
		return nil
	})
	return out, err
}

type ReopenCaseInput struct {
	CaseID string
	Reason string
}

// ReopenCase is deliberately NOT idempotent-as-no-op on a non-Closed
// case (unlike CloseCase) -- see domain.ServiceCase.Reopen's doc comment.
func (s *Service) ReopenCase(ctx context.Context, in ReopenCaseInput) (domain.ServiceCase, error) {
	current, err := s.store.GetCase(ctx, in.CaseID)
	if err != nil {
		return domain.ServiceCase{}, fmt.Errorf("loading case %s: %w", in.CaseID, err)
	}

	updated, err := current.Reopen(in.Reason, s.now())
	if err != nil {
		return domain.ServiceCase{}, err
	}

	var out domain.ServiceCase
	err = s.store.WithinTx(ctx, func(tx store.Tx) error {
		if err := tx.UpdateCaseConditional(ctx, updated, current.Version); err != nil {
			return err
		}
		entry, err := outbox.NewEntry(s.newID(), events.TopicCaseReopened, events.CaseReopenedPayload{
			CaseID: updated.ID, ReopenedAt: updated.UpdatedAt, SLADueAt: updated.SLADueAt,
		})
		if err != nil {
			return fmt.Errorf("building crm.case.reopened outbox entry: %w", err)
		}
		if err := tx.InsertOutboxEntry(ctx, entry); err != nil {
			return fmt.Errorf("inserting crm.case.reopened outbox entry: %w", err)
		}
		out = updated
		return nil
	})
	return out, err
}

// --- Case notes ------------------------------------------------------

type AddCaseNoteInput struct {
	CaseID   string
	AuthorID string
	Body     string
}

func (s *Service) AddCaseNote(ctx context.Context, in AddCaseNoteInput) (domain.CaseNote, error) {
	var out domain.CaseNote
	err := s.store.WithinTx(ctx, func(tx store.Tx) error {
		n := domain.CaseNote{ID: s.newID(), CaseID: in.CaseID, AuthorID: in.AuthorID, Body: in.Body, CreatedAt: s.now()}
		if err := tx.AddCaseNote(ctx, n); err != nil {
			return fmt.Errorf("adding case note: %w", err)
		}
		entry, err := outbox.NewEntry(s.newID(), events.TopicCaseNoteAdded, events.CaseNoteAddedPayload{
			NoteID: n.ID, CaseID: n.CaseID, AuthorID: n.AuthorID, CreatedAt: n.CreatedAt,
		})
		if err != nil {
			return fmt.Errorf("building crm.caseNote.added outbox entry: %w", err)
		}
		if err := tx.InsertOutboxEntry(ctx, entry); err != nil {
			return fmt.Errorf("inserting crm.caseNote.added outbox entry: %w", err)
		}
		out = n
		return nil
	})
	return out, err
}

// ListCaseNotes returns every note on a case, and access-logs the read
// (actor + timestamp) -- CaseNote.Body is PII-adjacent content, per this
// service's ground rules.
func (s *Service) ListCaseNotes(ctx context.Context, actorSubject, caseID string) ([]domain.CaseNote, error) {
	notes, err := s.store.ListCaseNotes(ctx, caseID)
	if err != nil {
		return nil, fmt.Errorf("listing case notes: %w", err)
	}
	if err := s.store.WithinTx(ctx, func(tx store.Tx) error {
		return tx.RecordAccess(ctx, actorSubject, "CaseNote", caseID, s.now())
	}); err != nil {
		return nil, fmt.Errorf("recording case note access: %w", err)
	}
	return notes, nil
}

// --- Relationship manager ----------------------------------------------

type AssignRelationshipManagerInput struct {
	PartyID               string
	RelationshipManagerID string
}

func (s *Service) AssignRelationshipManager(ctx context.Context, in AssignRelationshipManagerInput) (domain.RelationshipManagerAssignment, error) {
	prior, err := s.store.GetRelationshipManagerAssignment(ctx, in.PartyID)
	if err != nil {
		return domain.RelationshipManagerAssignment{}, fmt.Errorf("loading current rm assignment: %w", err)
	}
	if prior.RelationshipManagerID != nil && *prior.RelationshipManagerID == in.RelationshipManagerID {
		// Reassigning the same RM already on file is a no-op -- no new
		// history row, no event.
		return prior, nil
	}

	now := s.now()
	assignment := domain.RelationshipManagerAssignment{PartyID: in.PartyID, RelationshipManagerID: &in.RelationshipManagerID, AssignedAt: &now}

	var out domain.RelationshipManagerAssignment
	err = s.store.WithinTx(ctx, func(tx store.Tx) error {
		if err := tx.AssignRelationshipManager(ctx, assignment); err != nil {
			return fmt.Errorf("assigning relationship manager: %w", err)
		}
		entry, err := outbox.NewEntry(s.newID(), events.TopicRelationshipManagerAssigned, events.RelationshipManagerAssignedPayload{
			PartyID: in.PartyID, RelationshipManagerID: in.RelationshipManagerID,
			PreviousRelationshipManagerID: prior.RelationshipManagerID, AssignedAt: now,
		})
		if err != nil {
			return fmt.Errorf("building crm.relationshipManager.assigned outbox entry: %w", err)
		}
		if err := tx.InsertOutboxEntry(ctx, entry); err != nil {
			return fmt.Errorf("inserting crm.relationshipManager.assigned outbox entry: %w", err)
		}
		out = assignment
		return nil
	})
	return out, err
}

func (s *Service) GetRelationshipManager(ctx context.Context, partyID string) (domain.RelationshipManagerAssignment, error) {
	return s.store.GetRelationshipManagerAssignment(ctx, partyID)
}

// --- Communication preferences -----------------------------------------

func (s *Service) GetCommunicationPreferences(ctx context.Context, partyID string) (domain.CommunicationPreferences, error) {
	prefs, found, err := s.store.GetCommunicationPreferences(ctx, partyID)
	if err != nil {
		return domain.CommunicationPreferences{}, fmt.Errorf("loading communication preferences: %w", err)
	}
	if !found {
		return domain.DefaultCommunicationPreferences(partyID), nil
	}
	return prefs, nil
}

type UpdateCommunicationPreferencesInput struct {
	PartyID          string
	PreferredChannel *domain.PreferredChannel
	EmailOptIn       bool
	SMSOptIn         bool
	PhoneOptIn       bool
	MailOptIn        bool
	DoNotContact     bool
}

func (s *Service) UpdateCommunicationPreferences(ctx context.Context, in UpdateCommunicationPreferencesInput) (domain.CommunicationPreferences, error) {
	prefs := domain.CommunicationPreferences{
		PartyID: in.PartyID, PreferredChannel: in.PreferredChannel,
		EmailOptIn: in.EmailOptIn, SMSOptIn: in.SMSOptIn, PhoneOptIn: in.PhoneOptIn,
		MailOptIn: in.MailOptIn, DoNotContact: in.DoNotContact, UpdatedAt: s.now(),
	}

	var out domain.CommunicationPreferences
	err := s.store.WithinTx(ctx, func(tx store.Tx) error {
		if err := tx.UpsertCommunicationPreferences(ctx, prefs); err != nil {
			return fmt.Errorf("upserting communication preferences: %w", err)
		}
		var channel *string
		if prefs.PreferredChannel != nil {
			c := string(*prefs.PreferredChannel)
			channel = &c
		}
		entry, err := outbox.NewEntry(s.newID(), events.TopicCommunicationPreferencesUpdated, events.CommunicationPreferencesUpdatedPayload{
			PartyID: prefs.PartyID, PreferredChannel: channel, EmailOptIn: prefs.EmailOptIn, SMSOptIn: prefs.SMSOptIn,
			PhoneOptIn: prefs.PhoneOptIn, MailOptIn: prefs.MailOptIn, DoNotContact: prefs.DoNotContact, UpdatedAt: prefs.UpdatedAt,
		})
		if err != nil {
			return fmt.Errorf("building crm.communicationPreferences.updated outbox entry: %w", err)
		}
		if err := tx.InsertOutboxEntry(ctx, entry); err != nil {
			return fmt.Errorf("inserting crm.communicationPreferences.updated outbox entry: %w", err)
		}
		out = prefs
		return nil
	})
	return out, err
}

// --- Customer 360 -------------------------------------------------------

type Customer360 struct {
	PartyID                      string
	LoanAccountSummaries         []LoanAccountSummary
	RecentInteractions           []domain.Interaction
	OpenCases                    []domain.ServiceCase
	CurrentRelationshipManagerID *string
}

type LoanAccountSummary struct {
	LoanAccountID string
	Status        domain.LoanAccountStatus
}

// GetCustomer360 is built entirely from CRM's own data (interactions
// already logged, joined to a party via loan_account_links -- see
// store.Tx.LinkLoanAccountToParty's doc comment for the documented
// limitation this implies) rather than a live cross-service call to
// AccountAPI. Access-logs the read (actor + timestamp), since
// recentInteractions[].notes is PII-adjacent content.
func (s *Service) GetCustomer360(ctx context.Context, actorSubject, partyID string) (Customer360, error) {
	loanAccountIDs, err := s.store.ListLoanAccountIDsForParty(ctx, partyID)
	if err != nil {
		return Customer360{}, fmt.Errorf("listing loan account links: %w", err)
	}

	latest, err := s.store.LatestInteractionPerLoanAccount(ctx, loanAccountIDs)
	if err != nil {
		return Customer360{}, fmt.Errorf("loading latest interactions: %w", err)
	}
	summaries := make([]LoanAccountSummary, 0, len(loanAccountIDs))
	for _, id := range loanAccountIDs {
		i, ok := latest[id]
		if !ok {
			continue
		}
		summaries = append(summaries, LoanAccountSummary{LoanAccountID: id, Status: domain.InferLoanAccountStatus(i.EventType)})
	}

	recent, err := s.store.ListRecentInteractionsForLoanAccounts(ctx, loanAccountIDs, 20)
	if err != nil {
		return Customer360{}, fmt.Errorf("loading recent interactions: %w", err)
	}

	openCases, err := s.store.ListOpenCasesForParty(ctx, partyID)
	if err != nil {
		return Customer360{}, fmt.Errorf("loading open cases: %w", err)
	}

	rm, err := s.store.GetRelationshipManagerAssignment(ctx, partyID)
	if err != nil {
		return Customer360{}, fmt.Errorf("loading relationship manager assignment: %w", err)
	}

	if err := s.store.WithinTx(ctx, func(tx store.Tx) error {
		return tx.RecordAccess(ctx, actorSubject, "Customer360", partyID, s.now())
	}); err != nil {
		return Customer360{}, fmt.Errorf("recording customer360 access: %w", err)
	}

	return Customer360{
		PartyID:                      partyID,
		LoanAccountSummaries:         summaries,
		RecentInteractions:           recent,
		OpenCases:                    openCases,
		CurrentRelationshipManagerID: rm.RelationshipManagerID,
	}, nil
}

// --- SLA sweep -----------------------------------------------------------

// EvaluateSLABreaches is the periodic sweep a background ticker (see
// cmd/crm-service/main.go, mirroring services/party's outbox relay
// pattern) calls to find and escalate cases that have breached their
// SLA. Never called synchronously from a request handler -- GETs stay
// side-effect-free.
func (s *Service) EvaluateSLABreaches(ctx context.Context) (escalated int, err error) {
	now := s.now()
	cases, err := s.store.ListCasesPastSLA(ctx, now, 100)
	if err != nil {
		return 0, fmt.Errorf("listing cases past sla: %w", err)
	}

	for _, c := range cases {
		if !c.IsPastSLA(now) {
			continue // defense in depth: re-check the domain predicate, not just trust the SQL filter
		}
		c.Escalated = true
		c.Version++
		c.UpdatedAt = now
		prior := c.Version - 1

		writeErr := s.store.WithinTx(ctx, func(tx store.Tx) error {
			if err := tx.UpdateCaseConditional(ctx, c, prior); err != nil {
				return err
			}
			entry, err := outbox.NewEntry(s.newID(), events.TopicCaseEscalated, events.CaseEscalatedPayload{
				CaseID: c.ID, PartyID: c.PartyID, ReasonCode: string(c.ReasonCode), SLADueAt: c.SLADueAt, EscalatedAt: now,
			})
			if err != nil {
				return fmt.Errorf("building crm.case.escalated outbox entry: %w", err)
			}
			return tx.InsertOutboxEntry(ctx, entry)
		})
		if writeErr != nil {
			// A single case losing a race (someone updated it between our
			// read and our write) must not abort the whole sweep -- the
			// next sweep interval picks it up again if it's still overdue.
			continue
		}
		escalated++
	}
	return escalated, nil
}
