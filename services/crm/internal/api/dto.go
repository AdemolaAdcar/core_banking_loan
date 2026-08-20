// Package api is the HTTP transport layer: chi handlers that translate
// between the wire shapes defined in specs/openapi/crm.yaml and
// internal/service's calls. Every DTO here mirrors a schema in
// specs/schemas/service-case.schema.json or an inline request schema in
// crm.yaml field-for-field. No DTO here, and no field on any DTO here,
// ever carries a Money value — CRM has no balance-affecting operation.
package api

import (
	"time"

	"github.com/AdemolaAdcar/core_banking_loan/services/crm/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/crm/internal/service"
)

// interactionResponse mirrors service-case.schema.json#/$defs/Interaction.
type interactionResponse struct {
	InteractionID string `json:"interactionId"`
	LoanAccountID string `json:"loanAccountId"`
	EventType     string `json:"eventType"`
	OccurredAt    string `json:"occurredAt"`
	Notes         string `json:"notes,omitempty"`
}

func toInteractionResponse(i domain.Interaction) interactionResponse {
	return interactionResponse{
		InteractionID: i.ID,
		LoanAccountID: i.LoanAccountID,
		EventType:     string(i.EventType),
		OccurredAt:    i.OccurredAt.Format(time.RFC3339),
		Notes:         i.Notes,
	}
}

type logInteractionRequest struct {
	LoanAccountID string  `json:"loanAccountId"`
	EventType     string  `json:"eventType"`
	OccurredAt    string  `json:"occurredAt"`
	Notes         *string `json:"notes"`
}

// serviceCaseResponse mirrors service-case.schema.json#/$defs/ServiceCase.
type serviceCaseResponse struct {
	CaseID        string  `json:"caseId"`
	LoanAccountID *string `json:"loanAccountId"`
	PartyID       string  `json:"partyId"`
	Status        string  `json:"status"`
	ReasonCode    string  `json:"reasonCode"`
	AssignedTo    *string `json:"assignedTo"`
	Version       int     `json:"version"`
	SLADueAt      string  `json:"slaDueAt"`
	Escalated     bool    `json:"escalated"`
	CloseReason   *string `json:"closeReason"`
	ReopenReason  *string `json:"reopenReason"`
	OpenedAt      string  `json:"openedAt"`
	UpdatedAt     string  `json:"updatedAt"`
}

func toServiceCaseResponse(c domain.ServiceCase) serviceCaseResponse {
	return serviceCaseResponse{
		CaseID:        c.ID,
		LoanAccountID: c.LoanAccountID,
		PartyID:       c.PartyID,
		Status:        string(c.Status),
		ReasonCode:    string(c.ReasonCode),
		AssignedTo:    c.AssignedTo,
		Version:       c.Version,
		SLADueAt:      c.SLADueAt.Format(time.RFC3339),
		Escalated:     c.Escalated,
		CloseReason:   c.CloseReason,
		ReopenReason:  c.ReopenReason,
		OpenedAt:      c.OpenedAt.Format(time.RFC3339),
		UpdatedAt:     c.UpdatedAt.Format(time.RFC3339),
	}
}

type openCaseRequest struct {
	PartyID       string  `json:"partyId"`
	LoanAccountID *string `json:"loanAccountId"`
	ReasonCode    string  `json:"reasonCode"`
	AssignedTo    *string `json:"assignedTo"`
}

type updateCaseRequest struct {
	ExpectedVersion int     `json:"expectedVersion"`
	Status          *string `json:"status"`
	AssignedTo      *string `json:"assignedTo"`
}

type closeCaseRequest struct {
	Reason string `json:"reason"`
}

type reopenCaseRequest struct {
	Reason string `json:"reason"`
}

// caseNoteResponse mirrors service-case.schema.json#/$defs/CaseNote.
type caseNoteResponse struct {
	NoteID    string `json:"noteId"`
	CaseID    string `json:"caseId"`
	AuthorID  string `json:"authorId"`
	Body      string `json:"body"`
	CreatedAt string `json:"createdAt"`
}

func toCaseNoteResponse(n domain.CaseNote) caseNoteResponse {
	return caseNoteResponse{
		NoteID:    n.ID,
		CaseID:    n.CaseID,
		AuthorID:  n.AuthorID,
		Body:      n.Body,
		CreatedAt: n.CreatedAt.Format(time.RFC3339),
	}
}

type addCaseNoteRequest struct {
	AuthorID string `json:"authorId"`
	Body     string `json:"body"`
}

