package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
	"github.com/xuri/excelize/v2"
)

// The Summary sheet, laid out as in amazon_sample_output_report.xlsx. Each line
// shows one summary field; a heading shows the sum of its lines. Lines with no
// field are ones no config rule routes to, so they are always zero.
type summaryLine struct {
	row   int
	label string
	field string
}

var summaryBlocks = []struct {
	row     int
	heading string
	lines   []summaryLine
}{
	{4, "Sales", []summaryLine{
		{5, "Product Charges", "sales_product_charges"},
		{6, "Tax", "sales_tax"},
		{7, "Shipping", "sales_shipping"},
		{8, "Amazon fees", "sales_amazon_fees"},
		{9, "Inventory Reimbursements", "sales_inventory_reimbursements"},
		{10, "Cross-account Debt Adjustment", ""},
		{11, "Other", "sales_other"},
		{12, "FBA Fees", ""},
		{13, "Micro Deposit (Failed)", ""},
	}},
	{15, "Refunds", []summaryLine{
		{16, "Refund expenses", "refunded_expenses"},
		{17, "Refunded sales", "refunded_sales"},
	}},
	{19, "Expenses", []summaryLine{
		{20, "Promo rebates", "expenses_promotional_rebates"},
		{21, "FBA fees", "expenses_fba_fees"},
		{22, "Cost of Advertising", "expenses_cost_of_advertising"},
		{23, "Shipping Charges", ""},
		{24, "Amazon fees", "expenses_amazon_fees"},
		{25, "Reversed Reimbursements", "expenses_reversed_reimbursements"},
		{26, "Cross-account Debt Adjustment", ""},
		{27, "Other", "expenses_other"},
		{28, "Micro Deposit", "bank_account_transfer_round_off"},
	}},
	{0, "", []summaryLine{
		{32, "Paid To Amazon", "paid_to_amazon"},
	}},
}

const totalRow = 34

func report(ctx context.Context, db *pgx.Conn, out string) error {
	var n int
	if err := db.QueryRow(ctx, "SELECT count(*) FROM reconciliation").Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return errors.New("nothing reconciled (run: go run ./cmd/recon reconcile)")
	}

	amounts, err := loadSummary(ctx, db)
	if err != nil {
		return err
	}

	f := excelize.NewFile()
	defer f.Close()
	if err := f.SetSheetName("Sheet1", "Summary"); err != nil {
		return err
	}
	diffs, err := writeSummary(f, amounts)
	if err != nil {
		return err
	}
	if _, err := f.NewSheet("Consolidated Data"); err != nil {
		return err
	}
	if err := writeConsolidated(ctx, db, f); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	if err := f.SaveAs(out); err != nil {
		return err
	}

	fmt.Println("wrote", out)
	if len(diffs) == 0 {
		fmt.Println("every Summary line matches")
		return nil
	}
	fmt.Println("Summary lines that differ (payments - settlements):")
	for _, d := range diffs {
		fmt.Println("  " + d)
	}
	return nil
}

// loadSummary reads the summary built during ingest, and fails if a summarised
// field has no line on the Summary sheet.
func loadSummary(ctx context.Context, db *pgx.Conn) (map[string]map[string]decimal.Decimal, error) {
	known := map[string]bool{}
	for _, b := range summaryBlocks {
		for _, l := range b.lines {
			if l.field != "" {
				known[l.field] = true
			}
		}
	}
	amounts := map[string]map[string]decimal.Decimal{"payment": {}, "settlement": {}}
	rows, err := db.Query(ctx, "SELECT source, summary_field, amount FROM summary")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var source, field string
		var amount decimal.Decimal
		if err := rows.Scan(&source, &field, &amount); err != nil {
			return nil, err
		}
		if !known[field] {
			return nil, fmt.Errorf("summary field %q (from the %s config) has no line on the Summary sheet", field, source)
		}
		amounts[source][field] = amount
	}
	return amounts, rows.Err()
}

