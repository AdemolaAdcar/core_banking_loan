-- General Ledger & Posting Engine schema.
--
-- This migration enforces two of this role's hard invariants at the
-- database level, deliberately independent of anything the application
-- code does:
--
--   Invariant 1 (every entry's debits sum exactly to its credits, per
--   currency): a DEFERRABLE INITIALLY DEFERRED constraint trigger on
--   journal_entry_lines, fired once per inserted row but only actually
--   evaluated at COMMIT time (by which point every line of a posting's
--   single transaction has been inserted). This is the standard
--   Postgres pattern for a cross-row invariant a plain column CHECK
--   cannot express (CHECK constraints see only the one row being
--   written, never aggregate across others). If the trigger's aggregate
--   check fails, the whole transaction is rolled back -- an application
--   bug that tried to post an unbalanced entry never gets as far as a
--   committed row, regardless of what internal/domain's own (separate,
--   earlier) in-memory check did or didn't catch.
--
--   Invariant 3 (posted entries are immutable): the gl_app role this
--   migration creates is granted INSERT and SELECT on journal_entries
--   and journal_entry_lines, and NOTHING ELSE -- no UPDATE, no DELETE.
--   The explicit REVOKE statements at the bottom are defense-in-depth:
--   even though UPDATE/DELETE were never granted, a future migration
--   that does something broad like "GRANT ALL ON ALL TABLES" for an
--   unrelated reason must not silently re-open this without a separate,
--   explicit REVOKE first.
--
-- gl_app is created with LOGIN but NO PASSWORD -- a role with no
-- password cannot authenticate via password auth at all. Deployment
-- MUST run `ALTER ROLE gl_app WITH PASSWORD '...'` out of band (see
-- services/gl/PR_DESCRIPTION.md) before this role is usable; a
-- hardcoded password does not belong in a committed migration file.

DO $$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_catalog.pg_roles WHERE rolname = 'gl_app') THEN
    CREATE ROLE gl_app LOGIN;
  END IF;
END
$$;

CREATE TABLE journal_entries (
    id                           TEXT PRIMARY KEY,
    source_event_id              TEXT NOT NULL UNIQUE,
    posting_rule_code            TEXT NOT NULL,
    posting_rule_version         TEXT NOT NULL,
    loan_account_id              TEXT,
    posted_at                    TIMESTAMPTZ NOT NULL,
    period_id                    TEXT NOT NULL,
    is_prior_period_adjustment   BOOLEAN NOT NULL DEFAULT FALSE,
    adjustment_for_period_id     TEXT,
    reversal_of_source_event_id  TEXT REFERENCES journal_entries (source_event_id),
    metadata                     JSONB
);

CREATE INDEX idx_journal_entries_loan_account_id ON journal_entries (loan_account_id, posted_at);
CREATE INDEX idx_journal_entries_period_id ON journal_entries (period_id);
CREATE INDEX idx_journal_entries_posted_at ON journal_entries (posted_at);

CREATE TABLE journal_entry_lines (
    id                     BIGSERIAL PRIMARY KEY,
    journal_entry_id       TEXT NOT NULL REFERENCES journal_entries (id),
    line_order             INTEGER NOT NULL,
    gl_account             TEXT NOT NULL,
    direction              TEXT NOT NULL CHECK (direction IN ('DEBIT', 'CREDIT')),
    amount                 BIGINT NOT NULL CHECK (amount > 0),
    currency               TEXT NOT NULL,
    running_balance_after  BIGINT NOT NULL,

    UNIQUE (journal_entry_id, line_order)
);

CREATE INDEX idx_journal_entry_lines_journal_entry_id ON journal_entry_lines (journal_entry_id);
-- Backs both getTrialBalance (GROUP BY gl_account) and
-- getGlAccountBalance (WHERE gl_account = $1) -- both computed live,
-- never from a separately maintained running total (invariant 6).
CREATE INDEX idx_journal_entry_lines_gl_account ON journal_entry_lines (gl_account);

-- Invariant 1's database-level enforcement -- see the top-of-file
-- comment. Checks (a) every line for one journal_entry_id shares one
-- currency, (b) debit total equals credit total, (c) at least 2 lines
-- exist. Deferred to commit time so the whole atomic batch of INSERTs
-- for one posting is visible to the aggregate query.
CREATE OR REPLACE FUNCTION check_journal_entry_balanced() RETURNS TRIGGER AS $$
DECLARE
    v_journal_entry_id TEXT;
    v_currency_count   INTEGER;
    v_line_count       INTEGER;
    v_debit_total      BIGINT;
    v_credit_total     BIGINT;
BEGIN
    v_journal_entry_id := NEW.journal_entry_id;

    SELECT count(*), count(DISTINCT currency)
      INTO v_line_count, v_currency_count
      FROM journal_entry_lines
     WHERE journal_entry_id = v_journal_entry_id;

    IF v_line_count < 2 THEN
        RAISE EXCEPTION 'journal_entry % has fewer than 2 lines (%)', v_journal_entry_id, v_line_count;
    END IF;

    IF v_currency_count > 1 THEN
        RAISE EXCEPTION 'journal_entry % has lines in more than one currency -- multi-currency is out of scope', v_journal_entry_id;
    END IF;

    SELECT coalesce(sum(amount) FILTER (WHERE direction = 'DEBIT'), 0),
           coalesce(sum(amount) FILTER (WHERE direction = 'CREDIT'), 0)
      INTO v_debit_total, v_credit_total
      FROM journal_entry_lines
     WHERE journal_entry_id = v_journal_entry_id;

    IF v_debit_total <> v_credit_total THEN
        RAISE EXCEPTION 'journal_entry % is unbalanced: debits=% credits=%', v_journal_entry_id, v_debit_total, v_credit_total;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE CONSTRAINT TRIGGER trg_journal_entry_lines_balanced
    AFTER INSERT ON journal_entry_lines
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW
    EXECUTE FUNCTION check_journal_entry_balanced();

-- Invariant 7: period close. Chronological-order enforcement (cannot
-- close a period while an earlier one is still Open) is application-layer
-- logic (internal/service), since it requires comparing across every row
-- in this table, not just the one being written.
CREATE TABLE periods (
    id         TEXT PRIMARY KEY,
    status     TEXT NOT NULL CHECK (status IN ('Open', 'Closed')),
    closed_at  TIMESTAMPTZ,
    closed_by  TEXT
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

-- Invariant 3's database-level enforcement.
GRANT INSERT, SELECT ON journal_entries TO gl_app;
GRANT INSERT, SELECT ON journal_entry_lines TO gl_app;
GRANT USAGE ON SEQUENCE journal_entry_lines_id_seq TO gl_app;
GRANT INSERT, SELECT, UPDATE ON periods TO gl_app;
GRANT INSERT, SELECT, UPDATE ON outbox TO gl_app;
GRANT INSERT, SELECT ON idempotency_keys TO gl_app;

REVOKE UPDATE, DELETE ON journal_entries FROM gl_app;
REVOKE UPDATE, DELETE ON journal_entry_lines FROM gl_app;
