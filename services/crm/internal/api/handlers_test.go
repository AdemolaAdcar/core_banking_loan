package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/AdemolaAdcar/core_banking_loan/services/crm/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/crm/internal/service"
)

func newTestServer() (*Server, *fakeStore) {
	fs := newFakeStore()
	svc := service.New(fs)
	return NewServer(svc, fs, allowAllValidator()), fs
}

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

func TestLogInteraction_Creates201(t *testing.T) {
	srv, fs := newTestServer()
	body, _ := json.Marshal(logInteractionRequest{LoanAccountID: "loan-1", EventType: "ACCOUNT_DISBURSED", OccurredAt: "2026-08-19T00:00:00Z"})
	rec := doRequest(srv.Routes(), http.MethodPost, "/interactions:log", body, "key-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(fs.interactions) != 1 {
		t.Fatalf("expected 1 interaction persisted, got %d", len(fs.interactions))
	}
}

func TestOpenCase_MissingIdempotencyKey_400(t *testing.T) {
	srv, _ := newTestServer()
	body, _ := json.Marshal(openCaseRequest{PartyID: "party-1", ReasonCode: "GENERAL_INQUIRY"})
	rec := doRequest(srv.Routes(), http.MethodPost, "/cases:open", body, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestOpenCase_UsesIdempotencyKeyAsCaseID(t *testing.T) {
	srv, fs := newTestServer()
	body, _ := json.Marshal(openCaseRequest{PartyID: "party-1", ReasonCode: "GENERAL_INQUIRY"})
	rec := doRequest(srv.Routes(), http.MethodPost, "/cases:open", body, "case-abc-123")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp serviceCaseResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.CaseID != "case-abc-123" {
		t.Fatalf("expected caseId to equal the Idempotency-Key, got %q", resp.CaseID)
	}
	if _, ok := fs.cases["case-abc-123"]; !ok {
		t.Fatalf("expected case stored under the idempotency key")
	}
}

func TestIdempotency_ReplaySameKeySamePayload_NoSecondCaseCreated(t *testing.T) {
	srv, fs := newTestServer()
	body, _ := json.Marshal(openCaseRequest{PartyID: "party-1", ReasonCode: "GENERAL_INQUIRY"})

	first := doRequest(srv.Routes(), http.MethodPost, "/cases:open", body, "same-key")
	if first.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", first.Code, first.Body.String())
	}
	second := doRequest(srv.Routes(), http.MethodPost, "/cases:open", body, "same-key")
	if second.Code != http.StatusCreated {
		t.Fatalf("expected replayed 201, got %d: %s", second.Code, second.Body.String())
	}
	if first.Body.String() != second.Body.String() {
		t.Fatalf("expected identical replayed response")
	}
	if len(fs.cases) != 1 {
		t.Fatalf("expected exactly 1 case, got %d", len(fs.cases))
	}
}

func TestIdempotency_SameKeyDifferentPayload_409(t *testing.T) {
	srv, _ := newTestServer()
	body1, _ := json.Marshal(openCaseRequest{PartyID: "party-1", ReasonCode: "GENERAL_INQUIRY"})
	body2, _ := json.Marshal(openCaseRequest{PartyID: "party-2", ReasonCode: "HARDSHIP_REQUEST"})

	first := doRequest(srv.Routes(), http.MethodPost, "/cases:open", body1, "reused-key")
	if first.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", first.Code, first.Body.String())
	}
	second := doRequest(srv.Routes(), http.MethodPost, "/cases:open", body2, "reused-key")
	if second.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", second.Code, second.Body.String())
	}
}

