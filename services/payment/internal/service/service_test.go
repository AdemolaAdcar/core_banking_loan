package service

import (
	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/accountclient"
	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/rails/sandbox"
)

func usd(amount int64) domain.Money { return domain.Money{Amount: amount, Currency: "USD"} }

func newTestService() (*Service, *fakeStore, *sandbox.Sandbox, *accountclient.Fake) {
	st := newFakeStore()
	rail := sandbox.New()
	account := accountclient.NewFake()
	return New(st, rail, account), st, rail, account
}
