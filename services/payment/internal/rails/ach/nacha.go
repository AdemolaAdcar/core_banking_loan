package ach

// nacha.go builds a standards-shaped NACHA ACH file: 94-character
// fixed-width records (File Header '1', Batch Header '5', Entry Detail
// '6', Batch Control '8', File Control '9'). This is genuinely correct
// on RECORD LAYOUT/WIDTH and on the entry hash algorithm (sum of each
// entry's 8-digit receiving-DFI routing number, mod 10^10 — the actual
// NACHA rule); it deliberately simplifies two things real production
// origination software must also do, both called out in
// PR_DESCRIPTION.md as rail limitations for the Architect Agent:
//
//  1. Routing-number check-digit validation (the 10th digit of a real
//     ABA routing number is a checksum) is NOT verified here — a
//     malformed routing number from PayoutAccountResolver is trusted
//     as-is rather than rejected before file generation.
//  2. This adapter never actually TRANSMITS the generated file to an
//     ODFI/bank — CutBatch returns the file bytes for a caller
//     (cmd/payment-service, or in production some SFTP/bank-API
//     delivery mechanism entirely outside this repo's scope) to send
//     onward. Nothing in this package has network access.

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

func padRight(s string, n int) string {
	if len(s) >= n {
		return s[:n]
	}
	return s + strings.Repeat(" ", n-len(s))
}

func padLeftZero(s string, n int) string {
	if len(s) >= n {
		return s[len(s)-n:]
	}
	return strings.Repeat("0", n-len(s)) + s
}

func amountField(minorUnits int64) string {
	return padLeftZero(strconv.FormatInt(minorUnits, 10), 10)
}

// fileHeaderRecord builds the Type 1 record.
func fileHeaderRecord(cfg Config, fileIDModifier rune, createdAt time.Time) string {
	var b strings.Builder
	b.WriteString("1")
	b.WriteString("01")
	b.WriteString(padLeftZero(cfg.DestinationRoutingNumber, 10))
	b.WriteString(padLeftZero(cfg.OriginRoutingNumber, 10))
	b.WriteString(createdAt.Format("060102"))
	b.WriteString(createdAt.Format("1504"))
	b.WriteString(string(fileIDModifier))
	b.WriteString("094")
	b.WriteString("10")
	b.WriteString("1")
	b.WriteString(padRight(cfg.DestinationName, 23))
	b.WriteString(padRight(cfg.OriginName, 23))
	b.WriteString(padRight("", 8))
	return b.String()
}

// batchHeaderRecord builds the Type 5 record. serviceClassCode 220 =
// credits only (disbursements); 225 = mixed debits/credits (returns).
func batchHeaderRecord(cfg Config, serviceClassCode, batchNumber int, effectiveDate time.Time) string {
	var b strings.Builder
	b.WriteString("5")
	b.WriteString(strconv.Itoa(serviceClassCode))
	b.WriteString(padRight(cfg.CompanyName, 16))
	b.WriteString(padRight("", 20))
	b.WriteString(padRight(cfg.CompanyID, 10))
	b.WriteString("PPD")
	b.WriteString(padRight("DISB", 10))
	b.WriteString(padRight("", 6))
	b.WriteString(effectiveDate.Format("060102"))
	b.WriteString(padRight("", 3))
	b.WriteString("1")
	b.WriteString(padLeftZero(cfg.OriginRoutingNumber, 8))
	b.WriteString(padLeftZero(strconv.Itoa(batchNumber), 7))
	return b.String()
}

