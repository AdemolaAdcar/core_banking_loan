package coa

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_AllEightKnownAccounts(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]struct {
		typ           AccountType
		normalBalance NormalBalance
		contraAsset   bool
	}{
		CashNostro:             {AccountTypeAsset, NormalBalanceDebit, false},
		LoanReceivable:         {AccountTypeAsset, NormalBalanceDebit, false},
		InterestReceivable:     {AccountTypeAsset, NormalBalanceDebit, false},
		FeeReceivable:          {AccountTypeAsset, NormalBalanceDebit, false},
		AllowanceForLoanLosses: {AccountTypeAsset, NormalBalanceCredit, true},
		InterestIncome:         {AccountTypeIncome, NormalBalanceCredit, false},
		FeeIncome:              {AccountTypeIncome, NormalBalanceCredit, false},
		RecoveryIncome:         {AccountTypeIncome, NormalBalanceCredit, false},
	}
	if len(want) != len(c.accounts) {
		t.Fatalf("expected exactly %d accounts, got %d", len(want), len(c.accounts))
	}
	for code, w := range want {
		a, ok := c.Lookup(code)
		if !ok {
			t.Fatalf("expected account %s to exist", code)
		}
		if a.Type != w.typ || a.NormalBalance != w.normalBalance || a.ContraAsset != w.contraAsset {
			t.Fatalf("account %s: got %+v, want type=%s normalBalance=%s contraAsset=%v", code, a, w.typ, w.normalBalance, w.contraAsset)
		}
	}
}

func TestIsValidCode(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !c.IsValidCode(CashNostro) {
		t.Fatalf("expected %s to be valid", CashNostro)
	}
	if c.IsValidCode("9999") {
		t.Fatalf("expected an unknown code to be invalid")
	}
}

func TestAllowanceForLoanLosses_IsTheOnlyContraAsset(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, a := range c.Accounts() {
		if a.ContraAsset && a.Code != AllowanceForLoanLosses {
			t.Fatalf("expected only %s to be a contra-asset, but %s is also marked contraAsset", AllowanceForLoanLosses, a.Code)
		}
	}
	a, _ := c.Lookup(AllowanceForLoanLosses)
	if !a.ContraAsset {
		t.Fatalf("expected AllowanceForLoanLosses to be marked contraAsset")
	}
	if a.NormalBalance != NormalBalanceCredit {
		t.Fatalf("expected AllowanceForLoanLosses' normal balance to be CREDIT (opposite every other 1xxx asset), got %s", a.NormalBalance)
	}
}

// TestManifestMatchesCanonicalSource guards against the embedded copy in
// this package drifting from specs/coa/chart-of-accounts.json -- the
// canonical, human-reviewed source of truth (see coa.go's package doc
// comment for why a copy exists at all: go:embed cannot reach outside
// this module).
func TestManifestMatchesCanonicalSource(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatalf("resolving repo root: %v", err)
	}
	canonicalPath := filepath.Join(repoRoot, "specs", "coa", "chart-of-accounts.json")
	canonical, err := os.ReadFile(canonicalPath)
	if err != nil {
		t.Skipf("canonical manifest not found at %s (expected when this module is vendored/tested outside the monorepo checkout): %v", canonicalPath, err)
	}
	if string(canonical) != string(manifestJSON) {
		t.Fatalf("services/gl/internal/coa/chart-of-accounts.json has drifted from the canonical %s -- copy the canonical file over the embedded one", canonicalPath)
	}
}
