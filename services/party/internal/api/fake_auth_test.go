package api

import (
	"context"

	"github.com/AdemolaAdcar/core_banking_loan/services/party/internal/auth"
)

// fakeValidator is a test double for auth.Validator. By default it
// accepts any non-empty token and grants every scope, so handler tests
// unrelated to auth itself don't need to think about tokens. Tests that
// exercise auth specifically construct their own fakeValidator with a
// narrower grantedScopes set or a forced error.
type fakeValidator struct {
	grantedScopes []string
	err           error
}

func allowAllValidator() *fakeValidator {
	return &fakeValidator{grantedScopes: []string{auth.ScopePartyRead, auth.ScopePartyWrite}}
}

func (f *fakeValidator) Validate(_ context.Context, rawToken string) (auth.Claims, error) {
	if f.err != nil {
		return auth.Claims{}, f.err
	}
	if rawToken == "" {
		return auth.Claims{}, auth.ErrInvalidToken
	}
	return auth.Claims{Subject: "test-caller", Scopes: f.grantedScopes}, nil
}