// entryDetailRecord builds a Type 6 record for one credit entry.
// transactionCode 22 = checking credit, 32 = savings credit.
func entryDetailRecord(payout PayoutAccount, amountMinorUnits int64, individualID, individualName, traceNumber string) string {
	transactionCode := "22"
	if payout.AccountType == Savings {
		transactionCode = "32"
	}
	routing8 := padLeftZero(payout.RoutingNumber, 9)[:8]
	checkDigit := padLeftZero(payout.RoutingNumber, 9)[8:9]

	var b strings.Builder
	b.WriteString("6")
	b.WriteString(transactionCode)
	b.WriteString(routing8)
	b.WriteString(checkDigit)
	b.WriteString(padRight(payout.AccountNumber, 17))
	b.WriteString(amountField(amountMinorUnits))
	b.WriteString(padRight(individualID, 15))
	b.WriteString(padRight(individualName, 22))
	b.WriteString(padRight("", 2))
	b.WriteString("0")
	b.WriteString(padLeftZero(traceNumber, 15))
	return b.String()
}

// batchControlRecord builds the Type 8 record. entryHash is the sum of
// every entry's 8-digit receiving-DFI routing number in this batch,
// mod 10^10 — the actual NACHA algorithm, not simplified.
func batchControlRecord(cfg Config, serviceClassCode, entryCount int, entryHash, totalCredits int64, batchNumber int) string {
	var b strings.Builder
	b.WriteString("8")
	b.WriteString(strconv.Itoa(serviceClassCode))
	b.WriteString(padLeftZero(strconv.Itoa(entryCount), 6))
	b.WriteString(padLeftZero(strconv.FormatInt(entryHash%10000000000, 10), 10))
	b.WriteString(padLeftZero("0", 12))
	b.WriteString(padLeftZero(strconv.FormatInt(totalCredits, 10), 12))
	b.WriteString(padRight(cfg.CompanyID, 10))
	b.WriteString(padRight("", 19))
	b.WriteString(padRight("", 6))
	b.WriteString(padLeftZero(cfg.OriginRoutingNumber, 8))
	b.WriteString(padLeftZero(strconv.Itoa(batchNumber), 7))
	return b.String()
}

// fileControlRecord builds the Type 9 record.
func fileControlRecord(batchCount, blockCount, entryCount int, entryHash, totalCredits int64) string {
	var b strings.Builder
	b.WriteString("9")
	b.WriteString(padLeftZero(strconv.Itoa(batchCount), 6))
	b.WriteString(padLeftZero(strconv.Itoa(blockCount), 6))
	b.WriteString(padLeftZero(strconv.Itoa(entryCount), 8))
	b.WriteString(padLeftZero(strconv.FormatInt(entryHash%10000000000, 10), 10))
	b.WriteString(padLeftZero("0", 12))
	b.WriteString(padLeftZero(strconv.FormatInt(totalCredits, 10), 12))
	b.WriteString(padRight("", 39))
	return b.String()
}

// buildFile assembles a complete NACHA file from one batch of entries.
// Every record is exactly 94 characters; the file is block-padded to a
// multiple of 10 records with '9'-filled filler records, per the NACHA
// spec's blocking-factor-of-10 convention this file header declares.
func buildFile(cfg Config, batchNumber int, entries []entry, effectiveDate, createdAt time.Time) string {
	var entryHash int64
	var totalCredits int64
	lines := []string{fileHeaderRecord(cfg, '1', createdAt), batchHeaderRecord(cfg, 220, batchNumber, effectiveDate)}

	for i, e := range entries {
		routing8, err := strconv.Atoi(padLeftZero(e.payout.RoutingNumber, 9)[:8])
		if err != nil {
			routing8 = 0
		}
		entryHash += int64(routing8)
		totalCredits += e.amount.Amount
		lines = append(lines, entryDetailRecord(e.payout, e.amount.Amount, fmt.Sprintf("%d", i+1), e.payout.AccountName, e.traceNumber))
	}

	lines = append(lines, batchControlRecord(cfg, 220, len(entries), entryHash, totalCredits, batchNumber))
	lines = append(lines, fileControlRecord(1, blockCountFor(len(entries)+4), len(entries), entryHash, totalCredits))

	for len(lines)%10 != 0 {
		lines = append(lines, strings.Repeat("9", 94))
	}
	return strings.Join(lines, "\n") + "\n"
}

// blockCountFor returns how many 10-record blocks n records need,
// rounded up.
func blockCountFor(n int) int {
	return (n + 9) / 10
}
