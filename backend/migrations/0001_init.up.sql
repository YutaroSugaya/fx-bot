-- Initial schema for fx-bot. One-shot squash of the legacy 0001..0007
-- migration history.
--
-- IMPORTANT — Live operations:
--   This file is a paper-mode-only operation. Once Live data exists, this
--   file is FROZEN and all schema changes are forward ALTER migrations
--   (0002+). Re-running this migration on a Live DB will fail (tables
--   already exist) — that is intentional.
--
-- Design principles (ADR-style reasoning lives in DATA_MODEL.md):
--
--   1. Eliminate NULLs in favour of junction tables.
--      A nullable column means "this attribute may or may not be present".
--      For state-dependent attributes (e.g. positions.closing_at only
--      makes sense once CLOSING is reached) we use a junction table
--      instead — its existence encodes the state.
--
--   2. CHECK constraints on every enum-like column.
--      The app-layer guards (port constants, State pattern) are NOT
--      sufficient on their own — raw SQL and manual ops bypass them.
--
--   3. FK every cross-table reference.
--      No more `'manual'` / `'recovered'` sentinel strings in
--      strategy_config_id. Manual / recovered positions live in their
--      own junction tables; the FK column always points to a real
--      strategy_configs.config_id.
--
--   4. State transitions are append-only.
--      position_state_events records every transition; positions.status
--      is the latest-transition cache. The two are kept in sync by
--      writing both rows in one Tx (app-side, not via trigger, so the
--      flow is explicit in the code path).

-- ===========================================================================
-- strategy_configs (main) + 3 junction tables
-- ===========================================================================

