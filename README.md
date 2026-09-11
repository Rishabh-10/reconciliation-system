# Amazon Payments vs Settlement Reconciliation

A small Go command-line tool. It loads Amazon's Payments report and Settlement report into
PostgreSQL, applies the two mapping configs, matches the reports on `record_ref`, and writes an
Excel report with a **Summary** sheet and a **Consolidated Data** sheet.

## Requirements

- Go 1.24 or newer
- Docker (it runs PostgreSQL 16)
- The four supplied files, unchanged, in `Assignment_050726_files/`:
  - `amazon_payments_data.csv`
  - `amazon_settlements_data.txt`
  - `amazon_payment_configs_au_old.csv`
  - `amazon_settlement_configs_au.csv`

## Run it

Run these from the repository root, in this order. They work the same in bash, zsh and PowerShell.

```
docker compose up -d --wait
go run ./cmd/recon migrate
go run ./cmd/recon load-config
go run ./cmd/recon all --out out/report_before_fix.xlsx
docker compose cp MAPPING_FIXES.sql db:MAPPING_FIXES.sql
docker compose exec -T db psql -U recon -d recon -v ON_ERROR_STOP=1 -f MAPPING_FIXES.sql
go run ./cmd/recon all --out out/report_after_fix.xlsx
```

What each step does:

1. Starts PostgreSQL on `localhost:5433` (user, password and database are all `recon`).
2. Creates the tables.
3. Loads both mapping configs into the database, exactly as shipped.
4. Ingests, reconciles and writes the report with the shipped configs. It ends with:

   ```
   summary total, payments:              212161.54
   summary total, settlements:           212118.95
   reconciled:               13289
   unreconciled payments:    1
   unreconciled settlements: 0
   wrote out/report_before_fix.xlsx
   Summary lines that differ (payments - settlements):
     Product Charges            103.78
     Shipping                   -103.42
     Other                      -0.36
     Refund expenses            42.59
   ```

5. and 6. Copy `MAPPING_FIXES.sql` into the database container and apply it to the config tables.
6. Runs the same flow with the fixed configs. It ends with:

   ```
   summary total, payments:              212118.95
   summary total, settlements:           212118.95
   wrote out/report_after_fix.xlsx
   every Summary line matches
   ```

To start over from nothing: `docker compose down -v`, then run the list again from the top.

## Commands

| Command               | What it does                                                                                 |
| --------------------- | -------------------------------------------------------------------------------------------- |
| `migrate`             | Creates the tables. Run once.                                                                |
| `load-config`         | Loads both config CSVs into `config_payment` and `config_settlement`. Run once.              |
| `ingest`              | Reads both data files, applies the configs, builds the summary. Replaces any earlier ingest. |
| `reconcile`           | Matches payment and settlement records on `record_ref`.                                      |
| `report [--out FILE]` | Writes the Excel report (default `out/report.xlsx`).                                         |
| `all [--out FILE]`    | `ingest`, `reconcile` and `report` in one go.                                                |

The tool connects to `postgres://recon:recon@localhost:5433/recon?sslmode=disable`. Set `RECON_DSN`
to use another database.

## Errors

The tool stops with an error and exit status 1 instead of guessing. For example:

- Postgres is not running
- `migrate` or `load-config` is run a second time (to start over: `docker compose down -v`)
- A command runs before the step it depends on (`ingest` before `load-config`, `report` before
  `reconcile`)
- An input file is missing, has an unexpected header, or has a row with the wrong number of columns
- An amount or date cannot be read
- An amount matches no config rule, or its rule has an empty `record_ref`
- A `record_ref` template uses a value the file does not have (such as `shipment_id` in the
  payments file)
- A summarised field has no line on the Summary sheet
- An unknown command, flag or extra argument

`MAPPING_FIXES.sql` runs as one transaction. Applying it a second time fails and changes nothing.

## Database dump

```
docker compose exec -T db pg_dump -U recon -d recon -Fc -f recon.dump
docker compose cp db:recon.dump out/recon.dump
```

`out/recon.dump` holds the data after the fixed run (restore it with `pg_restore`).

## Tables

| Table                                 | Holds                                                                                                                          |
| ------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------ |
| `config_payment`, `config_settlement` | The two mapping configs. `id` is the rule's line number in its CSV, so `MAPPING_FIXES.sql` can name the exact rule it changes. |
| `records`                             | Both data files in one table, one row per amount.                                                                              |
| `summary`                             | The summary total per source and summary field, built during ingest.                                                           |
| `reconciliation`                      | One row per `record_ref`, with the payment amount, settlement amount and difference.                                           |

