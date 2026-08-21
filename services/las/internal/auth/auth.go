// Package auth validates the OAuth2 client-credentials access tokens
// loan-account-subledger.yaml's `serviceAuth` security scheme requires
// on every operation, and checks the token carries the scope that
// operation declares. Ported verbatim from services/gl/internal/auth —
// only the scope constants below are specific to this service.
package auth

import (
	"context"
	"errors"
)

// Scope names match loan-account-subledger.yaml's
// securitySchemes.serviceAuth.flows.clientCredentials.scopes exactly —
// do not add a scope here that isn't also declared there.
const (
	ScopeBookingWrite      = "loan-subledger:booking:write"
	ScopeBookingRead       = "loan-subledger:booking:read"
	ScopeDisbursementWrite = "loan-subledger:disbursement:write"
	ScopeDisbursementRead  = "loan-subledger:disbursement:read"
	ScopeAccrualWrite      = "loan-subledger:accrual:write"
	ScopeAccrualRead       = "loan-subledger:accrual:read"
	ScopeRepaymentWrite    = "loan-subledger:repayment:write"
	ScopeRepaymentRead     = "loan-subledger:repayment:read"
	ScopeDelinquencyWrite  = "loan-subledger:delinquency:write"
	ScopeDelinquencyRead   = "loan-subledger:delinquency:read"
	ScopePayoffRead        = "loan-subledger:payoff:read"
	ScopeChargeoffWrite    = "loan-subledger:chargeoff:write"
	ScopeChargeoffRead     = "loan-subledger:chargeoff:read"
	ScopeModificationWrite = "loan-subledger:modification:write"
	ScopeModificationRead  = "loan-subledger:modification:read"
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
