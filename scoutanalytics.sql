-- scoutanalytics schema (PostgreSQL 12+). Idempotent: safe to run on every start.
-- Applied automatically when SCOUT_DATABASE_URL is set
-- (disable with SCOUT_DB_AUTO_MIGRATE=false and run this file by hand instead).
--
--   scout_calls ──< scout_investigations >── scout_investigation_tools
--        │  │               │
--        │  └──< scout_deliveries ──< scout_delivery_investigations
--        ├── scout_call_metrics (1:1)   └── scout_call_live_buys (1:n)
--        ├── scout_call_tracking (1:1)  └── scout_call_returns (1:n, per horizon)
--
-- Adding an investigation tool = one row in scout_investigation_tools
-- (inserted automatically from SCOUT_TOOLS); no schema change.

-- Registry of investigation tools (Telegram bots that return a report for a CA).
CREATE TABLE IF NOT EXISTS scout_investigation_tools (
    id                SERIAL PRIMARY KEY,
    code              TEXT         NOT NULL UNIQUE,          -- perceptor | salpha | …
    name              TEXT         NOT NULL,
    bot_username      TEXT         NOT NULL,
    command_template  TEXT         NOT NULL,                 -- e.g. "/scan {ca}" or "{ca}"
    parser            TEXT         NOT NULL DEFAULT 'text',  -- perceptor | text
    is_gate           BOOLEAN      NOT NULL DEFAULT false,   -- verdict decides delivery
    is_active         BOOLEAN      NOT NULL DEFAULT true,
    created_by        TEXT         NOT NULL,
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_by        TEXT         NOT NULL,
    updated_at        TIMESTAMPTZ  NOT NULL DEFAULT now()
);

-- One row per contract address found in a post of the watched channel
-- (e.g. @scoutrobinhood). A post with 2 CAs = 2 rows. Re-calls of a CA that was
-- already investigated are still recorded, with status = 'duplicate'.
CREATE TABLE IF NOT EXISTS scout_calls (
    id                SERIAL PRIMARY KEY,
    uuid              UUID         NOT NULL UNIQUE,
    channel_id        BIGINT       NOT NULL,
    channel_username  TEXT         NOT NULL,
    message_id        INTEGER      NOT NULL,
    message_date      TIMESTAMPTZ  NOT NULL,
    message_text      TEXT         NOT NULL DEFAULT '',
    urls              TEXT[]       NOT NULL DEFAULT '{}',
    contract_address  TEXT         NOT NULL,
    chain             TEXT         NOT NULL,               -- evm | solana
    status            TEXT         NOT NULL,               -- queued | duplicate | dropped | scanned | failed
    created_by        TEXT         NOT NULL,
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_by        TEXT         NOT NULL,
    updated_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT scout_calls_msg_ca_uq UNIQUE (channel_id, message_id, contract_address)
);
CREATE INDEX IF NOT EXISTS scout_calls_ca_idx   ON scout_calls (contract_address);
CREATE INDEX IF NOT EXISTS scout_calls_date_idx ON scout_calls (message_date DESC);

-- Data parsed from the call post (MCap, Liq, Tax, Age, Holders, Proof, …), one row per call.
CREATE TABLE IF NOT EXISTS scout_call_metrics (
    call_id               INTEGER      PRIMARY KEY REFERENCES scout_calls (id) ON DELETE CASCADE,
    token_symbol          TEXT,
    chain_name            TEXT,
    called_at_mcap_usd    NUMERIC,
    dex                   TEXT,
    mcap_usd              NUMERIC,
    liq_usd               NUMERIC,
    liq_pct               NUMERIC,                -- liquidity as % of mcap, as posted
    tax_buy_pct           NUMERIC,
    tax_sell_pct          NUMERIC,
    age_text              TEXT,                   -- as posted, e.g. "12m"
    age_seconds           INTEGER,
    launchpad             TEXT,
    holders               INTEGER,
    proof_elite           INTEGER,
    proof_good            INTEGER,
    live_buys_elite_count INTEGER      NOT NULL DEFAULT 0,
    live_buys_good_count  INTEGER      NOT NULL DEFAULT 0,
    live_buys_elite_usd   NUMERIC      NOT NULL DEFAULT 0,
    live_buys_good_usd    NUMERIC      NOT NULL DEFAULT 0,
    parsed                JSONB        NOT NULL DEFAULT '{}',  -- full parse result
    created_by            TEXT         NOT NULL,
    created_at            TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_by            TEXT         NOT NULL,
    updated_at            TIMESTAMPTZ  NOT NULL DEFAULT now()
);

