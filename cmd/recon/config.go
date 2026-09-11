package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
)

var paymentConfigHeader = []string{"transaction_type", "description", "amount_field", "record_ref",
	"to_summary_field_when_positive_amount", "to_summary_field_when_negative_amount"}

var settlementConfigHeader = []string{"transaction_type", "amount_type", "amount_description", "record_ref",
	"to_summary_field_when_positive_amount", "to_summary_field_when_negative_amount"}

// loadConfig copies both config CSVs into their tables. It runs once: after
// that the tables are edited by MAPPING_FIXES.sql, and reloading would undo
// those fixes.
func loadConfig(ctx context.Context, db *pgx.Conn) error {
	var n int
	if err := db.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM config_payment) + (SELECT count(*) FROM config_settlement)`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return errors.New("config is already loaded (to start over: docker compose down -v)")
	}

	pay, err := readConfigCSV(paymentConfig, paymentConfigHeader)
	if err != nil {
		return err
	}
	set, err := readConfigCSV(settlementConfig, settlementConfigHeader)
	if err != nil {
		return err
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := insertConfig(ctx, tx, "config_payment", paymentConfigHeader, pay); err != nil {
		return err
	}
	if err := insertConfig(ctx, tx, "config_settlement", settlementConfigHeader, set); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	fmt.Printf("loaded %d payment rules and %d settlement rules\n", len(pay), len(set))
	return nil
}

type configRow struct {
	line   int
	fields []string
}

func readConfigCSV(path string, header []string) ([]configRow, error) {
	rd, closeFile, err := openCSV(path, ',')
	if err != nil {
		return nil, err
	}
	defer closeFile()

	got, err := rd.Read()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if !slices.Equal(got, header) {
		return nil, fmt.Errorf("%s: header is %v, expected %v", path, got, header)
	}
	var rows []configRow
	for {
		rec, err := rd.Read()
		if err == io.EOF {
			return rows, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		line, _ := rd.FieldPos(0)
		for i := range rec {
			rec[i] = strings.TrimSpace(rec[i])
		}
		rows = append(rows, configRow{line: line, fields: rec})
	}
}

func insertConfig(ctx context.Context, tx pgx.Tx, table string, header []string, rows []configRow) error {
	sql := fmt.Sprintf("INSERT INTO %s (id, %s) VALUES ($1, $2, $3, $4, $5, $6, $7)", table, strings.Join(header, ", "))
	for _, r := range rows {
		args := []any{r.line}
		for _, f := range r.fields {
			args = append(args, f)
		}
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			return fmt.Errorf("%s line %d: %w", table, r.line, err)
		}
	}
	return nil
}

// openCSV opens a delimited file, skipping a leading UTF-8 byte-order mark.
func openCSV(path string, comma rune) (*csv.Reader, func() error, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	br := bufio.NewReader(f)
	if b, err := br.Peek(3); err == nil && bytes.Equal(b, []byte{0xEF, 0xBB, 0xBF}) {
		br.Discard(3)
	}
	rd := csv.NewReader(br)
	rd.Comma = comma
	return rd, f.Close, nil
}

// rule is one row of either config. Payments rules key on
// (transaction_type, amount_field, description) and settlement rules on
// (transaction_type, amount_type, amount_description), so both fit one shape.
// The three keys are stored normalised.
type rule struct {
	id           int
	typ          string
	field        string
	desc         string
	recordRef    string
	whenPositive string
	whenNegative string
}

// summaryFor picks the summary field by the sign of the amount.
// An empty result means the amount is not summarised.
func (r *rule) summaryFor(amount decimal.Decimal) string {
	if amount.IsNegative() {
		return r.whenNegative
	}
	return r.whenPositive
}

// loadRules reads a config table in file order. keys names the three match
// columns in (type, field, description) order.
func loadRules(ctx context.Context, db *pgx.Conn, table, keys string) ([]rule, error) {
	rows, err := db.Query(ctx, fmt.Sprintf(`SELECT id, %s, record_ref,
		to_summary_field_when_positive_amount, to_summary_field_when_negative_amount
		FROM %s ORDER BY id`, keys, table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rules []rule
	for rows.Next() {
		var r rule
		if err := rows.Scan(&r.id, &r.typ, &r.field, &r.desc, &r.recordRef, &r.whenPositive, &r.whenNegative); err != nil {
			return nil, err
		}
		r.typ, r.field, r.desc = norm(r.typ), norm(r.field), norm(r.desc)
		rules = append(rules, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(rules) == 0 {
		return nil, fmt.Errorf("%s is empty (run: go run ./cmd/recon load-config)", table)
	}
	return rules, nil
}

// norm puts a value into the form the configs use: upper case, with each run of
// whitespace replaced by "_". "Fulfilment by Amazon transaction fees" becomes
// "FULFILMENT_BY_AMAZON_TRANSACTION_FEES". Punctuation is kept because it tells
// rules apart ("FBA_REMOVAL_ORDER:_DISPOSAL_FEE").
func norm(s string) string {
	return strings.Join(strings.Fields(strings.ToUpper(s)), "_")
}

// matcher finds the rule for an amount, in this order:
//
//  1. exact type, field and description
//  2. exact type and field, description "any"
//  3. exact type and field, the longest rule description the value starts with
//     (payments says "To account ending with: 334", the config says
//     "TO_ACCOUNT_ENDING")
//  4. empty transaction_type: the catch-all rules
//
// When two rules tie, the one earlier in the file wins.
type matcher struct {
	rules []rule
	cache map[string]*rule
}

func newMatcher(rules []rule) *matcher {
	return &matcher{rules: rules, cache: map[string]*rule{}}
}

func (m *matcher) match(typ, field, desc string) *rule {
	key := typ + "\x00" + field + "\x00" + desc
	if r, ok := m.cache[key]; ok {
		return r
	}
	r := m.find(typ, field, desc)
	m.cache[key] = r
	return r
}

func (m *matcher) find(typ, field, desc string) *rule {
	for i := range m.rules {
		if r := &m.rules[i]; r.typ == typ && r.field == field && r.desc == desc {
			return r
		}
	}
	for i := range m.rules {
		if r := &m.rules[i]; r.typ == typ && r.field == field && r.desc == "ANY" {
			return r
		}
	}
	var best *rule
	for i := range m.rules {
		r := &m.rules[i]
		if r.typ == typ && r.field == field && r.desc != "" && strings.HasPrefix(desc, r.desc) &&
			(best == nil || len(r.desc) > len(best.desc)) {
			best = r
		}
	}
	if best != nil {
		return best
	}
	for i := range m.rules {
		if r := &m.rules[i]; r.typ == "" && r.field == field && (r.desc == "ANY" || r.desc == desc) {
			return r
		}
	}
	return nil
}

// tokenNames are the record_ref parts filled in from the row. Every other part
// of a template is literal text.
var tokenNames = map[string]bool{
	"txn_ref": true, "sku": true, "settlement_id": true, "date": true,
	"description": true, "shipment_id": true, "merchant_order_id": true,
}

// buildRecordRef fills a "+"-joined template such as "txn_ref+sku+date" from
// the row's tokens. A token the row does not have is an error.
func buildRecordRef(template string, tokens map[string]string) (string, error) {
	parts := strings.Split(template, "+")
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if tokenNames[p] {
			v, ok := tokens[p]
			if !ok {
				return "", fmt.Errorf("record_ref %q uses %q, which this file does not have", template, p)
			}
			p = v
		}
		parts[i] = p
	}
	return strings.Join(parts, "+"), nil
}
