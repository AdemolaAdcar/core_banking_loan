package api

import (
	"context"
	"time"

	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/outbox"
	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/store"
)

// fakeStore is a minimal in-memory store.Store/store.Tx double for
// exercising the HTTP layer without a live Postgres instance.
type fakeStore struct {
	entries         map[string]domain.JournalEntry
	entryOrder      []string
	bySourceEventID map[string]string
	periods         map[string]domain.Period
	idempotencyKeys map[string][]byte
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		entries:         map[string]domain.JournalEntry{},
		bySourceEventID: map[string]string{},
		periods:         map[string]domain.Period{},
		idempotencyKeys: map[string][]byte{},
	}
}

func (f *fakeStore) WithinTx(_ context.Context, fn func(store.Tx) error) error {
	return fn(f)
}

func (f *fakeStore) GetJournalEntry(_ context.Context, id string) (domain.JournalEntry, error) {
	e, ok := f.entries[id]
	if !ok {
		return domain.JournalEntry{}, store.ErrNotFound
	}
	return e, nil
}

func (f *fakeStore) FindBySourceEventID(_ context.Context, sourceEventID string) (domain.JournalEntry, bool, error) {
	id, ok := f.bySourceEventID[sourceEventID]
	if !ok {
		return domain.JournalEntry{}, false, nil
	}
	return f.entries[id], true, nil
}

func (f *fakeStore) GetLatestRunningBalance(_ context.Context, loanAccountID, glAccount, currency string) (domain.Money, error) {
	for i := len(f.entryOrder) - 1; i >= 0; i-- {
		e := f.entries[f.entryOrder[i]]
		if e.LoanAccountID != loanAccountID {
			continue
		}
		for j := len(e.Lines) - 1; j >= 0; j-- {
			if e.Lines[j].GLAccount == glAccount {
				return e.Lines[j].RunningBalanceAfter, nil
			}
		}
	}
	return domain.Money{Amount: 0, Currency: currency}, nil
}

func (f *fakeStore) GetTrialBalance(_ context.Context, asOf time.Time) ([]store.TrialBalanceLine, error) {
	totals := map[string]*store.TrialBalanceLine{}
	for _, e := range f.entries {
		if e.PostedAt.After(asOf) {
			continue
		}
		for _, l := range e.Lines {
			t, ok := totals[l.GLAccount]
			if !ok {
				t = &store.TrialBalanceLine{GLAccount: l.GLAccount, Currency: l.Amount.Currency}
				totals[l.GLAccount] = t
			}
			if l.Direction == domain.Debit {
				t.DebitTotal += l.Amount.Amount
			} else {
				t.CreditTotal += l.Amount.Amount
			}
		}
	}
	var out []store.TrialBalanceLine
	for _, t := range totals {
		out = append(out, *t)
	}
	return out, nil
}

func (f *fakeStore) GetStatementOfAccount(_ context.Context, loanAccountID string, asOf time.Time) ([]store.StatementLine, error) {
	var out []store.StatementLine
	for _, id := range f.entryOrder {
		e := f.entries[id]
		if e.LoanAccountID != loanAccountID || e.PostedAt.After(asOf) {
			continue
		}
		for _, l := range e.Lines {
			out = append(out, store.StatementLine{
				JournalEntryID: e.ID, PostedAt: e.PostedAt, PostingRuleCode: e.PostingRuleCode,
				GLAccount: l.GLAccount, Direction: l.Direction, Amount: l.Amount, RunningBalanceAfter: l.RunningBalanceAfter,
			})
		}
	}
	return out, nil
}

func (f *fakeStore) GetAccountBalance(_ context.Context, glAccountCode string, asOf time.Time) (domain.Money, error) {
	var debit, credit int64
	currency := "USD"
	for _, e := range f.entries {
		if e.PostedAt.After(asOf) {
			continue
		}
		for _, l := range e.Lines {
			if l.GLAccount != glAccountCode {
				continue
			}
			currency = l.Amount.Currency
			if l.Direction == domain.Debit {
				debit += l.Amount.Amount
			} else {
				credit += l.Amount.Amount
			}
		}
	}
	return domain.Money{Amount: debit - credit, Currency: currency}, nil
}

func (f *fakeStore) GetPeriod(_ context.Context, periodID string) (domain.Period, error) {
	p, ok := f.periods[periodID]
	if !ok {
		return domain.Period{ID: periodID, Status: domain.PeriodOpen}, nil
	}
	return p, nil
}

func (f *fakeStore) EarliestOpenPeriodBefore(_ context.Context, periodID string) (string, bool, error) {
	periodsWithData := map[string]bool{}
	for _, e := range f.entries {
		if e.PeriodID < periodID {
			periodsWithData[e.PeriodID] = true
		}
	}
	earliest, found := "", false
	for pid := range periodsWithData {
		p := f.periods[pid]
		if p.Status == domain.PeriodClosed {
			continue
		}
		if !found || pid < earliest {
			earliest, found = pid, true
		}
	}
	return earliest, found, nil
}

func (f *fakeStore) GetIdempotentResponse(_ context.Context, key string) (bool, []byte, error) {
	b, ok := f.idempotencyKeys[key]
	if !ok {
		return false, nil, nil
	}
	return true, b, nil
}

func (f *fakeStore) ListUnpublished(_ context.Context, _ int) ([]outbox.Entry, error) {
	return nil, nil
}
func (f *fakeStore) MarkPublished(_ context.Context, _ []string) error { return nil }

func (f *fakeStore) CreateJournalEntry(_ context.Context, e domain.JournalEntry) error {
	if _, exists := f.bySourceEventID[e.SourceEventID]; exists {
		return store.ErrDuplicateSourceEventID
	}
	f.entries[e.ID] = e
	f.entryOrder = append(f.entryOrder, e.ID)
	f.bySourceEventID[e.SourceEventID] = e.ID
	return nil
}

func (f *fakeStore) ClosePeriod(_ context.Context, p domain.Period) error {
	f.periods[p.ID] = p
	return nil
}

func (f *fakeStore) SaveIdempotentResponse(_ context.Context, key, _ string, responseJSON []byte) error {
	f.idempotencyKeys[key] = responseJSON
	return nil
}

func (f *fakeStore) InsertOutboxEntry(_ context.Context, _ outbox.Entry) error { return nil }
