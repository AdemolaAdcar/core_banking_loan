package accountclient

import (
	"context"
	"fmt"
	"sync"
)

// Fake is an in-memory Client double for internal/service's unit
// tests — it never performs I/O. Mirrors services/las/internal/glclient.Fake's
// shape: records every call, replays by IdempotencyKey, and lets tests
// preset both the next result and an error to inject.
type Fake struct {
	mu sync.Mutex

	NotifyCalls      []ReceiveRepaymentNotificationInput
	notifyByKey      map[string]ReceiveRepaymentNotificationResult
	NextNotifyResult *ReceiveRepaymentNotificationResult // if set, used instead of the default for the NEXT new (non-replay) call
	NextNotifyErr    error

	ReverseCalls      []ReverseRepaymentInput
	reverseByKey      map[string]ReverseRepaymentResult
	NextReverseResult *ReverseRepaymentResult
	NextReverseErr    error

	idCounter int
}

func NewFake() *Fake {
	return &Fake{notifyByKey: map[string]ReceiveRepaymentNotificationResult{}, reverseByKey: map[string]ReverseRepaymentResult{}}
}

func (f *Fake) ReceiveRepaymentNotification(_ context.Context, in ReceiveRepaymentNotificationInput) (ReceiveRepaymentNotificationResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.NotifyCalls = append(f.NotifyCalls, in)

	if existing, ok := f.notifyByKey[in.IdempotencyKey]; ok {
		return existing, nil // idempotent replay
	}

	if f.NextNotifyErr != nil {
		err := f.NextNotifyErr
		f.NextNotifyErr = nil
		return ReceiveRepaymentNotificationResult{}, err
	}

	f.idCounter++
	result := ReceiveRepaymentNotificationResult{Kind: KindRepayment, ID: fmt.Sprintf("repay-fake-%d", f.idCounter), Status: "Posted"}
	if f.NextNotifyResult != nil {
		result = *f.NextNotifyResult
		f.NextNotifyResult = nil
	}
	f.notifyByKey[in.IdempotencyKey] = result
	return result, nil
}

func (f *Fake) ReverseRepayment(_ context.Context, in ReverseRepaymentInput) (ReverseRepaymentResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.ReverseCalls = append(f.ReverseCalls, in)

	if existing, ok := f.reverseByKey[in.IdempotencyKey]; ok {
		return existing, nil // idempotent replay
	}

	if f.NextReverseErr != nil {
		err := f.NextReverseErr
		f.NextReverseErr = nil
		return ReverseRepaymentResult{}, err
	}

	result := ReverseRepaymentResult{Status: "Reversed"}
	if f.NextReverseResult != nil {
		result = *f.NextReverseResult
		f.NextReverseResult = nil
	}
	f.reverseByKey[in.IdempotencyKey] = result
	return result, nil
}

// CallCountForRepaymentID returns how many ReverseRepayment calls
// targeted the given repaymentId — mirrors glclient.Fake.CallCountForRule's
// "exactly one call" assertion pattern used throughout this repo.
func (f *Fake) CallCountForRepaymentID(repaymentID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.ReverseCalls {
		if c.RepaymentID == repaymentID {
			n++
		}
	}
	return n
}
