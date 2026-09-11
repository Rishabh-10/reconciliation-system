package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
)

// record is one amount from either file.
type record struct {
	source       string // "payment" or "settlement"
	file         string
	line         int
	raw          map[string]string
	settlementID string
	txnType      string
	amountType   string // payments: money column (config amount_field); settlement: amount-type
	description  string // payments: description; settlement: amount-description
	sku          string
	date         time.Time
	amount       decimal.Decimal
	status       string            // payments only: Released or Deferred
	tokens       map[string]string // values for record_ref templates

	configID     int
	recordRef    string
	summaryField string
	inScope      bool
}

// The payments money columns and the config amount_field each maps to.
var paymentMoneyColumns = []struct{ column, field string }{
	{"product sales", "product_sales"},
	{"shipping credits", "shipping_credits"},
	{"gift wrap credits", "gift_wrap_credits"},
	{"promotional rebates", "promotional_rebates"},
	{"sales tax collected", "sales_tax_collected"},
	{"low value goods", "low_value_goods"},
	{"selling fees", "selling_fees"},
	{"fulfilment by amazon fees", "fba_fees"},
	{"other transaction fees", "other_transaction_fees"},
	{"other", "other"},
	{"total", "total"},
}

func ingest(ctx context.Context, db *pgx.Conn) error {
	payRules, err := loadRules(ctx, db, "config_payment", "transaction_type, amount_field, description")
	if err != nil {
		return err
	}
	setRules, err := loadRules(ctx, db, "config_settlement", "transaction_type, amount_type, amount_description")
	if err != nil {
		return err
	}

	payments, err := readPayments(paymentsFile)
	if err != nil {
		return err
	}
	settlements, settlementID, payout, err := readSettlement(settlementFile)
	if err != nil {
		return err
	}
	if err := applyRules(payments, newMatcher(payRules), "payment config"); err != nil {
		return err
	}
	if err := applyRules(settlements, newMatcher(setRules), "settlement config"); err != nil {
		return err
	}

	// Scope is the settlement in the settlement file. A payments row belongs to
	// it only once released into it; Deferred rows have not been yet.
	// The summary is built here, in the same pass, per source.
	all := append(payments, settlements...)
	summary := map[[2]string]decimal.Decimal{}
	totals := map[string]decimal.Decimal{}
	for i := range all {
		r := &all[i]
		r.inScope = r.settlementID == settlementID && (r.source == "settlement" || r.status == "Released")
		if r.inScope && r.summaryField != "" {
			k := [2]string{r.source, r.summaryField}
			summary[k] = summary[k].Add(r.amount)
			totals[r.source] = totals[r.source].Add(r.amount)
		}
	}

	if err := saveRecords(ctx, db, all, summary); err != nil {
		return err
	}
	fmt.Printf("ingested %d payment amounts and %d settlement amounts\n", len(payments), len(settlements))
	fmt.Printf("settlement %s: Amazon's stated payout  %s\n", settlementID, payout.StringFixed(2))
	fmt.Printf("summary total, payments:              %s\n", totals["payment"].StringFixed(2))
	fmt.Printf("summary total, settlements:           %s\n", totals["settlement"].StringFixed(2))
	return nil
}

func applyRules(recs []record, m *matcher, config string) error {
	for i := range recs {
		r := &recs[i]
		rl := m.match(norm(r.txnType), norm(r.amountType), norm(r.description))
		if rl == nil {
			return fmt.Errorf("%s line %d: no rule in the %s matches type %q, amount %q, description %q",
				r.file, r.line, config, r.txnType, r.amountType, r.description)
		}
		if rl.recordRef == "" {
			return fmt.Errorf("%s rule %d has an empty record_ref (matched by %s line %d)", config, rl.id, r.file, r.line)
		}
		ref, err := buildRecordRef(rl.recordRef, r.tokens)
		if err != nil {
			return fmt.Errorf("%s rule %d, %s line %d: %w", config, rl.id, r.file, r.line, err)
		}
		r.configID = rl.id
		r.recordRef = ref
		r.summaryField = rl.summaryFor(r.amount)
	}
	return nil
}

