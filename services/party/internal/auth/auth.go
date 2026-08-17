// Package auth validates the OAuth2 client-credentials access tokens
// party-cif.yaml's `serviceAuth` security scheme requires on every
// operation, and checks the token carries the scope that operation
// declares (party:read or party:write). Nothing in internal/api trusts a
// request until it has passed through here.
package auth

import (
	"context"
	"errors"
)

// Scope names match party-cif.yaml's securitySchemes.serviceAuth.flows
// exactly — do not add a scope here that isn't also declared there.
const (
	ScopePartyRead  = "party:read"
	ScopePartyWrite = "party:write"
)

var (
	// ErrMissingToken: no (or malformed) Authorization: Bearer header.
	ErrMissingToken = errors.New("auth: missing bearer token")
	// ErrInvalidToken: token present but fails signature/expiry/issuer/
	// audience validation. Deliberately one error for all of these
	// failure modes — a caller must not be able to distinguish "expired"
	// from "bad signature" from the HTTP response, which would leak
	// validation internals to an attacker probing the endpoint.
	ErrInvalidToken = errors.New("auth: invalid token")
)

// Claims is the subset of a validated access token's claims this service
// acts on.
type Claims struct {
	Subject string
	Scopes  []string
}

// HasScope reports whether the token carries the given scope. There is
// no notion of a wildcard or admin scope that implies others — every
// operation checks for its own exact scope.
func (c Claims) HasScope(scope string) bool {
	for _, s := range c.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// Validator verifies a raw bearer token string and returns its claims.
// A narrow interface, deliberately: the JWKS-backed RS256 implementation
// (jwks.go) is the production path, but anything satisfying this
// interface — including a fixed-claims test double — can stand in for
// it, the same pattern internal/pii.Encryptor already establishes.
type Validator interface {
	Validate(ctx context.Context, rawToken string) (Claims, error)
}
