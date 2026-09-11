-- MAPPING_FIXES.sql
--
-- Fixes for the mapping defects in amazon_payment_configs_au_old.csv and
-- amazon_settlement_configs_au.csv. Config data changes only: no code is
-- special-cased and no total is plugged.
--
-- A rule's `id` is its line number in its CSV. Every statement also checks the
-- rule's content, so it can only change the row it names.
--
-- Runs as one transaction. Applying it twice fails on the INSERT (duplicate id)
-- and changes nothing.

BEGIN;

-- ROOT CAUSE (defects 1 to 4)
--
-- The payments report carries combined money columns; the settlement report
-- carries the separate components. On the ingested data, to the cent:
--
--   payments "sales tax collected" (ORDER)  = 13,675.59
--     = settlement ItemPrice/Tax + ItemPrice/ShippingTax
--                + Promotion/TaxDiscount + ItemPrice/GiftWrapTax
--   payments "low value goods" (ORDER)      =   -196.62
--     = settlement ItemWithheldTax/LowValueGoodsTax-Principal + -Shipping
--   payments "sales tax collected" (REFUND) =    -42.59
--     = settlement REFUND ItemPrice/Tax + ItemPrice/ShippingTax + Promotion/TaxDiscount
--
-- A combined column can go to only one summary field, so every settlement part
-- of it has to go to that same field. The shipped configs spread the tax parts
-- over Product Charges, Shipping and Other. All of them are tax, and the Summary
-- sheet has a Tax line (sales_tax) that nothing else feeds, so they move there.
--
-- Rejected alternative: send every tax part to sales_product_charges to match
-- the payments side as shipped. It also balances, but leaves the Tax line always
-- zero and counts GST as product revenue.


-- DEFECT 1: payments ORDER / sales_tax_collected has two rules, neither to Tax.
-- WAS: id 71 -> sales_product_charges, id 72 -> sales_shipping (earlier rule wins)
-- NOW: one rule -> sales_tax
-- WHY: the column is tax, and equals the four settlement tax parts moved in defect 3.

DELETE FROM config_payment
WHERE id = 72 AND transaction_type = 'ORDER' AND description = 'any' AND amount_field = 'sales_tax_collected';

UPDATE config_payment
SET to_summary_field_when_positive_amount = 'sales_tax',
    to_summary_field_when_negative_amount = 'sales_tax'
WHERE id = 71 AND transaction_type = 'ORDER' AND description = 'any' AND amount_field = 'sales_tax_collected';


-- DEFECT 2: payments ORDER / low_value_goods has two rules, neither to Tax.
-- WAS: id 5 -> sales_shipping, id 6 -> sales_product_charges
-- NOW: one rule -> sales_tax
-- WHY: Low Value Goods Tax equals the settlement's LowValueGoodsTax-Principal
--      + -Shipping (-196.62 over 50 rows), both moved in defect 3.

DELETE FROM config_payment
WHERE id = 6 AND transaction_type = 'ORDER' AND description = 'any' AND amount_field = 'low_value_goods';

UPDATE config_payment
SET to_summary_field_when_positive_amount = 'sales_tax',
    to_summary_field_when_negative_amount = 'sales_tax'
WHERE id = 5 AND transaction_type = 'ORDER' AND description = 'any' AND amount_field = 'low_value_goods';


-- DEFECT 3: settlement ORDER tax parts are spread over three Sales fields.
-- WAS: id 65  ItemPrice/Tax                         -> sales_product_charges
--      id 63  ItemPrice/ShippingTax                 -> sales_shipping
--      id 95  Promotion/TaxDiscount                 -> sales_shipping
--      id 61  ItemPrice/GiftWrapTax                 -> sales_other
--      id 119 ItemWithheldTax/LowValueGoodsTax-Principal -> sales_product_charges
--      id 118 ItemWithheldTax/LowValueGoodsTax-Shipping  -> sales_shipping
--      id 127 ItemWithheldTax/LowValueGoodsTax-Other     -> sales_other (no rows this period)
-- NOW: all seven -> sales_tax
-- WHY: together they are exactly the two payments tax columns. Splitting them is
--      what made Product Charges +103.78, Shipping -103.42 and Other -0.36, which
--      net to zero: the money was misfiled, not lost.

UPDATE config_settlement
SET to_summary_field_when_positive_amount = 'sales_tax',
    to_summary_field_when_negative_amount = 'sales_tax'
WHERE transaction_type = 'ORDER' AND id IN (61, 63, 65, 95, 118, 119, 127);


