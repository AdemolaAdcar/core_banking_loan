// Package domain holds the Party/CIF service's core business types and
// logic, independent of transport (HTTP) and persistence (Postgres). Every
// type here that carries PII documents exactly where that PII may and may
// not leave this package.
package domain

import "time"

type PartyStatus string

const (
	PartyStatusActive   PartyStatus = "Active"
	PartyStatusBlocked  PartyStatus = "Blocked"
	PartyStatusClosed   PartyStatus = "Closed"
	PartyStatusInactive PartyStatus = "Inactive"
)

type KYCStatus string

const (
	KYCStatusVerified   KYCStatus = "Verified"
	KYCStatusUnverified KYCStatus = "Unverified"
	KYCStatusExpired    KYCStatus = "Expired"
)

// Party is the domain representation of a customer record. FirstName,
// LastName, DateOfBirth, SSN, Email, and Phone are PII: they exist here in
// plaintext only transiently, in memory, for the duration of a single
// request. They are never logged directly — callers must use SSNLast4 (or
// the equivalent masked accessors) for anything that might be logged,
// returned in an API response, or persisted to the dedup audit trail. The
// pii package is the only place plaintext PII is deliberately handled at
// rest, and only to encrypt it.
type Party struct {
	ID          string
	Status      PartyStatus
	KYCStatus   KYCStatus
	FirstName   string
	LastName    string
	DateOfBirth time.Time
	SSN         string
	Email       string
	Phone       string

	// Tombstoned records are never deleted (7-year audit retention, same
	// as the ledger). A tombstoned party is still a fully valid, readable
	// record — Tombstoned is a flag, not an absence.
	Tombstoned      bool
	TombstoneReason string
	TombstonedBy    string
	TombstonedAt    *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// SSNLast4 is the only form of the SSN considered safe to log, return in
// an API response, or otherwise let leave this package's plaintext
// boundary. Never expose Party.SSN directly outside pii.Encryptor.
func (p Party) SSNLast4() string {
	digits := onlyDigits(p.SSN)
	if len(digits) < 4 {
		return ""
	}
	return digits[len(digits)-4:]
}

func onlyDigits(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= '0' && c <= '9' {
			out = append(out, c)
		}
	}
	return string(out)
}
