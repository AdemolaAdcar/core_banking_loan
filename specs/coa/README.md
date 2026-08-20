# Chart of Accounts

`chart-of-accounts.json` is the authoritative, machine-readable source of
truth for every GL account code this system posts to. It did not exist
as a committed artifact before this — only 8 bare account-name strings
appeared in an example inside `journal-entry.schema.json`'s `glAccount`
description. This manifest turns those same 8 accounts into structured
data (code, type, normal balance side, contra-asset flag) and nothing
more — no account is invented here beyond what the 11 approved posting
rules in `docs/design-notes.md` Appendix B already reference.

| Code | Name                    | Type   | Normal Balance | Contra-Asset |
|------|-------------------------|--------|-----------------|--------------|
| 1010 | CashNostro              | ASSET  | DEBIT           | No           |
| 1200 | LoanReceivable          | ASSET  | DEBIT           | No           |
| 1300 | InterestReceivable      | ASSET  | DEBIT           | No           |
| 1400 | FeeReceivable           | ASSET  | DEBIT           | No           |
| 1900 | AllowanceForLoanLosses  | ASSET  | CREDIT          | **Yes**      |
| 4100 | InterestIncome          | INCOME | CREDIT          | No           |
| 4200 | FeeIncome               | INCOME | CREDIT          | No           |
| 4300 | RecoveryIncome          | INCOME | CREDIT          | No           |

No Liability, Equity, or Expense accounts exist yet — none of the 11
posting rules currently reference one.

## Known gap: 1900 (AllowanceForLoanLosses) is never funded

`1900` is correctly modeled here as a contra-asset (natural CREDIT
balance, opposite every other 1xxx account). But as of this manifest's
authoring, **no posting rule in the 11-rule catalog ever credits it** —
PR-CHGOFF-01 is the only rule that touches this account, and it only
debits it. This was flagged in an earlier phase, remains unresolved, and
is explicitly re-flagged in `services/gl/PR_DESCRIPTION.md`. This
manifest documents the account's correct accounting nature; fixing the
missing provisioning rule is a Chart of Accounts & Posting Rules
Agent / Ledger & Solution Architect Agent decision, outside this
manifest's and this codegen role's authority.

`services/gl/internal/coa` embeds `chart-of-accounts.json` directly
(`go:embed`) as its only source of truth for account validation.
