package service

import (
	"context"
	"sync"
	"time"

	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/outbox"
	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/store"
)

// fakeStore is an in-memory store.Store/store.Tx double for this
// package's unit tests -- no I/O, mutex-guarded so tests can run with
// -race.
type fakeStore struct {
	mu sync.Mutex

	accounts      map[string]domain.LoanAccount
	termVersions  map[string][]domain.TermVersion
	projections   map[string]domain.BalanceProjection
	disbursements map[string]domain.Disbursement
	repayments    map[string]domain.Repayment
	payoffs       map[string]domain.Payoff
	chargeoffs    map[string]domain.ChargeOff
	recoveries    map[string]domain.Recovery
	modifications map[string]domain.Modification
	fees          map[string]domain.Fee
	outboxEntries []outbox.Entry
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		accounts: map[string]domain.LoanAccount{}, termVersions: map[string][]domain.TermVersion{}, projections: map[string]domain.BalanceProjection{},
		disbursements: map[string]domain.Disbursement{}, repayments: map[string]domain.Repayment{}, payoffs: map[string]domain.Payoff{},
		chargeoffs: map[string]domain.ChargeOff{}, recoveries: map[string]domain.Recovery{}, modifications: map[string]domain.Modification{}, fees: map[string]domain.Fee{},
	}
}

func (f *fakeStore) WithinTx(ctx context.Context, fn func(store.Tx) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fn(&fakeTx{f})
}

type fakeTx struct{ f *fakeStore }

func (t *fakeTx) InsertOutboxEntry(ctx context.Context, e outbox.Entry) error {
	t.f.outboxEntries = append(t.f.outboxEntries, e)
	return nil
}
func (t *fakeTx) SaveLoanAccount(ctx context.Context, a domain.LoanAccount) error {
	t.f.accounts[a.LoanAccountID] = a
	return nil
}
func (t *fakeTx) SaveTermVersion(ctx context.Context, loanAccountID string, v domain.TermVersion) error {
	t.f.termVersions[loanAccountID] = append(t.f.termVersions[loanAccountID], v)
	return nil
}
func (t *fakeTx) SaveBalanceProjection(ctx context.Context, p domain.BalanceProjection) error {
	t.f.projections[p.LoanAccountID] = p
	return nil
}
func (t *fakeTx) SaveDisbursement(ctx context.Context, d domain.Disbursement) error {
	t.f.disbursements[d.DisbursementID] = d
	return nil
}
func (t *fakeTx) SaveRepayment(ctx context.Context, r domain.Repayment) error {
	t.f.repayments[r.RepaymentID] = r
	return nil
}
func (t *fakeTx) SavePayoff(ctx context.Context, p domain.Payoff) error {
	t.f.payoffs[p.PayoffID] = p
	return nil
}
func (t *fakeTx) SaveChargeOff(ctx context.Context, c domain.ChargeOff) error {
	t.f.chargeoffs[c.ChargeoffID] = c
	return nil
}
func (t *fakeTx) SaveRecovery(ctx context.Context, r domain.Recovery) error {
	t.f.recoveries[r.RecoveryID] = r
	return nil
}
func (t *fakeTx) SaveModification(ctx context.Context, m domain.Modification) error {
	t.f.modifications[m.ModificationID] = m
	return nil
}
func (t *fakeTx) SaveFee(ctx context.Context, fee domain.Fee) error {
	t.f.fees[fee.FeeID] = fee
	return nil
}
func (t *fakeTx) SaveIdempotentResponse(ctx context.Context, idempotencyKey, requestHash string, responseJSON []byte) error {
	return nil
}

func (f *fakeStore) GetLoanAccount(ctx context.Context, id string) (domain.LoanAccount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.accounts[id]
	if !ok {
		return domain.LoanAccount{}, store.ErrNotFound
	}
	return a, nil
}

func (f *fakeStore) FindLoanAccountByApprovalReference(ctx context.Context, ref string) (domain.LoanAccount, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.accounts {
		if a.ApprovalReferenceID == ref {
			return a, true, nil
		}
	}
	return domain.LoanAccount{}, false, nil
}

func (f *fakeStore) ListTermVersions(ctx context.Context, loanAccountID string) ([]domain.TermVersion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.termVersions[loanAccountID], nil
}

