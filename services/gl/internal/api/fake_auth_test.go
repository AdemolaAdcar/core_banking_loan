package api

import (
	"context"

	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/auth"
)

type fakeValidator struct {
	subject       string
	grantedScopes []string
	err           error
}

func allowAllValidator() *fakeValidator {
	return &fakeValidator{
		subject: "test-caller",
		grantedScopes: []string{
			auth.ScopeJournalEntryWrite, auth.ScopeJournalEntryRead, auth.ScopeAccountBalanceRead,
			auth.ScopeTrialBalanceRead, auth.ScopePeriodRead, auth.ScopePeriodWrite,
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
