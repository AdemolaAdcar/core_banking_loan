package glclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/domain"
)

// TokenSource supplies the bearer token this client attaches to every
// GLPostingAPI call — the client-credentials side of the same OAuth2
// grant every service in this system already validates on the receiving
// end (internal/auth). Kept as a narrow interface so tests can supply a
// fixed token without standing up a real authorization server.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// HTTPClient is the production Client implementation: real HTTP calls
// to GLPostingAPI's postJournalEntry and getStatementOfAccount
// operations, exactly as specs/openapi/gl-posting-engine.yaml documents
// their wire shapes.
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
func fromMoneyDTO(m moneyDTO) domain.Money {
	return domain.Money{Amount: m.Amount, Currency: m.Currency}
}

type allocationDTO struct {
	FeeAmount       moneyDTO `json:"feeAmount"`
	InterestAmount  moneyDTO `json:"interestAmount"`
	PrincipalAmount moneyDTO `json:"principalAmount"`
}

type capitalizationDTO struct {
	InterestAmount moneyDTO `json:"interestAmount"`
	FeeAmount      moneyDTO `json:"feeAmount"`
}

// postJournalEntryRequestDTO mirrors GL's postJournalEntryRequest DTO
// (services/gl/internal/api/dto.go) field-for-field.
type postJournalEntryRequestDTO struct {
	PostingRuleCode         string             `json:"postingRuleCode"`
	LoanAccountID           string             `json:"loanAccountId"`
	Amount                  *moneyDTO          `json:"amount"`
	Allocation              *allocationDTO     `json:"allocation"`
	Capitalization          *capitalizationDTO `json:"capitalization"`
	ReversalOfSourceEventID *string            `json:"reversalOfSourceEventId"`
	Metadata                map[string]any     `json:"metadata"`
}

type lineDTO struct {
	GLAccount           string   `json:"glAccount"`
	Direction           string   `json:"direction"`
	Amount              moneyDTO `json:"amount"`
	RunningBalanceAfter moneyDTO `json:"runningBalanceAfter"`
}

type journalEntryDTO struct {
	JournalEntryID     string    `json:"journalEntryId"`
	SourceEventID      string    `json:"sourceEventId"`
	PostingRuleCode    string    `json:"postingRuleCode"`
	PostingRuleVersion string    `json:"postingRuleVersion"`
	LoanAccountID      string    `json:"loanAccountId"`
	Lines              []lineDTO `json:"lines"`
	Balanced           bool      `json:"balanced"`
	Immutable          bool      `json:"immutable"`
	PostedAt           string    `json:"postedAt"`
	PeriodID           string    `json:"periodId"`
}

