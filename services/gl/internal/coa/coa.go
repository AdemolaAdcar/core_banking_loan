// Package coa loads and validates the Chart of Accounts. chart-of-accounts.json
// in this directory is a build-time-embedded COPY of
// specs/coa/chart-of-accounts.json — the canonical, human-reviewable
// source of truth. go:embed cannot reach outside this module's directory
// tree, so this copy must be kept in sync by hand whenever the canonical
// manifest changes; TestManifestMatchesCanonicalSource in coa_test.go
// fails loudly (by comparing this embedded copy byte-for-byte against
// ../../../../specs/coa/chart-of-accounts.json) if the two ever drift,
// which is the best guard available without a build-time codegen step.
package coa

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
)

//go:embed chart-of-accounts.json
var manifestJSON []byte

type AccountType string

const (
	AccountTypeAsset     AccountType = "ASSET"
	AccountTypeLiability AccountType = "LIABILITY"
	AccountTypeEquity    AccountType = "EQUITY"
	AccountTypeIncome    AccountType = "INCOME"
	AccountTypeExpense   AccountType = "EXPENSE"
)

type NormalBalance string

const (
	NormalBalanceDebit  NormalBalance = "DEBIT"
	NormalBalanceCredit NormalBalance = "CREDIT"
)

// Named account-code constants, so posting rules never scatter magic
// strings — each matches specs/coa/chart-of-accounts.json exactly.
const (
	CashNostro             = "1010"
	LoanReceivable         = "1200"
	InterestReceivable     = "1300"
	FeeReceivable          = "1400"
	AllowanceForLoanLosses = "1900"
	InterestIncome         = "4100"
	FeeIncome              = "4200"
	RecoveryIncome         = "4300"
)

type Account struct {
	Code          string        `json:"code"`
	Name          string        `json:"name"`
	Type          AccountType   `json:"type"`
	NormalBalance NormalBalance `json:"normalBalance"`
	ContraAsset   bool          `json:"contraAsset"`
	Description   string        `json:"description"`
}

type manifest struct {
	Version  string    `json:"version"`
	Accounts []Account `json:"accounts"`
}

// Chart is the loaded, validated Chart of Accounts.
type Chart struct {
	Version  string
	accounts map[string]Account
}

// Load parses and validates the embedded manifest. Called once at
// process startup (see cmd/gl-service/main.go); callers hold the
// returned *Chart for the life of the process — there is no reload path,
// consistent with a chart of accounts being reviewed/approved config,
// not runtime-mutable data.
func Load() (*Chart, error) {
	var m manifest
	if err := json.Unmarshal(manifestJSON, &m); err != nil {
		return nil, fmt.Errorf("coa: parsing manifest: %w", err)
	}
	if len(m.Accounts) == 0 {
		return nil, fmt.Errorf("coa: manifest has no accounts")
	}
	accounts := make(map[string]Account, len(m.Accounts))
	for _, a := range m.Accounts {
		if a.Code == "" {
			return nil, fmt.Errorf("coa: account with empty code")
		}
		if _, dup := accounts[a.Code]; dup {
			return nil, fmt.Errorf("coa: duplicate account code %q", a.Code)
		}
		switch a.NormalBalance {
		case NormalBalanceDebit, NormalBalanceCredit:
		default:
			return nil, fmt.Errorf("coa: account %q has invalid normalBalance %q", a.Code, a.NormalBalance)
		}
		accounts[a.Code] = a
	}
	return &Chart{Version: m.Version, accounts: accounts}, nil
}

// MustLoad panics on a malformed manifest -- used at package-init time in
// contexts (like posting-rule construction) that treat the manifest as
// build-time-fixed data, not something that can fail at request time.
func MustLoad() *Chart {
	c, err := Load()
	if err != nil {
		panic(err)
	}
	return c
}

// Lookup returns the account for a code, or false if the code is unknown.
func (c *Chart) Lookup(code string) (Account, bool) {
	a, ok := c.accounts[code]
	return a, ok
}

// IsValidCode reports whether code is a known account.
func (c *Chart) IsValidCode(code string) bool {
	_, ok := c.accounts[code]
	return ok
}

// Accounts returns every account, sorted by code, for read endpoints
// that want to enumerate the full chart.
func (c *Chart) Accounts() []Account {
	out := make([]Account, 0, len(c.accounts))
	for _, a := range c.accounts {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}
