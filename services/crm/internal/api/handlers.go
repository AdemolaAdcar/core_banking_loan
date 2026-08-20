package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/AdemolaAdcar/core_banking_loan/services/crm/internal/auth"
	"github.com/AdemolaAdcar/core_banking_loan/services/crm/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/crm/internal/service"
	"github.com/AdemolaAdcar/core_banking_loan/services/crm/internal/store"
)

// Server wires the HTTP transport to internal/service (business writes
// and reads that need actor/access logging) and internal/auth (every
// route requires a valid bearer token carrying the operation's required
// scope; see crm.yaml's serviceAuth security scheme).
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

	r.Post("/interactions:log", withScope(s.validator, auth.ScopeInteractionWrite, withIdempotency(s.store, s.handleLogInteraction)))

	r.Post("/cases:open", withScope(s.validator, auth.ScopeCaseWrite, withIdempotency(s.store, s.handleOpenCase)))
	r.Get("/cases/{caseId}", withScope(s.validator, auth.ScopeCaseRead, s.handleGetCase))
	r.Post("/cases/{caseId}:update", withScope(s.validator, auth.ScopeCaseWrite, withIdempotency(s.store, s.handleUpdateCase)))
	r.Post("/cases/{caseId}:close", withScope(s.validator, auth.ScopeCaseWrite, withIdempotency(s.store, s.handleCloseCase)))
	r.Post("/cases/{caseId}:reopen", withScope(s.validator, auth.ScopeCaseWrite, withIdempotency(s.store, s.handleReopenCase)))

	r.Get("/cases/{caseId}/notes", withScope(s.validator, auth.ScopeCaseRead, s.handleListCaseNotes))
	r.Post("/cases/{caseId}/notes", withScope(s.validator, auth.ScopeCaseWrite, withIdempotency(s.store, s.handleAddCaseNote)))

	r.Get("/customers/{partyId}/360", withScope(s.validator, auth.ScopeCustomer360Read, s.handleGetCustomer360))

	r.Get("/customers/{partyId}/relationship-manager", withScope(s.validator, auth.ScopeRelationshipMgrRead, s.handleGetRelationshipManager))
	r.Post("/customers/{partyId}/relationship-manager", withScope(s.validator, auth.ScopeRelationshipMgrWrite, withIdempotency(s.store, s.handleAssignRelationshipManager)))

	r.Get("/customers/{partyId}/communication-preferences", withScope(s.validator, auth.ScopeCommPrefsRead, s.handleGetCommunicationPreferences))
	r.Put("/customers/{partyId}/communication-preferences", withScope(s.validator, auth.ScopeCommPrefsWrite, withIdempotency(s.store, s.handleUpdateCommunicationPreferences)))

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

// writeCaseError maps the shared domain/store errors every case-mutating
// handler can produce to their HTTP status, so each handler doesn't
// re-derive this mapping.
func writeCaseError(w http.ResponseWriter, err error) {
	var transErr *domain.ErrInvalidTransition
	if errors.As(err, &transErr) {
		writeError(w, http.StatusConflict, "INVALID_TRANSITION", err.Error())
		return
	}
	var staleErr *domain.ErrStaleVersion
	if errors.As(err, &staleErr) {
		writeError(w, http.StatusConflict, "STALE_VERSION", "expectedVersion is stale; reload the case and retry")
		return
	}
	if errors.Is(err, store.ErrStaleVersion) {
		writeError(w, http.StatusConflict, "STALE_VERSION", "case was updated concurrently; reload and retry")
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "CASE_NOT_FOUND", "no case with that ID")
		return
	}
	writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to process case operation")
}

