package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/AdemolaAdcar/core_banking_loan/services/party/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/party/internal/service"
)

func newTestServer() (*Server, *fakeStore) {
	fs := newFakeStore()
	svc := service.New(fs)
	return NewServer(svc, fs, allowAllValidator()), fs
}

// doRequest attaches a bearer token by default (accepted unconditionally
// by fakeValidator/allowAllValidator) so tests unrelated to auth aren't
// forced to think about it — auth-specific tests below construct their
// own request directly instead of using this helper.
func doRequest(h http.Handler, method, path string, body []byte, idempotencyKey string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestFindOrCreateParty_MissingIdempotencyKey_400(t *testing.T) {
	srv, _ := newTestServer()
	body, _ := json.Marshal(findOrCreatePartyRequest{FirstName: "Jane", LastName: "Doe", DateOfBirth: "1990-01-01", SSN: "123-45-6789"})
	rec := doRequest(srv.Routes(), http.MethodPost, "/parties:find-or-create", body, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestFindOrCreateParty_CreatesParty_201(t *testing.T) {
	srv, fs := newTestServer()
	body, _ := json.Marshal(findOrCreatePartyRequest{FirstName: "Jane", LastName: "Doe", DateOfBirth: "1990-01-01", SSN: "123-45-6789"})
	rec := doRequest(srv.Routes(), http.MethodPost, "/parties:find-or-create", body, "key-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp findOrCreatePartyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Party.FirstName != "Jane" {
		t.Fatalf("expected FirstName=Jane, got %q", resp.Party.FirstName)
	}
	if resp.MatchExplanation.Matched {
		t.Fatalf("expected matched=false for a brand new party")
	}
	if len(fs.parties) != 1 {
		t.Fatalf("expected exactly one party persisted, got %d", len(fs.parties))
	}
}

func TestIdempotency_ReplaySameKeySamePayload_ReturnsCachedResponse_NoSecondPartyCreated(t *testing.T) {
	srv, fs := newTestServer()
	body, _ := json.Marshal(findOrCreatePartyRequest{FirstName: "Jane", LastName: "Doe", DateOfBirth: "1990-01-01", SSN: "123-45-6789"})

	first := doRequest(srv.Routes(), http.MethodPost, "/parties:find-or-create", body, "same-key")
	if first.Code != http.StatusCreated {
		t.Fatalf("expected 201 on first call, got %d: %s", first.Code, first.Body.String())
	}

	second := doRequest(srv.Routes(), http.MethodPost, "/parties:find-or-create", body, "same-key")
	if second.Code != http.StatusCreated {
		t.Fatalf("expected replayed 201, got %d: %s", second.Code, second.Body.String())
	}
	if first.Body.String() != second.Body.String() {
		t.Fatalf("expected identical replayed response body:\nfirst:  %s\nsecond: %s", first.Body.String(), second.Body.String())
	}
	if len(fs.parties) != 1 {
		t.Fatalf("expected no second party created on replay, got %d parties", len(fs.parties))
	}
}

func TestIdempotency_SameKeyDifferentPayload_409(t *testing.T) {
	srv, _ := newTestServer()
	body1, _ := json.Marshal(findOrCreatePartyRequest{FirstName: "Jane", LastName: "Doe", DateOfBirth: "1990-01-01", SSN: "123-45-6789"})
	body2, _ := json.Marshal(findOrCreatePartyRequest{FirstName: "John", LastName: "Smith", DateOfBirth: "1985-05-05", SSN: "999-99-9999"})

	first := doRequest(srv.Routes(), http.MethodPost, "/parties:find-or-create", body1, "reused-key")
	if first.Code != http.StatusCreated {
		t.Fatalf("expected 201 on first call, got %d: %s", first.Code, first.Body.String())
	}

	second := doRequest(srv.Routes(), http.MethodPost, "/parties:find-or-create", body2, "reused-key")
	if second.Code != http.StatusConflict {
		t.Fatalf("expected 409 for reused key with different payload, got %d: %s", second.Code, second.Body.String())
	}
}

func TestGetParty_NotFound_404(t *testing.T) {
	srv, _ := newTestServer()
	rec := doRequest(srv.Routes(), http.MethodGet, "/parties/does-not-exist", nil, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestUpdateParty_Tombstoned_409(t *testing.T) {
	srv, fs := newTestServer()
	fs.parties["p1"] = domain.Party{ID: "p1", Tombstoned: true}

	body, _ := json.Marshal(updatePartyRequest{Email: strPtr("new@example.com")})
	rec := doRequest(srv.Routes(), http.MethodPatch, "/parties/p1", body, "update-key")
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 for update on tombstoned party, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestTombstoneParty_Idempotent_SecondCallStillReturns200(t *testing.T) {
	srv, fs := newTestServer()
	fs.parties["p1"] = domain.Party{ID: "p1"}

	body, _ := json.Marshal(tombstonePartyRequest{Reason: "duplicate", Actor: "ops@bank.com"})
	first := doRequest(srv.Routes(), http.MethodPost, "/parties/p1:tombstone", body, "tomb-key-1")
	if first.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", first.Code, first.Body.String())
	}

	second := doRequest(srv.Routes(), http.MethodPost, "/parties/p1:tombstone", body, "tomb-key-2")
	if second.Code != http.StatusOK {
		t.Fatalf("expected 200 on a fresh idempotency key against an already-tombstoned party, got %d: %s", second.Code, second.Body.String())
	}
}

func TestAddIdentityDocument_SecondVersionSupersedesFirst(t *testing.T) {
	srv, fs := newTestServer()
	fs.parties["p1"] = domain.Party{ID: "p1"}

	body1, _ := json.Marshal(addIdentityDocumentRequest{DocumentType: "PASSPORT", DocumentNumber: "X1111111", IssuingAuthority: "US DOS"})
	first := doRequest(srv.Routes(), http.MethodPost, "/parties/p1/documents", body1, "doc-key-1")
	if first.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", first.Code, first.Body.String())
	}
	var firstDoc identityDocumentResponse
	_ = json.Unmarshal(first.Body.Bytes(), &firstDoc)

	body2, _ := json.Marshal(addIdentityDocumentRequest{DocumentType: "PASSPORT", DocumentNumber: "X2222222", IssuingAuthority: "US DOS"})
	second := doRequest(srv.Routes(), http.MethodPost, "/parties/p1/documents", body2, "doc-key-2")
	if second.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", second.Code, second.Body.String())
	}
	var secondDoc identityDocumentResponse
	_ = json.Unmarshal(second.Body.Bytes(), &secondDoc)

	if secondDoc.Version != 2 {
		t.Fatalf("expected version 2, got %d", secondDoc.Version)
	}
	if secondDoc.SupersedesDocumentID == nil || *secondDoc.SupersedesDocumentID != firstDoc.DocumentID {
		t.Fatalf("expected second document to supersede first (%s), got %v", firstDoc.DocumentID, secondDoc.SupersedesDocumentID)
	}
}

func strPtr(s string) *string { return &s }

func TestAuth_MissingAuthorizationHeader_401(t *testing.T) {
	fs := newFakeStore()
	srv := NewServer(service.New(fs), fs, allowAllValidator())
	req := httptest.NewRequest(http.MethodGet, "/parties/p1", nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no Authorization header, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAuth_InvalidToken_401(t *testing.T) {
	fs := newFakeStore()
	srv := NewServer(service.New(fs), fs, &fakeValidator{err: errInvalidTokenForTest})
	req := httptest.NewRequest(http.MethodGet, "/parties/p1", nil)
	req.Header.Set("Authorization", "Bearer whatever")
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for a token the validator rejects, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAuth_ReadScopeCannotFindOrCreate_403(t *testing.T) {
	fs := newFakeStore()
	readOnly := &fakeValidator{grantedScopes: []string{"party:read"}}
	srv := NewServer(service.New(fs), fs, readOnly)

	body, _ := json.Marshal(findOrCreatePartyRequest{FirstName: "Jane", LastName: "Doe", DateOfBirth: "1990-01-01", SSN: "123-45-6789"})
	req := httptest.NewRequest(http.MethodPost, "/parties:find-or-create", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer whatever")
	req.Header.Set("Idempotency-Key", "key-1")
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a party:read-only token calling a party:write operation, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAuth_WriteScopeCannotGetParty_403(t *testing.T) {
	fs := newFakeStore()
	fs.parties["p1"] = domain.Party{ID: "p1"}
	writeOnly := &fakeValidator{grantedScopes: []string{"party:write"}}
	srv := NewServer(service.New(fs), fs, writeOnly)

	req := httptest.NewRequest(http.MethodGet, "/parties/p1", nil)
	req.Header.Set("Authorization", "Bearer whatever")
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a party:write-only token calling a party:read operation, got %d: %s", rec.Code, rec.Body.String())
	}
}

var errInvalidTokenForTest = errors.New("simulated invalid token")
