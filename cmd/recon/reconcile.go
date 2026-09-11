package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// reconcile matches the two files on record_ref, within the settlement being
// reconciled. The files differ in granularity (one payments row can be several
// settlement rows), so each side is summed per record_ref before comparing.
// The payments `total` column is left out: it is the sum of that row's other
// columns and would count the money twice.
const reconcileSQL = `
INSERT INTO reconciliation (record_ref, status, payment_amount, settlement_amount, difference)
SELECT record_ref,
       CASE WHEN bool_or(source = 'payment') AND bool_or(source = 'settlement') THEN 'reconciled'
            WHEN bool_or(source = 'payment') THEN 'unreconciled_payment'
            ELSE 'unreconciled_settlement' END,
       COALESCE(SUM(amount) FILTER (WHERE source = 'payment'), 0),
       COALESCE(SUM(amount) FILTER (WHERE source = 'settlement'), 0),
       COALESCE(SUM(amount) FILTER (WHERE source = 'payment'), 0)
     - COALESCE(SUM(amount) FILTER (WHERE source = 'settlement'), 0)
FROM records
WHERE in_scope AND NOT (source = 'payment' AND amount_type = 'total')
GROUP BY record_ref`

func reconcile(ctx context.Context, db *pgx.Conn) error {
	var n int
	if err := db.QueryRow(ctx, "SELECT count(*) FROM records").Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return errors.New("no records (run: go run ./cmd/recon ingest)")
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "TRUNCATE reconciliation"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, reconcileSQL); err != nil {
		return fmt.Errorf("reconcile: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}

	counts := map[string]int{}
	rows, err := db.Query(ctx, "SELECT status, count(*) FROM reconciliation GROUP BY status")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var c int
		if err := rows.Scan(&status, &c); err != nil {
			return err
		}
		counts[status] = c
	}
	if err := rows.Err(); err != nil {
		return err
	}
	fmt.Printf("reconciled:               %d\n", counts["reconciled"])
	fmt.Printf("unreconciled payments:    %d\n", counts["unreconciled_payment"])
	fmt.Printf("unreconciled settlements: %d\n", counts["unreconciled_settlement"])
	return nil
}
