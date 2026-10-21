-- =============================================================================
-- C_check.sql  (steps 5 and 7 of README.md)  READ-ONLY: changes nothing.
--
-- Shows the state of the calls B_fill.sql filled, after B and after
-- R_rollback.sql. Needs the backup table
-- scout_call_tracking_bak_price_rug_fill_20261020 (run it before the DROP
-- TABLE at the end of README.md). Nothing to edit. Run the whole file (F5).
--
-- ONE grid, one row per (section, item). Columns: calls (all), then the same
-- split by where B's time came from (candle, horizon, last_due; empty on the
-- rows that are not about backed-up calls), and what to expect after B and
-- after R (README.md "What C shows").
--
--   section 'backup'     the rows B saved (the calls it filled)
--   section 'state'      what those calls have now, one class each:
--                          as B left it  'price' with B's time
--                          drained       event, at_call or detected: the
--                                        tracker found the pool drained later
--                                        and replaced the approximate time
--                          no rug time   R restored it (or the call's state
--                                        started again)
--                          anything else another time or kind, or the row is
--                                        gone
--   section 'info'       backed-up calls whose row the tracker rewrote since B
--                        (updated_at moved: tracked again)
--   section 'sanity'     on the rows as B left them: a time after the last
--                        done horizon's due time, or before the post (0 both)
--   section 'selection'  calls B would select now (A_count.sql's "selected")
--   section 'other'      A_count.sql's three "other" rows: status done +
--                        status not done (2) = total (29), all unchanged
--   section 'left out'   A_count.sql's "last price NULL" row (0)
--   section 'kinds'      every rugged call, by its rug_at_kind
--
-- The selection, other and left-out rows use A_count.sql's base set and
-- classes, with the same text (see A_count.sql's header).
--
-- The time in the selection is the tracker's own query (priceRugAtSQL in
-- scout_models.data.go), copied as it is; only $1 became t.call_id and $2
-- became t.entry_price_usd. Do not change it here.
-- =============================================================================

WITH bak AS (
  SELECT b.call_id, b.src, b.new_rug_at, b.old_updated_at,
         t.call_id IS NOT NULL AS present, t.rug_at, t.rug_at_kind, t.entry_at, t.updated_at,
         CASE WHEN t.call_id IS NULL THEN 'else'
              WHEN t.rug_at_kind = 'price' AND t.rug_at = b.new_rug_at THEN 'as_b'
              WHEN t.rug_at_kind IN ('event', 'at_call', 'detected') THEN 'drained'
              WHEN t.rug_at IS NULL AND t.rug_at_kind IS NULL THEN 'cleared'
              ELSE 'else' END AS state
  FROM scout_call_tracking_bak_price_rug_fill_20261020 b
  LEFT JOIN scout_call_tracking t ON t.call_id = b.call_id),
base AS (
  SELECT t.call_id, t.status, t.entry_at, t.entry_price_usd, t.entry_price_source,
         t.current_price_usd, t.onchain
  FROM scout_call_tracking t
  WHERE t.rugged IS TRUE
    AND t.rug_at IS NULL AND t.rug_at_kind IS NULL),
cls AS (
  SELECT t.call_id, t.status,
         CASE WHEN t.entry_price_source IS NULL OR t.entry_price_source NOT LIKE 'onchain-%'
                   OR t.onchain IS NULL THEN 'not_onchain'
              WHEN COALESCE((t.onchain->>'rug_block')::numeric, 0) > 0 THEN 'rug_block'
              WHEN t.entry_price_usd IS NULL OR t.entry_price_usd <= 0 THEN 'no_entry_price'
              WHEN t.current_price_usd IS NULL THEN 'no_last_price'
              WHEN t.current_price_usd >= t.entry_price_usd * 0.05 THEN 'other'
              WHEN COALESCE((t.onchain->>'v')::int, 0) < 2 THEN 'v_under_2'
              WHEN t.status IS DISTINCT FROM 'done' THEN 'not_done'
              WHEN p.no_done OR p.at IS NULL THEN 'no_done_horizon'
              ELSE 'selected' END AS cls
  FROM base t
  -- BEGIN copy of priceRugAtSQL (scout_models.data.go): $1 = t.call_id, $2 = t.entry_price_usd
  CROSS JOIN LATERAL (
WITH last AS (
	SELECT max(due_at) AS due FROM scout_call_returns WHERE call_id = t.call_id AND status = 'done'
), candle AS (
	SELECT LEAST(c.bucket_start + c.interval_seconds * interval '1 second', last.due) AS at
	  FROM scout_call_candles c, last
	 WHERE c.call_id = t.call_id AND c.interval_seconds = 3600 AND c.bucket_start < last.due
	   AND c.close < t.entry_price_usd::numeric * 0.05
	 ORDER BY c.bucket_start LIMIT 1
), horizon AS (
	SELECT min(r.due_at) AS at FROM scout_call_returns r
	 WHERE r.call_id = t.call_id AND r.status = 'done' AND r.price_usd < t.entry_price_usd::numeric * 0.05
)
SELECT COALESCE((SELECT at FROM candle), horizon.at, last.due),
       CASE WHEN EXISTS (SELECT 1 FROM candle) THEN 'candle'
            WHEN horizon.at IS NOT NULL THEN 'horizon'
            ELSE 'last_due' END,
       last.due IS NULL
  FROM last, horizon
  ) AS p(at, src, no_done)
  -- END copy of priceRugAtSQL
),
grid AS (
  SELECT 1 AS ord, 'backup' AS section, 'calls B filled (rows in the backup table)' AS item,
         count(*) AS calls,
         count(*) FILTER (WHERE src = 'candle') AS candle,
         count(*) FILTER (WHERE src = 'horizon') AS horizon,
         count(*) FILTER (WHERE src = 'last_due') AS last_due,
         'B''s TOTAL, always' AS after_b, 'B''s TOTAL, always' AS after_r
  FROM bak
  UNION ALL
  SELECT 2, 'state', 'as B left it: ''price'' with B''s time', count(*),
         count(*) FILTER (WHERE src = 'candle'), count(*) FILTER (WHERE src = 'horizon'),
         count(*) FILTER (WHERE src = 'last_due'),
         'the backup rows, less "drained"', '0'
  FROM bak WHERE state = 'as_b'
  UNION ALL
  SELECT 3, 'state', 'drained: event, at_call or detected (found by the tracker later)', count(*),
         count(*) FILTER (WHERE src = 'candle'), count(*) FILTER (WHERE src = 'horizon'),
         count(*) FILTER (WHERE src = 'last_due'),
         '0 at first, may grow', 'as before R (R leaves them)'
  FROM bak WHERE state = 'drained'
  UNION ALL
  SELECT 4, 'state', 'no rug time (restored by R, or its state started again)', count(*),
         count(*) FILTER (WHERE src = 'candle'), count(*) FILTER (WHERE src = 'horizon'),
         count(*) FILTER (WHERE src = 'last_due'),
         '0', 'the rows R restored'
  FROM bak WHERE state = 'cleared'
  UNION ALL
  SELECT 5, 'state', 'anything else (another time or kind, or the row is gone)', count(*),
         count(*) FILTER (WHERE src = 'candle'), count(*) FILTER (WHERE src = 'horizon'),
         count(*) FILTER (WHERE src = 'last_due'),
         '0', '0'
  FROM bak WHERE state = 'else'
  UNION ALL
  SELECT 6, 'info', 'backed-up calls tracked again since B (updated_at moved)', count(*),
         count(*) FILTER (WHERE src = 'candle'), count(*) FILTER (WHERE src = 'horizon'),
         count(*) FILTER (WHERE src = 'last_due'),
         '0 (rarely more)', 'as after B'
  FROM bak WHERE present AND updated_at IS DISTINCT FROM old_updated_at
  UNION ALL
  SELECT 7, 'sanity', 'as B left it, but the time is after the last done horizon''s due time', count(*),
         NULL, NULL, NULL, '0', '0'
  FROM bak
  WHERE state = 'as_b'
    AND rug_at > (SELECT max(r.due_at) FROM scout_call_returns r WHERE r.call_id = bak.call_id AND r.status = 'done')
  UNION ALL
  SELECT 8, 'sanity', 'as B left it, but the time is before the post (rug_at < entry_at)', count(*),
         NULL, NULL, NULL, '0', '0'
  FROM bak WHERE state = 'as_b' AND rug_at < entry_at
  UNION ALL
  SELECT 9, 'selection', 'calls B would select now (A_count.sql "selected")', count(*),
         NULL, NULL, NULL, '0', 'the rows R restored'
  FROM cls WHERE cls = 'selected'
  UNION ALL
  SELECT 10, 'other', 'status done', count(*),
         NULL, NULL, NULL, '27 (unchanged)', '27 (unchanged)'
  FROM cls WHERE cls = 'other' AND status = 'done'
  UNION ALL
  SELECT 11, 'other', 'status not done', count(*),
         NULL, NULL, NULL, '2 (unchanged)', '2 (unchanged)'
  FROM cls WHERE cls = 'other' AND status IS DISTINCT FROM 'done'
  UNION ALL
  SELECT 12, 'other', 'total (on-chain, no rug block, last price at or above 5 % of the entry, any status)', count(*),
         NULL, NULL, NULL, '29 (unchanged)', '29 (unchanged)'
  FROM cls WHERE cls = 'other'
  UNION ALL
  SELECT 13, 'left out', 'last price NULL (A_count.sql "left out")', count(*),
         NULL, NULL, NULL, '0', '0'
  FROM cls WHERE cls = 'no_last_price'
  UNION ALL
  SELECT 14, 'kinds', COALESCE(t.rug_at_kind, '(no rug time)'), count(*),
         NULL, NULL, NULL, '''price'' up by the backup rows', 'as before B'
  FROM scout_call_tracking t
  WHERE t.rugged IS TRUE
  GROUP BY t.rug_at_kind)
SELECT section, item, calls, candle, horizon, last_due, after_b, after_r
FROM grid
ORDER BY ord, calls DESC, item;
