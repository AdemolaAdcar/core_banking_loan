package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/auth"
	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/glclient"
	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/service"
	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/store"
)

// Server wires the HTTP transport to internal/service and internal/auth
// — every route requires a valid bearer token carrying the operation's
// required scope, per loan-account-subledger.yaml's serviceAuth scheme.
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

	r.Post("/loan-accounts:book", withScope(s.validator, auth.ScopeBookingWrite, withIdempotency(s.store, s.handleBookLoanAccount)))
	r.Get("/loan-accounts/{loanAccountId}", withScope(s.validator, auth.ScopeBookingRead, s.handleGetLoanAccount))

	r.Post("/loan-accounts/{loanAccountId}/disbursements", withScope(s.validator, auth.ScopeDisbursementWrite, withIdempotency(s.store, s.handleCreateDisbursement)))
	r.Get("/loan-accounts/{loanAccountId}/disbursements/{disbursementId}", withScope(s.validator, auth.ScopeDisbursementRead, s.handleGetDisbursement))
	r.Post("/loan-accounts/{loanAccountId}/disbursements/{disbursementId}:reverse", withScope(s.validator, auth.ScopeDisbursementWrite, withIdempotency(s.store, s.handleReverseDisbursement)))

	r.Get("/loan-accounts:accrual-eligible", withScope(s.validator, auth.ScopeAccrualRead, s.handleListAccrualEligible))
	r.Post("/loan-accounts:accrue", withScope(s.validator, auth.ScopeAccrualWrite, s.handleRunDailyAccrual))

	r.Post("/repayments:notify", withScope(s.validator, auth.ScopeRepaymentWrite, withIdempotency(s.store, s.handleReceiveRepaymentNotification)))
	r.Get("/repayments/{repaymentId}", withScope(s.validator, auth.ScopeRepaymentRead, s.handleGetRepayment))
	r.Post("/repayments/{repaymentId}:reverse", withScope(s.validator, auth.ScopeRepaymentWrite, withIdempotency(s.store, s.handleReverseRepayment)))

	r.Get("/loan-accounts:past-due", withScope(s.validator, auth.ScopeDelinquencyRead, s.handleListPastDue))
	r.Post("/loan-accounts:assess-delinquency", withScope(s.validator, auth.ScopeDelinquencyWrite, s.handleRunDailyDelinquencyAssessment))
	r.Get("/fees/{feeId}", withScope(s.validator, auth.ScopeDelinquencyRead, s.handleGetFee))
	r.Post("/fees/{feeId}:waive", withScope(s.validator, auth.ScopeDelinquencyWrite, withIdempotency(s.store, s.handleWaiveFee)))

	r.Get("/loan-accounts/{loanAccountId}/payoff-quote", withScope(s.validator, auth.ScopePayoffRead, s.handleGetPayoffQuote))
	r.Get("/payoffs/{payoffId}", withScope(s.validator, auth.ScopePayoffRead, s.handleGetPayoff))

	r.Post("/loan-accounts/{loanAccountId}:chargeoff", withScope(s.validator, auth.ScopeChargeoffWrite, withIdempotency(s.store, s.handleRecordChargeOff)))
	r.Get("/chargeoffs/{chargeoffId}", withScope(s.validator, auth.ScopeChargeoffRead, s.handleGetChargeOff))
	r.Get("/recoveries/{recoveryId}", withScope(s.validator, auth.ScopeChargeoffRead, s.handleGetRecovery))

	r.Post("/loan-accounts/{loanAccountId}/modifications", withScope(s.validator, auth.ScopeModificationWrite, withIdempotency(s.store, s.handleApplyModification)))
	r.Get("/modifications/{modificationId}", withScope(s.validator, auth.ScopeModificationRead, s.handleGetModification))
	r.Get("/loan-accounts/{loanAccountId}/term-versions", withScope(s.validator, auth.ScopeModificationRead, s.handleListTermVersions))
	r.Get("/loan-accounts/{loanAccountId}/term-versions:effective", withScope(s.validator, auth.ScopeModificationRead, s.handleGetEffectiveTermVersion))

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

