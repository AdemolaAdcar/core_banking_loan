package api

import (
	"context"

	"github.com/AdemolaAdcar/core_banking_loan/services/crm/internal/auth"
)

// fakeValidator is a test double for auth.Validator. By default it
// accepts any non-empty token and grants every scope.
type fakeValidator struct {
	grantedScopes []string
	subject       string
	err           error
}

func allowAllValidator() *fakeValidator {
	return &fakeValidator{
		subject: "test-caller",
		grantedScopes: []string{
			auth.ScopeInteractionWrite, auth.ScopeCaseWrite, auth.ScopeCaseRead, auth.ScopeCustomer360Read,
			auth.ScopeRelationshipMgrWrite, auth.ScopeRelationshipMgrRead, auth.ScopeCommPrefsWrite, auth.ScopeCommPrefsRead,
		},
	}
}

func (f *fakeValidator) Validate(_ context.Context, rawToken string) (auth.Claims, error) {
	if f.err != nil {
		return auth.Claims{}, f.err
	}
	if rawToken == "" {
		return auth.Claims{}, auth.ErrInvalidToken
	}
	subject := f.subject
	if subject == "" {
		subject = "test-caller"
	}
	return auth.Claims{Subject: subject, Scopes: f.grantedScopes}, nil
}
