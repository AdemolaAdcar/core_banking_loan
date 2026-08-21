// Package api is the HTTP transport layer: chi handlers that translate
// between the wire shapes defined in specs/openapi/payment-execution.yaml
// and internal/service's calls. Every DTO here mirrors that spec (or
// specs/schemas/payment-instruction.schema.json / money.schema.json /
// error.schema.json) field-for-field.
package api

import (
	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/domain"
)

type moneyDTO struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

func toMoneyDTO(m domain.Money) moneyDTO { return moneyDTO{Amount: m.Amount, Currency: m.Currency} }
func fromMoneyDTO(m moneyDTO) domain.Money {
	return domain.Money{Amount: m.Amount, Currency: m.Currency}
}

// paymentInstructionDTO mirrors
// payment-instruction.schema.json#/$defs/PaymentInstruction field-for-
// field.
type paymentInstructionDTO struct {
	InstructionID  string   `json:"instructionId"`
	LoanAccountID  string   `json:"loanAccountId"`
	Direction      string   `json:"direction"`
	Purpose        string   `json:"purpose"`
	Amount         moneyDTO `json:"amount"`
	PartyID        *string  `json:"partyId"`
	JournalEntryID *string  `json:"journalEntryId"`
	Status         string   `json:"status"`
	Rail           *string  `json:"rail"`
	FailureReason  *string  `json:"failureReason"`
}

func toPaymentInstructionDTO(p domain.PaymentInstruction) paymentInstructionDTO {
	var failureReason *string
	if p.FailureReason != nil {
		s := string(*p.FailureReason)
		failureReason = &s
	}
	return paymentInstructionDTO{
		InstructionID: p.InstructionID, LoanAccountID: p.LoanAccountID, Direction: string(p.Direction), Purpose: string(p.Purpose),
		Amount: toMoneyDTO(p.Amount), PartyID: p.PartyID, JournalEntryID: p.JournalEntryID, Status: string(p.Status),
		Rail: p.Rail, FailureReason: failureReason,
	}
}

// initiateDisbursementRequestDTO mirrors
// payment-execution.yaml#/components/schemas/InitiateDisbursementRequest.
type initiateDisbursementRequestDTO struct {
	LoanAccountID  string   `json:"loanAccountId"`
	PartyID        string   `json:"partyId"`
	JournalEntryID string   `json:"journalEntryId"`
	Amount         moneyDTO `json:"amount"`
}

type errorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
