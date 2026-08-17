package domain

import "time"

type DocumentType string

const (
	DocumentTypeDriversLicense    DocumentType = "DRIVERS_LICENSE"
	DocumentTypePassport          DocumentType = "PASSPORT"
	DocumentTypeStateID           DocumentType = "STATE_ID"
	DocumentTypeOtherGovernmentID DocumentType = "OTHER_GOVERNMENT_ID"
)

// IdentityDocument is versioned and immutable, the same discipline as
// TermVersion on the loan side of this system: a replacement document
// never edits or deletes a prior version, it appends a new one that
// references the version it supersedes.
type IdentityDocument struct {
	ID                   string
	PartyID              string
	DocumentType         DocumentType
	Version              int
	SupersedesDocumentID *string
	DocumentNumber       string // full number, plaintext, in-memory only — see pii.Encryptor
	IssuingAuthority     string
	ExpiresAt            *time.Time
	CreatedAt            time.Time
}

// DocumentNumberLast4 is the only form of the document number considered
// safe to log or return — same masking discipline as Party.SSNLast4.
func (d IdentityDocument) DocumentNumberLast4() string {
	if len(d.DocumentNumber) < 4 {
		return ""
	}
	return d.DocumentNumber[len(d.DocumentNumber)-4:]
}