func (s *Server) handleLogInteraction(w http.ResponseWriter, r *http.Request) {
	var req logInteractionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "request body is not valid JSON")
		return
	}
	if req.LoanAccountID == "" || req.EventType == "" || req.OccurredAt == "" {
		writeError(w, http.StatusBadRequest, "MISSING_FIELD", "loanAccountId, eventType, and occurredAt are required")
		return
	}
	occurredAt, err := time.Parse(time.RFC3339, req.OccurredAt)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_DATE", "occurredAt must be an ISO-8601 date-time")
		return
	}

	out, err := s.svc.LogInteraction(r.Context(), service.LogInteractionInput{
		LoanAccountID: req.LoanAccountID,
		EventType:     domain.EventType(req.EventType),
		OccurredAt:    occurredAt,
		Notes:         derefOrEmpty(req.Notes),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to log interaction")
		return
	}
	writeJSON(w, http.StatusCreated, toInteractionResponse(out))
}

func (s *Server) handleOpenCase(w http.ResponseWriter, r *http.Request) {
	var req openCaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "request body is not valid JSON")
		return
	}
	if req.PartyID == "" || req.ReasonCode == "" {
		writeError(w, http.StatusBadRequest, "MISSING_FIELD", "partyId and reasonCode are required")
		return
	}
	key := idempotencyKeyFromContext(r.Context())
	if key == "" {
		writeError(w, http.StatusBadRequest, "MISSING_IDEMPOTENCY_KEY", "Idempotency-Key is required and is used as the caseId")
		return
	}

	out, err := s.svc.OpenCase(r.Context(), service.OpenCaseInput{
		CaseID: key, PartyID: req.PartyID, LoanAccountID: req.LoanAccountID,
		ReasonCode: domain.ReasonCode(req.ReasonCode), AssignedTo: req.AssignedTo,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to open case")
		return
	}
	writeJSON(w, http.StatusCreated, toServiceCaseResponse(out))
}

func (s *Server) handleGetCase(w http.ResponseWriter, r *http.Request) {
	caseID := chi.URLParam(r, "caseId")
	c, err := s.store.GetCase(r.Context(), caseID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "CASE_NOT_FOUND", "no case with that ID")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to load case")
		return
	}
	writeJSON(w, http.StatusOK, toServiceCaseResponse(c))
}

func (s *Server) handleUpdateCase(w http.ResponseWriter, r *http.Request) {
	caseID := chi.URLParam(r, "caseId")
	var req updateCaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "request body is not valid JSON")
		return
	}

	var newStatus *domain.CaseStatus
	if req.Status != nil {
		st := domain.CaseStatus(*req.Status)
		newStatus = &st
	}
	out, err := s.svc.UpdateCase(r.Context(), service.UpdateCaseInput{
		CaseID: caseID, ExpectedVersion: req.ExpectedVersion, NewStatus: newStatus, NewAssignedTo: req.AssignedTo,
	})
	if err != nil {
		writeCaseError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toServiceCaseResponse(out))
}

func (s *Server) handleCloseCase(w http.ResponseWriter, r *http.Request) {
	caseID := chi.URLParam(r, "caseId")
	var req closeCaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "request body is not valid JSON")
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "MISSING_FIELD", "reason is required")
		return
	}

	out, err := s.svc.CloseCase(r.Context(), service.CloseCaseInput{CaseID: caseID, Reason: req.Reason})
	if err != nil {
		writeCaseError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toServiceCaseResponse(out))
}

func (s *Server) handleReopenCase(w http.ResponseWriter, r *http.Request) {
	caseID := chi.URLParam(r, "caseId")
	var req reopenCaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "request body is not valid JSON")
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "MISSING_FIELD", "reason is required")
		return
	}

	out, err := s.svc.ReopenCase(r.Context(), service.ReopenCaseInput{CaseID: caseID, Reason: req.Reason})
	if err != nil {
		writeCaseError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toServiceCaseResponse(out))
}

