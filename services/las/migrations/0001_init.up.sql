-- Loan Account Subledger schema.
--
-- balance_projections is a REBUILDABLE CACHE, not a source of truth --
-- see internal/store's package doc comment and internal/domain's
-- RebuildFromLines. Every row in it is written by SaveBalanceProjection
-- with a whole domain.BalanceProjection freshly produced by summing
-- GLPostingAPI's actual posted lines for that account; there is no
-- UPDATE ... SET outstanding_principal = outstanding_principal - $1
-- style statement anywhere in this codebase, and the application role
-- this migration creates is granted no more than ordinary INSERT/
-- SELECT/UPDATE on it (unlike GL's append-only journal_entries, these
-- are all ordinary mutable entity tables -- there is no ledger-
-- immutability invariant to enforce at the database level here; GL, not
-- LAS, owns that).
--
-- las_app is created with LOGIN but NO PASSWORD -- deployment MUST run
-- `ALTER ROLE las_app WITH PASSWORD '...'` out of band (see
-- services/las/PR_DESCRIPTION.md) before this role is usable.

DO $$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_catalog.pg_roles WHERE rolname = 'las_app') THEN
    CREATE ROLE las_app LOGIN;
  END IF;
END
$$;

CREATE TABLE loan_accounts (
    id                      TEXT PRIMARY KEY,
    approval_reference_id   TEXT NOT NULL UNIQUE,
    party_id                TEXT NOT NULL,
    status                  TEXT NOT NULL CHECK (status IN ('Approved','PendingDisbursement','Disbursed','Closed','ChargedOff')),
    booked_by               TEXT NOT NULL,
    dpd                     INTEGER NOT NULL DEFAULT 0,
    dpd_bucket              TEXT NOT NULL DEFAULT 'Current' CHECK (dpd_bucket IN ('Current','1-29','30-59','60-89','90+')),
    non_accrual_flag        BOOLEAN NOT NULL DEFAULT FALSE,
    non_accrual_since       DATE,
    created_at              TIMESTAMPTZ NOT NULL,
    updated_at              TIMESTAMPTZ NOT NULL
);
CREATE INDEX idx_loan_accounts_status ON loan_accounts (status);

CREATE TABLE term_versions (
    loan_account_id           TEXT NOT NULL REFERENCES loan_accounts (id),
    version                   INTEGER NOT NULL,
    effective_date            DATE NOT NULL,
    principal_amount          BIGINT NOT NULL,
    currency                  TEXT NOT NULL,
    annual_interest_rate_bps  INTEGER NOT NULL,
    term_months               INTEGER NOT NULL,
    day_count_convention      TEXT NOT NULL,
    PRIMARY KEY (loan_account_id, version)
);

CREATE TABLE balance_projections (
    loan_account_id        TEXT PRIMARY KEY REFERENCES loan_accounts (id),
    outstanding_principal  BIGINT NOT NULL,
    interest_receivable    BIGINT NOT NULL,
    fee_receivable         BIGINT NOT NULL,
    currency               TEXT NOT NULL,
    line_count             INTEGER NOT NULL,
    rebuilt_at              TIMESTAMPTZ NOT NULL
);

CREATE TABLE disbursements (
    id                 TEXT PRIMARY KEY,
    loan_account_id    TEXT NOT NULL REFERENCES loan_accounts (id),
    status             TEXT NOT NULL,
    principal_amount   BIGINT NOT NULL,
    currency           TEXT NOT NULL,
    rejection_reason   TEXT,
    journal_entry_id   TEXT,
    instruction_id     TEXT,
    requested_by       TEXT NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL,
    updated_at         TIMESTAMPTZ NOT NULL
);
CREATE INDEX idx_disbursements_loan_account ON disbursements (loan_account_id);

CREATE TABLE repayments (
    id                      TEXT PRIMARY KEY,
    loan_account_id         TEXT,
    status                  TEXT NOT NULL,
    amount                  BIGINT NOT NULL,
    currency                TEXT NOT NULL,
    alloc_fee_amount        BIGINT,
    alloc_interest_amount   BIGINT,
    alloc_principal_amount  BIGINT,
    suspense_amount         BIGINT NOT NULL DEFAULT 0,
    journal_entry_id        TEXT,
    unmatched_reason_code   TEXT,
    corrected_repayment_id  TEXT,
    received_at             TIMESTAMPTZ NOT NULL,
    updated_at              TIMESTAMPTZ NOT NULL
);
CREATE INDEX idx_repayments_loan_account ON repayments (loan_account_id);

