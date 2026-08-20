// Package events defines the domain event payloads this service
// publishes, matching specs/asyncapi/gl-posting-engine-events.yaml
// exactly.
package events

import "time"

const (
	TopicEntryPosted  = "gl.entry.posted"
	TopicPeriodClosed = "gl.period.closed"
)

// LinePayload mirrors journal-entry.schema.json#/$defs/JournalEntryLine.
type LinePayload struct {
	GLAccount           string `json:"glAccount"`
	Direction           string `json:"direction"`
	Amount              Money  `json:"amount"`
	RunningBalanceAfter Money  `json:"runningBalanceAfter"`
}

type Money struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

// EntryPostedPayload mirrors journal-entry.schema.json#/$defs/JournalEntry
// exactly -- gl.entry.posted's payload is a direct $ref to that shared
// schema, not a locally duplicated one.
type EntryPostedPayload struct {
	JournalEntryID          string         `json:"journalEntryId"`
	SourceEventID           string         `json:"sourceEventId"`
	PostingRuleCode         string         `json:"postingRuleCode"`
	PostingRuleVersion      string         `json:"postingRuleVersion"`
	LoanAccountID           string         `json:"loanAccountId,omitempty"`
	Lines                   []LinePayload  `json:"lines"`
	Balanced                bool           `json:"balanced"`
	Immutable               bool           `json:"immutable"`
	PostedAt                time.Time      `json:"postedAt"`
	PeriodID                string         `json:"periodId"`
	IsPriorPeriodAdjustment bool           `json:"isPriorPeriodAdjustment"`
	AdjustmentForPeriodID   *string        `json:"adjustmentForPeriodId"`
	Metadata                map[string]any `json:"metadata,omitempty"`
}

type PeriodClosedPayload struct {
	PeriodID string    `json:"periodId"`
	ClosedAt time.Time `json:"closedAt"`
	ClosedBy string    `json:"closedBy"`
}
