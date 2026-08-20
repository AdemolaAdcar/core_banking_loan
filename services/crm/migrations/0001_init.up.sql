-- CRM service schema.
--
-- No column here or in any table below ever references a Money value or
-- a balance/principal figure -- CRM has no path into GLPostingAPI, and
-- this schema structurally cannot become one (see the CRM Codegen
-- Agent's ground rules).
--
-- PII-adjacent free text (case notes, close/reopen reasons, interaction
-- notes) is stored ONLY as ciphertext (*_enc columns, produced by
-- internal/pii.Encryptor) -- the same AES-256-GCM discipline
-- migrations/0001_init.up.sql in services/party uses for actual PII.
-- Every read of that content is access-logged (case_note_access_log)
-- with actor and timestamp, per this service's ground rules.
--
-- Records are never deleted. A case is closed, not removed; a
-- relationship-manager assignment is superseded by a new row, not
-- edited in place; a case note is append-only.

CREATE TABLE cases (
    id                TEXT PRIMARY KEY,
    party_id          TEXT NOT NULL,
    loan_account_id   TEXT,
    status            TEXT NOT NULL,
    reason_code       TEXT NOT NULL,
    assigned_to       TEXT,
    version           INTEGER NOT NULL,
    sla_due_at        TIMESTAMPTZ NOT NULL,
    escalated         BOOLEAN NOT NULL DEFAULT FALSE,
    close_reason_enc  TEXT,
    reopen_reason_enc TEXT,
    opened_at         TIMESTAMPTZ NOT NULL,
    updated_at        TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_cases_party_id ON cases (party_id);
-- Backs the SLA sweep's query: active, not-yet-escalated cases past due.
CREATE INDEX idx_cases_sla_sweep ON cases (sla_due_at) WHERE escalated = FALSE AND status IN ('Open', 'InProgress');

CREATE TABLE interactions (
    id               TEXT PRIMARY KEY,
    loan_account_id  TEXT NOT NULL,
    event_type       TEXT NOT NULL,
    occurred_at      TIMESTAMPTZ NOT NULL,
    notes_enc        TEXT NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_interactions_loan_account_id ON interactions (loan_account_id, occurred_at DESC);

CREATE TABLE case_notes (
    id          TEXT PRIMARY KEY,
    case_id     TEXT NOT NULL REFERENCES cases (id),
    author_id   TEXT NOT NULL,
    body_enc    TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_case_notes_case_id ON case_notes (case_id, created_at ASC);

-- Every read of PII-adjacent content (case notes directly, or
-- interaction notes surfaced indirectly via getCustomer360) is logged
-- here with actor and timestamp -- never deleted, part of this
-- service's own audit trail, not itself PII-adjacent (actor is an
-- internal identity, resource_id is an opaque ID).
CREATE TABLE case_note_access_log (
    id             BIGSERIAL PRIMARY KEY,
    actor_subject  TEXT NOT NULL,
    resource_type  TEXT NOT NULL,
    resource_id    TEXT NOT NULL,
    accessed_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_case_note_access_log_resource ON case_note_access_log (resource_type, resource_id);

CREATE TABLE communication_preferences (
    party_id           TEXT PRIMARY KEY,
    preferred_channel  TEXT,
    email_opt_in       BOOLEAN NOT NULL DEFAULT FALSE,
    sms_opt_in         BOOLEAN NOT NULL DEFAULT FALSE,
    phone_opt_in       BOOLEAN NOT NULL DEFAULT FALSE,
    mail_opt_in        BOOLEAN NOT NULL DEFAULT FALSE,
    do_not_contact     BOOLEAN NOT NULL DEFAULT FALSE,
    updated_at         TIMESTAMPTZ NOT NULL
);

-- Append-only history -- "current" assignment is the latest row per
-- party_id. A reassignment supersedes the prior one; nothing is ever
-- updated or deleted here.
CREATE TABLE relationship_manager_assignments (
    id                        BIGSERIAL PRIMARY KEY,
    party_id                  TEXT NOT NULL,
    relationship_manager_id   TEXT NOT NULL,
    assigned_at               TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_rm_assignments_party_id ON relationship_manager_assignments (party_id, assigned_at DESC);

-- Records the (loan_account_id, party_id) association the first time
-- CRM learns of it -- only via openCase, which is the only operation
-- that ever supplies both fields together (see store.Tx's
-- LinkLoanAccountToParty doc comment for the documented limitation this
-- implies and the spec gap it's flagged against).
CREATE TABLE loan_account_links (
    loan_account_id  TEXT PRIMARY KEY,
    party_id         TEXT NOT NULL,
    linked_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_loan_account_links_party_id ON loan_account_links (party_id);

-- Transactional outbox -- identical discipline to services/party's.
CREATE TABLE outbox (
    id            TEXT PRIMARY KEY,
    topic         TEXT NOT NULL,
    payload_json  JSONB NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL,
    published_at  TIMESTAMPTZ
);

CREATE INDEX idx_outbox_unpublished ON outbox (created_at) WHERE published_at IS NULL;

-- Idempotency-Key replay cache for every state-changing endpoint.
CREATE TABLE idempotency_keys (
    idempotency_key  TEXT PRIMARY KEY,
    request_hash     TEXT NOT NULL,
    response_json    JSONB NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
