// Package events defines the domain event payloads this service publishes,
// matching specs/asyncapi/crm-events.yaml exactly. Every payload here
// deliberately excludes case-note/narrative content (CaseNote.Body,
// CloseCaseRequest/ReopenCaseRequest.reason) -- PII-adjacent free text
// never appears in an event payload, the same principle Party/CIF's
// events package applies to PII.
package events

import "time"

// Topic names match specs/asyncapi/crm-events.yaml's channel keys
// exactly — <context>.<entity>.<eventPastTense>, no version suffix.
const (
	TopicInteractionLogged               = "crm.interaction.logged"
	TopicCaseOpened                      = "crm.case.opened"
	TopicCaseUpdated                     = "crm.case.updated"
	TopicCaseClosed                      = "crm.case.closed"
	TopicCaseReopened                    = "crm.case.reopened"
	TopicCaseEscalated                   = "crm.case.escalated"
	TopicCaseNoteAdded                   = "crm.caseNote.added"
	TopicRelationshipManagerAssigned     = "crm.relationshipManager.assigned"
	TopicCommunicationPreferencesUpdated = "crm.communicationPreferences.updated"
)

type InteractionLoggedPayload struct {
	InteractionID string    `json:"interactionId"`
	LoanAccountID string    `json:"loanAccountId"`
	EventType     string    `json:"eventType"`
	OccurredAt    time.Time `json:"occurredAt"`
}

type CaseOpenedPayload struct {
	CaseID        string    `json:"caseId"`
	PartyID       string    `json:"partyId"`
	LoanAccountID *string   `json:"loanAccountId"`
	ReasonCode    string    `json:"reasonCode"`
	Status        string    `json:"status"`
	OpenedAt      time.Time `json:"openedAt"`
}

type CaseUpdatedPayload struct {
	CaseID        string    `json:"caseId"`
	ChangedFields []string  `json:"changedFields"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

type CaseClosedPayload struct {
	CaseID   string    `json:"caseId"`
	ClosedAt time.Time `json:"closedAt"`
}

type CaseReopenedPayload struct {
	CaseID     string    `json:"caseId"`
	ReopenedAt time.Time `json:"reopenedAt"`
	SLADueAt   time.Time `json:"slaDueAt"`
}

type CaseEscalatedPayload struct {
	CaseID      string    `json:"caseId"`
	PartyID     string    `json:"partyId"`
	ReasonCode  string    `json:"reasonCode"`
	SLADueAt    time.Time `json:"slaDueAt"`
	EscalatedAt time.Time `json:"escalatedAt"`
}

type CaseNoteAddedPayload struct {
	NoteID    string    `json:"noteId"`
	CaseID    string    `json:"caseId"`
	AuthorID  string    `json:"authorId"`
	CreatedAt time.Time `json:"createdAt"`
}

type RelationshipManagerAssignedPayload struct {
	PartyID                       string    `json:"partyId"`
	RelationshipManagerID         string    `json:"relationshipManagerId"`
	PreviousRelationshipManagerID *string   `json:"previousRelationshipManagerId"`
	AssignedAt                    time.Time `json:"assignedAt"`
}

type CommunicationPreferencesUpdatedPayload struct {
	PartyID          string    `json:"partyId"`
	PreferredChannel *string   `json:"preferredChannel"`
	EmailOptIn       bool      `json:"emailOptIn"`
	SMSOptIn         bool      `json:"smsOptIn"`
	PhoneOptIn       bool      `json:"phoneOptIn"`
	MailOptIn        bool      `json:"mailOptIn"`
	DoNotContact     bool      `json:"doNotContact"`
	UpdatedAt        time.Time `json:"updatedAt"`
}
