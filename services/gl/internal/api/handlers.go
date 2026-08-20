package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/auth"
	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/postingrules"
	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/service"
	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/store"
)

// Server wires the HTTP transport to internal/service and internal/auth
// (every route requires a valid bearer token carrying the operation's
// required scope; see gl-posting-engine.yaml's serviceAuth security
// scheme).
type Server struct {
	svc       *service.Service
	store     store.Store
	validator auth.Validator
}

func NewServer(svc *service.Service, st store.Store, validator auth.Validator) *Server {
	return &Server{svc: svc, store: st, validator: validator}
}

func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()

	r.Post("/journal-entries", withScope(s.validator, auth.ScopeJournalEntryWrite, withIdempotency(s.store, s.handlePostJournalEntry)))
	r.Get("/journal-entries/search", withScope(s.validator, auth.ScopeJournalEntryRead, s.handleFindBySourceEvent))
	r.Get("/journal-entries/{journalEntryId}", withScope(s.validator, auth.ScopeJournalEntryRead, s.handleGetJournalEntry))

	r.Get("/gl-accounts/{glAccountCode}/balance", withScope(s.validator, auth.ScopeAccountBalanceRead, s.handleGetAccountBalance))

	r.Get("/trial-balance", withScope(s.validator, auth.ScopeTrialBalanceRead, s.handleGetTrialBalance))
	r.Get("/loan-accounts/{loanAccountId}/statement", withScope(s.validator, auth.ScopeTrialBalanceRead, s.handleGetStatementOfAccount))

	r.Get("/periods/{periodId}", withScope(s.validator, auth.ScopePeriodRead, s.handleGetPeriod))
	r.Post("/periods/{periodId}:close", withScope(s.validator, auth.ScopePeriodWrite, withIdempotency(s.store, s.handleClosePeriod)))

	return r
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorResponse{Code: code, Message: message})
}

func (s *Server) handlePostJournalEntry(w http.ResponseWriter, r *http.Request) {
	var req postJournalEntryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "request body is not valid JSON")
		return
	}
	if req.PostingRuleCode == "" || req.LoanAccountID == "" {
		writeError(w, http.StatusBadRequest, "MISSING_FIELD", "postingRuleCode and loanAccountId are required")
		return
	}
	if !postingrules.IsKnownRuleCode(req.PostingRuleCode) {
		writeError(w, http.StatusBadRequest, "UNKNOWN_RULE_CODE", "unknown postingRuleCode")
		return
	}
	key := idempotencyKeyFromContext(r.Context())
	if key == "" {
		writeError(w, http.StatusBadRequest, "MISSING_IDEMPOTENCY_KEY", "Idempotency-Key is required")
		return
	}

	out, err := s.svc.PostJournalEntry(r.Context(), service.PostJournalEntryInput{
		IdempotencyKey: key, PostingRuleCode: postingrules.RuleCode(req.PostingRuleCode), LoanAccountID: req.LoanAccountID,
		Amount: amountFromDTO(req.Amount), Allocation: allocationFromDTO(req.Allocation), Capitalization: capitalizationFromDTO(req.Capitalization),
		ReversalOfSourceEventID: req.ReversalOfSourceEventID, PriorPeriodAdjustmentForPeriodID: req.PriorPeriodAdjustmentForPeriodID,
		Metadata: req.Metadata,
	})
	if err != nil {
		writePostJournalEntryError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toJournalEntryDTO(out))
}

func writePostJournalEntryError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrUnknownRuleCode):
		writeError(w, http.StatusBadRequest, "UNKNOWN_RULE_CODE", err.Error())
	case errors.Is(err, service.ErrWrongInputShape):
		writeError(w, http.StatusBadRequest, "WRONG_INPUT_SHAPE", err.Error())
	case errors.Is(err, service.ErrMissingReversalTarget):
		writeError(w, http.StatusBadRequest, "MISSING_REVERSAL_TARGET", err.Error())
	case errors.Is(err, service.ErrMissingRequiredMetadata):
		writeError(w, http.StatusBadRequest, "MISSING_REQUIRED_METADATA", err.Error())
	case errors.Is(err, service.ErrReversalTargetNotFound):
		writeError(w, http.StatusUnprocessableEntity, "REVERSAL_TARGET_NOT_FOUND", err.Error())
	case errors.Is(err, service.ErrReversalTargetPeriodClosed):
		writeError(w, http.StatusUnprocessableEntity, "REVERSAL_TARGET_PERIOD_CLOSED", err.Error())
	case errors.Is(err, service.ErrReversalAmountMismatch):
		writeError(w, http.StatusUnprocessableEntity, "REVERSAL_AMOUNT_MISMATCH", err.Error())
	case errors.Is(err, service.ErrAdjustmentPeriodNotClosed):
		writeError(w, http.StatusUnprocessableEntity, "ADJUSTMENT_PERIOD_NOT_CLOSED", err.Error())
	default:
		// Covers domain.ErrUnbalanced / domain.ErrMultiCurrency and any
		// other domain-level validation error (e.g. multi-currency
		// rejection) that reaches this far -- all are malformed-request
		// cases, not server errors.
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
	}
}