CREATE TABLE payoffs (
    id                TEXT PRIMARY KEY,
    loan_account_id   TEXT NOT NULL REFERENCES loan_accounts (id),
    quote_id          TEXT NOT NULL,
    status            TEXT NOT NULL,
    amount_posted     BIGINT NOT NULL,
    suspense_amount   BIGINT NOT NULL DEFAULT 0,
    currency          TEXT NOT NULL,
    journal_entry_id  TEXT,
    closed_at         TIMESTAMPTZ,
    updated_at        TIMESTAMPTZ NOT NULL
);

CREATE TABLE chargeoffs (
    id                           TEXT PRIMARY KEY,
    loan_account_id              TEXT NOT NULL REFERENCES loan_accounts (id),
    status                       TEXT NOT NULL,
    write_off_amount             BIGINT NOT NULL,
    currency                     TEXT NOT NULL,
    journal_entry_id             TEXT,
    chargeoff_decision_reference TEXT NOT NULL,
    confirmed_by                 TEXT NOT NULL,
    charged_off_at               TIMESTAMPTZ,
    updated_at                   TIMESTAMPTZ NOT NULL
);
CREATE INDEX idx_chargeoffs_loan_account ON chargeoffs (loan_account_id);

CREATE TABLE recoveries (
    id                TEXT PRIMARY KEY,
    loan_account_id   TEXT NOT NULL REFERENCES loan_accounts (id),
    amount            BIGINT NOT NULL,
    currency          TEXT NOT NULL,
    journal_entry_id  TEXT,
    received_at       TIMESTAMPTZ NOT NULL,
    updated_at        TIMESTAMPTZ NOT NULL
);

CREATE TABLE modifications (
    id                     TEXT PRIMARY KEY,
    loan_account_id        TEXT NOT NULL REFERENCES loan_accounts (id),
    effective_date         DATE NOT NULL,
    previous_term_version  INTEGER NOT NULL,
    new_term_version       INTEGER NOT NULL,
    capitalized_amount     BIGINT,
    currency               TEXT,
    journal_entry_id       TEXT,
    confirmed_by           TEXT NOT NULL,
    updated_at             TIMESTAMPTZ NOT NULL
);

CREATE TABLE fees (
    id                  TEXT PRIMARY KEY,
    loan_account_id     TEXT NOT NULL REFERENCES loan_accounts (id),
    scheduled_due_date  DATE NOT NULL,
    amount              BIGINT NOT NULL,
    currency            TEXT NOT NULL,
    status              TEXT NOT NULL,
    journal_entry_id    TEXT,
    waived_by           TEXT,
    waive_reason_code   TEXT,
    waived_at           TIMESTAMPTZ,
    assessed_at         TIMESTAMPTZ NOT NULL,
    updated_at          TIMESTAMPTZ NOT NULL
);
CREATE INDEX idx_fees_loan_account ON fees (loan_account_id);

CREATE TABLE outbox (
    id            TEXT PRIMARY KEY,
    topic         TEXT NOT NULL,
    payload_json  JSONB NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL,
    published_at  TIMESTAMPTZ
);
CREATE INDEX idx_outbox_unpublished ON outbox (created_at) WHERE published_at IS NULL;

CREATE TABLE idempotency_keys (
    idempotency_key  TEXT PRIMARY KEY,
    request_hash     TEXT NOT NULL,
    response_json    JSONB NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

GRANT INSERT, SELECT, UPDATE ON loan_accounts TO las_app;
GRANT INSERT, SELECT ON term_versions TO las_app;
GRANT INSERT, SELECT, UPDATE ON balance_projections TO las_app;
GRANT INSERT, SELECT, UPDATE ON disbursements TO las_app;
GRANT INSERT, SELECT, UPDATE ON repayments TO las_app;
GRANT INSERT, SELECT, UPDATE ON payoffs TO las_app;
GRANT INSERT, SELECT, UPDATE ON chargeoffs TO las_app;
GRANT INSERT, SELECT, UPDATE ON recoveries TO las_app;
GRANT INSERT, SELECT, UPDATE ON modifications TO las_app;
GRANT INSERT, SELECT, UPDATE ON fees TO las_app;
GRANT INSERT, SELECT, UPDATE ON outbox TO las_app;
GRANT INSERT, SELECT ON idempotency_keys TO las_app;
