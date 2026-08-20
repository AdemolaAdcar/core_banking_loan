package domain

import "time"

// EventType mirrors party-cif and loan-side adapters' own domain events
// one-for-one -- logInteraction is CRM's write path for exactly those
// events, never a general-purpose note field for balance data.
type EventType string

const (
	EventAccountBooked            EventType = "ACCOUNT_BOOKED"
	EventAccountDisbursed         EventType = "ACCOUNT_DISBURSED"
	EventRepaymentPosted          EventType = "REPAYMENT_POSTED"
	EventDelinquencyStatusChanged EventType = "DELINQUENCY_STATUS_CHANGED"
	EventAccountClosed            EventType = "ACCOUNT_CLOSED"
	EventAccountChargedOff        EventType = "ACCOUNT_CHARGED_OFF"
	EventTermsModified            EventType = "TERMS_MODIFIED"
)

// Interaction is a logged record of a domain event from another module,
// referencing LoanAccount only by opaque ID -- never embedding or
// duplicating balance figures, which would drift from the ledger the
// moment they're cached.
type Interaction struct {
	ID            string
	LoanAccountID string
	EventType     EventType
	OccurredAt    time.Time
	Notes         string // PII-adjacent free text; see CaseNote.Body's doc comment for the handling discipline this implies
	CreatedAt     time.Time
}

// CaseNote is an append-only case narrative entry. A correction is a new
// note, never an edit of a prior one -- same immutability discipline as
// everywhere else in this system.
type CaseNote struct {
	ID        string
	CaseID    string
	AuthorID  string
	Body      string // PII-adjacent: encrypted at rest, every read access-logged, excluded from bulk/analytics export
	CreatedAt time.Time
}

// PreferredChannel is nullable at the domain level (a pointer, not this
// type alone) precisely because "never explicitly set" is a real, valid
// state distinct from any of these four values.
type PreferredChannel string

const (
	ChannelEmail PreferredChannel = "EMAIL"
	ChannelSMS   PreferredChannel = "SMS"
	ChannelPhone PreferredChannel = "PHONE"
	ChannelMail  PreferredChannel = "MAIL"
	ChannelNone  PreferredChannel = "NONE"
)

// CommunicationPreferences always exists conceptually for every party,
// even before any row is written -- DefaultCommunicationPreferences is
// the conservative zero-value a caller gets back before that first
// write.
type CommunicationPreferences struct {
	PartyID          string
	PreferredChannel *PreferredChannel
	EmailOptIn       bool
	SMSOptIn         bool
	PhoneOptIn       bool
	MailOptIn        bool
	DoNotContact     bool
	UpdatedAt        time.Time
}

// DefaultCommunicationPreferences is the conservative default: no
// channel preferred, every opt-in false. Returned by GetCommunicationPreferences
// when a party has never explicitly set preferences, rather than a 404 --
// every party implicitly has this (unset) preference state.
func DefaultCommunicationPreferences(partyID string) CommunicationPreferences {
	return CommunicationPreferences{PartyID: partyID}
}

// RelationshipManagerAssignment is a persistent, per-customer
// relationship -- distinct from ServiceCase.AssignedTo, which is the CSR
// working one specific case and has no bearing on who the customer's
// ongoing relationship manager is. A reassignment supersedes the prior
// one; it is never deleted (retained in the audit trail via the store's
// append-only assignment history), same discipline as everywhere else in
// this system.
type RelationshipManagerAssignment struct {
	PartyID               string
	RelationshipManagerID *string
	AssignedAt            *time.Time
}