// saveRecords replaces the records and the summary, so ingest can be re-run
// after a config fix.
func saveRecords(ctx context.Context, db *pgx.Conn, recs []record, summary map[[2]string]decimal.Decimal) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, "TRUNCATE records, summary, reconciliation RESTART IDENTITY"); err != nil {
		return err
	}
	rows := make([][]any, 0, len(recs))
	for _, r := range recs {
		raw, err := json.Marshal(r.raw)
		if err != nil {
			return err
		}
		rows = append(rows, []any{r.source, r.file, r.line, raw, r.settlementID, r.txnType, r.amountType,
			r.description, r.sku, r.date, r.amount, r.configID, r.recordRef, r.summaryField, r.inScope})
	}
	cols := []string{"source", "source_file", "line_no", "raw", "settlement_id", "transaction_type", "amount_type",
		"description", "sku", "record_date", "amount", "config_id", "record_ref", "summary_field", "in_scope"}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"records"}, cols, pgx.CopyFromRows(rows)); err != nil {
		return fmt.Errorf("save records: %w", err)
	}
	for k, amount := range summary {
		if _, err := tx.Exec(ctx, "INSERT INTO summary (source, summary_field, amount) VALUES ($1, $2, $3)",
			k[0], k[1], amount); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// readPayments reads the payments CSV. The file opens with a few lines of notes;
// the header is the first row containing "settlement ID". Each non-zero money
// column becomes its own record, and `total` always does.
func readPayments(path string) ([]record, error) {
	rd, closeFile, err := openCSV(path, ',')
	if err != nil {
		return nil, err
	}
	defer closeFile()
	rd.FieldsPerRecord = -1 // the notes before the header have one column

	required := []string{"date/time", "settlement id", "type", "order id", "sku", "description",
		"transaction status", "transaction release date"}
	for _, m := range paymentMoneyColumns {
		required = append(required, m.column)
	}

	var header []string
	col := map[string]int{}
	var out []record
	for {
		rec, err := rd.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		line, _ := rd.FieldPos(0)

		if header == nil {
			if !slices.Contains(rec, "settlement ID") {
				continue
			}
			header = rec
			for i, h := range rec {
				col[strings.ToLower(strings.TrimSpace(h))] = i
			}
			for _, name := range required {
				if _, ok := col[name]; !ok {
					return nil, fmt.Errorf("%s: missing column %q", path, name)
				}
			}
			continue
		}
		if len(rec) != len(header) {
			return nil, fmt.Errorf("%s line %d: %d columns, expected %d", path, line, len(rec), len(header))
		}
		get := func(name string) string { return strings.TrimSpace(rec[col[name]]) }

		// The date that lines up with the settlement report is the release
		// date, converted to UTC. Deferred rows have no release date yet, so
		// they fall back to the posted date.
		when := get("transaction release date")
		if when == "" {
			when = get("date/time")
		}
		date, err := parsePaymentTime(when)
		if err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, line, err)
		}

		raw := make(map[string]string, len(header))
		for i, h := range header {
			raw[h] = rec[i]
		}
		orderID := get("order id")
		base := record{
			source: "payment", file: filepath.Base(path), line: line, raw: raw,
			settlementID: get("settlement id"), txnType: get("type"), description: get("description"),
			sku: get("sku"), date: date, status: get("transaction status"),
		}
		base.tokens = map[string]string{
			"txn_ref": orderID, "merchant_order_id": orderID, "sku": base.sku,
			"settlement_id": base.settlementID, "date": date.Format("2006-01-02"),
			"description": norm(base.description),
		}
		for _, m := range paymentMoneyColumns {
			amount, err := parseAmount(get(m.column))
			if err != nil {
				return nil, fmt.Errorf("%s line %d, column %q: %w", path, line, m.column, err)
			}
			// Zero amounts are skipped, except `total`: keeping it means every
			// payments line is stored, even one whose money columns are all zero.
			if amount.IsZero() && m.field != "total" {
				continue
			}
			r := base
			r.amountType = m.field
			r.amount = amount
			out = append(out, r)
		}
	}
	if header == nil {
		return nil, fmt.Errorf("%s: no header row (a row containing \"settlement ID\")", path)
	}
	return out, nil
}