// writeServiceError is the single error-mapping switch every handler
// below funnels through — one error vocabulary from internal/service
// (and, underneath it, internal/glclient) all the way to the HTTP
// response.
func writeServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrMalformedTerms):
		writeError(w, http.StatusBadRequest, "MALFORMED_REQUEST", err.Error())
	case errors.Is(err, service.ErrLoanAccountNotFound), errors.Is(err, service.ErrNotFound), errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "NOT_FOUND", err.Error())
	case errors.Is(err, service.ErrTerminalAccount):
		writeError(w, http.StatusConflict, "TERMINAL_ACCOUNT", err.Error())
	case errors.Is(err, service.ErrNotModifiable):
		writeError(w, http.StatusConflict, "NOT_MODIFIABLE", err.Error())
	case isInvalidTransition(err):
		writeError(w, http.StatusConflict, "INVALID_TRANSITION", err.Error())
	case errors.Is(err, glclient.ErrRequestRejected):
		writeError(w, http.StatusUnprocessableEntity, "GL_REQUEST_REJECTED", err.Error())
	case errors.Is(err, glclient.ErrIdempotencyKeyReused):
		writeError(w, http.StatusConflict, "GL_IDEMPOTENCY_KEY_REUSED", err.Error())
	case errors.Is(err, glclient.ErrGLUnavailable):
		writeError(w, http.StatusBadGateway, "GL_UNAVAILABLE", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "INTERNAL", "unexpected error")
	}
}

func isInvalidTransition(err error) bool {
	var t *domain.ErrInvalidTransition
	return errors.As(err, &t)
}

func requireIdempotencyKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := idempotencyKeyFromContext(r.Context())
	if key == "" {
		writeError(w, http.StatusBadRequest, "MISSING_IDEMPOTENCY_KEY", "Idempotency-Key is required")
		return "", false
	}
	return key, true
}

func (s *Server) handleBookLoanAccount(w http.ResponseWriter, r *http.Request) {
	var req bookLoanAccountRequestDTO
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "request body is not valid JSON")
		return
	}
	if _, ok := requireIdempotencyKey(w, r); !ok {
		return
	}
	out, err := s.svc.BookLoanAccount(r.Context(), service.BookLoanAccountInput{
		ApprovalReferenceID: req.ApprovalReferenceID, PartyID: req.PartyID, BookedBy: req.BookedBy, Terms: fromTermSetDTO(req.Terms),
	})
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toLoanAccountDTO(out))
}

func (s *Server) handleGetLoanAccount(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "loanAccountId")
	out, err := s.svc.GetLoanAccount(r.Context(), id)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toLoanAccountDTO(out))
}

func (s *Server) handleCreateDisbursement(w http.ResponseWriter, r *http.Request) {
	loanAccountID := chi.URLParam(r, "loanAccountId")
	var req disbursementRequestDTO
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "request body is not valid JSON")
		return
	}
	key, ok := requireIdempotencyKey(w, r)
	if !ok {
		return
	}
	out, err := s.svc.CreateDisbursement(r.Context(), loanAccountID, key, req.RequestedBy)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toDisbursementDTO(out))
}

func (s *Server) handleGetDisbursement(w http.ResponseWriter, r *http.Request) {
	out, err := s.svc.GetDisbursement(r.Context(), chi.URLParam(r, "disbursementId"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toDisbursementDTO(out))
}

func (s *Server) handleReverseDisbursement(w http.ResponseWriter, r *http.Request) {
	disbursementID := chi.URLParam(r, "disbursementId")
	var req disbursementReverseRequestDTO
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "request body is not valid JSON")
		return
	}
	key, ok := requireIdempotencyKey(w, r)
	if !ok {
		return
	}
	out, err := s.svc.ReverseDisbursement(r.Context(), disbursementID, key, req.ConfirmedBy, req.ReasonCode)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, toDisbursementDTO(out))
}

func (s *Server) handleListAccrualEligible(w http.ResponseWriter, r *http.Request) {
	asOf := parseAsOfDate(r, "asOf")
	accounts, err := s.svc.ListAccrualEligibleAccounts(r.Context(), asOf)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	out := make([]loanAccountDTO, len(accounts))
	for i, a := range accounts {
		out[i] = toLoanAccountDTO(a)
	}
	writeJSON(w, http.StatusOK, map[string]any{"asOf": asOf.Format("2006-01-02"), "eligibleAccounts": out, "nextCursor": nil})
}

func (s *Server) handleRunDailyAccrual(w http.ResponseWriter, r *http.Request) {
	var req accrualBatchRequestDTO
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "request body is not valid JSON")
		return
	}
	asOf, err := time.Parse("2006-01-02", req.AsOf)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_DATE", "asOf must be a valid date")
		return
	}
	summary := s.svc.RunDailyAccrual(r.Context(), asOf)
	writeJSON(w, http.StatusOK, toAccrualBatchSummaryDTO(summary))
}

