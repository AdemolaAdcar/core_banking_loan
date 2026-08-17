package domain

import (
	"testing"
	"time"
)

func date(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func TestEvaluateCandidate_SSNExactMatch_HighestConfidence(t *testing.T) {
	applicant := Applicant{
		NormalizedFirstName: "jordan",
		NormalizedLastName:  "rivera",
		DateOfBirth:         date(1991, time.April, 12),
		SSNHash:             "hash-123-45-6789",
	}
	candidate := MatchCandidate{
		PartyID:             "party-1",
		NormalizedFirstName: "jorden", // deliberately different name — SSN should still win outright
		NormalizedLastName:  "riveraa",
		DateOfBirth:         date(1985, time.January, 1), // deliberately different DOB too
		SSNHash:             "hash-123-45-6789",
	}

	result := EvaluateCandidate(applicant, candidate)
	if result == nil {
		t.Fatal("expected a match, got nil")
	}
	if result.RuleID != RuleSSNExact {
		t.Errorf("RuleID = %q, want %q", result.RuleID, RuleSSNExact)
	}
	if result.Confidence != ConfidenceSSNExact {
		t.Errorf("Confidence = %v, want %v", result.Confidence, ConfidenceSSNExact)
	}
	if len(result.MatchedFields) != 1 || result.MatchedFields[0] != "ssn" {
		t.Errorf("MatchedFields = %v, want [ssn]", result.MatchedFields)
	}
}

func TestEvaluateCandidate_NameDOBExact(t *testing.T) {
	applicant := Applicant{
		NormalizedFirstName: "jordan",
		NormalizedLastName:  "rivera",
		DateOfBirth:         date(1991, time.April, 12),
		SSNHash:             "hash-different",
	}
	candidate := MatchCandidate{
		PartyID:             "party-1",
		NormalizedFirstName: "jordan",
		NormalizedLastName:  "rivera",
		DateOfBirth:         date(1991, time.April, 12),
		SSNHash:             "hash-other-entirely",
	}

	result := EvaluateCandidate(applicant, candidate)
	if result == nil || result.RuleID != RuleNameDOBExact {
		t.Fatalf("got %+v, want RuleID=%s", result, RuleNameDOBExact)
	}
}

// --- Edge case: near-match names ---------------------------------------

func TestEvaluateCandidate_NearMatchName_WithMatchingDOB(t *testing.T) {
	applicant := Applicant{
		NormalizedFirstName: "jordan",
		NormalizedLastName:  "rivera",
		DateOfBirth:         date(1991, time.April, 12),
	}
	// One transposed pair of letters in the last name ("rivera" -> "rievra")
	// plus a DOB match should hit the near-match rule, not silently fall
	// through to no match.
	candidate := MatchCandidate{
		PartyID:             "party-1",
		NormalizedFirstName: "jordan",
		NormalizedLastName:  "rievra",
		DateOfBirth:         date(1991, time.April, 12),
	}

	result := EvaluateCandidate(applicant, candidate)
	if result == nil {
		t.Fatal("expected a near-match, got nil")
	}
	if result.RuleID != RuleNameNearMatchDOB {
		t.Errorf("RuleID = %q, want %q", result.RuleID, RuleNameNearMatchDOB)
	}
	if result.Confidence != ConfidenceNameNearMatchDOB {
		t.Errorf("Confidence = %v, want %v", result.Confidence, ConfidenceNameNearMatchDOB)
	}
	// Below AutoMatchThreshold — must never auto-match on a near-match alone.
	if result.Confidence >= AutoMatchThreshold {
		t.Errorf("near-match confidence %v must be below AutoMatchThreshold %v", result.Confidence, AutoMatchThreshold)
	}
}

func TestEvaluateCandidate_NearMatchName_TooDifferent_NoMatch(t *testing.T) {
	applicant := Applicant{NormalizedFirstName: "jordan", NormalizedLastName: "rivera", DateOfBirth: date(1991, time.April, 12)}
	// "rivera" vs "gonzalez" is a completely different name, not a
	// near-match, even with a matching DOB — the engine must not treat
	// two unrelated people who happen to share a birthday as related.
	candidate := MatchCandidate{PartyID: "party-1", NormalizedFirstName: "jordan", NormalizedLastName: "gonzalez", DateOfBirth: date(1991, time.April, 12)}

	if result := EvaluateCandidate(applicant, candidate); result != nil {
		t.Errorf("expected no match for a genuinely different surname, got %+v", result)
	}
}

func TestNamesNearMatch_BoundedByLength(t *testing.T) {
	// A 2-character edit distance on a very short name is proportionally
	// huge and must NOT be treated as a near-match ("Al"/"Bo" are
	// unrelated, not a typo of each other).
	if NamesNearMatch("al", "bo", "al", "bo") {
		t.Fatal("identical names should not go through NamesNearMatch's edit-distance path at all")
	}
	if NamesNearMatch("al", "", "bo", "") {
		t.Error("2-char totally different short names must not be a near-match (distance too large relative to length)")
	}
}

// --- Edge case: reused phone/email across applicants --------------------

func TestEvaluateCandidate_EmailReusedAcrossHousehold_NotAutoMatched(t *testing.T) {
	// Two different family members sharing one household email address,
	// with genuinely different names/DOB — email alone must never be
	// enough to auto-match.
	applicant := Applicant{
		NormalizedFirstName: "alex",
		NormalizedLastName:  "chen",
		DateOfBirth:         date(1998, time.June, 3),
		Email:               "family@example.com",
	}
	candidate := MatchCandidate{
		PartyID:             "party-parent",
		NormalizedFirstName: "pat",
		NormalizedLastName:  "chen",
		DateOfBirth:         date(1965, time.February, 20),
		Email:               "family@example.com",
	}

	result := EvaluateCandidate(applicant, candidate)
	if result == nil {
		t.Fatal("expected the shared email to still register as a weak candidate for audit purposes")
	}
	if result.RuleID != RuleEmailOnly {
		t.Errorf("RuleID = %q, want %q", result.RuleID, RuleEmailOnly)
	}
	if result.Confidence >= AutoMatchThreshold {
		t.Errorf("bare email match confidence %v must never reach AutoMatchThreshold %v on its own", result.Confidence, AutoMatchThreshold)
	}
	if result.Confidence < AuditFloor {
		t.Errorf("bare email match confidence %v should still clear AuditFloor %v so it's logged for review", result.Confidence, AuditFloor)
	}
}

func TestEvaluateCandidate_PhoneReusedAcrossHousehold_NotAutoMatched(t *testing.T) {
	applicant := Applicant{NormalizedFirstName: "sam", NormalizedLastName: "okafor", Phone: "15125551234"}
	candidate := MatchCandidate{PartyID: "party-sibling", NormalizedFirstName: "dana", NormalizedLastName: "okafor", Phone: "15125551234"}

	result := EvaluateCandidate(applicant, candidate)
	if result == nil || result.RuleID != RulePhoneOnly {
		t.Fatalf("got %+v, want RuleID=%s", result, RulePhoneOnly)
	}
	if result.Confidence >= AutoMatchThreshold {
		t.Errorf("bare phone match must never auto-match on its own, confidence=%v", result.Confidence)
	}
}

func TestEvaluateCandidate_EmailPlusNameCorroborated_HigherConfidenceThanEmailAlone(t *testing.T) {
	applicant := Applicant{NormalizedFirstName: "jordan", NormalizedLastName: "rivera", Email: "jordan.rivera@example.com"}
	candidate := MatchCandidate{PartyID: "party-1", NormalizedFirstName: "jordan", NormalizedLastName: "rivera", Email: "jordan.rivera@example.com"}

	result := EvaluateCandidate(applicant, candidate)
	if result == nil || result.RuleID != RuleEmailNameCorroborated {
		t.Fatalf("got %+v, want RuleID=%s", result, RuleEmailNameCorroborated)
	}
	if result.Confidence <= ConfidenceEmailOnly {
		t.Errorf("name-corroborated email match (%v) must score higher than a bare email match (%v)", result.Confidence, ConfidenceEmailOnly)
	}
	if result.Confidence < AutoMatchThreshold {
		t.Errorf("name-corroborated email match confidence %v should clear AutoMatchThreshold %v", result.Confidence, AutoMatchThreshold)
	}
}

// --- EvaluateAll / Decide -------------------------------------------------

func TestEvaluateAll_SortsByDescendingConfidence(t *testing.T) {
	applicant := Applicant{
		NormalizedFirstName: "jordan", NormalizedLastName: "rivera",
		DateOfBirth: date(1991, time.April, 12), SSNHash: "hash-abc",
	}
	candidates := []MatchCandidate{
		{PartyID: "weak-email-only", Email: ""}, // no overlap at all -- should not even appear
		{PartyID: "phone-only", NormalizedFirstName: "someone", NormalizedLastName: "else", Phone: "5551234567"},
		{PartyID: "ssn-match", NormalizedFirstName: "different", NormalizedLastName: "name", SSNHash: "hash-abc"},
	}
	applicant.Phone = "5551234567"

	results := EvaluateAll(applicant, candidates)
	if len(results) != 2 {
		t.Fatalf("expected 2 results (ssn-match + phone-only), got %d: %+v", len(results), results)
	}
	if results[0].PartyID != "ssn-match" {
		t.Errorf("first result should be the highest-confidence (SSN) match, got %+v", results[0])
	}
	if results[0].Confidence < results[1].Confidence {
		t.Error("results must be sorted by descending confidence")
	}
}

func TestDecide_NoResults_NotMatched(t *testing.T) {
	d := Decide(nil)
	if d.Matched {
		t.Error("expected no match when there are no results")
	}
}

func TestDecide_BelowThreshold_NotMatched(t *testing.T) {
	d := Decide([]MatchResult{{PartyID: "p1", RuleID: RulePhoneOnly, Confidence: ConfidencePhoneOnly}})
	if d.Matched {
		t.Errorf("a %v-confidence result must not auto-match (threshold is %v)", ConfidencePhoneOnly, AutoMatchThreshold)
	}
	if len(d.AllCandidates) != 1 {
		t.Error("candidate should still be retained on the Decision for audit logging even when not matched")
	}
}

func TestDecide_AboveThreshold_Matched(t *testing.T) {
	d := Decide([]MatchResult{{PartyID: "p1", RuleID: RuleSSNExact, Confidence: ConfidenceSSNExact, MatchedFields: []string{"ssn"}}})
	if !d.Matched {
		t.Fatal("expected an SSN-exact result to auto-match")
	}
	if d.MatchedPartyID != "p1" {
		t.Errorf("MatchedPartyID = %q, want p1", d.MatchedPartyID)
	}
	if d.RuleID != RuleSSNExact {
		t.Errorf("RuleID = %q, want %q", d.RuleID, RuleSSNExact)
	}
}

func TestDecide_AmbiguousTopTwoDifferentParties_ConservativelyNotMatched(t *testing.T) {
	// Two DIFFERENT existing parties both confidently match the same
	// applicant -- this must never be silently resolved by picking one.
	d := Decide([]MatchResult{
		{PartyID: "party-a", RuleID: RuleNameDOBExact, Confidence: ConfidenceNameDOBExact},
		{PartyID: "party-b", RuleID: RuleNameDOBExact, Confidence: ConfidenceNameDOBExact},
	})
	if d.Matched {
		t.Fatal("ambiguous match between two distinct existing parties must not auto-match")
	}
	if len(d.AllCandidates) != 2 {
		t.Error("both ambiguous candidates must be retained for human review")
	}
}

func TestDecide_TopTwoSameParty_StillMatched(t *testing.T) {
	// Two DIFFERENT rules both firing for the SAME candidate party (e.g.
	// this shouldn't normally happen since EvaluateCandidate returns only
	// one result per candidate, but Decide itself must not treat two
	// results for the same PartyID as an ambiguity).
	d := Decide([]MatchResult{
		{PartyID: "party-a", RuleID: RuleSSNExact, Confidence: ConfidenceSSNExact},
		{PartyID: "party-a", RuleID: RuleNameDOBExact, Confidence: ConfidenceNameDOBExact},
	})
	if !d.Matched || d.MatchedPartyID != "party-a" {
		t.Errorf("expected a confident match on party-a, got %+v", d)
	}
}

// --- Edge case: re-application after tombstone ---------------------------

func TestEvaluateCandidate_MatchesTombstonedParty_EngineDoesNotFilterTombstoned(t *testing.T) {
	// The dedup ENGINE itself is deliberately tombstone-agnostic — it
	// still reports the match. It is the SERVICE layer's job to decide
	// what "matched an existing, tombstoned party" means for the
	// find-or-create outcome (per the PartyAPI spec: still treated as a
	// match, not silently re-created as a duplicate). This test locks in
	// that the engine does not quietly drop tombstoned candidates from
	// consideration -- if it did, re-application after tombstone would
	// always create a duplicate, which is exactly the bug this test
	// guards against.
	applicant := Applicant{
		NormalizedFirstName: "jordan", NormalizedLastName: "rivera",
		DateOfBirth: date(1991, time.April, 12), SSNHash: "hash-123",
	}
	tombstonedCandidate := MatchCandidate{
		PartyID: "party-tombstoned", NormalizedFirstName: "jordan", NormalizedLastName: "rivera",
		DateOfBirth: date(1991, time.April, 12), SSNHash: "hash-123", Tombstoned: true,
	}

	result := EvaluateCandidate(applicant, tombstonedCandidate)
	if result == nil {
		t.Fatal("expected the engine to still match against a tombstoned candidate")
	}
	if result.PartyID != "party-tombstoned" {
		t.Errorf("PartyID = %q, want party-tombstoned", result.PartyID)
	}
	if result.RuleID != RuleSSNExact {
		t.Errorf("RuleID = %q, want %q (re-application with the same SSN should still be the strongest possible signal)", result.RuleID, RuleSSNExact)
	}
}

func TestEvaluateAll_ReapplicationAfterTombstone_StillSurfacesAsTopCandidate(t *testing.T) {
	applicant := Applicant{
		NormalizedFirstName: "jordan", NormalizedLastName: "rivera",
		DateOfBirth: date(1991, time.April, 12), SSNHash: "hash-123",
	}
	candidates := []MatchCandidate{
		{PartyID: "unrelated", NormalizedFirstName: "someone", NormalizedLastName: "else", Phone: "5559999999"},
		{PartyID: "party-tombstoned", NormalizedFirstName: "jordan", NormalizedLastName: "rivera",
			DateOfBirth: date(1991, time.April, 12), SSNHash: "hash-123", Tombstoned: true},
	}

	results := EvaluateAll(applicant, candidates)
	decision := Decide(results)

	if !decision.Matched {
		t.Fatal("re-application with the same SSN as a tombstoned party should auto-match, not silently create a duplicate")
	}
	if decision.MatchedPartyID != "party-tombstoned" {
		t.Errorf("MatchedPartyID = %q, want party-tombstoned", decision.MatchedPartyID)
	}
}

// --- Normalization helpers -----------------------------------------------

func TestNormalizeName(t *testing.T) {
	cases := map[string]string{
		"O'Brien":      "obrien",
		" Jordan ":     "jordan",
		"Rivera-Gomez": "riveragomez",
		"":             "",
	}
	for in, want := range cases {
		if got := NormalizeName(in); got != want {
			t.Errorf("NormalizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeEmail(t *testing.T) {
	if got := NormalizeEmail(" Jordan.Rivera@Example.COM "); got != "jordan.rivera@example.com" {
		t.Errorf("NormalizeEmail = %q, want lowercased/trimmed", got)
	}
}

func TestNormalizePhone(t *testing.T) {
	if got := NormalizePhone("+1 (512) 555-1234"); got != "15125551234" {
		t.Errorf("NormalizePhone = %q, want 15125551234", got)
	}
}