CREATE TABLE strategy_configs (
    id                       BIGSERIAL PRIMARY KEY,
    config_id                TEXT NOT NULL UNIQUE,
    source                   TEXT NOT NULL
        CHECK (source IN ('auto', 'manual', 'event', 'fallback')),
    mode                     TEXT NOT NULL
        CHECK (mode IN ('paper_config', 'live_config', 'disabled')),
    symbol                   TEXT NOT NULL,
    enabled                  BOOLEAN NOT NULL,
    -- market_regime_type / strategy_name are nullable because parse-failed
    -- rows still need a strategy_configs entry so signal_rejections /
    -- config_validation_events can FK to it. See
    -- strategy_config_parse_failures for the raw_input audit trail of
    -- those parse failures.
    market_regime_type       TEXT,
    market_regime_confidence DOUBLE PRECISION NOT NULL DEFAULT 0,
    strategy_name            TEXT,
    valid_from               TIMESTAMPTZ NOT NULL,
    valid_until              TIMESTAMPTZ NOT NULL,
    raw_yaml                 TEXT NOT NULL,
    status                   TEXT NOT NULL
        CHECK (status IN ('generated', 'rejected', 'active', 'expired')),
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_strategy_configs_status ON strategy_configs (status);

-- Enforce "at most 1 active config per (symbol, mode)" at the DB level.
-- This used to be migration 0004; squash carries it forward.
CREATE UNIQUE INDEX uniq_strategy_configs_active_per_symbol_mode
    ON strategy_configs (symbol, mode)
    WHERE status = 'active';

-- rejection metadata for status='rejected' rows. 1:1 with strategy_configs
-- but optional (a `generated` row has no entry here).
CREATE TABLE strategy_config_rejections (
    config_id   TEXT PRIMARY KEY
        REFERENCES strategy_configs(config_id) ON DELETE CASCADE,
    reason      TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- activation timestamp for status='active' rows. Promoted into a junction
-- so the main row no longer carries a state-dependent nullable column.
CREATE TABLE strategy_config_activations (
    config_id    TEXT PRIMARY KEY
        REFERENCES strategy_configs(config_id) ON DELETE CASCADE,
    activated_at TIMESTAMPTZ NOT NULL
);

-- parse-failed advisor outputs. The main row gets status='rejected' (with
-- a matching strategy_config_rejections row); raw_input lives here so the
-- audit trail isn't lost.
CREATE TABLE strategy_config_parse_failures (
    config_id   TEXT PRIMARY KEY
        REFERENCES strategy_configs(config_id) ON DELETE CASCADE,
    raw_input   TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ===========================================================================
-- config_validation_events
-- ===========================================================================

CREATE TABLE config_validation_events (
    id              BIGSERIAL PRIMARY KEY,
    config_id       TEXT NOT NULL
        REFERENCES strategy_configs(config_id) ON DELETE CASCADE,
    validation_type TEXT NOT NULL
        CHECK (validation_type IN ('schema', 'hard_limit', 'semantic', 'risk')),
    status          TEXT NOT NULL
        CHECK (status IN ('pass', 'fail')),
    message         TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_validation_events_config_id
    ON config_validation_events (config_id);

-- ===========================================================================
-- market_summaries
-- ===========================================================================

CREATE TABLE market_summaries (
    id             BIGSERIAL PRIMARY KEY,
    symbol         TEXT NOT NULL,
    summary_window TEXT NOT NULL,
    raw_json       JSONB NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_market_summaries_symbol
    ON market_summaries (symbol, created_at DESC);

-- ===========================================================================
-- ai_advisor_runs (main) + 2 junction tables
-- ===========================================================================

CREATE TABLE ai_advisor_runs (
    id          BIGSERIAL PRIMARY KEY,
    run_id      TEXT NOT NULL UNIQUE,
    provider    TEXT NOT NULL,
    mode        TEXT NOT NULL,
    prompt_path TEXT NOT NULL,
    status      TEXT NOT NULL
        CHECK (status IN ('success', 'timeout', 'parse_error', 'cli_error')),
    source      TEXT NOT NULL
        CHECK (source IN ('auto', 'manual', 'event')),
    started_at  TIMESTAMPTZ NOT NULL,
    finished_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_advisor_runs_started ON ai_advisor_runs (started_at DESC);
CREATE INDEX idx_advisor_runs_source_started
    ON ai_advisor_runs (source, started_at DESC);

-- Large payloads split off so listing the main run table doesn't pay the
-- JSONB scan cost. status='success' implies a row exists in advisor_run_io;
-- status in (timeout, parse_error, cli_error) implies a row exists in
-- advisor_run_errors instead.
CREATE TABLE advisor_run_io (
    run_id      TEXT PRIMARY KEY
        REFERENCES ai_advisor_runs(run_id) ON DELETE CASCADE,
    input_json  JSONB NOT NULL,
    output_yaml TEXT NOT NULL
);

CREATE TABLE advisor_run_errors (
    run_id  TEXT PRIMARY KEY
        REFERENCES ai_advisor_runs(run_id) ON DELETE CASCADE,
    message TEXT NOT NULL
);

-- ===========================================================================
-- positions (main) + 3 junction tables + state event ledger
-- ===========================================================================

CREATE TABLE positions (
    id                                  BIGSERIAL PRIMARY KEY,
    symbol                              TEXT NOT NULL,
    side                                TEXT NOT NULL
        CHECK (side IN ('BUY', 'SELL')),
    quantity                            INTEGER NOT NULL,
    entry_price                         DOUBLE PRECISION NOT NULL,
    take_profit_pips                    DOUBLE PRECISION NOT NULL,
    stop_loss_pips                      DOUBLE PRECISION NOT NULL,
    max_hold_minutes                    INTEGER NOT NULL,
    extension_max_minutes               INTEGER NOT NULL DEFAULT 0,
    extension_unrealized_pips_threshold DOUBLE PRECISION NOT NULL DEFAULT 0,
    -- strategy_config_id is now a real FK. Manual / recovered entries
    -- still point to the active config at entry time; the "this was a
    -- manual entry" / "this was recovered" facts live in their own
    -- junction tables (manual_positions, recovered_positions).
    strategy_config_id                  TEXT NOT NULL
        REFERENCES strategy_configs(config_id) ON DELETE RESTRICT,
    -- status is the latest-transition cache of position_state_events.
    -- App layer writes both rows in one Tx; an integration test verifies
    -- the two never drift.
    status                              TEXT NOT NULL
        CHECK (status IN ('OPEN', 'CLOSING', 'CLOSED', 'UNKNOWN')),
    opened_at                           TIMESTAMPTZ NOT NULL,
    created_at                          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_positions_symbol_status ON positions (symbol, status);

-- Broker-side metadata. Populated for every persisted position that has
-- a broker-assigned id (both paper and live — the PaperBroker still
-- assigns an id used at close time). tp_order_id / sl_order_id are
-- populated only by live broker-side OCO settle orders; paper rows store empty strings.
-- The "is this live?" question is answered by strategy_configs.mode, not
-- by the existence of this row.
CREATE TABLE positions_live (
    position_id        BIGINT PRIMARY KEY
        REFERENCES positions(id) ON DELETE CASCADE,
    broker_position_id TEXT NOT NULL,
    tp_order_id        TEXT NOT NULL,
    sl_order_id        TEXT NOT NULL
);

-- "this position was entered via the manual trade command". Replaces the
-- pre-squash `strategy_config_id = 'manual'` sentinel.
CREATE TABLE manual_positions (
    position_id BIGINT PRIMARY KEY
        REFERENCES positions(id) ON DELETE CASCADE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- "this position was recovered by the reconciler from broker state that
-- the DB didn't know about". Replaces the pre-squash
-- `strategy_config_id = 'recovered'` sentinel.
--
-- Live: reconcile adopts an externally-opened broker position here
-- (recovery_reason='external_broker_adoption') when the symbol has an active config; with no
-- config it defers, then trips emergency_stop. Paper reconcile recoveries are recorded here too.
CREATE TABLE recovered_positions (
    position_id     BIGINT PRIMARY KEY
        REFERENCES positions(id) ON DELETE CASCADE,
    recovery_reason TEXT NOT NULL,
    recovered_at    TIMESTAMPTZ NOT NULL
);

-- Append-only ledger of every state transition for a position. Each
-- (position_id, state) pair appears at most once (PK), so a position
-- cannot "re-enter" a state it has already exited — enforces the
-- terminal-CLOSED invariant at the DB level.
CREATE TABLE position_state_events (
    position_id     BIGINT NOT NULL
        REFERENCES positions(id) ON DELETE CASCADE,
    state           TEXT NOT NULL
        CHECK (state IN ('OPEN', 'CLOSING', 'CLOSED', 'UNKNOWN')),
    transitioned_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (position_id, state)
);

CREATE INDEX idx_position_state_events_position_time
    ON position_state_events (position_id, transitioned_at DESC);

-- ===========================================================================
-- trades (main) + 1 junction table
-- ===========================================================================

CREATE TABLE trades (
    id                 BIGSERIAL PRIMARY KEY,
    -- trades are created only at close, so all of these are NOT NULL.
    position_id        BIGINT NOT NULL
        REFERENCES positions(id) ON DELETE RESTRICT,
    strategy_config_id TEXT NOT NULL
        REFERENCES strategy_configs(config_id) ON DELETE RESTRICT,
    symbol             TEXT NOT NULL,
    side               TEXT NOT NULL CHECK (side IN ('BUY', 'SELL')),
    quantity           INTEGER NOT NULL,
    entry_price        DOUBLE PRECISION NOT NULL,
    exit_price         DOUBLE PRECISION NOT NULL,
    profit_loss_pips   DOUBLE PRECISION NOT NULL,
    profit_loss_jpy    DOUBLE PRECISION NOT NULL,
    close_reason       TEXT NOT NULL
        CHECK (close_reason IN ('take_profit', 'stop_loss', 'max_hold', 'manual', 'reconcile_cold_close')),
    opened_at          TIMESTAMPTZ NOT NULL,
    closed_at          TIMESTAMPTZ NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_trades_closed_at ON trades (closed_at DESC);
CREATE INDEX idx_trades_config_id ON trades (strategy_config_id);

-- signal_id is auto-trade only. Manual close-and-record has no
-- strategy-issued signal so no row here. Existence encodes "auto trade".
CREATE TABLE trade_signals (
    trade_id  BIGINT PRIMARY KEY
        REFERENCES trades(id) ON DELETE CASCADE,
    signal_id TEXT NOT NULL
);

-- ===========================================================================
-- signal_rejections
-- ===========================================================================

CREATE TABLE signal_rejections (
    id                 BIGSERIAL PRIMARY KEY,
    -- Pre-squash this was nullable; in the new schema every reject must
    -- name a config (= the active config when the signal would have fired).
    strategy_config_id TEXT NOT NULL
        REFERENCES strategy_configs(config_id) ON DELETE RESTRICT,
    reason             TEXT NOT NULL,
    detail             JSONB,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_signal_rejections_created
    ON signal_rejections (created_at DESC);

-- ===========================================================================
-- candles
-- ===========================================================================

CREATE TABLE candles (
    id          BIGSERIAL PRIMARY KEY,
    symbol      TEXT NOT NULL,
    timeframe   TEXT NOT NULL
        CHECK (timeframe IN ('1m', '5m', '15m', '1h')),
    opened_at   TIMESTAMPTZ NOT NULL,
    open        DOUBLE PRECISION NOT NULL,
    high        DOUBLE PRECISION NOT NULL,
    low         DOUBLE PRECISION NOT NULL,
    close       DOUBLE PRECISION NOT NULL,
    volume      DOUBLE PRECISION NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (symbol, timeframe, opened_at)
);

CREATE INDEX idx_candles_lookup
    ON candles (symbol, timeframe, opened_at DESC);