-- DEFECT 4: payments REFUND / sales_tax_collected is never summarised.
-- WAS: id 14, both summary fields empty
-- NOW: refunded_expenses for both signs
-- WHY: the settlement side already books refund tax as refunded_expenses, and the
--      payments column equals those parts exactly (-42.59): the whole +42.59 gap
--      on Refund expenses and on the grand total.

UPDATE config_payment
SET to_summary_field_when_positive_amount = 'refunded_expenses',
    to_summary_field_when_negative_amount = 'refunded_expenses'
WHERE id = 14 AND transaction_type = 'REFUND' AND description = 'any' AND amount_field = 'sales_tax_collected';


-- DEFECTS 5 to 10 do not move any number for this settlement: they fix keys and
-- rules that this period's data does not use, or uses without effect.


-- DEFECT 5: settlement id 138 has a malformed record_ref.
-- WAS: ADJUSTMENT_OTHER+settlement_id+date+record_type+settlement_id
--      (settlement_id twice; record_type is not a token, so it stays literal text)
-- NOW: ADJUSTMENT_OTHER+settlement_id+date, as on the payments ADJUSTMENT / OTHER rule

UPDATE config_settlement
SET record_ref = 'ADJUSTMENT_OTHER+settlement_id+date'
WHERE id = 138 AND amount_description = 'PROMOTION_ADJUSTMENT';


-- DEFECT 6: settlement id 124 has no record_ref (ingest stops if a row matches it).
-- NOW: FAILED_DISBURSEMENT+description+settlement_id+date, as on the payments
--      ADJUSTMENT / FAILED_DISBURSEMENT rule. Summary fields stay empty on both sides.

UPDATE config_settlement
SET record_ref = 'FAILED_DISBURSEMENT+description+settlement_id+date'
WHERE id = 124 AND amount_description LIKE 'TRANSFER_OF_FUNDS_UNSUCCESSFUL%';


-- DEFECT 7: payments TRANSFER rows have no rule for their `other` amount.
-- WAS: the -133,756.51 fell through to the catch-all, whose record_ref
--      txn_ref+settlement_id+date gives "+12395580393+2026-07-18" (no order id).
-- NOW: explicit rules with the same record_ref as the TRANSFER `total` rules,
--      summarised nowhere. Ids from 1001 mark rules added here.

INSERT INTO config_payment (id, transaction_type, description, amount_field, record_ref,
    to_summary_field_when_positive_amount, to_summary_field_when_negative_amount)
VALUES
    (1001, 'TRANSFER', 'TO_ACCOUNT_ENDING',      'other', 'TRANSFER+description+settlement_id+date', '', ''),
    (1002, 'TRANSFER', 'TO_YOUR_ACCOUNT_ENDING', 'other', 'TRANSFER+description+settlement_id+date', '', '');


-- DEFECT 8: AWD storage fee keys differ by one letter.
-- WAS: payments txn_ref+AWD_STORAGE_FEES+...  vs  settlement txn_ref+AWD_STORAGE_FEE+...
-- NOW: payments uses the settlement's singular form, so the keys can meet.

UPDATE config_payment
SET record_ref = 'txn_ref+AWD_STORAGE_FEE+settlement_id+date'
WHERE transaction_type = 'SERVICE_FEE' AND description = 'AWD_STORAGE_FEES'
  AND record_ref = 'txn_ref+AWD_STORAGE_FEES+settlement_id+date';


-- DEFECT 9: one settlement Vine enrolment rule uses an order-style key.
-- WAS: VINE_ENROLLMENT_FEE / VINE_ENROLLMENT_FEE / VINE_ENROLLMENT_FEE -> txn_ref+sku+date,
--      while every other Vine rule, on both sides, uses AMAZON_FEES_VINE_ENROLLMENT_FEE+settlement_id+date.
-- NOW: the same key as the others.

UPDATE config_settlement
SET record_ref = 'AMAZON_FEES_VINE_ENROLLMENT_FEE+settlement_id+date'
WHERE transaction_type = 'VINE_ENROLLMENT_FEE' AND amount_description = 'VINE_ENROLLMENT_FEE'
  AND record_ref = 'txn_ref+sku+date';


-- DEFECT 10: settlement REFUND / ItemPrice/Tax treats the two signs differently.
-- WAS: positive -> total_refund_expense_or_sales_amt, negative -> refunded_expenses
-- NOW: refunded_expenses for both, like its sibling refund rules
-- EFFECT THIS PERIOD: none; all 13 matching rows are negative.

UPDATE config_settlement
SET to_summary_field_when_positive_amount = 'refunded_expenses'
WHERE id = 4 AND transaction_type = 'REFUND' AND amount_type = 'ITEMPRICE' AND amount_description = 'TAX';

COMMIT;
