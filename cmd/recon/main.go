// Command recon reconciles Amazon's Payments report against its Settlement
// report for one settlement period. README.md lists the exact steps.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/jackc/pgx/v5"
)

// The four input files, exactly as supplied.
const (
	dataDir          = "Assignment_050726_files"
	paymentsFile     = dataDir + "/amazon_payments_data.csv"
	settlementFile   = dataDir + "/amazon_settlements_data.txt"
	paymentConfig    = dataDir + "/amazon_payment_configs_au_old.csv"
	settlementConfig = dataDir + "/amazon_settlement_configs_au.csv"

	schemaFile = "migrations/001_schema.sql"
	defaultDSN = "postgres://recon:recon@localhost:5433/recon?sslmode=disable"
)

const usage = `usage: go run ./cmd/recon <command>

commands:
  migrate       create the tables (once)
  load-config   load both mapping configs into the database (once)
  ingest        load both data files, apply the configs, build the summary
  reconcile     match payment and settlement records on record_ref
  report        write the Excel report     [--out out/report.xlsx]
  all           ingest, reconcile, report  [--out out/report.xlsx]`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("no command given\n\n" + usage)
	}
	cmd := args[0]
	switch cmd {
	case "migrate", "load-config", "ingest", "reconcile", "report", "all":
	default:
		return fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
	}

	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var out string
	if cmd == "report" || cmd == "all" {
		fs.StringVar(&out, "out", "out/report.xlsx", "")
	}
	if err := fs.Parse(args[1:]); err != nil {
		return fmt.Errorf("%s: %w", cmd, err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("%s: unexpected argument %q", cmd, fs.Arg(0))
	}

	ctx := context.Background()
	dsn := os.Getenv("RECON_DSN")
	if dsn == "" {
		dsn = defaultDSN
	}
	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("cannot connect to Postgres (start it with: docker compose up -d --wait): %w", err)
	}
	defer db.Close(ctx)

	switch cmd {
	case "migrate":
		return migrate(ctx, db)
	case "load-config":
		return loadConfig(ctx, db)
	case "ingest":
		return ingest(ctx, db)
	case "reconcile":
		return reconcile(ctx, db)
	case "report":
		return report(ctx, db, out)
	}
	// all
	if err := ingest(ctx, db); err != nil {
		return err
	}
	if err := reconcile(ctx, db); err != nil {
		return err
	}
	return report(ctx, db, out)
}

func migrate(ctx context.Context, db *pgx.Conn) error {
	sql, err := os.ReadFile(schemaFile)
	if err != nil {
		return fmt.Errorf("run from the repository root: %w", err)
	}
	if _, err := db.Exec(ctx, string(sql)); err != nil {
		return fmt.Errorf("migrate: %w (to start over: docker compose down -v)", err)
	}
	fmt.Println("tables created")
	return nil
}
