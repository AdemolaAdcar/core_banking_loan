package accountclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/domain"
)

// HTTPClient is the production Client implementation: real HTTP calls to
// AccountAPI's receiveRepaymentNotification and reverseRepayment
// operations, exactly as
// specs/openapi/loan-account-subledger.yaml documents their wire shapes.
type HTTPClient struct {
	baseURL    string
	httpClient *http.Client
	tokens     TokenSource
}

func NewHTTPClient(baseURL string, tokens TokenSource) *HTTPClient {
	return &HTTPClient{baseURL: baseURL, httpClient: &http.Client{Timeout: 30 * time.Second}, tokens: tokens}
}

type moneyDTO struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

func toMoneyDTO(m domain.Money) moneyDTO { return moneyDTO{Amount: m.Amount, Currency: m.Currency} }

type receiveRepaymentNotificationRequestDTO struct {
	LoanAccountRef *string  `json:"loanAccountRef"`
	PayoffQuoteID  *string  `json:"payoffQuoteId"`
	Amount         moneyDTO `json:"amount"`
	Rail           string   `json:"rail"`
	ReceivedAt     string   `json:"receivedAt"`
}

// notificationResponseDTO is a superset covering repaymentDTO/payoffDTO/
// recoveryDTO's overlapping fields — enough to determine Kind and
// extract ID/status/journalEntryId without three separate unmarshal
// passes. AccountAPI's response is always exactly one of the three
// underlying resources; this struct never needs to represent an
// invalid combination because it's only ever populated by decoding one
// real response body.
type notificationResponseDTO struct {
	RepaymentID         string  `json:"repaymentId"`
	PayoffID            string  `json:"payoffId"`
	RecoveryID          string  `json:"recoveryId"`
	Status              string  `json:"status"`
	JournalEntryID      *string `json:"journalEntryId"`
	UnmatchedReasonCode *string `json:"unmatchedReasonCode"`
}