func (s *Server) handleReceiveRepaymentNotification(w http.ResponseWriter, r *http.Request) {
	var req receiveRepaymentNotificationRequestDTO
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "request body is not valid JSON")
		return
	}
	key, ok := requireIdempotencyKey(w, r)
	if !ok {
		return
	}
	receivedAt, err := time.Parse(time.RFC3339, req.ReceivedAt)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_DATE", "receivedAt must be RFC3339")
		return
	}
	result, err := s.svc.ReceiveRepaymentNotification(r.Context(), service.ReceiveRepaymentNotificationInput{
		IdempotencyKey: key, LoanAccountRef: req.LoanAccountRef, PayoffQuoteID: req.PayoffQuoteID, Amount: fromMoneyDTO(req.Amount), Rail: req.Rail, ReceivedAt: receivedAt,
	})
	if err != nil {
		writeServiceError(w, err)
		return
	}
	switch result.Kind {
	case service.KindPayoff:
		writeJSON(w, http.StatusCreated, toPayoffDTO(*result.Payoff))
	case service.KindRecovery:
		writeJSON(w, http.StatusCreated, toRecoveryDTO(*result.Recovery))
	default:
		writeJSON(w, http.StatusCreated, toRepaymentDTO(*result.Repayment))
	}
}

func (s *Server) handleGetRepayment(w http.ResponseWriter, r *http.Request) {
	out, err := s.svc.GetRepayment(r.Context(), chi.URLParam(r, "repaymentId"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toRepaymentDTO(out))
}

func (s *Server) handleReverseRepayment(w http.ResponseWriter, r *http.Request) {
	repaymentID := chi.URLParam(r, "repaymentId")
	var req reverseRepaymentRequestDTO
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "request body is not valid JSON")
		return
	}
	if req.ReasonCode == string(domain.ReasonMisapplied) && (req.CorrectedLoanAccountID == nil || *req.CorrectedLoanAccountID == "") {
		writeError(w, http.StatusBadRequest, "MISSING_FIELD", "correctedLoanAccountId is required when reasonCode is MISAPPLIED")
		return
	}
	key, ok := requireIdempotencyKey(w, r)
	if !ok {
		return
	}
	out, err := s.svc.ReverseRepayment(r.Context(), repaymentID, key, req.ConfirmedBy, req.ReasonCode, req.CorrectedLoanAccountID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, toRepaymentDTO(out))
}

func (s *Server) handleListPastDue(w http.ResponseWriter, r *http.Request) {
	asOf := parseAsOfDate(r, "asOf")
	accounts, err := s.svc.ListPastDueAccounts(r.Context(), asOf)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	out := make([]loanAccountDTO, len(accounts))
	for i, a := range accounts {
		out[i] = toLoanAccountDTO(a)
	}
	writeJSON(w, http.StatusOK, map[string]any{"asOf": asOf.Format("2006-01-02"), "accounts": out, "nextCursor": nil})
}

// handleRunDailyDelinquencyAssessment passes an empty update list — see
// internal/service.DelinquencyUpdate's doc comment: deriving DPD from a
// payment schedule needs a subsystem this build does not yet have. The
// full assessment logic (bucket transition, PR-DELINQ-01, non-accrual
// flagging) is implemented and tested in internal/service; only the
// schedule-derivation step feeding it is deferred (see PR_DESCRIPTION.md).
func (s *Server) handleRunDailyDelinquencyAssessment(w http.ResponseWriter, r *http.Request) {
	var req accrualBatchRequestDTO // same {asOf, partitionIndex, partitionCount} shape as DelinquencyBatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "request body is not valid JSON")
		return
	}
	asOf, err := time.Parse("2006-01-02", req.AsOf)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_DATE", "asOf must be a valid date")
		return
	}
	summary := s.svc.RunDailyDelinquencyAssessment(r.Context(), asOf, nil)
	writeJSON(w, http.StatusOK, toDelinquencyBatchSummaryDTO(summary))
}

func (s *Server) handleGetFee(w http.ResponseWriter, r *http.Request) {
	out, err := s.svc.GetFee(r.Context(), chi.URLParam(r, "feeId"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toFeeDTO(out))
}

func (s *Server) handleWaiveFee(w http.ResponseWriter, r *http.Request) {
	feeID := chi.URLParam(r, "feeId")
	var req waiveFeeRequestDTO
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "request body is not valid JSON")
		return
	}
	key, ok := requireIdempotencyKey(w, r)
	if !ok {
		return
	}
	out, err := s.svc.WaiveFee(r.Context(), feeID, key, req.ConfirmedBy, domain.WaiveReasonCode(req.ReasonCode))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, toFeeDTO(out))
}

