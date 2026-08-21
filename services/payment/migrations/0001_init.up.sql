-- Payment Execution schema.
--
-- payment_app is created with LOGIN but NO PASSWORD -- deployment MUST
-- run `ALTER ROLE payment_app WITH PASSWORD '...'` out of band (see
-- services/payment/PR_DESCRIPTION.md) before this role is usable.
--
-- There is no ledger-immutability invariant to enforce at the database
-- level here (unlike GL's append-only journal_entries) -- these are
-- ordinary mutable entity tables, and payment_app is granted ordinary
-- INSERT/SELECT/UPDATE. This service's own correctness invariant
-- (never mark a PaymentInstruction Executed except via a reconciled
-- rail confirmation) is enforced entirely in application code
-- (internal/domain's status-transition table), not by a database
-- constraint, since "who is allowed to call TransitionTo" isn't
-- something a GRANT can express.

DO $$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_catalog.pg_roles WHERE rolname = 'payment_app') THEN
    CREATE ROLE payment_app LOGIN;
  END IF;
END
$$;

CREATE TABLE payment_instructions (
    id                TEXT PRIMARY KEY,
    loan_account_id   TEXT NOT NULL,
    direction         TEXT NOT NULL CHECK (direction IN ('OUTBOUND','INBOUND')),
    purpose           TEXT NOT NULL CHECK (purpose IN ('DISBURSEMENT','REPAYMENT','PAYOFF')),
    amount            BIGINT NOT NULL,
    currency          TEXT NOT NULL,
    party_id          TEXT,
    journal_entry_id  TEXT,
    status            TEXT NOT NULL CHECK (status IN ('Submitted','Executed','Failed','Returned')),
    rail              TEXT,
    rail_reference    TEXT UNIQUE,
    failure_reason    TEXT CHECK (failure_reason IN ('PAYMENT_RETURNED','PAYMENT_FAILED','RAIL_REJECTED') OR failure_reason IS NULL),
    created_at        TIMESTAMPTZ NOT NULL,
    updated_at        TIMESTAMPTZ NOT NULL
);
CREATE INDEX idx_payment_instructions_loan_account ON payment_instructions (loan_account_id);
CREATE INDEX idx_payment_instructions_status ON payment_instructions (direction, status);

CREATE TABLE reconciliation_exceptions (
    id              TEXT PRIMARY KEY,
    kind            TEXT NOT NULL,
    rail_reference  TEXT NOT NULL,
    rail            TEXT,
    details         TEXT NOT NULL,
    occurred_at     TIMESTAMPTZ NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL
);
CREATE INDEX idx_reconciliation_exceptions_rail_reference ON reconciliation_exceptions (rail_reference);

CREATE TABLE inbound_cursors (
    name       TEXT PRIMARY KEY,
    cursor_at  TIMESTAMPTZ NOT NULL
);

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

GRANT INSERT, SELECT, UPDATE ON payment_instructions TO payment_app;
GRANT INSERT, SELECT ON reconciliation_exceptions TO payment_app;
GRANT INSERT, SELECT, UPDATE ON inbound_cursors TO payment_app;
GRANT INSERT, SELECT, UPDATE ON outbox TO payment_app;
GRANT INSERT, SELECT ON idempotency_keys TO payment_app;