func (s *Server) handleGetJournalEntry(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "journalEntryId")
	e, err := s.svc.GetJournalEntry(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "JOURNAL_ENTRY_NOT_FOUND", "no journal entry with that ID")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to load journal entry")
		return
	}
	writeJSON(w, http.StatusOK, toJournalEntryDTO(e))
}

func (s *Server) handleFindBySourceEvent(w http.ResponseWriter, r *http.Request) {
	sourceEventID := r.URL.Query().Get("sourceEventId")
	if sourceEventID == "" {
		writeError(w, http.StatusBadRequest, "MISSING_FIELD", "sourceEventId query parameter is required")
		return
	}
	e, found, err := s.svc.FindBySourceEventID(r.Context(), sourceEventID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to look up journal entry")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "no entry posted yet for this sourceEventId")
		return
	}
	writeJSON(w, http.StatusOK, toJournalEntryDTO(e))
}

func (s *Server) handleGetAccountBalance(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "glAccountCode")
	asOf := parseAsOf(r)
	balance, err := s.svc.GetAccountBalance(r.Context(), code, asOf)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "UNKNOWN_ACCOUNT", "unknown glAccountCode")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to load account balance")
		return
	}
	writeJSON(w, http.StatusOK, glAccountBalanceDTO{GLAccountCode: code, Balance: toMoneyDTO(balance), AsOf: asOf.Format(time.RFC3339)})
}

func (s *Server) handleGetTrialBalance(w http.ResponseWriter, r *http.Request) {
	asOf := parseAsOf(r)
	lines, err := s.svc.GetTrialBalance(r.Context(), asOf)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to compute trial balance")
		return
	}
	writeJSON(w, http.StatusOK, toTrialBalanceDTO(lines, asOf))
}

func (s *Server) handleGetStatementOfAccount(w http.ResponseWriter, r *http.Request) {
	loanAccountID := chi.URLParam(r, "loanAccountId")
	asOf := parseAsOf(r)
	lines, err := s.svc.GetStatementOfAccount(r.Context(), loanAccountID, asOf)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to compute statement of account")
		return
	}
	if len(lines) == 0 {
		writeError(w, http.StatusNotFound, "NO_ENTRIES", "loanAccountId has no posted entries")
		return
	}
	writeJSON(w, http.StatusOK, toStatementOfAccountDTO(loanAccountID, lines, asOf))
}

func (s *Server) handleGetPeriod(w http.ResponseWriter, r *http.Request) {
	periodID := chi.URLParam(r, "periodId")
	p, err := s.svc.GetPeriod(r.Context(), periodID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to load period")
		return
	}
	writeJSON(w, http.StatusOK, toPeriodDTO(p))
}

func (s *Server) handleClosePeriod(w http.ResponseWriter, r *http.Request) {
	periodID := chi.URLParam(r, "periodId")
	var req closePeriodRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "request body is not valid JSON")
		return
	}
	if req.ClosedBy == "" {
		writeError(w, http.StatusBadRequest, "MISSING_FIELD", "closedBy is required")
		return
	}

	p, err := s.svc.ClosePeriod(r.Context(), periodID, req.ClosedBy)
	if err != nil {
		var chronErr *domain.ErrEarlierPeriodOpen
		if errors.As(err, &chronErr) {
			writeError(w, http.StatusConflict, "EARLIER_PERIOD_OPEN", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to close period")
		return
	}
	writeJSON(w, http.StatusOK, toPeriodDTO(p))
}

func parseAsOf(r *http.Request) time.Time {
	v := r.URL.Query().Get("asOf")
	if v == "" {
		return time.Now().UTC()
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Now().UTC()
	}
	return t
}