type errorDTO struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (c *HTTPClient) PostJournalEntry(ctx context.Context, in PostJournalEntryInput) (JournalEntry, error) {
	req := postJournalEntryRequestDTO{
		PostingRuleCode:         string(in.PostingRuleCode),
		LoanAccountID:           in.LoanAccountID,
		ReversalOfSourceEventID: in.ReversalOfSourceEventID,
		Metadata:                in.Metadata,
	}
	if in.Amount != nil {
		m := toMoneyDTO(*in.Amount)
		req.Amount = &m
	}
	if in.Allocation != nil {
		req.Allocation = &allocationDTO{
			FeeAmount: toMoneyDTO(in.Allocation.FeeAmount), InterestAmount: toMoneyDTO(in.Allocation.InterestAmount), PrincipalAmount: toMoneyDTO(in.Allocation.PrincipalAmount),
		}
	}
	if in.Capitalization != nil {
		req.Capitalization = &capitalizationDTO{InterestAmount: toMoneyDTO(in.Capitalization.InterestAmount), FeeAmount: toMoneyDTO(in.Capitalization.FeeAmount)}
	}

	body, err := json.Marshal(req)
	if err != nil {
		return JournalEntry{}, fmt.Errorf("%w: marshaling request: %v", ErrGLUnavailable, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/journal-entries", bytes.NewReader(body))
	if err != nil {
		return JournalEntry{}, fmt.Errorf("%w: building request: %v", ErrGLUnavailable, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Idempotency-Key", in.IdempotencyKey)
	if err := c.attachAuth(ctx, httpReq); err != nil {
		return JournalEntry{}, err
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return JournalEntry{}, fmt.Errorf("%w: %v", ErrGLUnavailable, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return JournalEntry{}, fmt.Errorf("%w: reading response: %v", ErrGLUnavailable, err)
	}

	switch resp.StatusCode {
	case http.StatusCreated, http.StatusOK:
		var dto journalEntryDTO
		if err := json.Unmarshal(respBody, &dto); err != nil {
			return JournalEntry{}, fmt.Errorf("%w: decoding response: %v", ErrGLUnavailable, err)
		}
		return fromJournalEntryDTO(dto)
	case http.StatusConflict:
		return JournalEntry{}, fmt.Errorf("%w: %s", ErrIdempotencyKeyReused, decodeErrorMessage(respBody))
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return JournalEntry{}, fmt.Errorf("%w: %s", ErrRequestRejected, decodeErrorMessage(respBody))
	default:
		return JournalEntry{}, fmt.Errorf("%w: unexpected status %d: %s", ErrGLUnavailable, resp.StatusCode, decodeErrorMessage(respBody))
	}
}

type statementLineDTO struct {
	JournalEntryID string   `json:"journalEntryId"`
	PostedAt       string   `json:"postedAt"`
	GLAccount      string   `json:"glAccount"`
	Direction      string   `json:"direction"`
	Amount         moneyDTO `json:"amount"`
}

type statementOfAccountDTO struct {
	LoanAccountID string             `json:"loanAccountId"`
	Lines         []statementLineDTO `json:"lines"`
}

func (c *HTTPClient) GetStatementOfAccount(ctx context.Context, loanAccountID string) ([]domain.StatementLine, error) {
	url := c.baseURL + "/loan-accounts/" + loanAccountID + "/statement"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: building request: %v", ErrGLUnavailable, err)
	}
	if err := c.attachAuth(ctx, httpReq); err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrGLUnavailable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		// No entries posted yet for this account -- an empty, valid
		// statement, not an error (mirrors a freshly-Approved,
		// undisbursed loan account).
		return nil, nil
	}
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: reading response: %v", ErrGLUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: unexpected status %d: %s", ErrGLUnavailable, resp.StatusCode, decodeErrorMessage(respBody))
	}

	var dto statementOfAccountDTO
	if err := json.Unmarshal(respBody, &dto); err != nil {
		return nil, fmt.Errorf("%w: decoding response: %v", ErrGLUnavailable, err)
	}
	lines := make([]domain.StatementLine, len(dto.Lines))
	for i, l := range dto.Lines {
		postedAt, err := time.Parse(time.RFC3339, l.PostedAt)
		if err != nil {
			return nil, fmt.Errorf("%w: parsing postedAt: %v", ErrGLUnavailable, err)
		}
		lines[i] = domain.StatementLine{
			JournalEntryID: l.JournalEntryID, GLAccount: l.GLAccount,
			Direction: domain.Direction(l.Direction), Amount: fromMoneyDTO(l.Amount), PostedAt: postedAt,
		}
	}
	return lines, nil
}

func (c *HTTPClient) attachAuth(ctx context.Context, req *http.Request) error {
	if c.tokens == nil {
		return nil
	}
	token, err := c.tokens.Token(ctx)
	if err != nil {
		return fmt.Errorf("%w: acquiring access token: %v", ErrGLUnavailable, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

func fromJournalEntryDTO(dto journalEntryDTO) (JournalEntry, error) {
	postedAt, err := time.Parse(time.RFC3339, dto.PostedAt)
	if err != nil {
		return JournalEntry{}, fmt.Errorf("%w: parsing postedAt: %v", ErrGLUnavailable, err)
	}
	lines := make([]JournalEntryLine, len(dto.Lines))
	for i, l := range dto.Lines {
		lines[i] = JournalEntryLine{
			GLAccount: l.GLAccount, Direction: domain.Direction(l.Direction),
			Amount: fromMoneyDTO(l.Amount), RunningBalanceAfter: fromMoneyDTO(l.RunningBalanceAfter),
		}
	}
	return JournalEntry{
		JournalEntryID: dto.JournalEntryID, SourceEventID: dto.SourceEventID, PostingRuleCode: dto.PostingRuleCode,
		PostingRuleVersion: dto.PostingRuleVersion, LoanAccountID: dto.LoanAccountID, Lines: lines,
		Balanced: dto.Balanced, Immutable: dto.Immutable, PostedAt: postedAt, PeriodID: dto.PeriodID,
	}, nil
}

func decodeErrorMessage(body []byte) string {
	var e errorDTO
	if err := json.Unmarshal(body, &e); err != nil || e.Message == "" {
		return string(body)
	}
	return e.Code + ": " + e.Message
}