func (s *Server) handleGetPayoffQuote(w http.ResponseWriter, r *http.Request) {
	loanAccountID := chi.URLParam(r, "loanAccountId")
	goodThrough, err := time.Parse("2006-01-02", r.URL.Query().Get("goodThrough"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_DATE", "goodThrough must be a valid date")
		return
	}
	out, err := s.svc.GetPayoffQuote(r.Context(), loanAccountID, goodThrough)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toPayoffQuoteDTO(out))
}

func (s *Server) handleGetPayoff(w http.ResponseWriter, r *http.Request) {
	out, err := s.svc.GetPayoff(r.Context(), chi.URLParam(r, "payoffId"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toPayoffDTO(out))
}

func (s *Server) handleRecordChargeOff(w http.ResponseWriter, r *http.Request) {
	loanAccountID := chi.URLParam(r, "loanAccountId")
	var req recordChargeOffRequestDTO
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "request body is not valid JSON")
		return
	}
	if _, ok := requireIdempotencyKey(w, r); !ok {
		return
	}
	out, err := s.svc.RecordChargeOff(r.Context(), loanAccountID, req.ChargeoffDecisionReference, req.ConfirmedBy)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toChargeOffDTO(out))
}

func (s *Server) handleGetChargeOff(w http.ResponseWriter, r *http.Request) {
	out, err := s.svc.GetChargeOff(r.Context(), chi.URLParam(r, "chargeoffId"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toChargeOffDTO(out))
}

func (s *Server) handleGetRecovery(w http.ResponseWriter, r *http.Request) {
	out, err := s.svc.GetRecovery(r.Context(), chi.URLParam(r, "recoveryId"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toRecoveryDTO(out))
}

func (s *Server) handleApplyModification(w http.ResponseWriter, r *http.Request) {
	loanAccountID := chi.URLParam(r, "loanAccountId")
	var req applyModificationRequestDTO
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "request body is not valid JSON")
		return
	}
	key, ok := requireIdempotencyKey(w, r)
	if !ok {
		return
	}
	effectiveDate, err := time.Parse("2006-01-02", req.EffectiveDate)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_DATE", "effectiveDate must be a valid date")
		return
	}
	var capitalization *domain.Capitalization
	if req.Capitalization != nil {
		capitalization = &domain.Capitalization{InterestAmount: fromMoneyDTO(req.Capitalization.InterestAmount), FeeAmount: fromMoneyDTO(req.Capitalization.FeeAmount)}
	}
	out, err := s.svc.ApplyModification(r.Context(), service.ApplyModificationInput{
		LoanAccountID: loanAccountID, ModificationID: key, EffectiveDate: effectiveDate, ConfirmedBy: req.ConfirmedBy,
		NewAnnualInterestRateBps: req.NewAnnualInterestRateBps, NewTermMonths: req.NewTermMonths, Capitalization: capitalization,
	})
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toModificationDTO(out))
}

func (s *Server) handleGetModification(w http.ResponseWriter, r *http.Request) {
	out, err := s.svc.GetModification(r.Context(), chi.URLParam(r, "modificationId"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toModificationDTO(out))
}

func (s *Server) handleListTermVersions(w http.ResponseWriter, r *http.Request) {
	versions, err := s.svc.ListTermVersions(r.Context(), chi.URLParam(r, "loanAccountId"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	out := make([]termVersionDTO, len(versions))
	for i, v := range versions {
		out[i] = toTermVersionDTO(v)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetEffectiveTermVersion(w http.ResponseWriter, r *http.Request) {
	loanAccountID := chi.URLParam(r, "loanAccountId")
	asOf, err := time.Parse("2006-01-02", r.URL.Query().Get("asOf"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_DATE", "asOf must be a valid date")
		return
	}
	out, err := s.svc.GetEffectiveTermVersion(r.Context(), loanAccountID, asOf)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toTermVersionDTO(out))
}

func parseAsOfDate(r *http.Request, param string) time.Time {
	v := r.URL.Query().Get(param)
	if v == "" {
		return time.Now().UTC()
	}
	t, err := time.Parse("2006-01-02", v)
	if err != nil {
		return time.Now().UTC()
	}
	return t
}
