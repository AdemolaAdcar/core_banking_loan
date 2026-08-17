// Package events defines the domain event payloads this service publishes,
// matching specs/asyncapi/party-events.yaml exactly. Every payload here
// carries zero PII by construction (field types alone make it impossible
// to accidentally add a PII value) — enforced by review, not by a runtime
// check, since Go's type system can't express "this struct must never
// gain a string field," but the shape of these structs is deliberately
// minimal for that reason.
package events

import "time"

// Topic names match specs/asyncapi/party-events.yaml's channel keys
// exactly — <context>.<entity>.<eventPastTense>, no version suffix.
const (
	TopicPartyCreated    = "party.created"
	TopicPartyUpdated    = "party.updated"
	TopicPartyTombstoned = "party.tombstoned"
)

type PartyCreatedPayload struct {
	PartyID   string    `json:"partyId"`
	Status    string    `json:"status"`
	KYCStatus string    `json:"kycStatus"`
	CreatedAt time.Time `json:"createdAt"`
}

type PartyUpdatedPayload struct {
	PartyID       string    `json:"partyId"`
	ChangedFields []string  `json:"changedFields"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

type PartyTombstonedPayload struct {
	PartyID      string    `json:"partyId"`
	Reason       string    `json:"reason"`
	Actor        string    `json:"actor"`
	TombstonedAt time.Time `json:"tombstonedAt"`
}