Why `records` has one row per amount: a payments row spreads its money across columns (product
sales, fees, total, and so on), while a settlement row holds a single amount. Splitting each payments
row into one record per non-zero money column gives both files the same shape, so matching is a
`GROUP BY record_ref`. The `total` column is always stored, even when it is zero, so every line of
both files is in the table.

Every record keeps `source_file`, `line_no` and the full original row in `raw`, plus `config_id`,
the rule that routed it. Any number in the report can be traced to its input line and its rule.

## How it works

**Matching a rule.** Values are upper-cased and spaces become `_` before matching
(`Fulfilment by Amazon transaction fees` becomes `FULFILMENT_BY_AMAZON_TRANSACTION_FEES`). The rule
is chosen in this order:

1. exact transaction type, amount field and description
2. description `any`
3. the longest rule description the value starts with (payments says
   `To account ending with: 334`, the config says `TO_ACCOUNT_ENDING`)
4. a rule with an empty transaction type (the catch-all)

If two rules tie, the one earlier in the file wins.

**record_ref.** The rule's template, such as `txn_ref+sku+date`, is filled in from the row. The
tokens are `txn_ref` (the order id), `sku`, `settlement_id`, `date`, `description`, `shipment_id` and
`merchant_order_id`. Any other part of the template is literal text.

**Summary field.** A positive amount goes to `to_summary_field_when_positive_amount` and a negative
one to `to_summary_field_when_negative_amount`. An empty field means the amount is stored but not
summarised. That is how the payments `total` column and the bank transfer stay out of the summary.

**Scope.** The settlement file covers one settlement, `12395580393`, and its header row gives
Amazon's payout: **212,118.95**. The payments file covers four settlements and includes Deferred
rows. Only payments rows of that settlement with status `Released` are in scope. The 2,398 Deferred
rows sum to 43,540.07, exactly the difference. Out-of-scope rows are still stored, with
`in_scope = false`.

**Dates.** Payments times are GMT+9 and settlement times are UTC. The `date` in `record_ref` is the
payments _Transaction Release Date_ converted to UTC, which equals the settlement's
`posted-date-time`. Deferred rows have no release date yet and use their posted date instead.

**Reconcile.** Each side is summed per `record_ref` and then compared, because one payments row can
be several settlement rows. The payments `total` column is left out here, since it repeats the
row's other columns.

## Result

| Summary line    | Before fix (payments - settlements) |                   After fix |
| --------------- | ----------------------------------: | --------------------------: |
| Product Charges |                              103.78 |                        0.00 |
| Tax             |              0.00 (both sides 0.00) | 0.00 (both sides 13,478.97) |
| Shipping        |                             -103.42 |                        0.00 |
| Other           |                               -0.36 |                        0.00 |
| Refund expenses |                               42.59 |                        0.00 |
| **Total**       |                           **42.59** |                    **0.00** |

The payments report carries combined columns, and the settlement report carries their parts.
Payments `sales tax collected` equals settlement Tax + ShippingTax + TaxDiscount + GiftWrapTax
(13,675.59), and `low value goods` equals the two LowValueGoodsTax parts (-196.62). The shipped
configs sent those parts to three different lines, so Product Charges, Shipping and Other disagreed
by amounts that net to zero. All of them now go to `sales_tax`. Separately, the payments rule for tax
on refunds had empty summary fields, so -42.59 was never counted. Every change, with before, after
and why, is in [MAPPING_FIXES.sql](MAPPING_FIXES.sql). The investigation is in
[PROGRESS.md](PROGRESS.md).

After the fixes, 13,289 keys are reconciled. The one unreconciled payment is the -133,756.51 bank
transfer, which has no settlement counterpart.

## Files

```
cmd/recon/main.go        commands, database connection, migrate
cmd/recon/config.go      load-config, rule matching, record_ref
cmd/recon/ingest.go      reading both files, applying rules, the summary
cmd/recon/reconcile.go   matching on record_ref
cmd/recon/report.go      the Excel report
migrations/001_schema.sql
docker-compose.yml
MAPPING_FIXES.sql        the config fixes, one commented block per defect
PROGRESS.md              the investigation log
out/                     report_before_fix.xlsx, report_after_fix.xlsx, recon.dump
```
