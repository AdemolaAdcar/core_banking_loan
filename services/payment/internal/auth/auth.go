// Package auth validates the OAuth2 client-credentials access tokens
// payment-execution.yaml's `serviceAuth` security scheme requires on
// every operation, and checks the token carries the scope that
// operation declares. Ported verbatim from services/las/internal/auth —
// only the scope constants below are specific to this service.
package auth

import (
	"context"
	"errors"
)

// Scope names match payment-execution.yaml's
// securitySchemes.serviceAuth.flows.clientCredentials.scopes exactly —
// do not add a scope here that isn't also declared there.
const (
	ScopeDisbursementWrite = "payment-execution:disbursement:write"
	ScopeDisbursementRead  = "payment-execution:disbursement:read"
)

var (
	// ErrMissingToken: no (or malformed) Authorization: Bearer header.
	ErrMissingToken = errors.New("auth: missing bearer token")
	// ErrInvalidToken: token present but fails signature/expiry/issuer/
	// audience validation. Deliberately one error for all of these
	// failure modes — a caller must not be able to distinguish "expired"
	// from "bad signature" from the HTTP response.
	ErrInvalidToken = errors.New("auth: invalid token")
)

// Claims is the subset of a validated access token's claims this service
// acts on.
type Claims struct {
	Subject string
	Scopes  []string
}

func (c Claims) HasScope(scope string) bool {
	for _, s := range c.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// Validator verifies a raw bearer token string and returns its claims.
type Validator interface {
	Validate(ctx context.Context, rawToken string) (Claims, error)
}