func (c *HTTPClient) ReceiveRepaymentNotification(ctx context.Context, in ReceiveRepaymentNotificationInput) (ReceiveRepaymentNotificationResult, error) {
	req := receiveRepaymentNotificationRequestDTO{
		LoanAccountRef: in.LoanAccountRef, PayoffQuoteID: in.PayoffQuoteID,
		Amount: toMoneyDTO(in.Amount), Rail: in.Rail, ReceivedAt: in.ReceivedAt.Format(time.RFC3339),
	}
	body, err := json.Marshal(req)
	if err != nil {
		return ReceiveRepaymentNotificationResult{}, fmt.Errorf("%w: marshaling request: %v", ErrAccountUnavailable, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/repayments:notify", bytes.NewReader(body))
	if err != nil {
		return ReceiveRepaymentNotificationResult{}, fmt.Errorf("%w: building request: %v", ErrAccountUnavailable, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Idempotency-Key", in.IdempotencyKey)
	if err := c.attachAuth(ctx, httpReq); err != nil {
		return ReceiveRepaymentNotificationResult{}, err
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return ReceiveRepaymentNotificationResult{}, fmt.Errorf("%w: %v", ErrAccountUnavailable, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return ReceiveRepaymentNotificationResult{}, fmt.Errorf("%w: reading response: %v", ErrAccountUnavailable, err)
	}

	switch resp.StatusCode {
	case http.StatusCreated, http.StatusOK:
		var dto notificationResponseDTO
		if err := json.Unmarshal(respBody, &dto); err != nil {
			return ReceiveRepaymentNotificationResult{}, fmt.Errorf("%w: decoding response: %v", ErrAccountUnavailable, err)
		}
		result := ReceiveRepaymentNotificationResult{Status: dto.Status, JournalEntryID: dto.JournalEntryID}
		switch {
		case dto.PayoffID != "":
			result.Kind, result.ID = KindPayoff, dto.PayoffID
		case dto.RecoveryID != "":
			result.Kind, result.ID = KindRecovery, dto.RecoveryID
		default:
			result.Kind, result.ID = KindRepayment, dto.RepaymentID
			result.Unmatched = dto.Status == "Unmatched"
		}
		return result, nil
	case http.StatusBadRequest:
		return ReceiveRepaymentNotificationResult{}, fmt.Errorf("%w: %s", ErrRequestRejected, decodeErrorMessage(respBody))
	case http.StatusNotFound:
		return ReceiveRepaymentNotificationResult{}, fmt.Errorf("%w: %s", ErrNotFound, decodeErrorMessage(respBody))
	case http.StatusConflict:
		return ReceiveRepaymentNotificationResult{}, fmt.Errorf("%w: %s", ErrConflict, decodeErrorMessage(respBody))
	default:
		return ReceiveRepaymentNotificationResult{}, fmt.Errorf("%w: unexpected status %d: %s", ErrAccountUnavailable, resp.StatusCode, decodeErrorMessage(respBody))
	}
}

type reverseRepaymentRequestDTO struct {
	ConfirmedBy            string  `json:"confirmedBy"`
	ReasonCode             string  `json:"reasonCode"`
	CorrectedLoanAccountID *string `json:"correctedLoanAccountId"`
}

type repaymentResponseDTO struct {
	Status         string  `json:"status"`
	JournalEntryID *string `json:"journalEntryId"`
}

func (c *HTTPClient) ReverseRepayment(ctx context.Context, in ReverseRepaymentInput) (ReverseRepaymentResult, error) {
	req := reverseRepaymentRequestDTO{ConfirmedBy: in.ConfirmedBy, ReasonCode: in.ReasonCode, CorrectedLoanAccountID: in.CorrectedLoanAccountID}
	body, err := json.Marshal(req)
	if err != nil {
		return ReverseRepaymentResult{}, fmt.Errorf("%w: marshaling request: %v", ErrAccountUnavailable, err)
	}

	url := c.baseURL + "/repayments/" + in.RepaymentID + ":reverse"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return ReverseRepaymentResult{}, fmt.Errorf("%w: building request: %v", ErrAccountUnavailable, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Idempotency-Key", in.IdempotencyKey)
	if err := c.attachAuth(ctx, httpReq); err != nil {
		return ReverseRepaymentResult{}, err
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return ReverseRepaymentResult{}, fmt.Errorf("%w: %v", ErrAccountUnavailable, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return ReverseRepaymentResult{}, fmt.Errorf("%w: reading response: %v", ErrAccountUnavailable, err)
	}

	switch resp.StatusCode {
	case http.StatusAccepted, http.StatusOK:
		var dto repaymentResponseDTO
		if err := json.Unmarshal(respBody, &dto); err != nil {
			return ReverseRepaymentResult{}, fmt.Errorf("%w: decoding response: %v", ErrAccountUnavailable, err)
		}
		return ReverseRepaymentResult{Status: dto.Status, JournalEntryID: dto.JournalEntryID}, nil
	case http.StatusBadRequest:
		return ReverseRepaymentResult{}, fmt.Errorf("%w: %s", ErrRequestRejected, decodeErrorMessage(respBody))
	case http.StatusNotFound:
		return ReverseRepaymentResult{}, fmt.Errorf("%w: %s", ErrNotFound, decodeErrorMessage(respBody))
	case http.StatusConflict:
		return ReverseRepaymentResult{}, fmt.Errorf("%w: %s", ErrConflict, decodeErrorMessage(respBody))
	default:
		return ReverseRepaymentResult{}, fmt.Errorf("%w: unexpected status %d: %s", ErrAccountUnavailable, resp.StatusCode, decodeErrorMessage(respBody))
	}
}

func (c *HTTPClient) attachAuth(ctx context.Context, req *http.Request) error {
	if c.tokens == nil {
		return nil
	}
	token, err := c.tokens.Token(ctx)
	if err != nil {
		return fmt.Errorf("%w: acquiring access token: %v", ErrAccountUnavailable, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

type errorDTO struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func decodeErrorMessage(body []byte) string {
	var e errorDTO
	if err := json.Unmarshal(body, &e); err != nil || e.Message == "" {
		return string(body)
	}
	return e.Code + ": " + e.Message
}
