-- =============================================================================
-- A_count.sql  (step 2 of README.txt)  READ-ONLY: changes nothing.
--
-- Shows which calls B_reset.sql would reset. Run the whole file (F5).
-- The result is ONE grid: one row per pool_dex, then a TOTAL row.
-- Paste the whole grid to the PM. The number to type into B_reset.sql is
-- the "selected" column of the TOTAL row.
--
-- Selection (the same text is in B_reset.sql and C_check.sql):
--   the FIRST call of each token (address compared without regard to case,
--   earliest message_date, lowest id on ties, update posts never count),
--   tracking status 'tracking', 'done' or 'error', AND either
--   * liquidity:   current_liquidity_usd < 2 x quote_side_usd
--                  (current_liquidity_usd stores 2 x the pool's quote side;
--                   NULL liquidity is not selected by this rule), or
--   * impossible:  a stored return / peak (call or late entry) > 100,000 %,
--                  or latest_return_pct > 100,000 %,
--                  or the 2^128 fingerprint in the on-chain state
--                  (log10(price) - (token_dec - quote_dec) > 38).
--
-- Columns:
--   selected          calls B would reset (TOTAL row: type this into B)
--   liquidity_only / impossible_only / both   the reason, split three ways
--   ret_over_1e5 / latest_over_1e5 / fp_2pow128   the impossible sub-reasons
--   tracking / done / error   current tracking status
--   live / backfilled  priority 0 / other
--   rugged_now        rugged = true today
--   returns_rows / candle_rows / precall_rows   rows B would delete
-- =============================================================================

WITH params AS (SELECT 500::numeric AS quote_side_usd),   -- <<< rug threshold: $ on the pool's quote side
fc AS (SELECT DISTINCT ON (lower(contract_address)) id FROM scout_calls
       WHERE post_kind IS DISTINCT FROM 'update'
       ORDER BY lower(contract_address), message_date, id),
flags AS (
  SELECT t.call_id, t.status, t.priority, t.pool_dex, t.rugged,
         COALESCE(t.current_liquidity_usd < 2 * p.quote_side_usd, false) AS liq,
         EXISTS (SELECT 1 FROM scout_call_returns r WHERE r.call_id = t.call_id
                 AND (r.return_pct > 1e5 OR r.return_late_pct > 1e5
                      OR r.max_gain_pct > 1e5 OR r.max_gain_late_pct > 1e5)) AS ret_big,
         COALESCE(t.latest_return_pct > 1e5, false) AS latest_big,
         (jsonb_typeof(t.onchain) = 'object' AND EXISTS (
            SELECT 1 FROM unnest(ARRAY['entry_price_q','last_price_q','run_max_q','run_min_q',
                                       'late_price_q','run_max_late_q','run_min_late_q','latest_price_q']) AS k(key)
            WHERE CASE WHEN jsonb_typeof(t.onchain -> k.key) IS DISTINCT FROM 'number' THEN false
                       WHEN (t.onchain ->> k.key)::numeric <= 0 THEN false
                       ELSE log((t.onchain ->> k.key)::numeric)
                            - (CASE WHEN jsonb_typeof(t.onchain -> 'token_dec') = 'number'
                                    THEN (t.onchain ->> 'token_dec')::numeric ELSE 0 END
                               - CASE WHEN jsonb_typeof(t.onchain -> 'quote_dec') = 'number'
                                      THEN (t.onchain ->> 'quote_dec')::numeric ELSE 0 END) > 38
                  END)) AS fp
  FROM scout_call_tracking t
  JOIN fc ON fc.id = t.call_id
  CROSS JOIN params p
  WHERE t.status IN ('tracking','done','error')),
sel AS (
  SELECT f.*, (f.ret_big OR f.latest_big OR f.fp) AS imp,
         (SELECT count(*) FROM scout_call_returns r WHERE r.call_id = f.call_id) AS n_ret,
         (SELECT count(*) FROM scout_call_candles c WHERE c.call_id = f.call_id) AS n_candle,
         (SELECT count(*) FROM scout_call_precall q WHERE q.call_id = f.call_id) AS n_pre
  FROM flags f
  WHERE f.liq OR f.ret_big OR f.latest_big OR f.fp)
SELECT CASE WHEN GROUPING(pool_dex) = 1 THEN 'TOTAL'
            ELSE COALESCE(pool_dex, '(no pool_dex)') END     AS pool_dex,
       count(*)                                              AS selected,
       count(*) FILTER (WHERE liq AND NOT imp)               AS liquidity_only,
       count(*) FILTER (WHERE imp AND NOT liq)               AS impossible_only,
       count(*) FILTER (WHERE liq AND imp)                   AS both,
       count(*) FILTER (WHERE ret_big)                       AS ret_over_1e5,
       count(*) FILTER (WHERE latest_big)                    AS latest_over_1e5,
       count(*) FILTER (WHERE fp)                            AS fp_2pow128,
       count(*) FILTER (WHERE status = 'tracking')           AS tracking,
       count(*) FILTER (WHERE status = 'done')               AS done,
       count(*) FILTER (WHERE status = 'error')              AS error,
       count(*) FILTER (WHERE priority = 0)                  AS live,
       count(*) FILTER (WHERE priority <> 0)                 AS backfilled,
       count(*) FILTER (WHERE rugged)                        AS rugged_now,
       COALESCE(sum(n_ret), 0)                               AS returns_rows,
       COALESCE(sum(n_candle), 0)                            AS candle_rows,
       COALESCE(sum(n_pre), 0)                               AS precall_rows
FROM sel
GROUP BY ROLLUP (pool_dex)
ORDER BY GROUPING(pool_dex), count(*) DESC, pool_dex;
