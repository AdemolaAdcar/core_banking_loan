package ach

// PayoutAccountResolver resolves a party ID to the ACH-routable account
// a disbursement should be sent to. A stand-in for a real PartyAPI/
// vault-backed lookup — deferred integration, same "flagged, not
// silently invented" discipline every other service in this repo has
// applied to its own out-of-scope cross-service dependencies (e.g.
// services/las's PartyAPI/PaymentAPI deferrals). InMemoryPayoutDirectory
// below is the only implementation this pass provides.
type PayoutAccountResolver interface {
	Resolve(partyID string) (PayoutAccount, bool)
}

// PayoutAccount is the minimum a NACHA entry detail record needs to
// route a credit to a destination account.
type PayoutAccount struct {
	RoutingNumber string // 9 digits (ABA routing/transit number)
	AccountNumber string
	AccountName   string
	AccountType   AccountType
}

type AccountType string

const (
	Checking AccountType = "checking"
	Savings  AccountType = "savings"
)

// InMemoryPayoutDirectory is a fixed, in-process map — never a
// production PayoutAccountResolver, only a deterministic stand-in for
// this pass's tests and for local/sandbox running of
// cmd/payment-service.
type InMemoryPayoutDirectory map[string]PayoutAccount

func (d InMemoryPayoutDirectory) Resolve(partyID string) (PayoutAccount, bool) {
	a, ok := d[partyID]
	return a, ok
}