// readSettlement reads the tab-separated settlement report. Its first data row
// is Amazon's statement header: no transaction-type, but the settlement id and
// the total payout.
func readSettlement(path string) (recs []record, settlementID string, payout decimal.Decimal, err error) {
	rd, closeFile, err := openCSV(path, '\t')
	if err != nil {
		return nil, "", payout, err
	}
	defer closeFile()

	header, err := rd.Read()
	if err != nil {
		return nil, "", payout, fmt.Errorf("%s: %w", path, err)
	}
	col := map[string]int{}
	for i, h := range header {
		col[strings.TrimSpace(h)] = i
	}
	for _, name := range []string{"settlement-id", "total-amount", "transaction-type", "order-id",
		"merchant-order-id", "adjustment-id", "shipment-id", "amount-type", "amount-description",
		"amount", "posted-date-time", "sku"} {
		if _, ok := col[name]; !ok {
			return nil, "", payout, fmt.Errorf("%s: missing column %q", path, name)
		}
	}

	for {
		rec, err := rd.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", payout, fmt.Errorf("%s: %w", path, err)
		}
		line, _ := rd.FieldPos(0)
		get := func(name string) string { return strings.TrimSpace(rec[col[name]]) }

		if get("transaction-type") == "" {
			if settlementID != "" || get("total-amount") == "" {
				return nil, "", payout, fmt.Errorf("%s line %d: row has no transaction-type", path, line)
			}
			settlementID = get("settlement-id")
			if payout, err = parseAmount(get("total-amount")); err != nil {
				return nil, "", payout, fmt.Errorf("%s line %d: %w", path, line, err)
			}
			continue
		}

		amount, err := parseAmount(get("amount"))
		if err != nil {
			return nil, "", payout, fmt.Errorf("%s line %d: %w", path, line, err)
		}
		date, err := time.Parse("02.01.2006 15:04:05 MST", get("posted-date-time"))
		if err != nil {
			return nil, "", payout, fmt.Errorf("%s line %d: posted-date-time: %w", path, line, err)
		}
		date = date.UTC()

		txnRef := get("order-id")
		for _, alt := range []string{"adjustment-id", "shipment-id", "merchant-order-id"} {
			if txnRef == "" {
				txnRef = get(alt)
			}
		}
		raw := make(map[string]string, len(header))
		for i, h := range header {
			raw[h] = rec[i]
		}
		r := record{
			source: "settlement", file: filepath.Base(path), line: line, raw: raw,
			settlementID: get("settlement-id"), txnType: get("transaction-type"),
			amountType: get("amount-type"), description: get("amount-description"),
			sku: get("sku"), date: date, amount: amount,
		}
		r.tokens = map[string]string{
			"txn_ref": txnRef, "sku": r.sku, "settlement_id": r.settlementID,
			"date": date.Format("2006-01-02"), "description": norm(r.description),
			"shipment_id": get("shipment-id"), "merchant_order_id": get("merchant-order-id"),
		}
		recs = append(recs, r)
	}
	if settlementID == "" {
		return nil, "", payout, fmt.Errorf("%s: no statement header row (a row with total-amount)", path)
	}
	return recs, settlementID, payout, nil
}

// parseAmount reads a money value; payments writes thousands separators ("-133,756.51").
func parseAmount(s string) (decimal.Decimal, error) {
	return decimal.NewFromString(strings.ReplaceAll(strings.TrimSpace(s), ",", ""))
}

// parsePaymentTime reads "9 July 2026 9:08:05 am GMT+9" (or "1 Aug 2026 ...":
// the file uses both month forms) and returns it in UTC.
func parsePaymentTime(s string) (time.Time, error) {
	i := strings.LastIndex(s, " GMT")
	if i < 0 {
		return time.Time{}, fmt.Errorf("date %q has no GMT offset", s)
	}
	hours, err := strconv.Atoi(s[i+4:])
	if err != nil {
		return time.Time{}, fmt.Errorf("date %q: bad GMT offset", s)
	}
	loc := time.FixedZone("", hours*3600)
	for _, layout := range []string{"2 January 2006 3:04:05 pm", "2 Jan 2006 3:04:05 pm"} {
		if t, err := time.ParseInLocation(layout, s[:i], loc); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot read date %q", s)
}
