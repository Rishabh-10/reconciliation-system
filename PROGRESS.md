# Progress Log

What was found, what was built, and how each variance was traced to a config rule.

---

## 1. Findings, before writing any code

Goal: find one number to trust, so that a difference later means a mapping defect and not a pipeline
bug.

**The anchor is 212,118.95.** The settlement file's first row is Amazon's statement header for
settlement `12395580393`, and its 54,979 detail rows add up to exactly that.

**The payments file is wider than the settlement file.** It covers four settlements; the settlement
file covers one. Narrowing until the totals meet:

| payments filter, settlement 12395580393 | sum of `total` |
|---|---:|
| all rows | 121,902.51 |
| `Transaction status = Released` | 78,362.44 |
| Released, without the bank transfer | **212,118.95** |

The 2,398 Deferred rows sum to 43,540.07, exactly the gap. They are not in the settlement report
because they have not been released into it. The config schema has no status column, so this is a
scoping rule in the pipeline, not a mapping defect.

**The `date` in `record_ref` is the release date in UTC.** Payments times are GMT+9, settlement
times are UTC, and the payments *posted* date is not the settlement posted date. Tested on the
15,745 in-scope order rows, keyed on `order id + sku + date`:

| date rule | matches |
|---|---:|
| posted date, GMT+9 or UTC | 0 |
| release date, GMT+9 | 8,145 |
| **release date, converted to UTC** | **13,328** |

13,328 is exactly the number of `Order / ItemPrice / Principal` settlement rows. Confirmed on one
record: release `17 July 2026 4:26:32 pm GMT+9` is `2026-07-17 07:26:32 UTC`, the settlement's
`posted-date-time` to the second.

**Granularity differs.** Order `249-3364825-5895055` is one payments row and three settlement rows
(19.67, −1.97, −6.17), both sides summing to 11.53. Each side must be summed per key before matching.

**Config coverage.** Every settlement combination in the data has a rule. The payments config has one
unmapped amount and two duplicated rules.

**Ruled out:** filtering the other three settlements at ingest (no audit trail left; they are stored
with `in_scope = false`); treating the Deferred exclusion as a config defect; using the posted date
for keys, which matches nothing.

---

## 2. Implementation

Go, PostgreSQL 16 in Docker, `pgx`, `excelize`, `shopspring/decimal`. No `float64` in the money path.

One package, six commands (`migrate`, `load-config`, `ingest`, `reconcile`, `report`, `all`) and five
plain tables with primary keys only. Both files live in one `records` table, one row per amount, each
keeping its source file, line number and original row, plus the id of the rule that routed it. Config
rules use their CSV line number as `id`, so `MAPPING_FIXES.sql` names each rule directly and a fix is
a data change.

Anything unexpected stops the run rather than warning: an amount matching no rule, a rule with an
empty `record_ref`, a template token the file does not carry, an unreadable date or amount, a summary
field with no Summary line, or `migrate` / `load-config` run twice.

**Gate before reconciliation:** both sides had to reproduce 212,118.95 from their own rows first.
They do, which is what makes every later difference a config question.

Three bugs found while building:

- Month names differ inside one file: `9 July 2026` but `1 Aug 2026`. Ingest died on line 13,303.
- The payments control total first included the bank transfer, giving 78,362.44. Rather than
  excluding `Transfer` by name, a row counts only if at least one of its amounts routes to a summary
  field. The transfer's do not, so it drops out structurally.
- 62 payments lines whose money columns are all zero produced no record, so 22,964 of 23,026 lines
  were stored. `total` is now always kept, even at zero. No figure changes.

---

## 3. Root cause of the four variances

The defects were found with the help of Claude Code, running the queries below against the ingested
data. Every figure here comes from those queries.

Before the fixes:

| Summary line | payments − settlements |
|---|---:|
| Product Charges | +103.78 |
| Shipping | −103.42 |
| Other | −0.36 |
| Refund expenses | +42.59 |

The first three net to exactly zero, so that money is misfiled rather than missing. The fourth is the
entire gap between the payments total (212,161.54) and Amazon's payout.

**Breaking the lines down by rule** showed two parts identical on both sides, rows and cents:
payments `product_sales` = settlement `ItemPrice/Principal` (335,336.96 over 13,328 rows), and
`shipping_credits` = `ItemPrice/Shipping` (8,294.16 over 2,864 rows). Everything left over was tax.

**The cause.** The payments report carries combined money columns; the settlement report carries
their parts. Measured on the ingested data, to the cent:

| payments column | settlement parts | both |
|---|---|---:|
| ORDER `sales tax collected` | Tax + ShippingTax + TaxDiscount + GiftWrapTax | 13,675.59 |
| ORDER `low value goods` | LowValueGoodsTax-Principal + -Shipping | −196.62 |
| REFUND `sales tax collected` | refund Tax + ShippingTax + TaxDiscount | −42.59 |

A combined column can route to only one summary field, so every settlement part of it must route to
that same field. The shipped configs spread the tax parts over Product Charges, Shipping and Other.

**Which field.** All of it is tax, and the Summary sheet has a Tax line (`sales_tax`) that nothing
else feeds. Rejected alternative: send every part to `sales_product_charges` to match the payments
side as shipped. That also balances and touches fewer rows, but it leaves the Tax line permanently
zero and books GST as product revenue. A judgement, not arithmetic.

**Refund expenses** was separate: payments rule 14 (`REFUND / sales_tax_collected`) had both summary
fields empty, so −42.59 was stored and never counted, while the settlement side already booked the
same amount as a refund expense.

### How each defect was found

| Defect | Query |
|---|---|
| 1, 2 duplicate rules | `GROUP BY transaction_type, description, amount_field HAVING count(*) > 1` on `config_payment` |
| 3 tax scattered | one order's rows from both sides, then where the tax rules point, then the two sums above |
| 4 refund tax dropped | `WHERE in_scope AND summary_field = '' AND amount_type <> 'total'` on `records` |
| 5 malformed template | `record_ref LIKE '%settlement_id%settlement_id%'` |
| 6 empty template | `record_ref = ''` |
| 7 key with a blank part | `record_ref LIKE '+%'` on `records` |
| 8, 9 keys that cannot meet | template parts of one config `EXCEPT` those of the other |
| 10 asymmetric signs | list the `REFUND / ITEMPRICE` family and spot the odd row |

Defects 5 to 10 move no number this period; they were found by reading the config tables.

**Open question on defect 9.** There are three Vine enrolment key shapes: payments rules 79–82 use
`AMAZON_FEES_VINE_ENROLLMENT_FEE+settlement_id+date`, payments 102–103 use
`txn_ref+VINE_ENROLLMENT_FEE+settlement_id+date`, and the single settlement rule 99 uses
`txn_ref+sku+date`. The fix points rule 99 at the first shape; it could belong with the second. No
data exercises any of them, so nothing settles it.

**Also ruled out:** that the duplicate rules caused the variance by double counting. Only one rule is
ever applied, and the amounts they touch (24,243.29 and −341.93) match no observed difference. Their
real harm is that the routing was arbitrary.

### Result

Every Summary line matches, both columns at 212,118.95, and the Tax line reads 13,478.97 on both
sides instead of zero. 13,289 keys reconciled, no unreconciled settlements. The one unreconciled
payment is the −133,756.51 bank transfer, which has no settlement counterpart because it moves money
already counted.
