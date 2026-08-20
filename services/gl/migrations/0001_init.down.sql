REVOKE ALL ON idempotency_keys FROM gl_app;
REVOKE ALL ON outbox FROM gl_app;
REVOKE ALL ON periods FROM gl_app;
REVOKE ALL ON journal_entry_lines FROM gl_app;
REVOKE ALL ON journal_entries FROM gl_app;
REVOKE USAGE ON SEQUENCE journal_entry_lines_id_seq FROM gl_app;

DROP TABLE IF EXISTS idempotency_keys;
DROP TABLE IF EXISTS outbox;
DROP TABLE IF EXISTS periods;
DROP TRIGGER IF EXISTS trg_journal_entry_lines_balanced ON journal_entry_lines;
DROP FUNCTION IF EXISTS check_journal_entry_balanced();
DROP TABLE IF EXISTS journal_entry_lines;
DROP TABLE IF EXISTS journal_entries;
