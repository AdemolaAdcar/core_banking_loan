package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/coa"
	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/service"
)

// mustEntry builds a minimal, valid, balanced JournalEntry posted at a
// fixed instant within the given YYYY-MM period -- test fixture only.
func mustEntry(t *testing.T, periodID string) domain.JournalEntry {
	t.Helper()
	postedAt, err := time.Parse("2006-01", periodID)
	if err != nil {
		t.Fatalf("invalid periodID fixture %q: %v", periodID, err)
	}
	lines := []domain.JournalEntryLine{
		{Line: domain.Line{GLAccount: coa.LoanReceivable, Direction: domain.Debit, Amount: domain.Money{Amount: 1500000, Currency: "USD"}}, RunningBalanceAfter: domain.Money{Amount: 1500000, Currency: "USD"}},
		{Line: domain.Line{GLAccount: coa.CashNostro, Direction: domain.Credit, Amount: domain.Money{Amount: 1500000, Currency: "USD"}}, RunningBalanceAfter: domain.Money{Amount: -1500000, Currency: "USD"}},
	}
	e, err := domain.NewJournalEntry("je-fixture", "disb:fixture", "PR-DISB-01", "1.0.0", "loan-1", lines, postedAt, false, nil, nil, nil)
	if err != nil {
		t.Fatalf("building fixture entry: %v", err)
	}
	return e
}

func closedPeriod(periodID string) domain.Period {
	closedAt := time.Now().UTC()
	closedBy := "ops"
	return domain.Period{ID: periodID, Status: domain.PeriodClosed, ClosedAt: &closedAt, ClosedBy: &closedBy}
}

func newTestServer() (*Server, *fakeStore) {
	fs := newFakeStore()
	svc := service.New(fs, coa.MustLoad())
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

func TestPostJournalEntry_Creates201(t *testing.T) {
	srv, fs := newTestServer()
	body, _ := json.Marshal(postJournalEntryRequest{
		PostingRuleCode: "PR-DISB-01", LoanAccountID: "loan-1",
		Amount: &moneyDTO{Amount: 1500000, Currency: "USD"},
	})
	rec := doRequest(srv.Routes(), http.MethodPost, "/journal-entries", body, "disb:1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp journalEntryDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !resp.Balanced || !resp.Immutable {
		t.Fatalf("expected balanced=true immutable=true, got %+v", resp)
	}
	if len(fs.entries) != 1 {
		t.Fatalf("expected 1 entry persisted, got %d", len(fs.entries))
	}
}

func TestPostJournalEntry_MissingIdempotencyKey_400(t *testing.T) {
	srv, _ := newTestServer()
	body, _ := json.Marshal(postJournalEntryRequest{
		PostingRuleCode: "PR-DISB-01", LoanAccountID: "loan-1", Amount: &moneyDTO{Amount: 100, Currency: "USD"},
	})
	rec := doRequest(srv.Routes(), http.MethodPost, "/journal-entries", body, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestIdempotency_ReplaySameKeySamePayload_NoSecondEntry(t *testing.T) {
	srv, fs := newTestServer()
	body, _ := json.Marshal(postJournalEntryRequest{
		PostingRuleCode: "PR-DISB-01", LoanAccountID: "loan-1", Amount: &moneyDTO{Amount: 1500000, Currency: "USD"},
	})
	first := doRequest(srv.Routes(), http.MethodPost, "/journal-entries", body, "disb:1")
	if first.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", first.Code, first.Body.String())
	}
	second := doRequest(srv.Routes(), http.MethodPost, "/journal-entries", body, "disb:1")
	if second.Code != http.StatusCreated {
		t.Fatalf("expected replayed 201, got %d: %s", second.Code, second.Body.String())
	}
	if first.Body.String() != second.Body.String() {
		t.Fatalf("expected identical replayed response")
	}
	if len(fs.entries) != 1 {
		t.Fatalf("expected exactly 1 entry, got %d", len(fs.entries))
	}
}

func TestIdempotency_SameKeyDifferentPayload_409(t *testing.T) {
	srv, _ := newTestServer()
	body1, _ := json.Marshal(postJournalEntryRequest{PostingRuleCode: "PR-DISB-01", LoanAccountID: "loan-1", Amount: &moneyDTO{Amount: 1500000, Currency: "USD"}})
	body2, _ := json.Marshal(postJournalEntryRequest{PostingRuleCode: "PR-DISB-01", LoanAccountID: "loan-1", Amount: &moneyDTO{Amount: 999, Currency: "USD"}})

	first := doRequest(srv.Routes(), http.MethodPost, "/journal-entries", body1, "reused-key")
	if first.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", first.Code, first.Body.String())
	}
	second := doRequest(srv.Routes(), http.MethodPost, "/journal-entries", body2, "reused-key")
	if second.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", second.Code, second.Body.String())
	}
}

