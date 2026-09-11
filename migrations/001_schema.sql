-- Mapping configs, loaded as-is from the two CSVs.
-- `id` is the rule's line number in its CSV, so MAPPING_FIXES.sql can name the
-- exact rule it changes. Fixes are data changes against these two tables.

CREATE TABLE config_payment (
    id                                    INT PRIMARY KEY,
    transaction_type                      TEXT NOT NULL,
    description                           TEXT NOT NULL,
    amount_field                          TEXT NOT NULL,
    record_ref                            TEXT NOT NULL,
    to_summary_field_when_positive_amount TEXT NOT NULL,
    to_summary_field_when_negative_amount TEXT NOT NULL
);

CREATE TABLE config_settlement (
    id                                    INT PRIMARY KEY,
    transaction_type                      TEXT NOT NULL,
    amount_type                           TEXT NOT NULL,
    amount_description                    TEXT NOT NULL,
    record_ref                            TEXT NOT NULL,
    to_summary_field_when_positive_amount TEXT NOT NULL,
    to_summary_field_when_negative_amount TEXT NOT NULL
);

-- Both data files in one table, one row per amount.
-- A payments row has its money spread across columns (product sales, fees, ...),
-- so it becomes one row here per non-zero money column, plus its `total` even when
-- zero, so every line is stored. A settlement row already holds a single amount.
-- `source_file`, `line_no` and `raw` trace every row back to the exact input line.

CREATE TABLE records (
    id               BIGSERIAL PRIMARY KEY,
    source           TEXT NOT NULL CHECK (source IN ('payment', 'settlement')),
    source_file      TEXT NOT NULL,
    line_no          INT NOT NULL,
    raw              JSONB NOT NULL,
    settlement_id    TEXT NOT NULL,
    transaction_type TEXT NOT NULL,
    amount_type      TEXT NOT NULL,   -- payments: money column; settlement: amount-type
    description      TEXT NOT NULL,   -- payments: description; settlement: amount-description
    sku              TEXT NOT NULL,
    record_date      DATE NOT NULL,   -- UTC
    amount           NUMERIC(14, 2) NOT NULL,
    config_id        INT NOT NULL,    -- the rule that matched (id in config_payment or config_settlement)
    record_ref       TEXT NOT NULL,
    summary_field    TEXT NOT NULL,   -- '' = not summarised
    in_scope         BOOLEAN NOT NULL -- belongs to the settlement being reconciled
);

-- The accounting summary, one row per source and summary field, built during ingest.
CREATE TABLE summary (
    source        TEXT NOT NULL,
    summary_field TEXT NOT NULL,
    amount        NUMERIC(14, 2) NOT NULL,
    PRIMARY KEY (source, summary_field)
);

-- One row per record_ref.
CREATE TABLE reconciliation (
    record_ref        TEXT PRIMARY KEY,
    status            TEXT NOT NULL
                      CHECK (status IN ('reconciled', 'unreconciled_payment', 'unreconciled_settlement')),
    payment_amount    NUMERIC(14, 2) NOT NULL,
    settlement_amount NUMERIC(14, 2) NOT NULL,
    difference        NUMERIC(14, 2) NOT NULL
);