func writeSummary(f *excelize.File, amounts map[string]map[string]decimal.Decimal) ([]string, error) {
	const sheet = "Summary"
	money, err := f.NewStyle(&excelize.Style{NumFmt: 4})
	if err != nil {
		return nil, err
	}
	bold, err := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}, NumFmt: 4})
	if err != nil {
		return nil, err
	}
	f.SetColWidth(sheet, "B", "B", 32)
	f.SetColWidth(sheet, "C", "E", 22)
	f.SetSheetRow(sheet, "C1", &[]any{"Payments", "Settlements", "Payments - Settlements"})

	put := func(row int, label string, pay, set decimal.Decimal, style int) {
		f.SetSheetRow(sheet, fmt.Sprintf("B%d", row), &[]any{
			label, pay.InexactFloat64(), set.InexactFloat64(), pay.Sub(set).InexactFloat64()})
		f.SetCellStyle(sheet, fmt.Sprintf("B%d", row), fmt.Sprintf("E%d", row), style)
	}

	var diffs []string
	var totalPay, totalSet decimal.Decimal
	for _, b := range summaryBlocks {
		var blockPay, blockSet decimal.Decimal
		for _, l := range b.lines {
			pay, set := amounts["payment"][l.field], amounts["settlement"][l.field]
			put(l.row, l.label, pay, set, money)
			blockPay, blockSet = blockPay.Add(pay), blockSet.Add(set)
			if !pay.Equal(set) {
				diffs = append(diffs, fmt.Sprintf("%-26s %s", l.label, pay.Sub(set).StringFixed(2)))
			}
		}
		if b.heading != "" {
			put(b.row, b.heading, blockPay, blockSet, bold)
		}
		totalPay, totalSet = totalPay.Add(blockPay), totalSet.Add(blockSet)
	}
	put(totalRow, "Total", totalPay, totalSet, bold)
	return diffs, nil
}

// One row per record_ref: reconciled first, then unreconciled payments, then
// unreconciled settlements. The line columns point back to the input files.
const consolidatedSQL = `
SELECT r.status, r.record_ref,
       string_agg(DISTINCT d.settlement_id, ', '),
       min(d.record_date)::text,
       COALESCE(string_agg(DISTINCT NULLIF(d.sku, ''), ', '), ''),
       COALESCE(string_agg(DISTINCT d.transaction_type, ', ') FILTER (WHERE d.source = 'payment'), ''),
       COALESCE(string_agg(DISTINCT d.amount_type, ', ') FILTER (WHERE d.source = 'payment'), ''),
       COALESCE(string_agg(DISTINCT NULLIF(d.summary_field, ''), ', ') FILTER (WHERE d.source = 'payment'), ''),
       r.payment_amount,
       COALESCE(string_agg(DISTINCT d.transaction_type, ', ') FILTER (WHERE d.source = 'settlement'), ''),
       COALESCE(string_agg(DISTINCT d.amount_type || ' / ' || d.description, ', ') FILTER (WHERE d.source = 'settlement'), ''),
       COALESCE(string_agg(DISTINCT NULLIF(d.summary_field, ''), ', ') FILTER (WHERE d.source = 'settlement'), ''),
       r.settlement_amount,
       r.difference,
       COALESCE(string_agg(DISTINCT d.line_no::text, ', ') FILTER (WHERE d.source = 'payment'), ''),
       COALESCE(string_agg(DISTINCT d.line_no::text, ', ') FILTER (WHERE d.source = 'settlement'), '')
FROM reconciliation r
JOIN records d ON d.record_ref = r.record_ref
             AND d.in_scope AND NOT (d.source = 'payment' AND d.amount_type = 'total')
GROUP BY r.status, r.record_ref, r.payment_amount, r.settlement_amount, r.difference
ORDER BY CASE r.status WHEN 'reconciled' THEN 1 WHEN 'unreconciled_payment' THEN 2 ELSE 3 END, r.record_ref`

func writeConsolidated(ctx context.Context, db *pgx.Conn, f *excelize.File) error {
	sw, err := f.NewStreamWriter("Consolidated Data")
	if err != nil {
		return err
	}
	if err := sw.SetColWidth(1, 16, 20); err != nil {
		return err
	}
	if err := sw.SetRow("A1", []any{
		"Status", "record_ref", "Settlement ID", "Date", "SKU",
		"Payment type", "Payment amount columns", "Payment summary fields", "Payment amount",
		"Settlement type", "Settlement amount types", "Settlement summary fields", "Settlement amount",
		"Difference (payment - settlement)",
		"Payment lines (amazon_payments_data.csv)", "Settlement lines (amazon_settlements_data.txt)",
	}); err != nil {
		return err
	}

	rows, err := db.Query(ctx, consolidatedSQL)
	if err != nil {
		return err
	}
	defer rows.Close()
	row := 2
	for rows.Next() {
		var status, ref, settlementIDs, date, skus string
		var payType, payCols, payFields, setType, setCols, setFields, payLines, setLines string
		var payAmount, setAmount, diff decimal.Decimal
		if err := rows.Scan(&status, &ref, &settlementIDs, &date, &skus,
			&payType, &payCols, &payFields, &payAmount,
			&setType, &setCols, &setFields, &setAmount, &diff, &payLines, &setLines); err != nil {
			return err
		}
		cell, _ := excelize.CoordinatesToCellName(1, row)
		if err := sw.SetRow(cell, []any{status, ref, settlementIDs, date, skus,
			payType, payCols, payFields, payAmount.InexactFloat64(),
			setType, setCols, setFields, setAmount.InexactFloat64(), diff.InexactFloat64(),
			payLines, setLines}); err != nil {
			return err
		}
		row++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return sw.Flush()
}
