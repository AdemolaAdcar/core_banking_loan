DROP TABLE IF EXISTS idempotency_keys;
DROP TABLE IF EXISTS outbox;
DROP TABLE IF EXISTS fees;
DROP TABLE IF EXISTS modifications;
DROP TABLE IF EXISTS recoveries;
DROP TABLE IF EXISTS chargeoffs;
DROP TABLE IF EXISTS payoffs;
DROP TABLE IF EXISTS repayments;
DROP TABLE IF EXISTS disbursements;
DROP TABLE IF EXISTS balance_projections;
DROP TABLE IF EXISTS term_versions;
DROP TABLE IF EXISTS loan_accounts;

DROP ROLE IF EXISTS las_app;
