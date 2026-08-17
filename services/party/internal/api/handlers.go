package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/AdemolaAdcar/core_banking_loan/services/party/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/party/internal/service"
	"github.com/AdemolaAdcar/core_banking_loan/services/party/internal/store"
)

// Server wires the HTTP transport to internal/service (business writes)
// and internal/store.Store (read-only lookups — getParty,
// listIdentityDocuments, getIdentityDocument have no business logic of
// their own, so they call the store directly rather than through a
// pass-through service method).
type Server struct {
	svc   *service.Service
	store store.Store
}

func NewServer(svc *service.Service, st store.Store) *Server {
	return &Server{svc: svc, store: st}
}

func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Post("/parties:find-or-create", withIdempotency(s.store, s.handleFindOrCreateParty))
	r.Get("/parties/{partyId}", s.handleGetParty)
	r.Patch("/parties/{partyId}", withIdempotency(s.store, s.handleUpdateParty))
	r.Post("/parties/{partyId}:tombstone", withIdempotency(s.store, s.handleTombstoneParty))
	r.Get("/parties/{partyId}/documents", s.handleListIdentityDocuments)
	r.Post("/parties/{partyId}/documents", withIdempotency(s.store, s.handleAddIdentityDocument))
	r.Get("/parties/{partyId}/documents/{documentId}", s.handleGetIdentityDocument)
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

func (s *Server) handleFindOrCreateParty(w http.ResponseWriter, r *http.Request) {
	var req findOrCreatePartyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "request body is not valid JSON")
		return
	}
	if req.FirstName == "" || req.LastName == "" || req.DateOfBirth == "" || req.SSN == "" {
		writeError(w, http.StatusBadRequest, "MISSING_FIELD", "firstName, lastName, dateOfBirth, and ssn are required")
		return
	}
	dob, err := time.Parse(dateLayout, req.DateOfBirth)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_DATE", "dateOfBirth must be an ISO-8601 date")
		return
	}

	out, err := s.svc.FindOrCreateParty(r.Context(), service.FindOrCreateInput{
		IdempotencyKey: idempotencyKeyFromContext(r.Context()),
		FirstName:      req.FirstName,
		LastName:       req.LastName,
		DateOfBirth:    dob,
		SSN:            req.SSN,
		Email:          derefOrEmpty(req.Email),
		Phone:          derefOrEmpty(req.Phone),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to process find-or-create request")
		return
	}

	status := http.StatusCreated
	if !out.Created {
		status = http.StatusOK
	}
	writeJSON(w, status, findOrCreatePartyResponse{
		Party:            toPartyResponse(out.Party),
		MatchExplanation: toMatchExplanationResponse(out.Decision),
	})
}

func (s *Server) handleGetParty(w http.ResponseWriter, r *http.Request) {
	partyID := chi.URLParam(r, "partyId")
	p, err := s.store.GetParty(r.Context(), partyID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "PARTY_NOT_FOUND", "no party with that ID")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to load party")
		return
	}
	writeJSON(w, http.StatusOK, toPartyResponse(p))
}

func (s *Server) handleUpdateParty(w http.ResponseWriter, r *http.Request) {
	partyID := chi.URLParam(r, "partyId")
	var req updatePartyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "request body is not valid JSON")
		return
	}

	p, err := s.svc.UpdateParty(r.Context(), service.UpdatePartyInput{
		PartyID: partyID,
		Email:   req.Email,
		Phone:   req.Phone,
	})
	if errors.Is(err, service.ErrPartyTombstoned) {
		writeError(w, http.StatusConflict, "PARTY_TOMBSTONED", "cannot update a tombstoned party")
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "PARTY_NOT_FOUND", "no party with that ID")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to update party")
		return
	}
	writeJSON(w, http.StatusOK, toPartyResponse(p))
}

func (s *Server) handleTombstoneParty(w http.ResponseWriter, r *http.Request) {
	partyID := chi.URLParam(r, "partyId")
	var req tombstonePartyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "request body is not valid JSON")
		return
	}
	if req.Reason == "" || req.Actor == "" {
		writeError(w, http.StatusBadRequest, "MISSING_FIELD", "reason and actor are required")
		return
	}

	p, err := s.svc.TombstoneParty(r.Context(), service.TombstonePartyInput{
		PartyID: partyID, Reason: req.Reason, Actor: req.Actor,
	})
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "PARTY_NOT_FOUND", "no party with that ID")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to tombstone party")
		return
	}
	writeJSON(w, http.StatusOK, toPartyResponse(p))
}

func (s *Server) handleListIdentityDocuments(w http.ResponseWriter, r *http.Request) {
	partyID := chi.URLParam(r, "partyId")
	if _, err := s.store.GetParty(r.Context(), partyID); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "PARTY_NOT_FOUND", "no party with that ID")
		return
	}
	docs, err := s.store.ListIdentityDocuments(r.Context(), partyID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to load identity documents")
		return
	}
	out := make([]identityDocumentResponse, 0, len(docs))
	for _, d := range docs {
		out = append(out, toIdentityDocumentResponse(d))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetIdentityDocument(w http.ResponseWriter, r *http.Request) {
	partyID := chi.URLParam(r, "partyId")
	documentID := chi.URLParam(r, "documentId")
	d, err := s.store.GetIdentityDocument(r.Context(), partyID, documentID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "DOCUMENT_NOT_FOUND", "no identity document with that ID for this party")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to load identity document")
		return
	}
	writeJSON(w, http.StatusOK, toIdentityDocumentResponse(d))
}

func (s *Server) handleAddIdentityDocument(w http.ResponseWriter, r *http.Request) {
	partyID := chi.URLParam(r, "partyId")
	var req addIdentityDocumentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "request body is not valid JSON")
		return
	}
	if req.DocumentType == "" || req.DocumentNumber == "" || req.IssuingAuthority == "" {
		writeError(w, http.StatusBadRequest, "MISSING_FIELD", "documentType, documentNumber, and issuingAuthority are required")
		return
	}
	var expiresAt *time.Time
	if req.ExpiresAt != nil && *req.ExpiresAt != "" {
		t, err := time.Parse(dateLayout, *req.ExpiresAt)
		if err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_DATE", "expiresAt must be an ISO-8601 date")
			return
		}
		expiresAt = &t
	}

	if _, err := s.store.GetParty(r.Context(), partyID); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "PARTY_NOT_FOUND", "no party with that ID")
		return
	}

	d, err := s.svc.AddIdentityDocument(r.Context(), service.AddIdentityDocumentInput{
		PartyID:          partyID,
		DocumentType:     domain.DocumentType(req.DocumentType),
		DocumentNumber:   req.DocumentNumber,
		IssuingAuthority: req.IssuingAuthority,
		ExpiresAt:        expiresAt,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to add identity document")
		return
	}
	writeJSON(w, http.StatusCreated, toIdentityDocumentResponse(d))
}

func derefOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
