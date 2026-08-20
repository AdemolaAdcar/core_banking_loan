package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/AdemolaAdcar/core_banking_loan/services/crm/internal/auth"
)

const actorSubjectContextKey contextKey = "actorSubject"

// actorSubjectFromContext returns the authenticated caller's subject, as
// stashed by withScope after successful token validation -- this is the
// "actor" recorded in every read-access-log entry (listCaseNotes,
// getCustomer360), never a client-supplied value, per this service's
// ground rule that every read of PII-adjacent content is logged with
// actor and timestamp.
func actorSubjectFromContext(ctx context.Context) string {
	v, _ := ctx.Value(actorSubjectContextKey).(string)
	return v
}

// withScope enforces crm.yaml's serviceAuth security scheme: every route
// requires a valid bearer token AND the specific scope that operation
// declares — there is no unauthenticated route on this service.
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
		ctx := context.WithValue(r.Context(), actorSubjectContextKey, claims.Subject)
		next(w, r.WithContext(ctx))
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
