package domain

import (
	"sort"
	"strings"
	"time"
	"unicode"
)

// MatchCandidate is the comparison-safe view of an existing party the
// dedup engine evaluates an applicant against. Every field is already
// normalized or hashed by the caller (the service layer) before it
// reaches this package — the engine itself never touches raw PII, only
// normalized names, a DOB, an SSN hash, and normalized contact fields.
type MatchCandidate struct {
	PartyID             string
	NormalizedFirstName string
	NormalizedLastName  string
	DateOfBirth         time.Time
	SSNHash             string
	Email               string // normalized (lowercased), empty if none on file
	Phone               string // normalized (digits-only), empty if none on file
	Tombstoned          bool
}

// Applicant is the identity data on a findOrCreateParty call, normalized
// identically to MatchCandidate so every comparison is apples-to-apples.
type Applicant struct {
	NormalizedFirstName string
	NormalizedLastName  string
	DateOfBirth         time.Time
	SSNHash             string
	Email               string
	Phone               string
}

// MatchResult is the deterministic, auditable outcome of comparing one
// Applicant against one MatchCandidate. MatchedFields never contains a
// raw PII VALUE — only field NAMES — so a MatchResult is always safe to
// persist to the dedup audit log or return in an API response.
type MatchResult struct {
	PartyID       string
	RuleID        string
	Confidence    float64
	MatchedFields []string
}

// Named, fixed dedup rules. Every MatchResult.RuleID is one of these
// constants — the entire point of "deterministic and explainable" is
// that a reviewer auditing a decision can look up exactly which named
// rule fired and what it means, rather than trusting an opaque score.
// There is no ML model or fuzzy/probabilistic ranking anywhere in this
// engine.
const (
	RuleSSNExact              = "SSN_EXACT"
	RuleNameDOBExact          = "NAME_DOB_EXACT"
	RuleEmailNameCorroborated = "EMAIL_NAME_CORROBORATED"
	RulePhoneNameCorroborated = "PHONE_NAME_CORROBORATED"
	RuleNameNearMatchDOB      = "NAME_NEAR_MATCH_DOB_EXACT"
	RuleEmailOnly             = "EMAIL_ONLY"
	RulePhoneOnly             = "PHONE_ONLY"
)

const (
	ConfidenceSSNExact              = 0.99
	ConfidenceNameDOBExact          = 0.95
	ConfidenceEmailNameCorroborated = 0.85
	ConfidencePhoneNameCorroborated = 0.80
	ConfidenceNameNearMatchDOB      = 0.65
	ConfidenceEmailOnly             = 0.55
	ConfidencePhoneOnly             = 0.50

	// AutoMatchThreshold: a MatchResult at or above this confidence
	// causes findOrCreateParty to return the EXISTING party rather than
	// create a new one.
	AutoMatchThreshold = 0.80

	// AuditFloor: a MatchResult below AutoMatchThreshold but at or above
	// this floor is still persisted to the dedup audit log for human
	// review — a plausible-but-not-certain duplicate. Below this floor,
	// a candidate is not recorded as a match attempt at all (too weak a
	// signal to be worth an audit entry, e.g. a bare phone/email hit
	// with no other corroboration would still clear this floor and get
	// logged; something with truly nothing in common would not).
	AuditFloor = 0.40
)

// EvaluateCandidate runs every rule against one candidate, most specific
// first, and returns the single highest-confidence rule that fired, or
// nil if nothing reaches AuditFloor. A candidate never produces more than
// one MatchResult — the first (highest-confidence) rule to match wins,
// and lower-confidence rules are never evaluated once a higher one fires.
func EvaluateCandidate(applicant Applicant, candidate MatchCandidate) *MatchResult {
	nameExact := applicant.NormalizedFirstName != "" &&
		applicant.NormalizedFirstName == candidate.NormalizedFirstName &&
		applicant.NormalizedLastName == candidate.NormalizedLastName
	dobExact := !applicant.DateOfBirth.IsZero() && !candidate.DateOfBirth.IsZero() &&
		applicant.DateOfBirth.Equal(candidate.DateOfBirth)
	nameNear := !nameExact && NamesNearMatch(
		applicant.NormalizedFirstName, applicant.NormalizedLastName,
		candidate.NormalizedFirstName, candidate.NormalizedLastName)
	emailExact := applicant.Email != "" && applicant.Email == candidate.Email
	phoneExact := applicant.Phone != "" && applicant.Phone == candidate.Phone

	switch {
	case applicant.SSNHash != "" && applicant.SSNHash == candidate.SSNHash:
		return &MatchResult{
			PartyID: candidate.PartyID, RuleID: RuleSSNExact, Confidence: ConfidenceSSNExact,
			MatchedFields: []string{"ssn"},
		}

	case nameExact && dobExact:
		return &MatchResult{
			PartyID: candidate.PartyID, RuleID: RuleNameDOBExact, Confidence: ConfidenceNameDOBExact,
			MatchedFields: []string{"firstName", "lastName", "dateOfBirth"},
		}

	case emailExact && nameExact:
		return &MatchResult{
			PartyID: candidate.PartyID, RuleID: RuleEmailNameCorroborated, Confidence: ConfidenceEmailNameCorroborated,
			MatchedFields: []string{"email", "firstName", "lastName"},
		}

	case phoneExact && nameExact:
		return &MatchResult{
			PartyID: candidate.PartyID, RuleID: RulePhoneNameCorroborated, Confidence: ConfidencePhoneNameCorroborated,
			MatchedFields: []string{"phone", "firstName", "lastName"},
		}

	case nameNear && dobExact:
		return &MatchResult{
			PartyID: candidate.PartyID, RuleID: RuleNameNearMatchDOB, Confidence: ConfidenceNameNearMatchDOB,
			MatchedFields: []string{"firstName", "lastName", "dateOfBirth"},
		}

	case emailExact:
		return &MatchResult{
			PartyID: candidate.PartyID, RuleID: RuleEmailOnly, Confidence: ConfidenceEmailOnly,
			MatchedFields: []string{"email"},
		}

	case phoneExact:
		return &MatchResult{
			PartyID: candidate.PartyID, RuleID: RulePhoneOnly, Confidence: ConfidencePhoneOnly,
			MatchedFields: []string{"phone"},
		}

	default:
		return nil
	}
}