func (s *Server) handleListCaseNotes(w http.ResponseWriter, r *http.Request) {
	caseID := chi.URLParam(r, "caseId")
	if _, err := s.store.GetCase(r.Context(), caseID); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "CASE_NOT_FOUND", "no case with that ID")
		return
	}

	notes, err := s.svc.ListCaseNotes(r.Context(), actorSubjectFromContext(r.Context()), caseID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to load case notes")
		return
	}
	out := make([]caseNoteResponse, 0, len(notes))
	for _, n := range notes {
		out = append(out, toCaseNoteResponse(n))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAddCaseNote(w http.ResponseWriter, r *http.Request) {
	caseID := chi.URLParam(r, "caseId")
	var req addCaseNoteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "request body is not valid JSON")
		return
	}
	if req.AuthorID == "" || req.Body == "" {
		writeError(w, http.StatusBadRequest, "MISSING_FIELD", "authorId and body are required")
		return
	}
	if _, err := s.store.GetCase(r.Context(), caseID); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "CASE_NOT_FOUND", "no case with that ID")
		return
	}

	out, err := s.svc.AddCaseNote(r.Context(), service.AddCaseNoteInput{CaseID: caseID, AuthorID: req.AuthorID, Body: req.Body})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to add case note")
		return
	}
	writeJSON(w, http.StatusCreated, toCaseNoteResponse(out))
}

func (s *Server) handleGetCustomer360(w http.ResponseWriter, r *http.Request) {
	partyID := chi.URLParam(r, "partyId")
	c360, err := s.svc.GetCustomer360(r.Context(), actorSubjectFromContext(r.Context()), partyID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to load customer 360")
		return
	}
	writeJSON(w, http.StatusOK, toCustomer360Response(c360))
}

func (s *Server) handleGetRelationshipManager(w http.ResponseWriter, r *http.Request) {
	partyID := chi.URLParam(r, "partyId")
	a, err := s.svc.GetRelationshipManager(r.Context(), partyID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to load relationship manager assignment")
		return
	}
	writeJSON(w, http.StatusOK, toRelationshipManagerAssignmentResponse(a))
}

func (s *Server) handleAssignRelationshipManager(w http.ResponseWriter, r *http.Request) {
	partyID := chi.URLParam(r, "partyId")
	var req assignRelationshipManagerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "request body is not valid JSON")
		return
	}
	if req.RelationshipManagerID == "" {
		writeError(w, http.StatusBadRequest, "MISSING_FIELD", "relationshipManagerId is required")
		return
	}

	out, err := s.svc.AssignRelationshipManager(r.Context(), service.AssignRelationshipManagerInput{
		PartyID: partyID, RelationshipManagerID: req.RelationshipManagerID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to assign relationship manager")
		return
	}
	writeJSON(w, http.StatusOK, toRelationshipManagerAssignmentResponse(out))
}

func (s *Server) handleGetCommunicationPreferences(w http.ResponseWriter, r *http.Request) {
	partyID := chi.URLParam(r, "partyId")
	prefs, err := s.svc.GetCommunicationPreferences(r.Context(), partyID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to load communication preferences")
		return
	}
	writeJSON(w, http.StatusOK, toCommunicationPreferencesResponse(prefs))
}

func (s *Server) handleUpdateCommunicationPreferences(w http.ResponseWriter, r *http.Request) {
	partyID := chi.URLParam(r, "partyId")
	var req updateCommunicationPreferencesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "request body is not valid JSON")
		return
	}

	var channel *domain.PreferredChannel
	if req.PreferredChannel != nil {
		c := domain.PreferredChannel(*req.PreferredChannel)
		channel = &c
	}
	out, err := s.svc.UpdateCommunicationPreferences(r.Context(), service.UpdateCommunicationPreferencesInput{
		PartyID: partyID, PreferredChannel: channel, EmailOptIn: req.EmailOptIn, SMSOptIn: req.SMSOptIn,
		PhoneOptIn: req.PhoneOptIn, MailOptIn: req.MailOptIn, DoNotContact: req.DoNotContact,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to update communication preferences")
		return
	}
	writeJSON(w, http.StatusOK, toCommunicationPreferencesResponse(out))
}

func derefOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