// relationshipManagerAssignmentResponse mirrors
// service-case.schema.json#/$defs/RelationshipManagerAssignment.
type relationshipManagerAssignmentResponse struct {
	PartyID               string  `json:"partyId"`
	RelationshipManagerID *string `json:"relationshipManagerId"`
	AssignedAt            *string `json:"assignedAt"`
}

func toRelationshipManagerAssignmentResponse(a domain.RelationshipManagerAssignment) relationshipManagerAssignmentResponse {
	r := relationshipManagerAssignmentResponse{PartyID: a.PartyID, RelationshipManagerID: a.RelationshipManagerID}
	if a.AssignedAt != nil {
		s := a.AssignedAt.Format(time.RFC3339)
		r.AssignedAt = &s
	}
	return r
}

type assignRelationshipManagerRequest struct {
	RelationshipManagerID string `json:"relationshipManagerId"`
}

// communicationPreferencesResponse mirrors
// service-case.schema.json#/$defs/CommunicationPreferences.
type communicationPreferencesResponse struct {
	PartyID          string  `json:"partyId"`
	PreferredChannel *string `json:"preferredChannel"`
	EmailOptIn       bool    `json:"emailOptIn"`
	SMSOptIn         bool    `json:"smsOptIn"`
	PhoneOptIn       bool    `json:"phoneOptIn"`
	MailOptIn        bool    `json:"mailOptIn"`
	DoNotContact     bool    `json:"doNotContact"`
	UpdatedAt        string  `json:"updatedAt"`
}

func toCommunicationPreferencesResponse(p domain.CommunicationPreferences) communicationPreferencesResponse {
	r := communicationPreferencesResponse{
		PartyID: p.PartyID, EmailOptIn: p.EmailOptIn, SMSOptIn: p.SMSOptIn,
		PhoneOptIn: p.PhoneOptIn, MailOptIn: p.MailOptIn, DoNotContact: p.DoNotContact,
	}
	if p.PreferredChannel != nil {
		c := string(*p.PreferredChannel)
		r.PreferredChannel = &c
	}
	if !p.UpdatedAt.IsZero() {
		r.UpdatedAt = p.UpdatedAt.Format(time.RFC3339)
	}
	return r
}

type updateCommunicationPreferencesRequest struct {
	PreferredChannel *string `json:"preferredChannel"`
	EmailOptIn       bool    `json:"emailOptIn"`
	SMSOptIn         bool    `json:"smsOptIn"`
	PhoneOptIn       bool    `json:"phoneOptIn"`
	MailOptIn        bool    `json:"mailOptIn"`
	DoNotContact     bool    `json:"doNotContact"`
}

// customer360Response mirrors service-case.schema.json#/$defs/Customer360.
// displayName is intentionally omitted here -- see this service's PR
// description: CRM has no read access to Party/CIF's PII fields in this
// increment, a documented, flagged limitation, not an oversight.
type customer360Response struct {
	PartyID                      string                       `json:"partyId"`
	CurrentRelationshipManagerID *string                      `json:"currentRelationshipManagerId"`
	LoanAccountSummaries         []loanAccountSummaryResponse `json:"loanAccountSummaries"`
	RecentInteractions           []interactionResponse        `json:"recentInteractions"`
	OpenCases                    []serviceCaseResponse        `json:"openCases"`
}

type loanAccountSummaryResponse struct {
	LoanAccountID string `json:"loanAccountId"`
	Status        string `json:"status"`
}

func toCustomer360Response(c service.Customer360) customer360Response {
	summaries := make([]loanAccountSummaryResponse, 0, len(c.LoanAccountSummaries))
	for _, s := range c.LoanAccountSummaries {
		summaries = append(summaries, loanAccountSummaryResponse{LoanAccountID: s.LoanAccountID, Status: string(s.Status)})
	}
	interactions := make([]interactionResponse, 0, len(c.RecentInteractions))
	for _, i := range c.RecentInteractions {
		interactions = append(interactions, toInteractionResponse(i))
	}
	cases := make([]serviceCaseResponse, 0, len(c.OpenCases))
	for _, sc := range c.OpenCases {
		cases = append(cases, toServiceCaseResponse(sc))
	}
	return customer360Response{
		PartyID:                      c.PartyID,
		CurrentRelationshipManagerID: c.CurrentRelationshipManagerID,
		LoanAccountSummaries:         summaries,
		RecentInteractions:           interactions,
		OpenCases:                    cases,
	}
}

// errorResponse mirrors error.schema.json#/$defs/Error's minimal shape.
type errorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