// EvaluateAll runs EvaluateCandidate against every candidate, drops
// anything below AuditFloor, and returns the survivors sorted by
// descending confidence. The service layer persists every one of these
// to the dedup audit log — not just the eventual winner — so a human can
// see every plausible candidate that was considered, not only the one
// that was picked.
func EvaluateAll(applicant Applicant, candidates []MatchCandidate) []MatchResult {
	var results []MatchResult
	for _, c := range candidates {
		if r := EvaluateCandidate(applicant, c); r != nil && r.Confidence >= AuditFloor {
			results = append(results, *r)
		}
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Confidence > results[j].Confidence })
	return results
}

// Decision is the single, final outcome of a findOrCreateParty call.
type Decision struct {
	Matched        bool
	MatchedPartyID string
	Confidence     float64
	RuleID         string
	MatchedFields  []string
	AllCandidates  []MatchResult // every candidate considered, for the audit log — includes the winner, if any
}

// Decide turns a sorted list of MatchResults into a single auto-match
// decision. Ambiguity is handled conservatively: if the top two results
// both clear AutoMatchThreshold but reference DIFFERENT existing
// parties, this is explicitly NOT auto-matched — a confident collision
// between two distinct records must go to a human, not be silently
// resolved by picking whichever sorted first.
func Decide(results []MatchResult) Decision {
	if len(results) == 0 {
		return Decision{Matched: false}
	}
	top := results[0]
	if top.Confidence < AutoMatchThreshold {
		return Decision{Matched: false, AllCandidates: results}
	}
	if len(results) > 1 && results[1].Confidence >= AutoMatchThreshold && results[1].PartyID != top.PartyID {
		return Decision{Matched: false, AllCandidates: results}
	}
	return Decision{
		Matched:        true,
		MatchedPartyID: top.PartyID,
		Confidence:     top.Confidence,
		RuleID:         top.RuleID,
		MatchedFields:  top.MatchedFields,
		AllCandidates:  results,
	}
}

// NormalizeName lowercases and strips everything but letters/digits, so
// "O'Brien", "OBrien", and " obrien " all normalize identically.
func NormalizeName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// NormalizeEmail lowercases and trims — email comparison in this engine
// is case-insensitive exact match, never a fuzzy comparison.
func NormalizeEmail(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// NormalizePhone strips everything but digits — "+1 (512) 555-1234" and
// "5125551234" normalize identically. Deliberately does not attempt
// country-code inference beyond digit-stripping; a phone number that
// includes a leading "1" and one that doesn't will not normalize
// identically, which is a conservative (fewer false matches) choice.
func NormalizePhone(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// NamesNearMatch is a bounded, deterministic edit-distance check — never
// a probabilistic similarity score. Two already-normalized full names
// are a "near match" if their combined Levenshtein distance is small
// both in absolute terms and relative to length: this catches common
// data-entry variants (a transposed pair of letters, one missing or
// extra character) without being loose enough to match genuinely
// different names.
func NamesNearMatch(aFirst, aLast, bFirst, bLast string) bool {
	aFull, bFull := aFirst+aLast, bFirst+bLast
	if aFull == "" || bFull == "" {
		return false
	}
	dist := levenshtein(aFull, bFull)
	maxLen := len(aFull)
	if len(bFull) > maxLen {
		maxLen = len(bFull)
	}
	return dist > 0 && dist <= 2 && float64(dist) <= 0.2*float64(maxLen)
}

func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	la, lb := len(ra), len(rb)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	prev := make([]int, lb+1)
	curr := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			del := prev[j] + 1
			ins := curr[j-1] + 1
			sub := prev[j-1] + cost
			m := del
			if ins < m {
				m = ins
			}
			if sub < m {
				m = sub
			}
			curr[j] = m
		}
		prev, curr = curr, prev
	}
	return prev[lb]
}