func TestGetCase_NotFound_404(t *testing.T) {
	srv, _ := newTestServer()
	rec := doRequest(srv.Routes(), http.MethodGet, "/cases/does-not-exist", nil, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestUpdateCase_ConcurrentUpdate_409(t *testing.T) {
	srv, fs := newTestServer()
	fs.cases["case-1"] = domain.ServiceCase{ID: "case-1", PartyID: "party-1", Status: domain.CaseStatusOpen, ReasonCode: domain.ReasonGeneralInquiry, Version: 5}

	body, _ := json.Marshal(updateCaseRequest{ExpectedVersion: 1, Status: strPtr("InProgress")})
	rec := doRequest(srv.Routes(), http.MethodPost, "/cases/case-1:update", body, "upd-key")
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 for a stale version, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestUpdateCase_InvalidTransition_409(t *testing.T) {
	srv, fs := newTestServer()
	fs.cases["case-1"] = domain.ServiceCase{ID: "case-1", PartyID: "party-1", Status: domain.CaseStatusResolved, ReasonCode: domain.ReasonGeneralInquiry, Version: 1}

	body, _ := json.Marshal(updateCaseRequest{ExpectedVersion: 1, Status: strPtr("Open")})
	rec := doRequest(srv.Routes(), http.MethodPost, "/cases/case-1:update", body, "upd-key-2")
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 for an invalid transition, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCloseCase_Idempotent_SecondCallStillReturns200(t *testing.T) {
	srv, fs := newTestServer()
	fs.cases["case-1"] = domain.ServiceCase{ID: "case-1", PartyID: "party-1", Status: domain.CaseStatusOpen, ReasonCode: domain.ReasonGeneralInquiry, Version: 1}

	body, _ := json.Marshal(closeCaseRequest{Reason: "resolved"})
	first := doRequest(srv.Routes(), http.MethodPost, "/cases/case-1:close", body, "close-key-1")
	if first.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", first.Code, first.Body.String())
	}
	second := doRequest(srv.Routes(), http.MethodPost, "/cases/case-1:close", body, "close-key-2")
	if second.Code != http.StatusOK {
		t.Fatalf("expected 200 (idempotent close) on a fresh idempotency key against an already-closed case, got %d: %s", second.Code, second.Body.String())
	}
}

func TestReopenCase_NotClosed_409(t *testing.T) {
	srv, fs := newTestServer()
	fs.cases["case-1"] = domain.ServiceCase{ID: "case-1", PartyID: "party-1", Status: domain.CaseStatusOpen, ReasonCode: domain.ReasonGeneralInquiry, Version: 1}

	body, _ := json.Marshal(reopenCaseRequest{Reason: "mistaken reopen"})
	rec := doRequest(srv.Routes(), http.MethodPost, "/cases/case-1:reopen", body, "reopen-key")
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 reopening a non-closed case, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAddCaseNote_ThenListCaseNotes(t *testing.T) {
	srv, fs := newTestServer()
	fs.cases["case-1"] = domain.ServiceCase{ID: "case-1", PartyID: "party-1", Status: domain.CaseStatusOpen, ReasonCode: domain.ReasonGeneralInquiry, Version: 1}

	body, _ := json.Marshal(addCaseNoteRequest{AuthorID: "csr.jdoe", Body: "customer disputes a fee"})
	addRec := doRequest(srv.Routes(), http.MethodPost, "/cases/case-1/notes", body, "note-key")
	if addRec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", addRec.Code, addRec.Body.String())
	}

	listRec := doRequest(srv.Routes(), http.MethodGet, "/cases/case-1/notes", nil, "")
	if listRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", listRec.Code, listRec.Body.String())
	}
	var notes []caseNoteResponse
	_ = json.Unmarshal(listRec.Body.Bytes(), &notes)
	if len(notes) != 1 || notes[0].Body != "customer disputes a fee" {
		t.Fatalf("unexpected notes: %+v", notes)
	}
}

func TestAssignThenGetRelationshipManager(t *testing.T) {
	srv, _ := newTestServer()
	body, _ := json.Marshal(assignRelationshipManagerRequest{RelationshipManagerID: "rm-1"})
	assignRec := doRequest(srv.Routes(), http.MethodPost, "/customers/party-1/relationship-manager", body, "rm-key")
	if assignRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", assignRec.Code, assignRec.Body.String())
	}

	getRec := doRequest(srv.Routes(), http.MethodGet, "/customers/party-1/relationship-manager", nil, "")
	if getRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", getRec.Code, getRec.Body.String())
	}
	var resp relationshipManagerAssignmentResponse
	_ = json.Unmarshal(getRec.Body.Bytes(), &resp)
	if resp.RelationshipManagerID == nil || *resp.RelationshipManagerID != "rm-1" {
		t.Fatalf("unexpected assignment: %+v", resp)
	}
}

func TestGetCommunicationPreferences_NeverSet_ReturnsDefaults(t *testing.T) {
	srv, _ := newTestServer()
	rec := doRequest(srv.Routes(), http.MethodGet, "/customers/party-1/communication-preferences", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp communicationPreferencesResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.PreferredChannel != nil || resp.EmailOptIn || resp.DoNotContact {
		t.Fatalf("expected conservative defaults, got %+v", resp)
	}
}

func TestUpdateCommunicationPreferences_RoundTrips(t *testing.T) {
	srv, _ := newTestServer()
	channel := "EMAIL"
	body, _ := json.Marshal(updateCommunicationPreferencesRequest{PreferredChannel: &channel, EmailOptIn: true})
	rec := doRequest(srv.Routes(), http.MethodPut, "/customers/party-1/communication-preferences", body, "prefs-key")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp communicationPreferencesResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.PreferredChannel == nil || *resp.PreferredChannel != "EMAIL" || !resp.EmailOptIn {
		t.Fatalf("unexpected preferences: %+v", resp)
	}
}

func TestGetCustomer360_ReturnsOpenCasesAndSummaries(t *testing.T) {
	srv, fs := newTestServer()
	loanID := "loan-1"
	fs.cases["case-1"] = domain.ServiceCase{ID: "case-1", PartyID: "party-1", LoanAccountID: &loanID, Status: domain.CaseStatusOpen, ReasonCode: domain.ReasonGeneralInquiry, Version: 1}
	fs.loanAccountLink[loanID] = "party-1"
	fs.interactions = append(fs.interactions, domain.Interaction{ID: "int-1", LoanAccountID: loanID, EventType: domain.EventAccountDisbursed})

	rec := doRequest(srv.Routes(), http.MethodGet, "/customers/party-1/360", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp customer360Response
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.OpenCases) != 1 {
		t.Fatalf("expected 1 open case, got %d", len(resp.OpenCases))
	}
	if len(resp.LoanAccountSummaries) != 1 || resp.LoanAccountSummaries[0].Status != "Disbursed" {
		t.Fatalf("unexpected loan account summaries: %+v", resp.LoanAccountSummaries)
	}
}

// --- Auth ---------------------------------------------------------------

func TestAuth_MissingAuthorizationHeader_401(t *testing.T) {
	fs := newFakeStore()
	srv := NewServer(service.New(fs), fs, allowAllValidator())
	req := httptest.NewRequest(http.MethodGet, "/cases/case-1", nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAuth_InsufficientScope_403(t *testing.T) {
	fs := newFakeStore()
	readOnly := &fakeValidator{subject: "reader", grantedScopes: []string{"crm:case:read"}}
	srv := NewServer(service.New(fs), fs, readOnly)

	body, _ := json.Marshal(openCaseRequest{PartyID: "party-1", ReasonCode: "GENERAL_INQUIRY"})
	req := httptest.NewRequest(http.MethodPost, "/cases:open", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer whatever")
	req.Header.Set("Idempotency-Key", "key-1")
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a read-only token calling a write operation, got %d: %s", rec.Code, rec.Body.String())
	}
}

func strPtr(s string) *string { return &s }