func (f *fakeStore) GetEffectiveTermVersion(ctx context.Context, loanAccountID string, asOf time.Time) (domain.TermVersion, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var best domain.TermVersion
	found := false
	for _, v := range f.termVersions[loanAccountID] {
		if v.EffectiveDate.After(asOf) {
			continue
		}
		if !found || v.EffectiveDate.After(best.EffectiveDate) || (v.EffectiveDate.Equal(best.EffectiveDate) && v.Version > best.Version) {
			best = v
			found = true
		}
	}
	return best, found, nil
}

func (f *fakeStore) GetBalanceProjection(ctx context.Context, loanAccountID string) (domain.BalanceProjection, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.projections[loanAccountID]
	return p, ok, nil
}

func (f *fakeStore) GetDisbursement(ctx context.Context, id string) (domain.Disbursement, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.disbursements[id]
	if !ok {
		return domain.Disbursement{}, store.ErrNotFound
	}
	return d, nil
}

func (f *fakeStore) ListDisbursementsByLoanAccount(ctx context.Context, loanAccountID string) ([]domain.Disbursement, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.Disbursement
	for _, d := range f.disbursements {
		if d.LoanAccountID == loanAccountID {
			out = append(out, d)
		}
	}
	return out, nil
}

func (f *fakeStore) GetRepayment(ctx context.Context, id string) (domain.Repayment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.repayments[id]
	if !ok {
		return domain.Repayment{}, store.ErrNotFound
	}
	return r, nil
}

func (f *fakeStore) GetPayoff(ctx context.Context, id string) (domain.Payoff, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.payoffs[id]
	if !ok {
		return domain.Payoff{}, store.ErrNotFound
	}
	return p, nil
}

func (f *fakeStore) GetChargeOff(ctx context.Context, id string) (domain.ChargeOff, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.chargeoffs[id]
	if !ok {
		return domain.ChargeOff{}, store.ErrNotFound
	}
	return c, nil
}

func (f *fakeStore) FindChargeOffByLoanAccount(ctx context.Context, loanAccountID string) (domain.ChargeOff, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.chargeoffs {
		if c.LoanAccountID == loanAccountID {
			return c, true, nil
		}
	}
	return domain.ChargeOff{}, false, nil
}

func (f *fakeStore) GetRecovery(ctx context.Context, id string) (domain.Recovery, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.recoveries[id]
	if !ok {
		return domain.Recovery{}, store.ErrNotFound
	}
	return r, nil
}

func (f *fakeStore) GetModification(ctx context.Context, id string) (domain.Modification, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.modifications[id]
	if !ok {
		return domain.Modification{}, store.ErrNotFound
	}
	return m, nil
}

func (f *fakeStore) GetFee(ctx context.Context, id string) (domain.Fee, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fee, ok := f.fees[id]
	if !ok {
		return domain.Fee{}, store.ErrNotFound
	}
	return fee, nil
}

func (f *fakeStore) ListAccrualEligibleAccounts(ctx context.Context, asOf time.Time) ([]domain.LoanAccount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.LoanAccount
	for _, a := range f.accounts {
		if a.Status == domain.StatusDisbursed && !a.NonAccrualFlag {
			out = append(out, a)
		}
	}
	return out, nil
}

func (f *fakeStore) ListPastDueAccounts(ctx context.Context, asOf time.Time) ([]domain.LoanAccount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.LoanAccount
	for _, a := range f.accounts {
		if a.Status == domain.StatusDisbursed && a.DPDBucket != domain.DPDCurrent {
			out = append(out, a)
		}
	}
	return out, nil
}

func (f *fakeStore) ListAllDisbursedOrChargedOff(ctx context.Context) ([]domain.LoanAccount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.LoanAccount
	for _, a := range f.accounts {
		if a.Status == domain.StatusDisbursed || a.Status == domain.StatusChargedOff {
			out = append(out, a)
		}
	}
	return out, nil
}

func (f *fakeStore) GetIdempotentResponse(ctx context.Context, key string) (bool, []byte, error) {
	return false, nil, nil
}

func (f *fakeStore) ListUnpublished(ctx context.Context, limit int) ([]outbox.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.outboxEntries, nil
}

func (f *fakeStore) MarkPublished(ctx context.Context, ids []string) error { return nil }
