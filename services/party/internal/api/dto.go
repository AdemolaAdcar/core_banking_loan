// Package api is the HTTP transport layer: chi handlers that translate
// between the wire shapes defined in specs/openapi/party-cif.yaml and
// internal/service's plaintext domain calls. Every DTO here mirrors a
// schema in specs/schemas/party.schema.json or an inline request schema
// in party-cif.yaml field-for-field — this package owns no business
// logic beyond that translation and HTTP-level concerns (status codes,
// Idempotency-Key handling).
package api

import (
	"time"

	"github.com/AdemolaAdcar/core_banking_loan/services/party/internal/domain"
)

const dateLayout = "2006-01-02" // OpenAPI format: date

// partyResponse mirrors party.schema.json#/$defs/Party. displayName is
// deliberately omitted — the spec documents it as present only on
// operations that explicitly need it for human display (CRM), never on
// this service's own responses.
type partyResponse struct {
	PartyID         string  `json:"partyId"`
	Status          string  `json:"status"`
	KYCStatus       string  `json:"kycStatus"`
	FirstName       string  `json:"firstName"`
	LastName        string  `json:"lastName"`
	DateOfBirth     string  `json:"dateOfBirth"`
	Email           *string `json:"email"`
	Phone           *string `json:"phone"`
	SSNLast4        string  `json:"ssnLast4"`
	Tombstoned      bool    `json:"tombstoned"`
	TombstoneReason *string `json:"tombstoneReason"`
	TombstonedBy    *string `json:"tombstonedBy"`
	TombstonedAt    *string `json:"tombstonedAt"`
	CreatedAt       string  `json:"createdAt"`
	UpdatedAt       string  `json:"updatedAt"`
}

func toPartyResponse(p domain.Party) partyResponse {
	r := partyResponse{
		PartyID:     p.ID,
		Status:      string(p.Status),
		KYCStatus:   string(p.KYCStatus),
		FirstName:   p.FirstName,
		LastName:    p.LastName,
		DateOfBirth: p.DateOfBirth.Format(dateLayout),
		SSNLast4:    p.SSNLast4(),
		Tombstoned:  p.Tombstoned,
		CreatedAt:   p.CreatedAt.Format(time.RFC3339),
		UpdatedAt:   p.UpdatedAt.Format(time.RFC3339),
	}
	if p.Email != "" {
		r.Email = &p.Email
	}
	if p.Phone != "" {
		r.Phone = &p.Phone
	}
	if p.Tombstoned {
		r.TombstoneReason = nonEmptyPtr(p.TombstoneReason)
		r.TombstonedBy = nonEmptyPtr(p.TombstonedBy)
		if p.TombstonedAt != nil {
			s := p.TombstonedAt.Format(time.RFC3339)
			r.TombstonedAt = &s
		}
	}
	return r
}

func nonEmptyPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// identityDocumentResponse mirrors party.schema.json#/$defs/IdentityDocument.
type identityDocumentResponse struct {
	DocumentID           string  `json:"documentId"`
	PartyID              string  `json:"partyId"`
	DocumentType         string  `json:"documentType"`
	Version              int     `json:"version"`
	SupersedesDocumentID *string `json:"supersedesDocumentId"`
	DocumentNumberLast4  string  `json:"documentNumberLast4"`
	IssuingAuthority     string  `json:"issuingAuthority"`
	ExpiresAt            *string `json:"expiresAt"`
	CreatedAt            string  `json:"createdAt"`
}

func toIdentityDocumentResponse(d domain.IdentityDocument) identityDocumentResponse {
	r := identityDocumentResponse{
		DocumentID:           d.ID,
		PartyID:              d.PartyID,
		DocumentType:         string(d.DocumentType),
		Version:              d.Version,
		SupersedesDocumentID: d.SupersedesDocumentID,
		DocumentNumberLast4:  d.DocumentNumberLast4(),
		IssuingAuthority:     d.IssuingAuthority,
		CreatedAt:            d.CreatedAt.Format(time.RFC3339),
	}
	if d.ExpiresAt != nil {
		s := d.ExpiresAt.Format(dateLayout)
		r.ExpiresAt = &s
	}
	return r
}

// findOrCreatePartyRequest mirrors party-cif.yaml's inline
// FindOrCreatePartyRequest schema.
type findOrCreatePartyRequest struct {
	FirstName   string  `json:"firstName"`
	LastName    string  `json:"lastName"`
	DateOfBirth string  `json:"dateOfBirth"`
	SSN         string  `json:"ssn"`
	Email       *string `json:"email"`
	Phone       *string `json:"phone"`
}

// matchExplanationResponse mirrors party.schema.json#/$defs/DedupMatchExplanation.
type matchExplanationResponse struct {
	Matched        bool     `json:"matched"`
	MatchedPartyID *string  `json:"matchedPartyId"`
	Confidence     float64  `json:"confidence"`
	MatchedFields  []string `json:"matchedFields"`
	RuleID         string   `json:"ruleId"`
}

func toMatchExplanationResponse(d domain.Decision) matchExplanationResponse {
	fields := d.MatchedFields
	if fields == nil {
		fields = []string{}
	}
	r := matchExplanationResponse{
		Matched:       d.Matched,
		Confidence:    d.Confidence,
		MatchedFields: fields,
		RuleID:        d.RuleID,
	}
	if d.Matched {
		r.MatchedPartyID = &d.MatchedPartyID
	}
	return r
}

type findOrCreatePartyResponse struct {
	Party            partyResponse            `json:"party"`
	MatchExplanation matchExplanationResponse `json:"matchExplanation"`
}

// updatePartyRequest mirrors party-cif.yaml's UpdatePartyRequest. A nil
// field (omitted, or explicit JSON null — Go's encoding/json cannot tell
// these apart on a *string without a presence-tracking wrapper) means
// "leave unchanged", matching internal/service.UpdatePartyInput's own
// documented contract.
type updatePartyRequest struct {
	Email *string `json:"email"`
	Phone *string `json:"phone"`
}

type tombstonePartyRequest struct {
	Reason string `json:"reason"`
	Actor  string `json:"actor"`
}

type addIdentityDocumentRequest struct {
	DocumentType     string  `json:"documentType"`
	DocumentNumber   string  `json:"documentNumber"`
	IssuingAuthority string  `json:"issuingAuthority"`
	ExpiresAt        *string `json:"expiresAt"`
}

// errorResponse mirrors error.schema.json#/$defs/Error's minimal shape.
type errorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
