-- =============================================================================
-- A_count.sql  (step 2 of README.md)  READ-ONLY: changes nothing.
--
-- Shows which calls B_fill.sql would give a price-rule rug time
-- (rug_at_kind 'price'). Run the whole file (F5). Nothing to edit in this
-- file. The result is ONE grid, one row per (section, item); the "expected"
-- column says what each row should show.
--
--   section 'selected'  the calls B fills, by where their time comes from
--                       (src: candle, horizon, last_due), with the earliest
--                       and latest time after the post (rug_at - entry_at)
--   section 'TOTAL'     the number to type into B_fill.sql: its "calls" value
--   section 'check'     selected calls whose time is before the post (0)
--   section 'other'     rugged on-chain calls, ANY status, no rug block, no
--                       rug time, entry price above 0, whose last price is
--                       known and at or above 5 % of the entry (class 5
--                       below). Three rows that add up: 'status done' +
--                       'status not done' (expected 2: calls 9947 and 9948
--                       are now 'repeat' rows) = 'total' (expected 29).
--                       If 'total' is not 29, STOP: do not run B, send this
--                       whole grid to the PM.
--   section 'left out'  what the filters leave out, for the PM (0 unless the
--                       "expected" column says otherwise)
--   section 'base'      every rugged call with no rug time; the selected
--                       (TOTAL), other (total) and every 'left out' row above
--                       it add up to it
--   section 'kinds'     every rugged call, by its rug_at_kind (for the PM)
--
-- Base set: EVERY rugged call with no rug time (rug_at and rug_at_kind both
-- NULL), any status. Each base row falls in exactly ONE class, tested in this
-- order:
--   1. not priced on-chain (entry_price_source not
--      'onchain-...') or no on-chain state            -> left out
--   2. a rug block in the state (rug_block above 0)   -> left out
--   3. entry price NULL or 0                          -> left out
--   4. last price (current_price_usd) NULL            -> left out
--   5. last price at or above 5 % of the entry        -> other (any status)
--   6. on-chain state version under 2                 -> left out
--   7. status not 'done'                              -> left out
--   8. no done horizon (no time can be given)         -> left out
--   9. everything else: the price rule, as the tracker
--      applies it (last price strictly under 5 % of
--      the entry)                                     -> selected
-- B_fill.sql selects exactly class 9: its WHERE clause is the base set with
-- classes 1 to 8 ruled out.
--
-- The time (columns at, src, no_done) is the tracker's own query
-- (priceRugAtSQL in scout_models.data.go), copied as it is; only $1 became
-- t.call_id and $2 became t.entry_price_usd. Do not change it here.
-- =============================================================================

WITH base AS (
  SELECT t.call_id, t.status, t.entry_at, t.entry_price_usd, t.entry_price_source,
         t.current_price_usd, t.onchain
  FROM scout_call_tracking t
  WHERE t.rugged IS TRUE
    AND t.rug_at IS NULL AND t.rug_at_kind IS NULL),
cls AS (
  SELECT t.call_id, t.status, t.entry_at, p.at, p.src,
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
  SELECT 1 AS ord, 'selected' AS section, src AS item, count(*) AS calls,
         min(at - entry_at) AS min_after_entry, max(at - entry_at) AS max_after_entry,
         'for the PM' AS expected
  FROM cls WHERE cls = 'selected' GROUP BY src
  UNION ALL
  SELECT 2, 'TOTAL', 'selected = expected_count for B_fill.sql', count(*),
         min(at - entry_at), max(at - entry_at), 'type this into B_fill.sql'
  FROM cls WHERE cls = 'selected'
  UNION ALL
  SELECT 3, 'check', 'selected with a time before the post (rug_at < entry_at)', count(*),
         NULL, NULL, '0'
  FROM cls WHERE cls = 'selected' AND at < entry_at
  UNION ALL
  SELECT 4, 'other', 'status done', count(*), NULL, NULL, '27 (the total less the row below)'
  FROM cls WHERE cls = 'other' AND status = 'done'
  UNION ALL
  SELECT 5, 'other', 'status not done', count(*), NULL, NULL, '2 (9947, 9948: now repeat rows)'
  FROM cls WHERE cls = 'other' AND status IS DISTINCT FROM 'done'
  UNION ALL
  SELECT 6, 'other', 'total (on-chain, no rug block, last price at or above 5 % of the entry, any status)', count(*),
         NULL, NULL, '29: if not, STOP and send this grid to the PM'
  FROM cls WHERE cls = 'other'
  UNION ALL
  SELECT 7, 'left out', 'last price NULL', count(*), NULL, NULL, '0'
  FROM cls WHERE cls = 'no_last_price'
  UNION ALL
  SELECT 8, 'left out', 'entry price NULL or 0', count(*), NULL, NULL, '0'
  FROM cls WHERE cls = 'no_entry_price'
  UNION ALL
  SELECT 9, 'left out', 'on-chain state version under 2', count(*), NULL, NULL, '0'
  FROM cls WHERE cls = 'v_under_2'
  UNION ALL
  SELECT 10, 'left out', 'price rule, but status is not done', count(*), NULL, NULL, '0'
  FROM cls WHERE cls = 'not_done'
  UNION ALL
  SELECT 11, 'left out', 'price rule, but no done horizon (no time to give)', count(*), NULL, NULL, '0'
  FROM cls WHERE cls = 'no_done_horizon'
  UNION ALL
  SELECT 12, 'left out', 'GeckoTerminal-priced (not on-chain) or no on-chain state, any status', count(*), NULL, NULL,
         'any: keeps no time (the tracker gives none either)'
  FROM cls WHERE cls = 'not_onchain'
  UNION ALL
  SELECT 13, 'left out', 'a rug block in the state, any status', count(*), NULL, NULL,
         'any: -backfill-timing gives them their drained time'
  FROM cls WHERE cls = 'rug_block'
  UNION ALL
  SELECT 14, 'base', 'every rugged call with no rug time (= TOTAL + other total + the left out rows above)', count(*),
         NULL, NULL, 'the sum'
  FROM cls
  UNION ALL
  SELECT 15, 'left out', 'rugged with only one of rug_at / rug_at_kind set (not in base)', count(*), NULL, NULL, '0'
  FROM scout_call_tracking t
  WHERE t.rugged IS TRUE AND (t.rug_at IS NULL) <> (t.rug_at_kind IS NULL)
  UNION ALL
  SELECT 16, 'kinds', COALESCE(t.rug_at_kind, '(no rug time)'), count(*), NULL, NULL, 'for the PM'
  FROM scout_call_tracking t
  WHERE t.rugged IS TRUE
  GROUP BY t.rug_at_kind)
SELECT section, item, calls, min_after_entry, max_after_entry, expected
FROM grid
ORDER BY ord, calls DESC, item;