-- The "Live buys" lines of a call post, categorised elite / good.
CREATE TABLE IF NOT EXISTS scout_call_live_buys (
    id                SERIAL PRIMARY KEY,
    call_id           INTEGER      NOT NULL REFERENCES scout_calls (id) ON DELETE CASCADE,
    position          INTEGER      NOT NULL,       -- order in the post (1 = first)
    tier              TEXT         NOT NULL,       -- elite | good | other
    tier_emoji        TEXT         NOT NULL DEFAULT '',
    amount_usd        NUMERIC,
    wallet_display    TEXT         NOT NULL,       -- as posted, e.g. 0x79f6…5c5d
    wallet_prefix     TEXT         NOT NULL,
    wallet_suffix     TEXT         NOT NULL,
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT scout_call_live_buys_pos_uq UNIQUE (call_id, position)
);
CREATE INDEX IF NOT EXISTS scout_call_live_buys_tier_idx ON scout_call_live_buys (tier);

-- One row per (CA, tool) request: what was sent, what came back, the verdict.
CREATE TABLE IF NOT EXISTS scout_investigations (
    id                SERIAL PRIMARY KEY,
    uuid              UUID         NOT NULL UNIQUE,
    call_id           INTEGER      REFERENCES scout_calls (id) ON DELETE SET NULL,  -- NULL for manual -scan runs
    tool_id           INTEGER      NOT NULL REFERENCES scout_investigation_tools (id),
    contract_address  TEXT         NOT NULL,
    request_text      TEXT         NOT NULL,               -- exact text sent to the bot
    requested_at      TIMESTAMPTZ  NOT NULL,
    completed_at      TIMESTAMPTZ,
    status            TEXT         NOT NULL,               -- completed | failed | timeout | rate_limited
    bot_message_ids   INTEGER[]    NOT NULL DEFAULT '{}',  -- the bot's reply message ids
    report_text       TEXT         NOT NULL DEFAULT '',    -- all reply texts, in order
    report_urls       TEXT[]       NOT NULL DEFAULT '{}',
    report_url        TEXT,                                -- canonical report link, if any
    external_id       TEXT,                                -- tool's own report id, if any
    verdict_level     TEXT         NOT NULL DEFAULT 'unknown', -- clean | caution | red_flags | unknown
    verdict_label     TEXT,
    ticker            TEXT,
    verdict_summary   TEXT,
    verdict_source    TEXT,                                -- perceptor.info | telegram_text
    details           JSONB        NOT NULL DEFAULT '{}',  -- tool-specific extras (attempts, rate_limit_waits_s, media, buttons, …)
    error             TEXT,
    created_by        TEXT         NOT NULL,
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_by        TEXT         NOT NULL,
    updated_at        TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS scout_investigations_call_idx  ON scout_investigations (call_id);
CREATE INDEX IF NOT EXISTS scout_investigations_tool_idx  ON scout_investigations (tool_id, requested_at DESC);
CREATE INDEX IF NOT EXISTS scout_investigations_ca_idx    ON scout_investigations (contract_address);
CREATE INDEX IF NOT EXISTS scout_investigations_level_idx ON scout_investigations (verdict_level, requested_at DESC);

-- One row per message bundle sent to you (or a failed attempt).
CREATE TABLE IF NOT EXISTS scout_deliveries (
    id                SERIAL PRIMARY KEY,
    uuid              UUID         NOT NULL UNIQUE,
    call_id           INTEGER      REFERENCES scout_calls (id) ON DELETE SET NULL,
    contract_address  TEXT         NOT NULL,
    target            TEXT         NOT NULL,               -- "me", @channel, chat id, "bot API chat …"
    status            TEXT         NOT NULL,               -- sent | failed
    header_text       TEXT         NOT NULL DEFAULT '',
    delivered_at      TIMESTAMPTZ,
    error             TEXT,
    created_by        TEXT         NOT NULL,
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_by        TEXT         NOT NULL,
    updated_at        TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS scout_deliveries_call_idx ON scout_deliveries (call_id);
CREATE INDEX IF NOT EXISTS scout_deliveries_ca_idx   ON scout_deliveries (contract_address);

-- Which investigation reports were attached to a delivery.
CREATE TABLE IF NOT EXISTS scout_delivery_investigations (
    delivery_id       INTEGER      NOT NULL REFERENCES scout_deliveries (id) ON DELETE CASCADE,
    investigation_id  INTEGER      NOT NULL REFERENCES scout_investigations (id) ON DELETE CASCADE,
    PRIMARY KEY (delivery_id, investigation_id)
);

-- Convenience view: investigations with their tool and originating post.
CREATE OR REPLACE VIEW scout_investigations_v AS
SELECT i.id, i.call_id, t.code AS tool, t.bot_username, i.contract_address, i.request_text,
       i.requested_at, i.completed_at, i.status, i.verdict_level, i.verdict_label, i.ticker,
       i.verdict_summary, i.report_url, i.report_text, i.error,
       c.channel_username, c.message_id AS source_message_id, c.message_date AS source_message_date,
       EXISTS (SELECT 1 FROM scout_delivery_investigations di
               JOIN scout_deliveries d ON d.id = di.delivery_id AND d.status = 'sent'
               WHERE di.investigation_id = i.id) AS delivered
FROM scout_investigations i
JOIN scout_investigation_tools t ON t.id = i.tool_id
LEFT JOIN scout_calls c ON c.id = i.call_id;

-- Convenience view: each call with its parsed post data.
CREATE OR REPLACE VIEW scout_calls_v AS
SELECT c.id AS call_id, c.channel_username, c.message_id, c.message_date, c.contract_address, c.chain, c.status,
       m.token_symbol, m.chain_name, m.called_at_mcap_usd, m.dex, m.mcap_usd, m.liq_usd, m.liq_pct,
       m.tax_buy_pct, m.tax_sell_pct, m.age_text, m.age_seconds, m.launchpad, m.holders,
       m.proof_elite, m.proof_good, m.live_buys_elite_count, m.live_buys_good_count,
       m.live_buys_elite_usd, m.live_buys_good_usd
FROM scout_calls c
LEFT JOIN scout_call_metrics m ON m.call_id = c.id;

-- Price tracking per call: pool used, entry price, schedule. One row per call.
CREATE TABLE IF NOT EXISTS scout_call_tracking (
    call_id               INTEGER      PRIMARY KEY REFERENCES scout_calls (id) ON DELETE CASCADE,
    contract_address      TEXT         NOT NULL,
    entry_at              TIMESTAMPTZ  NOT NULL,          -- when the call was posted
    priority              SMALLINT     NOT NULL DEFAULT 0, -- 0 = live call, 1 = backfilled history
    status                TEXT         NOT NULL,          -- pending | tracking | done | no_pool | error | gave_up
    pool_address          TEXT,
    pool_name             TEXT,
    pool_dex              TEXT,
    pool_created_at       TIMESTAMPTZ,
    entry_price_usd       NUMERIC,                        -- in price_unit (usd unless noted)
    entry_price_source    TEXT,                           -- onchain-v2/v3/v4, or minute | hour (GeckoTerminal candles)
    current_price_usd     NUMERIC,
    current_liquidity_usd NUMERIC,
    rugged                BOOLEAN,
    next_check_at         TIMESTAMPTZ  NOT NULL,
    last_checked_at       TIMESTAMPTZ,
    attempts              INTEGER      NOT NULL DEFAULT 0,
    error                 TEXT,
    created_at            TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ  NOT NULL DEFAULT now()
);
-- added with the on-chain price source
ALTER TABLE scout_call_tracking ADD COLUMN IF NOT EXISTS price_unit TEXT;   -- usd, or the quote asset's symbol (no USD source)
ALTER TABLE scout_call_tracking ADD COLUMN IF NOT EXISTS onchain    JSONB;  -- pool kind/id, quote asset, scan progress
CREATE INDEX IF NOT EXISTS scout_call_tracking_due_idx ON scout_call_tracking (status, priority, next_check_at);
-- Repair: an interrupted run (Ctrl+C) used to mark the call it was working on as given up.
UPDATE scout_call_tracking SET status = 'pending', next_check_at = now(), attempts = 0, error = NULL
WHERE status IN ('gave_up', 'error') AND error LIKE '%context canceled%';
CREATE INDEX IF NOT EXISTS scout_call_tracking_ca_idx  ON scout_call_tracking (contract_address);

-- Performance of a call after each horizon (1h, 1d, 3d, 7d, 30d by default).
CREATE TABLE IF NOT EXISTS scout_call_returns (
    call_id           INTEGER      NOT NULL REFERENCES scout_calls (id) ON DELETE CASCADE,
    horizon           TEXT         NOT NULL,              -- 1h | 1d | 3d | 7d | 30d | …
    horizon_seconds   INTEGER      NOT NULL,
    due_at            TIMESTAMPTZ  NOT NULL,
    status            TEXT         NOT NULL,              -- done | no_data
    price_usd         NUMERIC,                            -- close at entry + horizon
    return_pct        NUMERIC,                            -- (price / entry − 1) × 100
    max_gain_pct      NUMERIC,                            -- best high within the window vs entry
    max_drawdown_pct  NUMERIC,                            -- worst low within the window vs entry
    max_price_usd     NUMERIC,
    min_price_usd     NUMERIC,
    last_trade_at     TIMESTAMPTZ,
    computed_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    PRIMARY KEY (call_id, horizon)
);

-- Realistic entry: the pool price SCOUT_ENTRY_DELAY (60s) after the post, and returns measured from it.
ALTER TABLE scout_call_tracking ADD COLUMN IF NOT EXISTS entry_late_price_usd  NUMERIC;
ALTER TABLE scout_call_returns  ADD COLUMN IF NOT EXISTS return_late_pct       NUMERIC;
ALTER TABLE scout_call_returns  ADD COLUMN IF NOT EXISTS max_gain_late_pct     NUMERIC;  -- peak after the late entry
ALTER TABLE scout_call_returns  ADD COLUMN IF NOT EXISTS max_drawdown_late_pct NUMERIC;

-- Calls tracked before state version 2 lack the late entry, candles and pre-call
-- stats: queue them to be tracked again (their old results stay until replaced).
UPDATE scout_call_tracking SET status = 'pending', next_check_at = now(), attempts = 0, error = NULL
WHERE onchain IS NOT NULL AND COALESCE((onchain->>'v')::int, 0) < 2
  AND status IN ('done', 'tracking', 'error', 'gave_up');

-- Price path of a call: 5-minute candles for the first 24 hours (interval_seconds = 300)
-- and hourly candles for the whole tracked window (3600). Prices are in the call's
-- price_unit (scout_call_tracking.price_unit). Only buckets with trades are stored.
CREATE TABLE IF NOT EXISTS scout_call_candles (
    call_id           INTEGER      NOT NULL REFERENCES scout_calls (id) ON DELETE CASCADE,
    interval_seconds  INTEGER      NOT NULL,
    bucket_start      TIMESTAMPTZ  NOT NULL,
    open              NUMERIC      NOT NULL,
    high              NUMERIC      NOT NULL,
    low               NUMERIC      NOT NULL,
    close             NUMERIC      NOT NULL,
    events            INTEGER      NOT NULL DEFAULT 0,
    PRIMARY KEY (call_id, interval_seconds, bucket_start)
);

-- Trading in the pool during the hour before the call (features known at call time).
CREATE TABLE IF NOT EXISTS scout_call_precall (
    call_id            INTEGER      PRIMARY KEY REFERENCES scout_calls (id) ON DELETE CASCADE,
    window_seconds     INTEGER      NOT NULL,
    vol_unit           TEXT         NOT NULL,       -- usd, or the quote asset's symbol
    first_trade_age_s  INTEGER,                     -- first trade in the window → call (NULL = no trades)
    swaps_5m   INTEGER NOT NULL DEFAULT 0, swaps_15m  INTEGER NOT NULL DEFAULT 0, swaps_60m  INTEGER NOT NULL DEFAULT 0,
    buys_5m    INTEGER NOT NULL DEFAULT 0, buys_15m   INTEGER NOT NULL DEFAULT 0, buys_60m   INTEGER NOT NULL DEFAULT 0,
    sells_5m   INTEGER NOT NULL DEFAULT 0, sells_15m  INTEGER NOT NULL DEFAULT 0, sells_60m  INTEGER NOT NULL DEFAULT 0,
    buy_vol_5m  NUMERIC NOT NULL DEFAULT 0, buy_vol_15m  NUMERIC NOT NULL DEFAULT 0, buy_vol_60m  NUMERIC NOT NULL DEFAULT 0,
    sell_vol_5m NUMERIC NOT NULL DEFAULT 0, sell_vol_15m NUMERIC NOT NULL DEFAULT 0, sell_vol_60m NUMERIC NOT NULL DEFAULT 0,
    price_chg_5m_pct   NUMERIC,                     -- price at the call vs 5 minutes earlier
    price_chg_15m_pct  NUMERIC,
    price_chg_60m_pct  NUMERIC,
    computed_at        TIMESTAMPTZ  NOT NULL DEFAULT now()
);

-- Model scores. The table is shared with the first model experiment (rug_prob,
-- ret_1d_pred, pos_7d_prob: one row per scoring). The bucket model adds one row
-- per call, model version and holding-period bucket, in the columns added below.
CREATE TABLE IF NOT EXISTS scout_call_predictions (
    id                SERIAL PRIMARY KEY,
    uuid              UUID         NOT NULL UNIQUE,
    call_id           INTEGER      REFERENCES scout_calls (id) ON DELETE CASCADE,
    contract_address  TEXT         NOT NULL,
    model_version     TEXT         NOT NULL,
    rug_prob          DOUBLE PRECISION,
    ret_1d_pred       DOUBLE PRECISION,
    pos_7d_prob       DOUBLE PRECISION,
    created_by        TEXT         NOT NULL,
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT now()
);
ALTER TABLE scout_call_predictions ADD COLUMN IF NOT EXISTS bucket          TEXT;     -- short | 3day | medium | long (NULL = first experiment's rows)
ALTER TABLE scout_call_predictions ADD COLUMN IF NOT EXISTS runner_prob     NUMERIC;  -- 0..1
ALTER TABLE scout_call_predictions ADD COLUMN IF NOT EXISTS collapse_prob   NUMERIC;  -- 0..1
ALTER TABLE scout_call_predictions ADD COLUMN IF NOT EXISTS runner_rank_pct NUMERIC;  -- % of recent calls with a lower runner score
ALTER TABLE scout_call_predictions ADD COLUMN IF NOT EXISTS features        JSONB;    -- the feature row that was scored
CREATE INDEX IF NOT EXISTS scout_call_predictions_call_idx ON scout_call_predictions (call_id);
CREATE INDEX IF NOT EXISTS scout_call_predictions_ca_idx   ON scout_call_predictions (contract_address);
-- one bucket row per call and model version (rows without a bucket are not limited)
CREATE UNIQUE INDEX IF NOT EXISTS scout_call_predictions_bucket_uq ON scout_call_predictions (call_id, model_version, bucket);
CREATE INDEX IF NOT EXISTS scout_calls_ca_date_idx ON scout_calls (contract_address, message_date);

-- Training dataset: one row per call = features known at call time + outcomes.
DROP VIEW IF EXISTS scout_call_predictions_v;
DROP VIEW IF EXISTS scout_call_dataset_v;
CREATE VIEW scout_call_dataset_v AS
SELECT cv.call_id, cv.message_id, cv.message_date, cv.contract_address, cv.status AS call_status,
       cv.token_symbol, cv.dex, cv.launchpad,
       cv.called_at_mcap_usd::float8 AS called_at_mcap_usd, cv.mcap_usd::float8 AS mcap_usd,
       cv.liq_usd::float8 AS liq_usd, cv.liq_pct::float8 AS liq_pct,
       cv.tax_buy_pct::float8 AS tax_buy_pct, cv.tax_sell_pct::float8 AS tax_sell_pct,
       cv.age_seconds, cv.holders, cv.proof_elite, cv.proof_good,
       cv.live_buys_elite_count, cv.live_buys_good_count,
       cv.live_buys_elite_usd::float8 AS live_buys_elite_usd, cv.live_buys_good_usd::float8 AS live_buys_good_usd,
       lb.live_buy_max_usd,
       -- repeat calls and how busy the channel was (all known at call time)
       pc.prior_calls, pc.secs_since_prev_call, bz.calls_prev_1h, bz.calls_prev_24h,
       -- trading in the hour before the call
       x.swaps_5m AS pre_swaps_5m, x.swaps_15m AS pre_swaps_15m, x.swaps_60m AS pre_swaps_60m,
       x.buys_5m AS pre_buys_5m, x.buys_15m AS pre_buys_15m, x.buys_60m AS pre_buys_60m,
       x.sells_5m AS pre_sells_5m, x.sells_15m AS pre_sells_15m, x.sells_60m AS pre_sells_60m,
       x.buy_vol_5m::float8 AS pre_buy_vol_5m, x.buy_vol_15m::float8 AS pre_buy_vol_15m, x.buy_vol_60m::float8 AS pre_buy_vol_60m,
       x.sell_vol_5m::float8 AS pre_sell_vol_5m, x.sell_vol_15m::float8 AS pre_sell_vol_15m, x.sell_vol_60m::float8 AS pre_sell_vol_60m,
       x.price_chg_5m_pct::float8 AS pre_price_chg_5m_pct, x.price_chg_15m_pct::float8 AS pre_price_chg_15m_pct,
       x.price_chg_60m_pct::float8 AS pre_price_chg_60m_pct,
       x.first_trade_age_s AS pre_first_trade_age_s, x.vol_unit AS pre_vol_unit,
       p.verdict_level AS perceptor_verdict,
       t.status AS tracking_status, t.pool_address, t.pool_dex,
       t.entry_price_usd::float8 AS entry_price_usd, t.entry_price_source, t.price_unit,
       t.onchain->>'quote_sym' AS quote_asset,
       t.entry_late_price_usd::float8 AS entry_late_price_usd,
       t.current_liquidity_usd::float8 AS current_liquidity_usd, t.rugged,
       r.ret_1h, r.max_gain_1h, r.max_dd_1h, r.ret_late_1h, r.max_gain_late_1h, r.max_dd_late_1h,
       r.ret_1d, r.max_gain_1d, r.max_dd_1d, r.ret_late_1d, r.max_gain_late_1d, r.max_dd_late_1d,
       r.ret_3d, r.max_gain_3d, r.max_dd_3d, r.ret_late_3d, r.max_gain_late_3d, r.max_dd_late_3d,
       r.ret_7d, r.max_gain_7d, r.max_dd_7d, r.ret_late_7d, r.max_gain_late_7d, r.max_dd_late_7d,
       r.ret_30d, r.max_gain_30d, r.max_dd_30d, r.ret_late_30d, r.max_gain_late_30d, r.max_dd_late_30d
FROM scout_calls_v cv
LEFT JOIN scout_call_tracking t ON t.call_id = cv.call_id
LEFT JOIN scout_call_precall x ON x.call_id = cv.call_id
LEFT JOIN LATERAL (
    SELECT max(amount_usd)::float8 AS live_buy_max_usd FROM scout_call_live_buys WHERE call_id = cv.call_id
) lb ON true
LEFT JOIN LATERAL (
    SELECT count(*)::int AS prior_calls,
           extract(epoch FROM cv.message_date - max(c2.message_date))::int AS secs_since_prev_call
    FROM scout_calls c2
    WHERE c2.contract_address = cv.contract_address AND c2.message_date < cv.message_date
) pc ON true
LEFT JOIN LATERAL (
    SELECT count(*) FILTER (WHERE c3.message_date >= cv.message_date - interval '1 hour')::int AS calls_prev_1h,
           count(*)::int AS calls_prev_24h
    FROM scout_calls c3
    WHERE c3.message_date < cv.message_date AND c3.message_date >= cv.message_date - interval '24 hours'
) bz ON true
-- Perceptor verdict: this call's own scan, else the latest earlier scan of the same token.
LEFT JOIN LATERAL (
    SELECT i.verdict_level FROM scout_investigations i
    JOIN scout_investigation_tools tt ON tt.id = i.tool_id
    WHERE tt.code = 'perceptor' AND i.status = 'completed'
      AND (i.call_id = cv.call_id OR (i.contract_address = cv.contract_address AND i.requested_at <= cv.message_date))
    ORDER BY (i.call_id = cv.call_id) DESC, i.requested_at DESC LIMIT 1
) p ON true
LEFT JOIN LATERAL (
    SELECT
      max(return_pct)            FILTER (WHERE horizon = '1h')::float8 AS ret_1h,
      max(max_gain_pct)          FILTER (WHERE horizon = '1h')::float8 AS max_gain_1h,
      max(max_drawdown_pct)      FILTER (WHERE horizon = '1h')::float8 AS max_dd_1h,
      max(return_late_pct)       FILTER (WHERE horizon = '1h')::float8 AS ret_late_1h,
      max(max_gain_late_pct)     FILTER (WHERE horizon = '1h')::float8 AS max_gain_late_1h,
      max(max_drawdown_late_pct) FILTER (WHERE horizon = '1h')::float8 AS max_dd_late_1h,
      max(return_pct)            FILTER (WHERE horizon = '1d')::float8 AS ret_1d,
      max(max_gain_pct)          FILTER (WHERE horizon = '1d')::float8 AS max_gain_1d,
      max(max_drawdown_pct)      FILTER (WHERE horizon = '1d')::float8 AS max_dd_1d,
      max(return_late_pct)       FILTER (WHERE horizon = '1d')::float8 AS ret_late_1d,
      max(max_gain_late_pct)     FILTER (WHERE horizon = '1d')::float8 AS max_gain_late_1d,
      max(max_drawdown_late_pct) FILTER (WHERE horizon = '1d')::float8 AS max_dd_late_1d,
      max(return_pct)            FILTER (WHERE horizon = '3d')::float8 AS ret_3d,
      max(max_gain_pct)          FILTER (WHERE horizon = '3d')::float8 AS max_gain_3d,
      max(max_drawdown_pct)      FILTER (WHERE horizon = '3d')::float8 AS max_dd_3d,
      max(return_late_pct)       FILTER (WHERE horizon = '3d')::float8 AS ret_late_3d,
      max(max_gain_late_pct)     FILTER (WHERE horizon = '3d')::float8 AS max_gain_late_3d,
      max(max_drawdown_late_pct) FILTER (WHERE horizon = '3d')::float8 AS max_dd_late_3d,
      max(return_pct)            FILTER (WHERE horizon = '7d')::float8 AS ret_7d,
      max(max_gain_pct)          FILTER (WHERE horizon = '7d')::float8 AS max_gain_7d,
      max(max_drawdown_pct)      FILTER (WHERE horizon = '7d')::float8 AS max_dd_7d,
      max(return_late_pct)       FILTER (WHERE horizon = '7d')::float8 AS ret_late_7d,
      max(max_gain_late_pct)     FILTER (WHERE horizon = '7d')::float8 AS max_gain_late_7d,
      max(max_drawdown_late_pct) FILTER (WHERE horizon = '7d')::float8 AS max_dd_late_7d,
      max(return_pct)            FILTER (WHERE horizon = '30d')::float8 AS ret_30d,
      max(max_gain_pct)          FILTER (WHERE horizon = '30d')::float8 AS max_gain_30d,
      max(max_drawdown_pct)      FILTER (WHERE horizon = '30d')::float8 AS max_dd_30d,
      max(return_late_pct)       FILTER (WHERE horizon = '30d')::float8 AS ret_late_30d,
      max(max_gain_late_pct)     FILTER (WHERE horizon = '30d')::float8 AS max_gain_late_30d,
      max(max_drawdown_late_pct) FILTER (WHERE horizon = '30d')::float8 AS max_dd_late_30d
    FROM scout_call_returns WHERE call_id = cv.call_id AND status = 'done'
) r ON true;

-- Scores next to what really happened (for a website or a live-accuracy check).
CREATE VIEW scout_call_predictions_v AS
SELECT pr.call_id, pr.model_version, pr.bucket, pr.runner_prob::float8 AS runner_prob,
       pr.collapse_prob::float8 AS collapse_prob, pr.runner_rank_pct::float8 AS runner_rank_pct, pr.created_at AS scored_at,
       d.message_id, d.message_date, d.contract_address, d.token_symbol, d.price_unit, d.rugged,
       d.ret_late_1d, d.max_gain_late_1d, d.ret_late_3d, d.ret_late_7d, d.ret_late_30d
FROM scout_call_predictions pr
JOIN scout_call_dataset_v d ON d.call_id = pr.call_id
WHERE pr.bucket IS NOT NULL;

-- One-time upgrade from the earlier single-tool table (scout_scan_reports), if present.
-- The old table is left in place; drop it yourself once you're happy.
DO $$
BEGIN
    IF to_regclass('public.scout_scan_reports') IS NOT NULL THEN
        INSERT INTO scout_investigation_tools (code, name, bot_username, command_template, parser, is_gate, created_by, updated_by)
        VALUES ('perceptor', 'Perceptor', 'perceptor0xBot', '/scan {ca}', 'perceptor', true, 'scoutanalytics', 'scoutanalytics')
        ON CONFLICT (code) DO NOTHING;

        INSERT INTO scout_investigations (uuid, call_id, tool_id, contract_address, request_text, requested_at,
            completed_at, status, bot_message_ids, report_text, report_urls, report_url, external_id,
            verdict_level, verdict_label, ticker, verdict_summary, verdict_source, error,
            created_by, created_at, updated_by, updated_at)
        SELECT r.uuid, r.call_id, t.id, r.contract_address, r.scan_command, r.requested_at,
            r.completed_at,
            CASE WHEN r.bot_message_id IS NULL THEN 'failed' ELSE 'completed' END,
            CASE WHEN r.bot_message_id IS NULL THEN '{}'::INTEGER[] ELSE ARRAY[r.bot_message_id] END,
            r.report_text, r.report_urls, r.perceptor_url, r.perceptor_id,
            r.verdict_level, r.verdict_label, r.ticker, r.verdict_summary, r.verdict_source, r.error,
            r.created_by, r.created_at, r.updated_by, r.updated_at
        FROM scout_scan_reports r
        JOIN scout_investigation_tools t ON t.code = 'perceptor'
        WHERE NOT EXISTS (SELECT 1 FROM scout_investigations i WHERE i.uuid = r.uuid);

        INSERT INTO scout_deliveries (uuid, call_id, contract_address, target, status, delivered_at,
            created_by, created_at, updated_by, updated_at)
        SELECT r.uuid, r.call_id, r.contract_address, COALESCE(r.deliver_target, ''), 'sent', r.delivered_at,
            r.created_by, r.created_at, r.updated_by, r.updated_at
        FROM scout_scan_reports r
        WHERE r.delivered AND NOT EXISTS (SELECT 1 FROM scout_deliveries d WHERE d.uuid = r.uuid);

        INSERT INTO scout_delivery_investigations (delivery_id, investigation_id)
        SELECT d.id, i.id FROM scout_deliveries d JOIN scout_investigations i ON i.uuid = d.uuid
        ON CONFLICT DO NOTHING;
    END IF;
END
$$;
