package service

import (
	"context"
	"sync"
	"time"

	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/outbox"
	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/store"
)

// fakeStore is an in-memory store.Store/store.Tx double used only in
// tests. Unlike the fakes in services/party and services/crm, this one
// is guarded by a mutex -- the concurrent-posting-to-the-same-account
// tests in service_test.go run real goroutines against it under `go
// test -race`, and a plain map would be a genuine data race even though
// it's just a test double.
type fakeStore struct {
	mu              sync.Mutex
	entries         map[string]domain.JournalEntry // by ID
	entryOrder      []string                       // IDs in creation order, for "latest" tie-breaking
	bySourceEventID map[string]string              // sourceEventID -> entry ID
	periods         map[string]domain.Period
	outboxEntries   []outbox.Entry
	idempotencyKeys map[string][]byte

	// failNextCreateWithDuplicate simulates the concurrent-idempotency-
	// race backstop: the next CreateJournalEntry call returns
	// store.ErrDuplicateSourceEventID instead of actually writing.
	failNextCreateWithDuplicate bool
	// failNextOutboxInsert simulates a failure partway through a
	// transaction, to prove atomicity (invariant 2): the entry that was
	// about to be created must NOT appear after WithinTx returns an
	// error.
	failNextOutboxInsert bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		entries:         map[string]domain.JournalEntry{},
		bySourceEventID: map[string]string{},
		periods:         map[string]domain.Period{},
		idempotencyKeys: map[string][]byte{},
	}
}

func (f *fakeStore) WithinTx(ctx context.Context, fn func(store.Tx) error) error {
	f.mu.Lock()
	snapEntries := cloneEntries(f.entries)
	snapOrder := append([]string(nil), f.entryOrder...)
	snapBySource := cloneStringMap(f.bySourceEventID)
	snapPeriods := clonePeriods(f.periods)
	snapOutbox := append([]outbox.Entry(nil), f.outboxEntries...)
	f.mu.Unlock()

	if err := fn(f); err != nil {
		f.mu.Lock()
		f.entries = snapEntries
		f.entryOrder = snapOrder
		f.bySourceEventID = snapBySource
		f.periods = snapPeriods
		f.outboxEntries = snapOutbox
		f.mu.Unlock()
		return err
	}
	return nil
}

func cloneEntries(m map[string]domain.JournalEntry) map[string]domain.JournalEntry {
	out := make(map[string]domain.JournalEntry, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
func cloneStringMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
func clonePeriods(m map[string]domain.Period) map[string]domain.Period {
	out := make(map[string]domain.Period, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// --- Store read methods --------------------------------------------------

func (f *fakeStore) GetJournalEntry(_ context.Context, id string) (domain.JournalEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.entries[id]
	if !ok {
		return domain.JournalEntry{}, store.ErrNotFound
	}
	return e, nil
}

func (f *fakeStore) FindBySourceEventID(_ context.Context, sourceEventID string) (domain.JournalEntry, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.bySourceEventID[sourceEventID]
	if !ok {
		return domain.JournalEntry{}, false, nil
	}
	return f.entries[id], true, nil
}

func (f *fakeStore) GetLatestRunningBalance(_ context.Context, loanAccountID, glAccount, currency string) (domain.Money, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
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
	f.mu.Lock()
	defer f.mu.Unlock()
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
	f.mu.Lock()
	defer f.mu.Unlock()
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
	f.mu.Lock()
	defer f.mu.Unlock()
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
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.periods[periodID]
	if !ok {
		return domain.Period{ID: periodID, Status: domain.PeriodOpen}, nil
	}
	return p, nil
}

func (f *fakeStore) EarliestOpenPeriodBefore(_ context.Context, periodID string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	periodsWithData := map[string]bool{}
	for _, e := range f.entries {
		if e.PeriodID < periodID {
			periodsWithData[e.PeriodID] = true
		}
	}
	earliest := ""
	found := false
	for pid := range periodsWithData {
		p := f.periods[pid]
		if p.Status == domain.PeriodClosed {
			continue
		}
		if !found || pid < earliest {
			earliest = pid
			found = true
		}
	}
	return earliest, found, nil
}

func (f *fakeStore) GetIdempotentResponse(_ context.Context, key string) (bool, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.idempotencyKeys[key]
	if !ok {
		return false, nil, nil
	}
	return true, b, nil
}

func (f *fakeStore) ListUnpublished(_ context.Context, limit int) ([]outbox.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.outboxEntries) > limit {
		return f.outboxEntries[:limit], nil
	}
	return f.outboxEntries, nil
}

func (f *fakeStore) MarkPublished(_ context.Context, _ []string) error { return nil }

// --- Tx write methods ------------------------------------------------

func (f *fakeStore) CreateJournalEntry(_ context.Context, e domain.JournalEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNextCreateWithDuplicate {
		f.failNextCreateWithDuplicate = false
		return store.ErrDuplicateSourceEventID
	}
	if _, exists := f.bySourceEventID[e.SourceEventID]; exists {
		return store.ErrDuplicateSourceEventID
	}
	f.entries[e.ID] = e
	f.entryOrder = append(f.entryOrder, e.ID)
	f.bySourceEventID[e.SourceEventID] = e.ID
	return nil
}

func (f *fakeStore) ClosePeriod(_ context.Context, p domain.Period) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.periods[p.ID] = p
	return nil
}

func (f *fakeStore) SaveIdempotentResponse(_ context.Context, key, _ string, responseJSON []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.idempotencyKeys[key] = responseJSON
	return nil
}

func (f *fakeStore) InsertOutboxEntry(_ context.Context, e outbox.Entry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNextOutboxInsert {
		f.failNextOutboxInsert = false
		return errSimulatedOutboxFailure
	}
	f.outboxEntries = append(f.outboxEntries, e)
	return nil
}

var errSimulatedOutboxFailure = &simulatedError{"simulated outbox insert failure"}

type simulatedError struct{ msg string }

func (e *simulatedError) Error() string { return e.msg }
