package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/AdemolaAdcar/core_banking_loan/services/party/internal/auth"
)

// withScope enforces party-cif.yaml's serviceAuth security scheme: every
// route requires a valid bearer token AND the specific scope that
// operation declares (party:read or party:write) — there is no
// unauthenticated route on this service.
func withScope(validator auth.Validator, requiredScope string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, err := bearerToken(r)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "MISSING_TOKEN", "Authorization: Bearer <token> header is required")
			return
		}
		claims, err := validator.Validate(r.Context(), token)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "INVALID_TOKEN", "access token is invalid or expired")
			return
		}
		if !claims.HasScope(requiredScope) {
			writeError(w, http.StatusForbidden, "INSUFFICIENT_SCOPE", "token is missing the required scope: "+requiredScope)
			return
		}
		next(w, r)
	}
}

var errMissingBearerToken = errors.New("missing or malformed Authorization header")

func bearerToken(r *http.Request) (string, error) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return "", errMissingBearerToken
	}
	token := strings.TrimSpace(strings.TrimPrefix(h, prefix))
	if token == "" {
		return "", errMissingBearerToken
	}
	return token, nil
}