func TestGetJournalEntry_NotFound_404(t *testing.T) {
	srv, _ := newTestServer()
	rec := doRequest(srv.Routes(), http.MethodGet, "/journal-entries/does-not-exist", nil, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestFindBySourceEvent_RoundTrips(t *testing.T) {
	srv, _ := newTestServer()
	body, _ := json.Marshal(postJournalEntryRequest{PostingRuleCode: "PR-DISB-01", LoanAccountID: "loan-1", Amount: &moneyDTO{Amount: 1500000, Currency: "USD"}})
	doRequest(srv.Routes(), http.MethodPost, "/journal-entries", body, "disb:findme")

	rec := doRequest(srv.Routes(), http.MethodGet, "/journal-entries/search?sourceEventId=disb:findme", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestGetAccountBalance_UnknownAccount_404(t *testing.T) {
	srv, _ := newTestServer()
	rec := doRequest(srv.Routes(), http.MethodGet, "/gl-accounts/9999/balance", nil, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestGetTrialBalance_ReflectsPostedEntries(t *testing.T) {
	srv, _ := newTestServer()
	body, _ := json.Marshal(postJournalEntryRequest{PostingRuleCode: "PR-DISB-01", LoanAccountID: "loan-1", Amount: &moneyDTO{Amount: 1500000, Currency: "USD"}})
	doRequest(srv.Routes(), http.MethodPost, "/journal-entries", body, "disb:1")

	rec := doRequest(srv.Routes(), http.MethodGet, "/trial-balance", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var tb trialBalanceDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &tb)
	if tb.TotalDebits.Amount != tb.TotalCredits.Amount {
		t.Fatalf("expected totalDebits == totalCredits, got %+v", tb)
	}
	if tb.TotalDebits.Amount != 1500000 {
		t.Fatalf("expected totals of 1500000, got %+v", tb)
	}
}

func TestClosePeriod_ChronologicalOrder_409(t *testing.T) {
	srv, fs := newTestServer()
	fs.entries["je-1"] = mustEntry(t, "2026-07")

	body, _ := json.Marshal(closePeriodRequest{ClosedBy: "ops"})
	rec := doRequest(srv.Routes(), http.MethodPost, "/periods/2026-08:close", body, "close-key")
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestClosePeriod_Idempotent(t *testing.T) {
	srv, _ := newTestServer()
	body, _ := json.Marshal(closePeriodRequest{ClosedBy: "ops"})
	first := doRequest(srv.Routes(), http.MethodPost, "/periods/2026-08:close", body, "close-key-1")
	if first.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", first.Code, first.Body.String())
	}
	second := doRequest(srv.Routes(), http.MethodPost, "/periods/2026-08:close", body, "close-key-2")
	if second.Code != http.StatusOK {
		t.Fatalf("expected 200 (idempotent) on a fresh idempotency key against an already-closed period, got %d: %s", second.Code, second.Body.String())
	}
}

func TestPostJournalEntry_ReversalOfClosedPeriod_422(t *testing.T) {
	srv, fs := newTestServer()
	julyEntry := mustEntry(t, "2026-07")
	julyEntry.SourceEventID = "disb:1"
	fs.entries[julyEntry.ID] = julyEntry
	fs.entryOrder = append(fs.entryOrder, julyEntry.ID)
	fs.bySourceEventID["disb:1"] = julyEntry.ID
	fs.periods["2026-07"] = closedPeriod("2026-07")

	body, _ := json.Marshal(postJournalEntryRequest{
		PostingRuleCode: "PR-DISB-02", LoanAccountID: "loan-1", Amount: &moneyDTO{Amount: 1500000, Currency: "USD"},
		ReversalOfSourceEventID: strPtr("disb:1"),
	})
	rec := doRequest(srv.Routes(), http.MethodPost, "/journal-entries", body, "disb:1:reversal")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
}

// --- Auth ---------------------------------------------------------------

func TestAuth_MissingAuthorizationHeader_401(t *testing.T) {
	fs := newFakeStore()
	srv := NewServer(service.New(fs, coa.MustLoad()), fs, allowAllValidator())
	req := httptest.NewRequest(http.MethodGet, "/journal-entries/x", nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAuth_InsufficientScope_403(t *testing.T) {
	fs := newFakeStore()
	readOnly := &fakeValidator{subject: "reader", grantedScopes: []string{"gl:journal-entry:read"}}
	srv := NewServer(service.New(fs, coa.MustLoad()), fs, readOnly)

	body, _ := json.Marshal(postJournalEntryRequest{PostingRuleCode: "PR-DISB-01", LoanAccountID: "loan-1", Amount: &moneyDTO{Amount: 100, Currency: "USD"}})
	req := httptest.NewRequest(http.MethodPost, "/journal-entries", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer whatever")
	req.Header.Set("Idempotency-Key", "key-1")
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

func strPtr(s string) *string { return &s }
