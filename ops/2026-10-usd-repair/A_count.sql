-- =============================================================================
-- A_count.sql  (step 2 of README.md)  READ-ONLY: changes nothing.
--
-- Shows which calls B_reset.sql would reset. Run the whole file (F5).
-- Nothing to edit in this file (it has no EDIT line). The only optional
-- setting is the OPTION line near the top (include_gap_heuristic); if you
-- change it, change it the same way in B_reset.sql and C_check.sql.
--
-- The result is ONE grid, one row per (section, item):
--   section 'reason'     the reason(s) a call is selected, as exclusive
--                        combinations (a call with two reasons is counted
--                        once, under e.g. 'quote_units+candle_error'), so
--                        these rows add up to TOTAL
--   section 'price_unit' the selected calls by their current price_unit
--   section 'status'     the selected calls by their current tracking status
--   section 'TOTAL'      the number to type into B_reset.sql: its "selected"
--                        value
--   section 'info'       NOT part of the selection, for the PM:
--                        * gap_heuristic_candidates: calls rule (c) finds
--                          (counted even while include_gap_heuristic is
--                          false; only selected when it is true)
--                        * E_entry_usd_errors: what E_nudge_entry_errors.sql
--                          would nudge (status 'error')
--                        * E_entry_usd_errors_gave_up: the same errors on
--                          'gave_up' rows (E does not touch them)
-- Columns returns_rows / candle_rows / precall_rows: rows B would delete.
--
-- Selection (the same text is in B_reset.sql and C_check.sql):
--   the FIRST call of each token (address compared without regard to case,
--   earliest message_date, lowest id on ties, update posts never count),
--   status not 'repeat', AND any of
--   (a) quote_units:  price_unit set and not 'usd', status tracking/done/error
--   (b) candle_error: error starts with 'candles to +' and contains
--                     'no USD source' or 'no trades in the' (any status but
--                     'repeat'; such rows are often 'gave_up')
--   (c) candle_gap:   ONLY when include_gap_heuristic is true: status
--                     tracking/done/error, on-chain state present, and a
--                     scout_call_returns.last_trade_at whose UTC hour has no
--                     hourly candle (interval_seconds 3600). A heuristic with
--                     false positives: see README.md.
-- =============================================================================

WITH params AS (SELECT false AS include_gap_heuristic),   -- OPTION: true adds rule (c); same value in A, B and C
fc AS (SELECT DISTINCT ON (lower(contract_address)) id FROM scout_calls
       WHERE post_kind IS DISTINCT FROM 'update'
       ORDER BY lower(contract_address), message_date, id),
flags AS (
  SELECT t.call_id, t.status, t.price_unit,
         (t.status IN ('tracking','done','error')
          AND t.price_unit IS NOT NULL AND t.price_unit <> 'usd') AS a_unit,
         COALESCE(t.error LIKE 'candles to +%'
                  AND (t.error LIKE '%no USD source%' OR t.error LIKE '%no trades in the%'), false) AS b_err,
         (t.status IN ('tracking','done','error') AND t.onchain IS NOT NULL AND EXISTS (
            SELECT 1 FROM scout_call_returns r
            WHERE r.call_id = t.call_id AND r.last_trade_at IS NOT NULL
              AND NOT EXISTS (SELECT 1 FROM scout_call_candles c
                              WHERE c.call_id = r.call_id AND c.interval_seconds = 3600
                                AND c.bucket_start = date_trunc('hour', r.last_trade_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'))) AS c_gap,
         p.include_gap_heuristic AS use_gap
  FROM scout_call_tracking t
  JOIN fc ON fc.id = t.call_id
  CROSS JOIN params p
  WHERE t.status <> 'repeat'),
sel AS (
  SELECT f.*,
         concat_ws('+', CASE WHEN f.a_unit THEN 'quote_units' END,
                        CASE WHEN f.b_err THEN 'candle_error' END,
                        CASE WHEN f.use_gap AND f.c_gap THEN 'candle_gap' END) AS reason,
         (SELECT count(*) FROM scout_call_returns r WHERE r.call_id = f.call_id) AS n_ret,
         (SELECT count(*) FROM scout_call_candles c WHERE c.call_id = f.call_id) AS n_candle,
         (SELECT count(*) FROM scout_call_precall q WHERE q.call_id = f.call_id) AS n_pre
  FROM flags f
  WHERE f.a_unit OR f.b_err OR (f.use_gap AND f.c_gap)),
grid AS (
  SELECT 1 AS ord, 'reason' AS section, reason AS item, count(*) AS selected,
         sum(n_ret) AS returns_rows, sum(n_candle) AS candle_rows, sum(n_pre) AS precall_rows
  FROM sel GROUP BY reason
  UNION ALL
  SELECT 2, 'price_unit', COALESCE(price_unit, '(none)'), count(*), sum(n_ret), sum(n_candle), sum(n_pre)
  FROM sel GROUP BY price_unit
  UNION ALL
  SELECT 3, 'status', status, count(*), sum(n_ret), sum(n_candle), sum(n_pre)
  FROM sel GROUP BY status
  UNION ALL
  SELECT 4, 'TOTAL', 'selected = expected_count for B_reset.sql', count(*),
         COALESCE(sum(n_ret), 0), COALESCE(sum(n_candle), 0), COALESCE(sum(n_pre), 0)
  FROM sel
  UNION ALL
  SELECT 5, 'info', 'gap_heuristic_candidates (selected only if include_gap_heuristic = true)',
         count(*), NULL, NULL, NULL
  FROM flags WHERE c_gap
  UNION ALL
  SELECT 6, 'info', 'E_entry_usd_errors (what E_nudge_entry_errors.sql nudges)', count(*), NULL, NULL, NULL
  FROM scout_call_tracking
  WHERE status = 'error' AND error LIKE 'USD price of %'
    AND (error LIKE '%no USD source%' OR error LIKE '%no trades in the%')
  UNION ALL
  SELECT 7, 'info', 'E_entry_usd_errors_gave_up (same errors on gave_up rows; E does not touch them)',
         count(*), NULL, NULL, NULL
  FROM scout_call_tracking
  WHERE status = 'gave_up' AND error LIKE 'USD price of %'
    AND (error LIKE '%no USD source%' OR error LIKE '%no trades in the%'))
SELECT section, item, selected, returns_rows, candle_rows, precall_rows
FROM grid
ORDER BY ord, selected DESC, item;
