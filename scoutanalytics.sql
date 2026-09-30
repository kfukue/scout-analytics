-- scoutanalytics schema (PostgreSQL 12+). Idempotent: safe to run on every start.
-- Applied automatically when SCOUT_DATABASE_URL is set
-- (disable with SCOUT_DB_AUTO_MIGRATE=false and run this file by hand instead).
--
--   scout_calls ──< scout_investigations >── scout_investigation_tools
--        │                  │
--        └──< scout_deliveries ──< scout_delivery_investigations
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
