package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/auth"
	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/service"
	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/store"
)

// Server wires the HTTP transport to internal/service and internal/auth
// — every route requires a valid bearer token carrying the operation's
// required scope, per payment-execution.yaml's serviceAuth scheme.
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
	r.Post("/payment-instructions:disburse", withScope(s.validator, auth.ScopeDisbursementWrite, withIdempotency(s.store, s.handleInitiateDisbursement)))
	r.Get("/payment-instructions/{instructionId}", withScope(s.validator, auth.ScopeDisbursementRead, s.handleGetPaymentInstruction))
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

func (s *Server) handleInitiateDisbursement(w http.ResponseWriter, r *http.Request) {
	var req initiateDisbursementRequestDTO
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "request body is not valid JSON")
		return
	}
	key := idempotencyKeyFromContext(r.Context())
	if key == "" {
		writeError(w, http.StatusBadRequest, "MISSING_IDEMPOTENCY_KEY", "Idempotency-Key is required")
		return
	}
	if req.LoanAccountID == "" || req.PartyID == "" {
		writeError(w, http.StatusBadRequest, "MISSING_FIELD", "loanAccountId and partyId are required")
		return
	}

	out, err := s.svc.InitiateDisbursement(r.Context(), service.InitiateDisbursementInput{
		IdempotencyKey: key, LoanAccountID: req.LoanAccountID, PartyID: req.PartyID,
		JournalEntryID: req.JournalEntryID, Amount: fromMoneyDTO(req.Amount),
	})
	if err != nil {
		writeInitiateDisbursementError(w, err)
		return
	}
	// Always 202 -- see PR_DESCRIPTION.md: the spec's separate 200
	// "idempotent replay" response code isn't distinguished from a fresh
	// submission here, matching every other idempotent-create endpoint
	// in this repo (e.g. services/las's bookLoanAccount), which return
	// one status code uniformly regardless of whether the underlying
	// call was a first execution or a domain-level idempotent lookup.
	// The functional guarantee the spec cares about — original
	// PaymentInstruction returned, nothing re-submitted — holds either
	// way.
	writeJSON(w, http.StatusAccepted, toPaymentInstructionDTO(out))
}

func writeInitiateDisbursementError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrMissingJournalEntry):
		writeError(w, http.StatusBadRequest, "MISSING_JOURNAL_ENTRY", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "INTERNAL", "unexpected error")
	}
}

func (s *Server) handleGetPaymentInstruction(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "instructionId")
	out, err := s.svc.GetPaymentInstruction(r.Context(), id)
	if errors.Is(err, service.ErrNotFound) || errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "PAYMENT_INSTRUCTION_NOT_FOUND", "no payment instruction with that ID")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to load payment instruction")
		return
	}
	writeJSON(w, http.StatusOK, toPaymentInstructionDTO(out))
}
